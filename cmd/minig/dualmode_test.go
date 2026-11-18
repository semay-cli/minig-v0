package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestHeadlessServeStructuredRun(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Fatal("python3 is required to verify the ephemeral sandbox")
	}
	empty := t.TempDir()
	t.Chdir(empty)

	before := minigRunDirs()
	var sawAuth atomic.Bool
	var chatCalls atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer sre-token" {
			sawAuth.Store(true)
		}
		switch r.URL.Path {
		case "/health", "/props":
			w.WriteHeader(http.StatusOK)
		case "/v1/chat/completions/input_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":40}`))
		case "/v1/chat/completions":
			n := chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch n {
			case 1:
				_, _ = w.Write(chatBody("{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Compress and score the spike\",\"tools\":[\"sandbox_python\"]}]}", "", ""))
			case 2:
				_, _ = w.Write(chatBody("", "sandbox_python", `{"script":"import json\nprint(json.load(open('context.json'))['service'])\n"}`))
			default:
				_, _ = w.Write(chatBody("pool is hot\n{\"severity\":\"high\",\"root_cause\":\"connection pool exhausted\",\"should_alert\":true,\"recommended_actions\":[\"restart pool\"]}", "", ""))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer llm.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stderr bytes.Buffer
	runErr := make(chan error, 1)
	go func() {
		runErr <- runIO(ctx, []string{
			"serve",
			"--host", "127.0.0.1",
			"--port", strconv.Itoa(port),
			"--url", llm.URL,
		}, bytes.NewReader(nil), io.Discard, &stderr)
	}()

	base := "http://127.0.0.1:" + strconv.Itoa(port)
	waitHealthy(t, base+"/v1/health")

	healthRes, err := http.Get(base + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	healthBody, _ := io.ReadAll(healthRes.Body)
	healthRes.Body.Close()
	if !strings.Contains(string(healthBody), `"workspace_mode":"none"`) {
		t.Fatalf("health = %s", healthBody)
	}

	toolsRes, err := http.Get(base + "/v1/tools")
	if err != nil {
		t.Fatal(err)
	}
	toolsBody, _ := io.ReadAll(toolsRes.Body)
	toolsRes.Body.Close()
	var toolsDoc struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(toolsBody, &toolsDoc); err != nil {
		t.Fatal(err)
	}
	names := map[string]json.RawMessage{}
	for _, tool := range toolsDoc.Tools {
		names[tool.Name] = tool.InputSchema
		if !json.Valid(tool.InputSchema) || !strings.Contains(string(tool.InputSchema), `"type"`) {
			t.Fatalf("tool %s has invalid inputSchema %s", tool.Name, tool.InputSchema)
		}
	}
	for _, required := range []string{"observability_http", "log_template_miner", "sandbox_python", "alert_dispatch", "web_fetch", "doc_extract", "session_scratchpad"} {
		if _, ok := names[required]; !ok {
			t.Fatalf("missing built-in tool %s in %v", required, keys(names))
		}
	}
	if _, ok := names["write_file"]; ok {
		t.Fatal("write_file must be hidden when workspace_mode is none")
	}

	schema := map[string]any{
		"type":     "object",
		"required": []string{"severity", "root_cause", "should_alert"},
		"properties": map[string]any{
			"severity":            map[string]any{"type": "string"},
			"root_cause":          map[string]any{"type": "string"},
			"should_alert":        map[string]any{"type": "boolean"},
			"recommended_actions": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
		},
	}
	payload := map[string]any{
		"prompt":          "Diagnose the checkout error spike",
		"workspace_mode":  "none",
		"allowed_tools":   []string{"sandbox_python", "log_template_miner"},
		"context_data":    map[string]any{"service": "checkout", "logs": "ERROR pool exhausted"},
		"response_schema": schema,
		"llm_config":      map[string]any{"model": "qwen-test"},
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, base+"/v1/agent/run", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-LLM-Base-URL", llm.URL)
	req.Header.Set("X-LLM-API-Key", "sre-token")
	req.Header.Set("X-LLM-Model", "qwen-test")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/agent/run: %v\nstderr: %s", err, stderr.String())
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s\nstderr: %s", resp.StatusCode, respBody, stderr.String())
	}
	var result struct {
		RunID            string `json:"run_id"`
		Status           string `json:"status"`
		StructuredOutput struct {
			Severity    string `json:"severity"`
			RootCause   string `json:"root_cause"`
			ShouldAlert bool   `json:"should_alert"`
		} `json:"structured_output"`
		ToolTrace []struct {
			Tool   string `json:"tool"`
			Output string `json:"output"`
			Error  string `json:"error"`
		} `json:"tool_trace"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("decode: %v\n%s", err, respBody)
	}
	if result.Status != "completed" || result.RunID == "" {
		t.Fatalf("unexpected result: %s", respBody)
	}
	if result.StructuredOutput.Severity != "high" || !result.StructuredOutput.ShouldAlert || result.StructuredOutput.RootCause == "" {
		t.Fatalf("structured output: %+v", result.StructuredOutput)
	}
	if len(result.ToolTrace) != 1 || result.ToolTrace[0].Tool != "sandbox_python" || result.ToolTrace[0].Error != "" {
		t.Fatalf("tool trace: %+v", result.ToolTrace)
	}
	if !strings.Contains(result.ToolTrace[0].Output, "checkout") {
		t.Fatalf("sandbox did not see context.json: %s", result.ToolTrace[0].Output)
	}
	if !sawAuth.Load() {
		t.Fatal("llama.cpp request missing Authorization bearer token")
	}
	entries, _ := os.ReadDir(empty)
	if len(entries) != 0 {
		t.Fatalf("headless run left files in the empty directory: %v", entries)
	}
	for dir := range minigRunDirs() {
		if _, ok := before[dir]; !ok {
			t.Fatalf("sandbox directory leaked: %s", dir)
		}
	}

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("serve did not shut down")
	}
}

