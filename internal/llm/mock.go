package llm

import (
	"context"
	"errors"
	"sync"

	"github.com/semay-cli/minig-v0/internal/textutil"
)

// MockProvider is a thread-safe scripted Provider for unit testing.
type MockProvider struct {
	mu        sync.Mutex
	HealthErr error
	Responses []ChatResponse
	Requests  []ChatRequest
	TokenFn   func(messages []Message, tools []ToolSpec) int
}

func (m *MockProvider) CheckHealth(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.HealthErr
}

func (m *MockProvider) CountTokens(ctx context.Context, _ string, messages []Message, tools []ToolSpec) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	fn := m.TokenFn
	m.mu.Unlock()

	if fn != nil {
		return fn(messages, tools), nil
	}
	total := 0
	for _, msg := range messages {
		total += textutil.EstimateTokens(msg.Content) + 4
		for _, tc := range msg.ToolCalls {
			total += textutil.EstimateTokens(tc.Name) + textutil.EstimateTokens(string(tc.Arguments))
		}
	}
	for _, tool := range tools {
		total += textutil.EstimateTokens(tool.Name) + textutil.EstimateTokens(tool.Description) + textutil.EstimateTokens(string(tool.Parameters))
	}
	return total, nil
}

func (m *MockProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if err := ctx.Err(); err != nil {
		return ChatResponse{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	m.Requests = append(m.Requests, req)
	if len(m.Responses) == 0 {
		return ChatResponse{}, errors.New("mock provider has no queued responses")
	}
	next := m.Responses[0]
	m.Responses = m.Responses[1:]

	if next.PromptTokens == 0 {
		for _, msg := range req.Messages {
			next.PromptTokens += textutil.EstimateTokens(msg.Content) + 4
		}
	}
	if next.CompletionTokens == 0 {
		next.CompletionTokens = textutil.EstimateTokens(next.Content) + 1
	}
	return next, nil
}
