package llamacpp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/llm"
)

func TestChatStripsThinkAndParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		var req struct {
			Tools      []wireTool `json:"tools"`
			ToolChoice string     `json:"tool_choice"`
			Stream     bool       `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Stream || len(req.Tools) != 1 || req.ToolChoice != "auto" {
			t.Fatalf("unexpected wire request: %+v", req)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"content": "<think>inspect main.go first</think>Reading file.",
					"tool_calls": [{
						"id": "call_1",
						"type": "function",
						"function": {
							"name": "read_range",
							"arguments": "{\"path\":\"main.go\",\"start\":1}"
						}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 42, "completion_tokens": 18}
		}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	resp, err := client.Chat(context.Background(), llm.ChatRequest{
		Model:    "qwen3-4b",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "inspect main.go"}},
		Tools: []llm.ToolSpec{{
			Name:        "read_range",
			Description: "read lines",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
		MaxTokens:   256,
		Temperature: 0.1,
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if resp.Content != "Reading file." || resp.Reasoning != "inspect main.go first" {
		t.Errorf("unexpected content/reasoning: %+v", resp)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_range" {
		t.Errorf("unexpected tool calls: %+v", resp.ToolCalls)
	}
	if resp.PromptTokens != 42 || resp.CompletionTokens != 18 {
		t.Errorf("unexpected usage: %+v", resp)
	}
}

func TestCountTokensEndpointAndFallback(t *testing.T) {
	// 1. Endpoint supported
	srvOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write([]byte(`{"input_tokens": 19}`))
	}))
	defer srvOK.Close()

	c1, err := NewClient(srvOK.URL, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()

	if err := c1.CheckHealth(context.Background()); err != nil {
		t.Fatalf("CheckHealth failed: %v", err)
	}
	tokens, err := c1.CountTokens(context.Background(), "local", []llm.Message{{Role: llm.RoleUser, Content: "hello"}}, nil)
	if err != nil || tokens != 19 {
		t.Fatalf("CountTokens = (%d, %v), want (19, nil)", tokens, err)
	}

	// 2. Fallback on 404
	srv404 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv404.Close()

	c2, err := NewClient(srv404.URL, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()

	est, err := c2.CountTokens(context.Background(), "local", []llm.Message{{Role: llm.RoleUser, Content: "hello world"}}, nil)
	if err != nil || est <= 0 {
		t.Fatalf("expected positive fallback token estimate, got (%d, %v)", est, err)
	}
}

func TestNewClientLoopbackGuard(t *testing.T) {
	if _, err := NewClient("http://10.0.0.5:8080", time.Second, false); err == nil {
		t.Fatal("expected remote IP to be rejected when allowRemote is false")
	}
	c, err := NewClient("http://10.0.0.5:8080", time.Second, true)
	if err != nil {
		t.Fatalf("expected remote IP to succeed when allowRemote is true: %v", err)
	}
	c.Close()
}
