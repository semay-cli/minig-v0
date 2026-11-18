package llamacpp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

const (
	MaxMessages        = 64
	MaxMessageBytes    = 32 * 1024
	MaxPromptBytes     = 64 * 1024
	MaxOutputTokens    = 4096
	DefaultOutputToken = 1024
	MaxResponseBytes   = 1 << 20
	MaxMetadataBytes   = 64 * 1024
)

// Client communicates with a llama.cpp OpenAI-compatible HTTP server.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient constructs a bounded HTTP client for llama-server.
func NewClient(rawURL string, timeout time.Duration, allowRemote bool) (*Client, error) {
	if timeout <= 0 {
		return nil, errors.New("request timeout must be greater than zero")
	}
	baseURL, err := normalizeBaseURL(rawURL, allowRemote)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL: baseURL,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                  nil,
				DisableCompression:     true,
				DialContext:            (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxConnsPerHost:        2,
				MaxIdleConns:           4,
				MaxIdleConnsPerHost:    2,
				IdleConnTimeout:        30 * time.Second,
				TLSHandshakeTimeout:    2 * time.Second,
				ResponseHeaderTimeout:  timeout,
				ExpectContinueTimeout:  time.Second,
				MaxResponseHeaderBytes: 16 * 1024,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// Close releases idle HTTP connections.
func (c *Client) Close() {
	if c != nil && c.http != nil {
		c.http.CloseIdleConnections()
	}
}

// CheckHealth verifies that llama-server is reachable and ready.
func (c *Client) CheckHealth(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("llama.cpp health check failed: %w", err)
	}
	defer resp.Body.Close()

	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024)); err != nil {
		return fmt.Errorf("read health response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp health endpoint returned %s", resp.Status)
	}
	return nil
}

// CountTokens queries llama.cpp's /v1/chat/completions/input_tokens endpoint and
// falls back to local estimation if the server build does not expose that route.
func (c *Client) CountTokens(ctx context.Context, model string, messages []llm.Message, tools []llm.ToolSpec) (int, error) {
	if strings.TrimSpace(model) == "" {
		return 0, errors.New("model must not be empty")
	}
	wireMsgs, err := toWireMessages(messages)
	if err != nil {
		return 0, err
	}
	wireTools, err := toWireTools(tools)
	if err != nil {
		return 0, err
	}

	payload := struct {
		Model    string        `json:"model"`
		Messages []wireMessage `json:"messages"`
		Tools    []wireTool    `json:"tools,omitempty"`
	}{
		Model:    model,
		Messages: wireMsgs,
		Tools:    wireTools,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("encode token count request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions/input_tokens", bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("create token count request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, fmt.Errorf("token count request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxMetadataBytes+1))
	if err != nil {
		return 0, fmt.Errorf("read token count response: %w", err)
	}
	if len(respBody) > MaxMetadataBytes {
		return 0, fmt.Errorf("token count response exceeds %d bytes", MaxMetadataBytes)
	}

	// Graceful fallback if an older llama.cpp binary does not implement /input_tokens.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return estimatePromptTokens(messages, tools), nil
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("token count endpoint returned %s: %s", resp.Status, extractErrorMessage(respBody))
	}

	var decoded struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return 0, fmt.Errorf("decode token count response: %w", err)
	}
	if decoded.InputTokens < 0 {
		return 0, errors.New("llama.cpp returned a negative token count")
	}
	return decoded.InputTokens, nil
}

