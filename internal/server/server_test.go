package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
)

func setupTestServer(t *testing.T, mock *llm.MockProvider) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.WorkspaceRoot = root
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ws, err := safety.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	reg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := builtin.RegisterAll(reg, ws, true); err != nil {
		t.Fatal(err)
	}

	srv, err := NewServer(cfg, mock, reg)
	if err != nil {
		t.Fatal(err)
	}
	return srv, root
}

func TestServerHealthAndToolsEndpoints(t *testing.T) {
	mock := &llm.MockProvider{}
	srv, _ := setupTestServer(t, mock)

	// Test GET /v1/health
	req := httptest.NewRequest(http.MethodGet, "/v1/health", nil)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var healthResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &healthResp); err != nil {
		t.Fatal(err)
	}
	if healthResp["status"] != "healthy" {
		t.Errorf("expected status healthy, got %v", healthResp["status"])
	}

	// Test GET /v1/tools
	toolsReq := httptest.NewRequest(http.MethodGet, "/v1/tools", nil)
	toolsW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(toolsW, toolsReq)

	if toolsW.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", toolsW.Code, toolsW.Body.String())
	}
	var toolsResp map[string]any
	if err := json.Unmarshal(toolsW.Body.Bytes(), &toolsResp); err != nil {
		t.Fatal(err)
	}
	count, ok := toolsResp["count"].(float64)
	if !ok || count < 5 {
		t.Errorf("expected at least 5 tools, got %v", toolsResp["count"])
	}
}

func TestServerRunEndpoint(t *testing.T) {
	mock := &llm.MockProvider{
		Responses: []llm.ChatResponse{
			// Planner response
			{
				Content: "{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Inspect code\",\"depends_on\":[],\"tools\":[\"read_range\"]}]}",
			},
			// t1 execution
			{
				Content: "main.go is a valid Go file.",
			},
		},
	}
	srv, _ := setupTestServer(t, mock)

	body, _ := json.Marshal(RunRequest{
		Goal: "Check main.go",
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/run", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var res agent.RunResult
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal RunResult: %v", err)
	}
	if res.Answer != "main.go is a valid Go file." {
		t.Errorf("expected answer, got %q", res.Answer)
	}
}

func TestServerRunStreamSSEEndpoint(t *testing.T) {
	mock := &llm.MockProvider{
		Responses: []llm.ChatResponse{
			{
				Content: "{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Inspect code\",\"depends_on\":[],\"tools\":[\"read_range\"]}]}",
			},
			{
				Content: "Streaming finished successfully.",
			},
		},
	}
	srv, _ := setupTestServer(t, mock)

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, _ := json.Marshal(RunRequest{
		Goal: "Stream check main.go",
	})
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, ts.URL+"/v1/run/stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected status 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Errorf("expected text/event-stream, got %s", resp.Header.Get("Content-Type"))
	}

	scanner := bufio.NewScanner(resp.Body)
	var events []string
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "event: ") {
			events = append(events, strings.TrimPrefix(line, "event: "))
		}
	}

	// Verify we got SSE events: plan_ready, task_start, task_done, done
	foundDone := false
	for _, ev := range events {
		if ev == "done" {
			foundDone = true
		}
	}
	if !foundDone {
		t.Errorf("missing 'done' event in streamed events: %v", events)
	}
}
