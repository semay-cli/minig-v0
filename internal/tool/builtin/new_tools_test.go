package builtin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
)

func TestObservabilityHTTPTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth != "Bearer secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error": "unauthorized"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status": "success", "data": {"result": [{"metric": {"app": "auth"}, "value": [1600000000, "42"]}]}}`))
	}))
	defer server.Close()

	httpTool := &observabilityHTTPTool{}

	// 1. Successful query with json_path extraction
	args := map[string]any{
		"url":        server.URL,
		"method":     "GET",
		"auth_token": "secret-token",
		"json_path":  "data.result.0.metric.app",
	}
	rawArgs, _ := json.Marshal(args)
	out, err := httpTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "HTTP 200 OK") || !strings.Contains(out, `"auth"`) {
		t.Errorf("unexpected output: %s", out)
	}

	// 2. Unauthorized request
	badArgs := map[string]any{
		"url":        server.URL,
		"auth_token": "wrong-token",
	}
	badRaw, _ := json.Marshal(badArgs)
	out, err = httpTool.Execute(context.Background(), badRaw)
	if err != nil {
		t.Fatalf("Execute should not return error on non-200 HTTP response, got: %v", err)
	}
	if !strings.Contains(out, "HTTP 401") {
		t.Errorf("expected HTTP 401 in output, got: %s", out)
	}
}

func TestLogTemplateMinerTool(t *testing.T) {
	miner := &logTemplateMinerTool{}

	rawLogs := `
2026-11-18T10:00:01Z ERROR [10.0.0.1] service 12345678-1234-1234-1234-123456789abc failed to connect to 192.168.1.5:8080 (err=0xdeadbeef)
2026-11-18T10:00:02Z ERROR [10.0.0.2] service 87654321-4321-4321-4321-cba987654321 failed to connect to 192.168.1.6:8080 (err=0xfeedface)
2026-11-18T10:00:03Z WARN retry attempt 1 for request 11112222-3333-4444-5555-666677778888
2026-11-18T10:00:04Z ERROR [10.0.0.3] service aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee failed to connect to 192.168.1.7:8080 (err=0x12345678)
Exception in thread "main" java.lang.NullPointerException
	at com.example.service.Worker.run(Worker.java:42)
	at java.lang.Thread.run(Thread.java:748)
`

	args := map[string]any{
		"logs":          rawLogs,
		"max_templates": 10,
	}
	rawArgs, _ := json.Marshal(args)
	out, err := miner.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if !strings.Contains(out, "<TIMESTAMP>") || !strings.Contains(out, "<IP>") || !strings.Contains(out, "<UUID>") {
		t.Errorf("expected placeholder replacements in output: %s", out)
	}
	if !strings.Contains(out, "Extracted Stack Traces") || !strings.Contains(out, "Worker.java:42") {
		t.Errorf("expected stack trace extraction in output: %s", out)
	}
	if !strings.Contains(out, "[   3x") {
		t.Errorf("expected count 3 for connection failure template: %s", out)
	}
}

func TestSandboxPythonTool(t *testing.T) {
	pyTool := &sandboxPythonTool{}

	// Test script reading context.json and outputting result
	script := `
import json

with open("context.json") as f:
    data = json.load(f)

print(f"Processed metric: {data['metric']} = {data['value'] * 2}")
`
	contextData := `{"metric": "cpu_spikes", "value": 21}`

	args := map[string]any{
		"script":          script,
		"context_data":    contextData,
		"timeout_seconds": 15,
	}
	rawArgs, _ := json.Marshal(args)
	out, err := pyTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v\nOutput: %s", err, out)
	}

	if !strings.Contains(out, "Exit code: 0") || !strings.Contains(out, "Processed metric: cpu_spikes = 42") {
		t.Errorf("unexpected python output: %s", out)
	}
}

func TestAlertDispatchTool(t *testing.T) {
	var receivedBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&receivedBody)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok": true}`))
	}))
	defer server.Close()

	alertTool := &alertDispatchTool{}

	args := map[string]any{
		"webhook_url":  server.URL,
		"target_type":  "slack",
		"summary":      "Database connection saturation",
		"severity":     "critical",
		"service_name": "payment-api",
		"description":  "Active connections reached 98% of pool capacity",
	}
	rawArgs, _ := json.Marshal(args)
	out, err := alertTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "Alert successfully dispatched") {
		t.Errorf("unexpected output: %s", out)
	}
	if text, ok := receivedBody["text"].(string); !ok || !strings.Contains(text, "[CRITICAL]") {
		t.Errorf("slack text missing or malformed: %v", receivedBody)
	}
}