// Chat executes a non-streaming completion against /v1/chat/completions.
func (c *Client) Chat(ctx context.Context, input llm.ChatRequest) (llm.ChatResponse, error) {
	if strings.TrimSpace(input.Model) == "" {
		return llm.ChatResponse{}, errors.New("model must not be empty")
	}
	maxTokens := input.MaxTokens
	if maxTokens == 0 {
		maxTokens = DefaultOutputToken
	}
	if maxTokens < 1 || maxTokens > MaxOutputTokens {
		return llm.ChatResponse{}, fmt.Errorf("max tokens must be between 1 and %d", MaxOutputTokens)
	}
	if input.Temperature < 0 || input.Temperature > 2 {
		return llm.ChatResponse{}, errors.New("temperature must be between 0 and 2")
	}

	wireMsgs, err := toWireMessages(input.Messages)
	if err != nil {
		return llm.ChatResponse{}, err
	}
	wireTools, err := toWireTools(input.Tools)
	if err != nil {
		return llm.ChatResponse{}, err
	}

	toolChoice := ""
	if len(wireTools) > 0 {
		toolChoice = "auto"
	}

	body, err := json.Marshal(struct {
		Model       string        `json:"model"`
		Messages    []wireMessage `json:"messages"`
		Tools       []wireTool    `json:"tools,omitempty"`
		ToolChoice  string        `json:"tool_choice,omitempty"`
		MaxTokens   int           `json:"max_tokens"`
		Temperature float64       `json:"temperature"`
		Stream      bool          `json:"stream"`
	}{
		Model:       input.Model,
		Messages:    wireMsgs,
		Tools:       wireTools,
		ToolChoice:  toolChoice,
		MaxTokens:   maxTokens,
		Temperature: input.Temperature,
		Stream:      false,
	})
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("encode chat request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("llama.cpp chat request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("read chat response: %w", err)
	}
	if len(respBody) > MaxResponseBytes {
		return llm.ChatResponse{}, fmt.Errorf("chat response exceeds %d bytes", MaxResponseBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return llm.ChatResponse{}, fmt.Errorf("chat endpoint returned %s: %s", resp.Status, extractErrorMessage(respBody))
	}

	var decoded struct {
		Choices []struct {
			Message struct {
				Content          string         `json:"content"`
				ReasoningContent string         `json:"reasoning_content"`
				ToolCalls        []wireToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &decoded); err != nil {
		return llm.ChatResponse{}, fmt.Errorf("decode chat response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return llm.ChatResponse{}, errors.New("llama.cpp chat response contained no choices")
	}

	choice := decoded.Choices[0]
	cleanContent, inlineThought := textutil.StripThink(choice.Message.Content)
	reasoning := strings.TrimSpace(choice.Message.ReasoningContent)
	if inlineThought != "" {
		if reasoning != "" {
			reasoning += "\n" + inlineThought
		} else {
			reasoning = inlineThought
		}
	}

	toolCalls := make([]llm.ToolCall, 0, len(choice.Message.ToolCalls))
	for i, tc := range choice.Message.ToolCalls {
		name := strings.TrimSpace(tc.Function.Name)
		if name == "" {
			return llm.ChatResponse{}, fmt.Errorf("tool call %d missing function name", i)
		}
		rawArgs := strings.TrimSpace(tc.Function.Arguments)
		if rawArgs == "" {
			rawArgs = "{}"
		}
		if !json.Valid([]byte(rawArgs)) {
			return llm.ChatResponse{}, fmt.Errorf("tool call %q has invalid JSON arguments", name)
		}
		id := strings.TrimSpace(tc.ID)
		if id == "" {
			id = fmt.Sprintf("call_%d", i+1)
		}
		toolCalls = append(toolCalls, llm.ToolCall{
			ID:        id,
			Name:      name,
			Arguments: json.RawMessage(rawArgs),
		})
	}

	promptTokens := decoded.Usage.PromptTokens
	if promptTokens == 0 {
		promptTokens = estimatePromptTokens(input.Messages, input.Tools)
	}
	completionTokens := decoded.Usage.CompletionTokens
	if completionTokens == 0 {
		completionTokens = textutil.EstimateTokens(choice.Message.Content)
	}

	return llm.ChatResponse{
		Content:          cleanContent,
		Reasoning:        reasoning,
		ToolCalls:        toolCalls,
		FinishReason:     choice.FinishReason,
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
	}, nil
}

type wireTool struct {
	Type     string           `json:"type"`
	Function wireToolFunction `json:"function"`
}

type wireToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type wireToolCall struct {
	ID       string               `json:"id"`
	Type     string               `json:"type"`
	Function wireToolCallFunction `json:"function"`
}

type wireToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type wireMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	Name       string         `json:"name,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

func toWireMessages(messages []llm.Message) ([]wireMessage, error) {
	if len(messages) == 0 || len(messages) > MaxMessages {
		return nil, fmt.Errorf("messages must contain between 1 and %d items", MaxMessages)
	}
	totalBytes := 0
	out := make([]wireMessage, 0, len(messages))

	for _, msg := range messages {
		switch msg.Role {
		case llm.RoleSystem, llm.RoleUser, llm.RoleAssistant, llm.RoleTool:
		default:
			return nil, fmt.Errorf("unsupported message role %q", msg.Role)
		}

		if len(msg.Content) > MaxMessageBytes {
			return nil, fmt.Errorf("message content exceeds %d bytes", MaxMessageBytes)
		}
		if strings.TrimSpace(msg.Content) == "" && len(msg.ToolCalls) == 0 {
			return nil, errors.New("message must contain non-empty content or tool calls")
		}
		if msg.Role == llm.RoleTool && strings.TrimSpace(msg.ToolCallID) == "" {
			return nil, errors.New("tool message requires tool_call_id")
		}

		totalBytes += len(msg.Content)
		wm := wireMessage{
			Role:       string(msg.Role),
			Content:    msg.Content,
			Name:       msg.Name,
			ToolCallID: msg.ToolCallID,
		}
		if len(msg.ToolCalls) > 0 {
			wm.ToolCalls = make([]wireToolCall, 0, len(msg.ToolCalls))
			for _, tc := range msg.ToolCalls {
				args := "{}"
				if len(tc.Arguments) > 0 {
					args = string(tc.Arguments)
				}
				totalBytes += len(tc.Name) + len(args)
				wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: wireToolCallFunction{
						Name:      tc.Name,
						Arguments: args,
					},
				})
			}
		}
		if totalBytes > MaxPromptBytes {
			return nil, fmt.Errorf("total prompt payload exceeds %d bytes", MaxPromptBytes)
		}
		out = append(out, wm)
	}
	return out, nil
}

func toWireTools(tools []llm.ToolSpec) ([]wireTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]wireTool, 0, len(tools))
	for _, t := range tools {
		if strings.TrimSpace(t.Name) == "" {
			return nil, errors.New("tool spec name must not be empty")
		}
		params := t.Parameters
		if len(params) == 0 {
			params = json.RawMessage(`{"type":"object","properties":{}}`)
		} else if !json.Valid(params) {
			return nil, fmt.Errorf("tool %q has invalid parameter schema JSON", t.Name)
		}
		out = append(out, wireTool{
			Type: "function",
			Function: wireToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out, nil
}

func estimatePromptTokens(messages []llm.Message, tools []llm.ToolSpec) int {
	total := 0
	for _, m := range messages {
		total += textutil.EstimateTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			total += textutil.EstimateTokens(tc.Name) + textutil.EstimateTokens(string(tc.Arguments))
		}
	}
	for _, t := range tools {
		total += textutil.EstimateTokens(t.Name) + textutil.EstimateTokens(t.Description) + textutil.EstimateTokens(string(t.Parameters))
	}
	return total
}

func extractErrorMessage(body []byte) string {
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err == nil && decoded.Error.Message != "" {
		return decoded.Error.Message
	}
	msg := strings.TrimSpace(string(body))
	if len(msg) > 512 {
		msg = msg[:512] + "..."
	}
	if msg == "" {
		return "empty response body"
	}
	return msg
}

func normalizeBaseURL(raw string, allowRemote bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse llama URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("llama URL must be http/https with a host and no credentials, query, or fragment")
	}
	if !allowRemote {
		host := parsed.Hostname()
		if !strings.EqualFold(host, "localhost") {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return "", errors.New("llama URL must use localhost or a loopback IP unless allowRemote is true")
			}
		}
	}
	if port := parsed.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return "", fmt.Errorf("llama URL has invalid port %q", port)
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}
