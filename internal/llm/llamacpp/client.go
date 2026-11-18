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
	"regexp"
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
	baseURL     string
	apiKey      string
	allowRemote bool
	http        *http.Client
}

// NewClient constructs a bounded HTTP client for llama-server without authentication.
func NewClient(rawURL string, timeout time.Duration, allowRemote bool) (*Client, error) {
	return NewClientWithAuth(rawURL, "", timeout, allowRemote)
}

// NewClientWithAuth constructs a bounded HTTP client for llama-server with an optional API key/token.
func NewClientWithAuth(rawURL, apiKey string, timeout time.Duration, allowRemote bool) (*Client, error) {
	if timeout <= 0 {
		return nil, errors.New("request timeout must be greater than zero")
	}
	baseURL, err := normalizeBaseURL(rawURL, allowRemote)
	if err != nil {
		return nil, err
	}
	return &Client{
		baseURL:     baseURL,
		apiKey:      strings.TrimSpace(apiKey),
		allowRemote: allowRemote,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				Proxy:                  nil,
				DisableCompression:     true,
				DialContext:            (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
				MaxConnsPerHost:        4,
				MaxIdleConns:           8,
				MaxIdleConnsPerHost:    4,
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

// CheckHealth verifies that llama-server is reachable and ready via /health or /props.
func (c *Client) CheckHealth(ctx context.Context) error {
	// Try /health first
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return fmt.Errorf("create health request: %w", err)
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(req)
	if err == nil {
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4*1024))
		if resp.StatusCode == http.StatusOK {
			return nil
		}
		// If 404, fallback to /props
		if resp.StatusCode != http.StatusNotFound {
			return fmt.Errorf("llama.cpp health endpoint returned %s", resp.Status)
		}
	}

	// Fallback to /props (llama-server slot introspection)
	propsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/props", nil)
	if err != nil {
		return fmt.Errorf("create props request: %w", err)
	}
	if c.apiKey != "" {
		propsReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	propsResp, err := c.http.Do(propsReq)
	if err != nil {
		return fmt.Errorf("llama.cpp health/props check failed: %w", err)
	}
	defer propsResp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(propsResp.Body, 4*1024))
	if propsResp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama.cpp props endpoint returned %s", propsResp.Status)
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
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

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

	requestPayload := map[string]any{
		"model":       input.Model,
		"messages":    wireMsgs,
		"tools":       wireTools,
		"max_tokens":  maxTokens,
		"temperature": input.Temperature,
		"stream":      false,
	}
	if toolChoice != "" {
		requestPayload["tool_choice"] = toolChoice
	}
	if len(input.ResponseFormat) > 0 {
		requestPayload["response_format"] = input.ResponseFormat
	}

	body, err := json.Marshal(requestPayload)
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("encode chat request: %w", err)
	}

	targetURL := c.baseURL
	if strings.TrimSpace(input.BaseURL) != "" {
		normalized, err := normalizeBaseURL(input.BaseURL, true)
		if err == nil {
			targetURL = normalized
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return llm.ChatResponse{}, fmt.Errorf("create chat request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	targetKey := c.apiKey
	if strings.TrimSpace(input.APIKey) != "" {
		targetKey = strings.TrimSpace(input.APIKey)
	}
	if targetKey != "" {
		req.Header.Set("Authorization", "Bearer "+targetKey)
	}

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

	// Fallback tool-calling parser if the model emitted tool calls in textual content instead of native tool_calls
	if len(toolCalls) == 0 && len(wireTools) > 0 {
		fallbackContent, fallbackCalls := parseFallbackToolCalls(cleanContent)
		if len(fallbackCalls) > 0 {
			cleanContent = fallbackContent
			toolCalls = fallbackCalls
		}
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

// XML / JSON fallback patterns for non-native llama.cpp tool calling
var (
	toolCallXMLRegex   = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)
	jsonCodeBlockRegex = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*?\\}|\\[.*?\\])\\s*```")
)

// parseFallbackToolCalls extracts structured tool calls from conversational content
func parseFallbackToolCalls(content string) (string, []llm.ToolCall) {
	var calls []llm.ToolCall

	// 1. Try <tool_call> ... </tool_call> XML tags
	if matches := toolCallXMLRegex.FindAllStringSubmatchIndex(content, -1); len(matches) > 0 {
		cleaned := content
		offset := 0
		for idx, m := range matches {
			fullStart, fullEnd := m[0]-offset, m[1]-offset
			groupStart, groupEnd := m[2]-offset, m[3]-offset
			rawJSON := cleaned[groupStart:groupEnd]

			var callItem struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(rawJSON), &callItem); err == nil && callItem.Name != "" {
				args := callItem.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				calls = append(calls, llm.ToolCall{
					ID:        fmt.Sprintf("fallback_call_%d", idx+1),
					Name:      callItem.Name,
					Arguments: args,
				})
				// Strip the <tool_call> block from cleaned text
				cleaned = cleaned[:fullStart] + cleaned[fullEnd:]
				offset += (fullEnd - fullStart)
			}
		}
		if len(calls) > 0 {
			return strings.TrimSpace(cleaned), calls
		}
	}

	// 2. Try markdown fenced code blocks with {"name": ..., "arguments": ...} or {"tool_calls": [...]}
	if matches := jsonCodeBlockRegex.FindAllStringSubmatchIndex(content, -1); len(matches) > 0 {
		cleaned := content
		offset := 0
		for idx, m := range matches {
			fullStart, fullEnd := m[0]-offset, m[1]-offset
			groupStart, groupEnd := m[2]-offset, m[3]-offset
			rawJSON := cleaned[groupStart:groupEnd]

			// Check for single tool call {"name": "...", "arguments": ...}
			var single struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(rawJSON), &single); err == nil && single.Name != "" {
				args := single.Arguments
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				calls = append(calls, llm.ToolCall{
					ID:        fmt.Sprintf("fallback_call_%d", idx+1),
					Name:      single.Name,
					Arguments: args,
				})
				cleaned = cleaned[:fullStart] + cleaned[fullEnd:]
				offset += (fullEnd - fullStart)
				continue
			}

			// Check for array of calls [{"name": "..."}]
			var list []struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal([]byte(rawJSON), &list); err == nil && len(list) > 0 && list[0].Name != "" {
				for j, item := range list {
					args := item.Arguments
					if len(args) == 0 {
						args = json.RawMessage("{}")
					}
					calls = append(calls, llm.ToolCall{
						ID:        fmt.Sprintf("fallback_call_%d_%d", idx+1, j+1),
						Name:      item.Name,
						Arguments: args,
					})
				}
				cleaned = cleaned[:fullStart] + cleaned[fullEnd:]
				offset += (fullEnd - fullStart)
			}
		}
		if len(calls) > 0 {
			return strings.TrimSpace(cleaned), calls
		}
	}

	return content, nil
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