func TestWebFetchTool(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><script>alert('bad');</script><style>body { color: red; }</style></head><body><h1>Runbook: High Memory</h1><p>Check the heap dump in <b>/var/log/dumps</b>.</p></body></html>`))
	}))
	defer server.Close()

	fetchTool := &webFetchTool{}
	args := map[string]any{"url": server.URL}
	rawArgs, _ := json.Marshal(args)

	out, err := fetchTool.Execute(context.Background(), rawArgs)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}

	if strings.Contains(out, "<script>") || strings.Contains(out, "alert('bad')") {
		t.Errorf("script tags were not stripped: %s", out)
	}
	if !strings.Contains(out, "Runbook: High Memory") || !strings.Contains(out, "/var/log/dumps") {
		t.Errorf("expected clean content: %s", out)
	}
}

func TestSessionScratchpadTool(t *testing.T) {
	toolInstance := &sessionScratchpadTool{}
	sessionID := "test-session-123"
	ctx := WithScratchpadSession(context.Background(), sessionID)
	defer CleanupScratchpad(sessionID)

	// 1. Set key
	setArgs, _ := json.Marshal(map[string]any{
		"operation": "set",
		"key":       "suspected_service",
		"value":     "auth-gateway",
	})
	out, err := toolInstance.Execute(ctx, setArgs)
	if err != nil || !strings.Contains(out, "stored key") {
		t.Fatalf("set failed: %v, out: %s", err, out)
	}

	// 2. Get key
	getArgs, _ := json.Marshal(map[string]any{
		"operation": "get",
		"key":       "suspected_service",
	})
	out, err = toolInstance.Execute(ctx, getArgs)
	if err != nil || out != "auth-gateway" {
		t.Fatalf("get failed: %v, out: %s", err, out)
	}

	// 3. List keys
	listArgs, _ := json.Marshal(map[string]any{"operation": "list"})
	out, err = toolInstance.Execute(ctx, listArgs)
	if err != nil || !strings.Contains(out, "suspected_service") {
		t.Fatalf("list failed: %v, out: %s", err, out)
	}

	// 4. Delete key
	delArgs, _ := json.Marshal(map[string]any{"operation": "delete", "key": "suspected_service"})
	out, err = toolInstance.Execute(ctx, delArgs)
	if err != nil || !strings.Contains(out, "deleted key") {
		t.Fatalf("delete failed: %v, out: %s", err, out)
	}
}

func TestWorkspaceTerminalTool(t *testing.T) {
	tempDir := t.TempDir()
	ws, err := safety.NewWorkspace(tempDir)
	if err != nil {
		t.Fatal(err)
	}

	termTool := &workspaceTerminalTool{ws: ws}

	// 1. Normal command
	args, _ := json.Marshal(map[string]any{
		"command": "echo 'minig terminal ok'",
	})
	out, err := termTool.Execute(context.Background(), args)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if !strings.Contains(out, "minig terminal ok") {
		t.Errorf("unexpected output: %s", out)
	}

	// 2. Dangerous command rejected
	dangerousArgs, _ := json.Marshal(map[string]any{
		"command": "rm -rf /",
	})
	_, err = termTool.Execute(context.Background(), dangerousArgs)
	if err == nil || !strings.Contains(err.Error(), "dangerous pattern") {
		t.Errorf("expected dangerous command rejection, got: %v", err)
	}
}

func TestRegisterHeadless(t *testing.T) {
	reg, err := tool.NewRegistry(8192, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if err := RegisterHeadless(reg); err != nil {
		t.Fatalf("RegisterHeadless failed: %v", err)
	}

	names := reg.Names()
	expected := []string{
		"alert_dispatch",
		"log_template_miner",
		"observability_http",
		"sandbox_python",
		"session_scratchpad",
		"web_fetch",
	}

	for _, exp := range expected {
		found := false
		for _, n := range names {
			if n == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected tool %q in headless registry, got: %v", exp, names)
		}
	}
}
