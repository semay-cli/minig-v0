package acp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
)

// Server speaks the Agent Client Protocol over newline-delimited JSON-RPC 2.0.
type Server struct {
	cfg      config.Config
	provider llm.Provider
	in       io.Reader
	out      io.Writer

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendMu  sync.Mutex
	pending map[string]chan inbound

	capMu sync.Mutex
	caps  clientCaps

	sessMu   sync.Mutex
	sessions map[string]*session
}

type clientCaps struct {
	Read     bool
	Write    bool
	Terminal bool
}

type session struct {
	id     string
	cwd    string
	mu     sync.Mutex
	cancel context.CancelFunc
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type inbound struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcResult struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcCall struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcNote struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// New constructs an ACP server. stdout is reserved for JSON-RPC messages.
func New(cfg config.Config, provider llm.Provider, in io.Reader, out io.Writer) *Server {
	return &Server{
		cfg:      cfg,
		provider: provider,
		in:       in,
		out:      out,
		pending:  make(map[string]chan inbound),
		sessions: make(map[string]*session),
	}
}

// Serve reads JSON-RPC requests until stdin closes or ctx is cancelled.
func (s *Server) Serve(ctx context.Context) error {
	sc := bufio.NewScanner(s.in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg inbound
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			_ = s.writeMsg(rpcResult{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if msg.Method == "" && len(msg.ID) > 0 && string(msg.ID) != "null" {
			s.deliver(msg)
			continue
		}
		if msg.Method == "session/cancel" {
			s.cancelSession(msg.Params)
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		go s.handle(ctx, msg)
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func (s *Server) handle(ctx context.Context, msg inbound) {
	switch msg.Method {
	case "initialize":
		s.capMu.Lock()
		s.caps = parseCaps(msg.Params)
		s.capMu.Unlock()
		_ = s.reply(msg.ID, map[string]any{
			"protocolVersion": 1,
			"agentCapabilities": map[string]any{
				"loadSession": false,
				"promptCapabilities": map[string]any{
					"image":           false,
					"audio":           false,
					"embeddedContext": true,
				},
			},
			"agentInfo":   map[string]any{"name": "minig", "version": "0.1.0"},
			"authMethods": []any{},
		})
	case "session/new":
		cwd := s.cfg.WorkspaceRoot
		var params struct {
			Cwd string `json:"cwd"`
		}
		_ = json.Unmarshal(msg.Params, &params)
		if strings.TrimSpace(params.Cwd) != "" {
			cwd = params.Cwd
		}
		if strings.TrimSpace(cwd) == "" {
			_ = s.fail(msg.ID, -32602, "cwd is required")
			return
		}
		id := newID("sess")
		s.sessMu.Lock()
		s.sessions[id] = &session{id: id, cwd: cwd}
		s.sessMu.Unlock()
		_ = s.reply(msg.ID, map[string]any{"sessionId": id})
	case "session/prompt":
		s.prompt(ctx, msg)
	case "session/cancel":
		_ = s.reply(msg.ID, map[string]any{})
	default:
		_ = s.fail(msg.ID, -32601, "method not found")
	}
}

func (s *Server) prompt(parent context.Context, msg inbound) {
	var params struct {
		SessionID string          `json:"sessionId"`
		Prompt    json.RawMessage `json:"prompt"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil || strings.TrimSpace(params.SessionID) == "" {
		_ = s.fail(msg.ID, -32602, "sessionId is required")
		return
	}
	text, err := promptText(params.Prompt)
	if err != nil {
		_ = s.fail(msg.ID, -32602, err.Error())
		return
	}

	s.sessMu.Lock()
	sess := s.sessions[params.SessionID]
	s.sessMu.Unlock()
	if sess == nil {
		_ = s.fail(msg.ID, -32602, "unknown session")
		return
	}

	pctx, cancel := context.WithCancel(parent)
	sess.mu.Lock()
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.cancel = cancel
	cwd := sess.cwd
	sess.mu.Unlock()
	defer cancel()

	ws, err := safety.NewWorkspace(cwd)
	if err != nil {
		s.emitError(params.SessionID, err)
		_ = s.reply(msg.ID, map[string]any{"stopReason": "cancelled"})
		return
	}
	reg, err := tool.NewRegistry(s.cfg.MaxToolOutputBytes, s.cfg.ToolTimeout)
	if err != nil {
		s.emitError(params.SessionID, err)
		_ = s.reply(msg.ID, map[string]any{"stopReason": "cancelled"})
		return
	}
	if err := builtin.RegisterAll(reg, ws, s.cfg.AllowWrite); err != nil {
		s.emitError(params.SessionID, err)
		_ = s.reply(msg.ID, map[string]any{"stopReason": "cancelled"})
		return
	}

	s.capMu.Lock()
	caps := s.caps
	s.capMu.Unlock()
	runCtx := builtin.WithEditorDelegate(pctx, &ideDelegate{srv: s, sessionID: params.SessionID, caps: caps})

	eng, err := agent.NewEngine(s.cfg, s.provider, reg, agent.ObserverFunc(func(ev agent.Event) {
		s.onEvent(params.SessionID, ev)
	}))
	if err != nil {
		s.emitError(params.SessionID, err)
		_ = s.reply(msg.ID, map[string]any{"stopReason": "cancelled"})
		return
	}

	res, runErr := eng.Run(runCtx, text)
	if runErr != nil {
		s.emitError(params.SessionID, runErr)
		stop := "end_turn"
		if errors.Is(runErr, context.Canceled) || errors.Is(pctx.Err(), context.Canceled) {
			stop = "cancelled"
		}
		_ = s.reply(msg.ID, map[string]any{"stopReason": stop})
		return
	}
	if strings.TrimSpace(res.Answer) != "" {
		s.notifyUpdate(params.SessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": res.Answer},
		})
	}
	_ = s.reply(msg.ID, map[string]any{"stopReason": "end_turn"})
}

func (s *Server) onEvent(sessionID string, ev agent.Event) {
	switch ev.Kind {
	case agent.EventPlanReady:
		s.notifyUpdate(sessionID, map[string]any{
			"sessionUpdate": "plan",
			"message":       ev.Message,
		})
	case agent.EventTaskStart:
		s.notifyUpdate(sessionID, map[string]any{
			"sessionUpdate": "agent_thought_chunk",
			"content":       map[string]any{"type": "text", "text": ev.Message},
		})
	case agent.EventToolCall:
		s.notifyUpdate(sessionID, map[string]any{
			"sessionUpdate": "tool_call",
			"toolCallId":    ev.TaskID,
			"title":         ev.Message,
			"status":        "pending",
			"kind":          "other",
		})
	case agent.EventToolResult:
		s.notifyUpdate(sessionID, map[string]any{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    ev.TaskID,
			"title":         ev.Message,
			"status":        "completed",
		})
	case agent.EventToken:
		s.notifyUpdate(sessionID, map[string]any{
			"sessionUpdate": "agent_message_chunk",
			"content":       map[string]any{"type": "text", "text": ev.Message},
		})
	}
}

func (s *Server) emitError(sessionID string, err error) {
	s.notifyUpdate(sessionID, map[string]any{
		"sessionUpdate": "error",
		"message":       err.Error(),
	})
}

func (s *Server) notifyUpdate(sessionID string, update map[string]any) {
	_ = s.writeMsg(rpcNote{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params: map[string]any{
			"sessionId": sessionID,
			"update":    update,
		},
	})
}

func (s *Server) cancelSession(params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(params, &p)
	s.sessMu.Lock()
	sess := s.sessions[p.SessionID]
	s.sessMu.Unlock()
	if sess == nil {
		return
	}
	sess.mu.Lock()
	if sess.cancel != nil {
		sess.cancel()
	}
	sess.mu.Unlock()
}

func (s *Server) call(ctx context.Context, method string, params any, out any) error {
	id := s.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan inbound, 1)
	s.pendMu.Lock()
	s.pending[key] = ch
	s.pendMu.Unlock()
	defer func() {
		s.pendMu.Lock()
		delete(s.pending, key)
		s.pendMu.Unlock()
	}()
	if err := s.writeMsg(rpcCall{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(30 * time.Second):
		return fmt.Errorf("%s timed out", method)
	case msg := <-ch:
		if msg.Error != nil {
			return errors.New(msg.Error.Message)
		}
		if out != nil && len(msg.Result) > 0 && string(msg.Result) != "null" {
			if err := json.Unmarshal(msg.Result, out); err != nil {
				return err
			}
		}
		return nil
	}
}

func (s *Server) deliver(msg inbound) {
	key := idKey(msg.ID)
	s.pendMu.Lock()
	ch := s.pending[key]
	delete(s.pending, key)
	s.pendMu.Unlock()
	if ch != nil {
		ch <- msg
	}
}

func (s *Server) reply(id json.RawMessage, result any) error {
	return s.writeMsg(rpcResult{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) fail(id json.RawMessage, code int, message string) error {
	return s.writeMsg(rpcResult{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}})
}

func (s *Server) writeMsg(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = s.out.Write(append(b, '\n'))
	return err
}

type ideDelegate struct {
	srv       *Server
	sessionID string
	caps      clientCaps
}

func (d *ideDelegate) SupportsRead() bool     { return d.caps.Read }
func (d *ideDelegate) SupportsWrite() bool    { return d.caps.Write }
func (d *ideDelegate) SupportsTerminal() bool { return d.caps.Terminal }

func (d *ideDelegate) ReadTextFile(ctx context.Context, path string, line, limit int) (string, error) {
	var res struct {
		Content string `json:"content"`
		Text    string `json:"text"`
	}
	params := map[string]any{
		"sessionId": d.sessionID,
		"path":      path,
	}
	if line > 0 {
		params["line"] = line
	}
	if limit > 0 {
		params["limit"] = limit
	}
	if err := d.srv.call(ctx, "fs/read_text_file", params, &res); err != nil {
		return "", err
	}
	text := res.Content
	if text == "" {
		text = res.Text
	}
	if limit > 0 {
		nlines := strings.Count(text, "\n") + 1
		if nlines > limit {
			sliced, _, _ := sliceLines(text, line, limit)
			if sliced != "" {
				text = sliced
			}
		}
	}
	return text, nil
}

func (d *ideDelegate) WriteTextFile(ctx context.Context, path, content string) (string, error) {
	var res map[string]any
	err := d.srv.call(ctx, "fs/write_text_file", map[string]any{
		"sessionId": d.sessionID,
		"path":      path,
		"content":   content,
	}, &res)
	if err != nil {
		return "", err
	}
	return "ok", nil
}

func (d *ideDelegate) CreateTerminal(ctx context.Context, command, cwd string) (string, error) {
	var res struct {
		Output     string `json:"output"`
		TerminalID string `json:"terminalId"`
		ExitCode   *int   `json:"exitCode"`
	}
	err := d.srv.call(ctx, "terminal/create", map[string]any{
		"sessionId": d.sessionID,
		"command":   command,
		"cwd":       cwd,
	}, &res)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Output) != "" {
		return res.Output, nil
	}
	if res.TerminalID != "" {
		return "terminal " + res.TerminalID, nil
	}
	return "terminal created", nil
}

func parseCaps(params json.RawMessage) clientCaps {
	var p struct {
		ClientCapabilities struct {
			FS struct {
				ReadTextFile  bool `json:"readTextFile"`
				WriteTextFile bool `json:"writeTextFile"`
			} `json:"fs"`
			Terminal any `json:"terminal"`
		} `json:"clientCapabilities"`
	}
	_ = json.Unmarshal(params, &p)
	term := false
	switch v := p.ClientCapabilities.Terminal.(type) {
	case bool:
		term = v
	case map[string]any:
		term = true
	}
	return clientCaps{
		Read:     p.ClientCapabilities.FS.ReadTextFile,
		Write:    p.ClientCapabilities.FS.WriteTextFile,
		Terminal: term,
	}
}

func promptText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", errors.New("prompt is required")
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		if strings.TrimSpace(s) == "" {
			return "", errors.New("prompt is required")
		}
		return s, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("prompt must be a string or an array of text blocks")
	}
	var b strings.Builder
	for _, block := range blocks {
		if strings.TrimSpace(block.Text) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(block.Text)
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", errors.New("prompt is required")
	}
	return b.String(), nil
}

func idKey(raw json.RawMessage) string {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var n json.Number
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&n); err == nil {
		return n.String()
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return "s:" + s
	}
	return string(raw)
}

func newID(prefix string) string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(b)
}

func sliceLines(content string, start, limit int) (string, int, bool) {
	if start < 1 {
		start = 1
	}
	lines := strings.Split(content, "\n")
	total := len(lines)
	if start > total {
		return "", total, false
	}
	end := total
	if limit > 0 && start-1+limit < end {
		end = start - 1 + limit
	}
	return strings.Join(lines[start-1:end], "\n"), total, end < total
}