func TestACPSessionPromptDelegatesFileRead(t *testing.T) {
	workspace := t.TempDir()
	var chatCalls atomic.Int32
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/chat/completions/input_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":30}`))
		case "/v1/chat/completions":
			n := chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch n {
			case 1:
				_, _ = w.Write(chatBody("{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Read hello.txt\",\"tools\":[\"read_range\"]}]}", "", ""))
			case 2:
				_, _ = w.Write(chatBody("", "read_range", `{"path":"hello.txt"}`))
			default:
				_, _ = w.Write(chatBody("The file says hello from workspace", "", ""))
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer llm.Close()

	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runIO(ctx, []string{"acp", "--url", llm.URL, "--workspace", workspace}, stdinR, stdoutW, io.Discard)
	}()

	peer := newLinePeer(stdoutR, stdinW)
	defer func() {
		_ = stdinW.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		_ = stdoutW.Close()
		peer.close()
	}()

	initRes := peer.call(t, 1, "initialize", map[string]any{
		"protocolVersion": 1,
		"clientCapabilities": map[string]any{
			"fs":       map[string]any{"readTextFile": true, "writeTextFile": true},
			"terminal": true,
		},
	})
	if !strings.Contains(string(initRes), "minig") {
		t.Fatalf("initialize result: %s", initRes)
	}
	newRes := peer.call(t, 2, "session/new", map[string]any{"cwd": workspace, "mcpServers": []any{}})
	var created struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(newRes, &created); err != nil || created.SessionID == "" {
		t.Fatalf("session/new: %s (%v)", newRes, err)
	}
	promptRes := peer.call(t, 3, "session/prompt", map[string]any{
		"sessionId": created.SessionID,
		"prompt":    []map[string]any{{"type": "text", "text": "Read hello.txt"}},
	})
	if !strings.Contains(string(promptRes), "end_turn") {
		t.Fatalf("session/prompt result: %s", promptRes)
	}
	if peer.reads() == 0 {
		t.Fatal("expected fs/read_text_file delegation to the IDE")
	}
	updates := peer.updates()
	blob := string(bytes.Join(updates, []byte("\n")))
	if !strings.Contains(blob, "session/update") || !strings.Contains(blob, "hello from workspace") {
		t.Fatalf("missing streamed session updates: %s", blob)
	}
}

