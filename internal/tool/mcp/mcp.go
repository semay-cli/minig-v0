package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/semay-cli/minig-v0/internal/tool"
)

const (
	protocolVersion    = "2024-11-05"
	maxScannerLineSize = 1 << 20 // 1 MiB max single JSON-RPC message
	maxToolPages       = 10
)

// ServerConfig describes how to launch a local stdio MCP server subprocess.
type ServerConfig struct {
	Name    string
	Command string
	Args    []string
	Env     []string
}

// ParseServerSpec parses "name=command arg1 arg2" into a ServerConfig.
func ParseServerSpec(spec string) (ServerConfig, error) {
	trimmed := strings.TrimSpace(spec)
	eq := strings.IndexByte(trimmed, '=')
	if eq <= 0 {
		return ServerConfig{}, fmt.Errorf("invalid MCP server spec %q: expected format name=command [args...]", spec)
	}

	name := sanitizeIdentifier(trimmed[:eq])
	if name == "" {
		return ServerConfig{}, fmt.Errorf("invalid MCP server name in %q", spec)
	}

	cmdLine := strings.TrimSpace(trimmed[eq+1:])
	parts, err := splitCommandLine(cmdLine)
	if err != nil {
		return ServerConfig{}, fmt.Errorf("parse command in %q: %w", spec, err)
	}
	if len(parts) == 0 {
		return ServerConfig{}, fmt.Errorf("missing command in MCP server spec %q", spec)
	}

	return ServerConfig{
		Name:    name,
		Command: parts[0],
		Args:    parts[1:],
	}, nil
}

// ToolInfo describes a tool discovered from an MCP server.
type ToolInfo struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Client manages a JSON-RPC 2.0 session over newline-delimited stdio streams.
type Client struct {
	serverName string
	stdin      io.WriteCloser
	cmd        *exec.Cmd

	writeMu sync.Mutex
	nextID  atomic.Int64

	pendMu  sync.Mutex
	pending map[int64]chan rpcEnvelope
	closed  bool
	readErr error
	done    chan struct{}
}

// NewStdioClient spawns the configured MCP server subprocess and starts its stdio transport.
// runCtx governs the subprocess lifetime; call Close() to terminate and reap the process.
func NewStdioClient(runCtx context.Context, cfg ServerConfig) (*Client, error) {
	if strings.TrimSpace(cfg.Name) == "" {
		return nil, errors.New("MCP server name must not be empty")
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, errors.New("MCP server command must not be empty")
	}

	cmd := exec.CommandContext(runCtx, cfg.Command, cfg.Args...)
	if len(cfg.Env) > 0 {
		cmd.Env = append(os.Environ(), cfg.Env...)
	}
	// Discard server stderr so debug logs never corrupt stdout or terminal UI.
	cmd.Stderr = io.Discard

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe for MCP server %q: %w", cfg.Name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("create stdout pipe for MCP server %q: %w", cfg.Name, err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start MCP server %q (%s): %w", cfg.Name, cfg.Command, err)
	}

	client := newStreamClient(cfg.Name, stdin, stdout, cmd)
	return client, nil
}

// newStreamClient creates a Client over arbitrary stdio streams (used by tests and NewStdioClient).
func newStreamClient(serverName string, stdin io.WriteCloser, stdout io.Reader, cmd *exec.Cmd) *Client {
	c := &Client{
		serverName: sanitizeIdentifier(serverName),
		stdin:      stdin,
		cmd:        cmd,
		pending:    make(map[int64]chan rpcEnvelope),
		done:       make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

// Close shuts down the stdio stream and waits briefly for the subprocess to exit.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.pendMu.Lock()
	alreadyClosed := c.closed
	c.closed = true
	c.pendMu.Unlock()

	var closeErr error
	if !alreadyClosed && c.stdin != nil {
		closeErr = c.stdin.Close()
	}

	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
	}

	if c.cmd != nil && c.cmd.Process != nil {
		waitDone := make(chan error, 1)
		go func() {
			waitDone <- c.cmd.Wait()
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Second):
			_ = c.cmd.Process.Kill()
			<-waitDone
		}
	}
	return closeErr
}

// Initialize performs the MCP protocol handshake.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo": map[string]string{
			"name":    "minig",
			"version": "0.1.0",
		},
	}

	var initResult struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.call(ctx, "initialize", params, &initResult); err != nil {
		return fmt.Errorf("MCP initialize %q: %w", c.serverName, err)
	}

	if err := c.notify("notifications/initialized", nil); err != nil {
		return fmt.Errorf("MCP initialized notification %q: %w", c.serverName, err)
	}
	return nil
}

