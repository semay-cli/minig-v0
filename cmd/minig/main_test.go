package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
)

func TestCLIHealthAndEndToEndMicroRun(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# minig-v0\nMicro-task agent.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var chatCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			w.WriteHeader(http.StatusOK)
		case "/v1/chat/completions/input_tokens":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens": 25}`))
		case "/v1/chat/completions":
			callNum := chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch callNum {
			case 1:
				// Planner decomposes into 1 micro-task using read_range
				_, _ = w.Write([]byte(`{
					"choices": [{"message": {"content": "{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Read README.md\",\"tools\":[\"read_range\"]}]}"}, "finish_reason": "stop"}],
					"usage": {"prompt_tokens": 25, "completion_tokens": 15}
				}`))
			case 2:
				// Micro-task t1 invokes read_range
				_, _ = w.Write([]byte(`{
					"choices": [{"message": {"content": "", "tool_calls": [{"id": "c1", "type": "function", "function": {"name": "read_range", "arguments": "{\"path\":\"README.md\"}"}}]}, "finish_reason": "tool_calls"}],
					"usage": {"prompt_tokens": 30, "completion_tokens": 12}
				}`))
			default:
				// Micro-task t1 completes with final summary
				_, _ = w.Write([]byte(`{
					"choices": [{"message": {"content": "README describes minig-v0 as a micro-task agent."}, "finish_reason": "stop"}],
					"usage": {"prompt_tokens": 45, "completion_tokens": 14}
				}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// 1. Test --health
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), []string{"--url", server.URL, "--health"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("--health failed: %v", err)
	}
	if !strings.Contains(stdout.String(), "healthy") {
		t.Errorf("unexpected health output: %q", stdout.String())
	}

	// 2. Test full micro-task run with --json
	stdout.Reset()
	stderr.Reset()
	err = run(context.Background(), []string{
		"--url", server.URL,
		"--workspace", workspace,
		"--json",
		"Summarize README.md",
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("CLI run failed: %v", err)
	}

	var result agent.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("unmarshal CLI JSON output: %v\nraw: %s", err, stdout.String())
	}
	if result.Answer != "README describes minig-v0 as a micro-task agent." {
		t.Errorf("Answer = %q", result.Answer)
	}
	if result.Stats.ToolCalls != 1 || result.Stats.TasksCompleted != 1 {
		t.Errorf("unexpected stats: %+v", result.Stats)
	}
}

func TestCLIServeMode(t *testing.T) {
	workspace := t.TempDir()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var stdout, stderr bytes.Buffer

	runErr := make(chan error, 1)
	go func() {
		runErr <- run(ctx, []string{
			"--url", server.URL,
			"--workspace", workspace,
			"--serve",
			"--port", "19080",
		}, &stdout, &stderr)
	}()

	// Wait briefly for server to bind
	time.Sleep(100 * time.Millisecond)

	// Verify health endpoint responds
	res, err := http.Get("http://127.0.0.1:19080/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health failed: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", res.StatusCode)
	}

	// Trigger graceful cancellation
	cancel()

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("expected clean shutdown, got: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not shut down in time")
	}
}