func TestMCPExposesBuiltinTools(t *testing.T) {
	workspace := t.TempDir()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- runIO(ctx, []string{"mcp", "--workspace", workspace, "--write"}, stdinR, stdoutW, io.Discard)
	}()
	peer := newLinePeer(stdoutR, stdinW)
	defer func() {
		_ = stdinW.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
		_ = stdoutW.Close()
		peer.close()
	}()

	initRes := peer.call(t, 1, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}})
	if !strings.Contains(string(initRes), "2024-11-05") {
		t.Fatalf("initialize: %s", initRes)
	}
	// Notification must not produce a response.
	peer.notify("notifications/initialized", map[string]any{})

	listed := peer.call(t, 2, "tools/list", map[string]any{})
	var catalog struct {
		Tools []struct {
			Name        string          `json:"name"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(listed, &catalog); err != nil {
		t.Fatalf("tools/list: %v %s", err, listed)
	}
	names := map[string]bool{}
	for _, tool := range catalog.Tools {
		names[tool.Name] = true
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil || schema["type"] != "object" {
			t.Fatalf("%s inputSchema = %s", tool.Name, tool.InputSchema)
		}
	}
	for _, required := range []string{
		"observability_http", "log_template_miner", "sandbox_python", "alert_dispatch",
		"web_fetch", "doc_extract", "session_scratchpad", "workspace_code", "workspace_terminal",
	} {
		if !names[required] {
			t.Fatalf("mcp missing %s", required)
		}
	}

	called := peer.call(t, 3, "tools/call", map[string]any{
		"name":      "log_template_miner",
		"arguments": map[string]any{"logs": "ERROR disk full\nERROR disk full\nWARN retry", "max_templates": 5},
	})
	if !strings.Contains(string(called), "template") {
		t.Fatalf("tools/call: %s", called)
	}
}

type linePeer struct {
	r       io.ReadCloser
	w       io.Writer
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan json.RawMessage
	notes   [][]byte
	readN   int
	done    chan struct{}
}

func newLinePeer(r io.Reader, w io.Writer) *linePeer {
	p := &linePeer{
		w:       w,
		pending: map[string]chan json.RawMessage{},
		done:    make(chan struct{}),
	}
	go func() {
		defer close(p.done)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			var msg struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				Result json.RawMessage `json:"result"`
			}
			if err := json.Unmarshal(line, &msg); err != nil {
				continue
			}
			if msg.Method == "fs/read_text_file" {
				p.mu.Lock()
				p.readN++
				p.mu.Unlock()
				p.reply(msg.ID, map[string]string{"content": "hello from workspace\n"})
				continue
			}
			if msg.Method != "" {
				p.mu.Lock()
				p.notes = append(p.notes, line)
				p.mu.Unlock()
				continue
			}
			key := strings.TrimSpace(string(msg.ID))
			p.mu.Lock()
			ch := p.pending[key]
			delete(p.pending, key)
			p.mu.Unlock()
			if ch != nil {
				ch <- msg.Result
			}
		}
	}()
	return p
}

func (p *linePeer) call(t *testing.T, id int, method string, params any) json.RawMessage {
	t.Helper()
	ch := make(chan json.RawMessage, 1)
	p.mu.Lock()
	p.pending[strconv.Itoa(id)] = ch
	p.mu.Unlock()
	p.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	select {
	case res := <-ch:
		return res
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", method)
		return nil
	}
}

func (p *linePeer) notify(method string, params any) {
	p.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (p *linePeer) reply(id json.RawMessage, result any) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	payload, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: result})
	_, _ = p.w.Write(append(payload, '\n'))
}

func (p *linePeer) send(v any) {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	b, _ := json.Marshal(v)
	_, _ = p.w.Write(append(b, '\n'))
}

func (p *linePeer) reads() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.readN
}

func (p *linePeer) updates() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.notes))
	copy(out, p.notes)
	return out
}

func (p *linePeer) close() {}

func chatBody(content, toolName, toolArgs string) []byte {
	msg := map[string]any{"content": content}
	finish := "stop"
	if toolName != "" {
		finish = "tool_calls"
		msg["content"] = ""
		msg["tool_calls"] = []any{map[string]any{
			"id":   "c1",
			"type": "function",
			"function": map[string]any{
				"name":      toolName,
				"arguments": toolArgs,
			},
		}}
	}
	body, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"message": msg, "finish_reason": finish}},
		"usage":   map[string]int{"prompt_tokens": 30, "completion_tokens": 12},
	})
	return body
}

func waitHealthy(t *testing.T, url string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		res, err := http.Get(url)
		if err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(40 * time.Millisecond)
	}
	t.Fatalf("server did not become healthy at %s", url)
}

func minigRunDirs() map[string]struct{} {
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "minig-run-*"))
	out := make(map[string]struct{}, len(matches))
	for _, m := range matches {
		out[m] = struct{}{}
	}
	return out
}

func keys(m map[string]json.RawMessage) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
