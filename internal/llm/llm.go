package llm

import (
	"context"
	"encoding/json"
)

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolSpec describes a single tool schema exposed to the model for one micro-task.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall represents a model-requested function invocation.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message represents a single turn in an isolated micro-task conversation.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Name       string     `json:"name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ChatRequest configures a single bounded completion request.
type ChatRequest struct {
	Model          string          `json:"model"`
	Messages       []Message       `json:"messages"`
	Tools          []ToolSpec      `json:"tools,omitempty"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    float64         `json:"temperature,omitempty"`
	BaseURL        string          `json:"base_url,omitempty"`
	APIKey         string          `json:"api_key,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

// ChatResponse holds the cleaned assistant output, optional reasoning, and token usage.
type ChatResponse struct {
	Content          string
	Reasoning        string
	ToolCalls        []ToolCall
	FinishReason     string
	PromptTokens     int
	CompletionTokens int
}

// Provider abstracts local or compatible LLM inference backends.
type Provider interface {
	CheckHealth(ctx context.Context) error
	CountTokens(ctx context.Context, model string, messages []Message, tools []ToolSpec) (int, error)
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
