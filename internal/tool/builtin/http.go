package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout   = 10 * time.Second
	maxHTTPTimeout       = 30 * time.Second
	maxHTTPResponseBytes = 256 * 1024 // 256KB
)

type observabilityHTTPTool struct{}

func (t *observabilityHTTPTool) Name() string   { return "observability_http" }
func (t *observabilityHTTPTool) ReadOnly() bool { return true }
func (t *observabilityHTTPTool) Description() string {
	return "Perform an HTTP request and return the response body. Supports GET, POST, PUT with custom headers, auth, query params, timeouts, and JSONPath extraction for querying Loki, Prometheus, health checks, or REST APIs."
}

func (t *observabilityHTTPTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["url"],
		"properties": {
			"url": {"type": "string", "description": "Target HTTP/HTTPS URL"},
			"method": {"type": "string", "description": "HTTP method: GET, POST, PUT (default GET)"},
			"headers": {"type": "object", "description": "Optional request headers as key-value pairs"},
			"body": {"type": "string", "description": "Optional request body for POST/PUT"},
			"auth_token": {"type": "string", "description": "Optional Bearer token for Authorization header"},
			"basic_user": {"type": "string", "description": "Optional HTTP basic auth username"},
			"basic_password": {"type": "string", "description": "Optional HTTP basic auth password"},
			"query": {"type": "object", "description": "Optional query parameters merged into the URL"},
			"backend": {"type": "string", "description": "Optional helper: loki or prometheus. Appends the query_range path when missing"},
			"query_expr": {"type": "string", "description": "LogQL or PromQL expression used with backend"},
			"timeout_seconds": {"type": "integer", "description": "Request timeout in seconds (default 10, max 30)"},
			"json_path": {"type": "string", "description": "Dot-separated JSON path to extract from response (e.g. 'data.result')"},
			"extract_regex": {"type": "string", "description": "Optional regex; up to 20 matches replace the body"}
		}
	}`)
}

func (t *observabilityHTTPTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		URL            string            `json:"url"`
		Method         string            `json:"method"`
		Headers        map[string]string `json:"headers"`
		Body           string            `json:"body"`
		AuthToken      string            `json:"auth_token"`
		BasicUser      string            `json:"basic_user"`
		BasicPassword  string            `json:"basic_password"`
		Query          map[string]string `json:"query"`
		Backend        string            `json:"backend"`
		QueryExpr      string            `json:"query_expr"`
		TimeoutSeconds int               `json:"timeout_seconds"`
		JSONPath       string            `json:"json_path"`
		ExtractRegex   string            `json:"extract_regex"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse observability_http arguments: %w", err)
	}

	trimmedURL := strings.TrimSpace(args.URL)
	if trimmedURL == "" {
		return "", errors.New("url is required")
	}
	if !strings.HasPrefix(trimmedURL, "http://") && !strings.HasPrefix(trimmedURL, "https://") {
		return "", errors.New("url must begin with http:// or https://")
	}

	trimmedURL, err := applyObservabilityURL(trimmedURL, args.Backend, args.QueryExpr, args.Query)
	if err != nil {
		return "", err
	}

	method := strings.ToUpper(strings.TrimSpace(args.Method))
	if method == "" {
		method = http.MethodGet
	}
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut:
		// allowed
	default:
		return "", fmt.Errorf("unsupported HTTP method %q: only GET, POST, PUT are supported", method)
	}

	timeout := defaultHTTPTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > maxHTTPTimeout {
			timeout = maxHTTPTimeout
		}
	}

	var bodyReader io.Reader
	if len(args.Body) > 0 {
		bodyReader = bytes.NewReader([]byte(args.Body))
	}

	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, method, trimmedURL, bodyReader)
	if err != nil {
		return "", fmt.Errorf("create HTTP request: %w", err)
	}

	// Set custom headers
	for k, v := range args.Headers {
		req.Header.Set(k, v)
	}

	// Set Authorization if provided. Bearer wins over basic auth.
	if strings.TrimSpace(args.AuthToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(args.AuthToken))
	} else if strings.TrimSpace(args.BasicUser) != "" {
		req.SetBasicAuth(args.BasicUser, args.BasicPassword)
	}

	// Default User-Agent if unset
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "minig/0.1.0 (observability_http)")
	}

	client := &http.Client{
		Timeout: timeout,
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP %s %s failed: %w", method, trimmedURL, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("read response body: %w", err)
	}

	truncated := false
	if len(respBytes) > maxHTTPResponseBytes {
		respBytes = respBytes[:maxHTTPResponseBytes]
		truncated = true
	}

	bodyStr := string(respBytes)

	if pattern := strings.TrimSpace(args.ExtractRegex); pattern != "" && len(pattern) <= 256 {
		if re, err := regexp.Compile(pattern); err == nil {
			found := re.FindAllString(bodyStr, 20)
			if len(found) > 0 {
				bodyStr = strings.Join(found, "\n")
			}
		}
	}

	// If json_path requested, extract sub-element
	if strings.TrimSpace(args.JSONPath) != "" {
		extracted, err := extractJSONPath(respBytes, strings.TrimSpace(args.JSONPath))
		if err == nil {
			bodyStr = extracted
		} else {
			bodyStr = fmt.Sprintf("(json_path %q extraction failed: %v)\n%s", args.JSONPath, err, bodyStr)
		}
	}

	var out strings.Builder
	fmt.Fprintf(&out, "HTTP %d %s\n", resp.StatusCode, http.StatusText(resp.StatusCode))
	out.WriteString(bodyStr)
	if truncated {
		out.WriteString("\n...[response body truncated at 256KB]")
	}
	return out.String(), nil
}

// extractJSONPath traverses a parsed JSON object using dot notation (e.g., "data.result.0.value").
func extractJSONPath(data []byte, path string) (string, error) {
	var current any
	if err := json.Unmarshal(data, &current); err != nil {
		return "", err
	}

	parts := strings.Split(path, ".")
	for _, part := range parts {
		if part == "" {
			continue
		}
		switch val := current.(type) {
		case map[string]any:
			next, ok := val[part]
			if !ok {
				return "", fmt.Errorf("key %q not found", part)
			}
			current = next
		case []any:
			var idx int
			if _, err := fmt.Sscanf(part, "%d", &idx); err != nil {
				return "", fmt.Errorf("expected array index in %q, got: %w", part, err)
			}
			if idx < 0 || idx >= len(val) {
				return "", fmt.Errorf("array index %d out of bounds (len=%d)", idx, len(val))
			}
			current = val[idx]
		default:
			return "", fmt.Errorf("cannot navigate into non-container type %T with %q", current, part)
		}
	}

	encoded, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", current), nil
	}
	return string(encoded), nil
}

func applyObservabilityURL(rawURL, backend, queryExpr string, query map[string]string) (string, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse url: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "loki":
		if !strings.Contains(parsed.Path, "/loki/api/") {
			parsed.Path = strings.TrimRight(parsed.Path, "/") + "/loki/api/v1/query_range"
		}
	case "prometheus":
		if !strings.Contains(parsed.Path, "/api/v1/") {
			parsed.Path = strings.TrimRight(parsed.Path, "/") + "/api/v1/query_range"
		}
	case "":
	default:
		return "", fmt.Errorf("unsupported observability backend %q", backend)
	}
	q := parsed.Query()
	if expr := strings.TrimSpace(queryExpr); expr != "" {
		q.Set("query", expr)
	}
	for k, v := range query {
		if strings.TrimSpace(k) != "" {
			q.Set(k, v)
		}
	}
	parsed.RawQuery = q.Encode()
	return parsed.String(), nil
}
