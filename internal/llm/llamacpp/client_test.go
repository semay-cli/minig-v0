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

func TestChatWithAuthAndPerRequestOverrides(t *testing.T) {
	var receivedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {"content": "ok from server"},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 5}
		}`))
	}))
	defer server.Close()

	// Initialized with client auth
	client, err := NewClientWithAuth("http://127.0.0.1:8080", "default-key", 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// Dynamic per-request override pointing to mock server with custom key
	resp, err := client.Chat(context.Background(), llm.ChatRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "ping"}},
		BaseURL:  server.URL,
		APIKey:   "custom-req-key",
	})
	if err != nil {
		t.Fatalf("Chat override failed: %v", err)
	}
	if receivedAuth != "Bearer custom-req-key" {
		t.Errorf("got auth %q, want 'Bearer custom-req-key'", receivedAuth)
	}
	if resp.Content != "ok from server" {
		t.Errorf("got content %q, want 'ok from server'", resp.Content)
	}
}

func TestFallbackToolParserXMLAndJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Server returns no native tool_calls, but content contains <tool_call> XML
		_, _ = w.Write([]byte(`{
			"choices": [{
				"message": {
					"content": "I will query Loki.\n<tool_call>\n{\"name\": \"observability_http\", \"arguments\": {\"url\": \"http://loki:3100\"}}\n</tool_call>\nDone.",
					"tool_calls": []
				},
				"finish_reason": "stop"
			}],
			"usage": {"prompt_tokens": 50, "completion_tokens": 30}
		}`))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	resp, err := client.Chat(context.Background(), llm.ChatRequest{
		Model:    "test-model",
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "fetch logs"}},
		Tools: []llm.ToolSpec{{
			Name:        "observability_http",
			Description: "query endpoint",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	})
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("expected 1 fallback tool call, got %d", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "observability_http" {
		t.Errorf("expected tool name 'observability_http', got %q", resp.ToolCalls[0].Name)
	}
}

func TestCheckHealthPropsFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.URL.Path == "/props" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"default_generation_settings": {}}`))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, 5*time.Second, false)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	if err := client.CheckHealth(context.Background()); err != nil {
		t.Fatalf("expected CheckHealth to succeed via /props fallback, got: %v", err)
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