// ListTools queries all tools exposed by the MCP server.
func (c *Client) ListTools(ctx context.Context) ([]ToolInfo, error) {
	var all []ToolInfo
	cursor := ""

	for page := 0; page < maxToolPages; page++ {
		var params any
		if cursor != "" {
			params = map[string]string{"cursor": cursor}
		}

		var res struct {
			Tools      []ToolInfo `json:"tools"`
			NextCursor string     `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &res); err != nil {
			return nil, fmt.Errorf("MCP tools/list %q: %w", c.serverName, err)
		}
		all = append(all, res.Tools...)
		if strings.TrimSpace(res.NextCursor) == "" {
			return all, nil
		}
		cursor = res.NextCursor
	}
	return all, nil
}

// CallTool invokes a tool on the MCP server and concatenates its text output blocks.
func (c *Client) CallTool(ctx context.Context, toolName string, rawArgs json.RawMessage) (string, error) {
	args := rawArgs
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}

	params := struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}{
		Name:      toolName,
		Arguments: args,
	}

	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := c.call(ctx, "tools/call", params, &res); err != nil {
		return "", err
	}

	var lines []string
	for _, block := range res.Content {
		if strings.TrimSpace(block.Text) != "" {
			lines = append(lines, block.Text)
		}
	}
	joined := strings.Join(lines, "\n")

	if res.IsError {
		if joined == "" {
			joined = "MCP tool returned an unspecified error"
		}
		return "", errors.New(joined)
	}
	if joined == "" {
		return "(ok)", nil
	}
	return joined, nil
}

// RegisterTools discovers tools from client and registers them into reg under
// namespaced identifiers ("mcp_<server>_<tool>").
func RegisterTools(ctx context.Context, reg *tool.Registry, client *Client) (int, error) {
	if reg == nil {
		return 0, errors.New("tool registry must not be nil")
	}
	if client == nil {
		return 0, errors.New("MCP client must not be nil")
	}

	discovered, err := client.ListTools(ctx)
	if err != nil {
		return 0, err
	}

	registered := 0
	for _, info := range discovered {
		origName := strings.TrimSpace(info.Name)
		if origName == "" {
			continue
		}
		exposedName := buildExposedName(client.serverName, origName)
		desc := strings.TrimSpace(info.Description)
		if desc == "" {
			desc = fmt.Sprintf("MCP tool %s from server %s", origName, client.serverName)
		}
		schema := info.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}

		adapter := &bridgedTool{
			exposedName: exposedName,
			remoteName:  origName,
			description: desc,
			parameters:  schema,
			client:      client,
		}
		if err := reg.Register(adapter); err != nil {
			return registered, err
		}
		registered++
	}
	return registered, nil
}

type bridgedTool struct {
	exposedName string
	remoteName  string
	description string
	parameters  json.RawMessage
	client      *Client
}

func (b *bridgedTool) Name() string                { return b.exposedName }
func (b *bridgedTool) Description() string         { return b.description }
func (b *bridgedTool) Parameters() json.RawMessage { return b.parameters }
func (b *bridgedTool) ReadOnly() bool              { return false }
func (b *bridgedTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	return b.client.CallTool(ctx, b.remoteName, rawArgs)
}

// --- JSON-RPC 2.0 Framing ---

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcEnvelope struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func (c *Client) notify(method string, params any) error {
	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	return c.writeLine(payload)
}

func (c *Client) call(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	respCh := make(chan rpcEnvelope, 1)

	c.pendMu.Lock()
	if c.closed {
		err := c.readErr
		c.pendMu.Unlock()
		if err != nil {
			return fmt.Errorf("MCP transport closed: %w", err)
		}
		return errors.New("MCP transport is closed")
	}
	c.pending[id] = respCh
	c.pendMu.Unlock()

	defer func() {
		c.pendMu.Lock()
		delete(c.pending, id)
		c.pendMu.Unlock()
	}()

	payload, err := json.Marshal(rpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return err
	}
	if err := c.writeLine(payload); err != nil {
		return err
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case env, ok := <-respCh:
		if !ok {
			c.pendMu.Lock()
			rErr := c.readErr
			c.pendMu.Unlock()
			if rErr != nil {
				return fmt.Errorf("MCP server %q disconnected: %w", c.serverName, rErr)
			}
			return fmt.Errorf("MCP server %q closed stdout", c.serverName)
		}
		if env.Error != nil {
			return fmt.Errorf("MCP error (%d): %s", env.Error.Code, env.Error.Message)
		}
		if out != nil && len(env.Result) > 0 {
			if err := json.Unmarshal(env.Result, out); err != nil {
				return fmt.Errorf("decode MCP %s result: %w", method, err)
			}
		}
		return nil
	}
}

func (c *Client) writeLine(line []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if _, err := c.stdin.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write to MCP server %q: %w", c.serverName, err)
	}
	return nil
}

func (c *Client) readLoop(stdout io.Reader) {
	defer close(c.done)

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), maxScannerLineSize)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var env rpcEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue
		}
		// Server notifications do not carry a request ID.
		if env.ID == nil {
			continue
		}

		c.pendMu.Lock()
		ch, ok := c.pending[*env.ID]
		if ok {
			delete(c.pending, *env.ID)
		}
		c.pendMu.Unlock()

		if ok {
			ch <- env
		}
	}

	scanErr := scanner.Err()
	c.pendMu.Lock()
	c.closed = true
	c.readErr = scanErr
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.pendMu.Unlock()
}

func buildExposedName(serverName, toolName string) string {
	full := "mcp_" + sanitizeIdentifier(serverName) + "_" + sanitizeIdentifier(toolName)
	if len(full) > 64 {
		return full[:64]
	}
	return full
}

func sanitizeIdentifier(raw string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

func splitCommandLine(input string) ([]string, error) {
	var args []string
	var current strings.Builder
	var quote rune
	escaped := false

	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
			continue
		}
		switch r {
		case '"', '\'':
			quote = r
		case ' ', '\t':
			if current.Len() > 0 {
				args = append(args, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if escaped || quote != 0 {
		return nil, errors.New("unterminated quote or escape in command line")
	}
	if current.Len() > 0 {
		args = append(args, current.String())
	}
	return args, nil
}
