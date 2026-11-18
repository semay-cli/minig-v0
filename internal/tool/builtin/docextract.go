package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/textutil"
)

type docExtractTool struct{}

func (t *docExtractTool) Name() string   { return "doc_extract" }
func (t *docExtractTool) ReadOnly() bool { return true }
func (t *docExtractTool) Description() string {
	return "Fetch a runbook, OpenAPI spec, or service document over HTTP, or clean a supplied HTML/text blob, and return compact Markdown-style text."
}

func (t *docExtractTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"url": {"type": "string", "description": "HTTP or HTTPS document URL"},
			"content": {"type": "string", "description": "Raw HTML or text to clean when url is omitted"},
			"max_bytes": {"type": "integer", "description": "Maximum bytes to read from url (default 32768, max 131072)"}
		}
	}`)
}

func (t *docExtractTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		URL      string `json:"url"`
		Content  string `json:"content"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse doc_extract arguments: %w", err)
	}

	rawURL := strings.TrimSpace(args.URL)
	rawContent := args.Content
	if rawURL == "" && strings.TrimSpace(rawContent) == "" {
		return "", errors.New("url or content is required")
	}

	source := "inline"
	if rawURL != "" {
		if !strings.HasPrefix(rawURL, "http://") && !strings.HasPrefix(rawURL, "https://") {
			return "", errors.New("url must begin with http:// or https://")
		}
		maxBytes := args.MaxBytes
		if maxBytes <= 0 {
			maxBytes = 32 * 1024
		}
		if maxBytes > 128*1024 {
			maxBytes = 128 * 1024
		}
		body, err := fetchDoc(ctx, rawURL, maxBytes)
		if err != nil {
			return "", err
		}
		rawContent = body
		source = rawURL
	}

	cleaned := cleanHTML(rawContent)
	if cleaned == "" {
		cleaned = strings.TrimSpace(rawContent)
	}
	cleaned = textutil.TruncateBytes(textutil.CompactWhitespace(cleaned), 12*1024)
	return fmt.Sprintf("# Document extract (%s)\n\n%s", source, cleaned), nil
}

func fetchDoc(ctx context.Context, rawURL string, maxBytes int) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "minig/0.1.0 (doc_extract)")
	req.Header.Set("Accept", "text/html,text/plain,application/json,application/yaml;q=0.9")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %q: %w", rawURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("doc_extract %q returned HTTP %d %s", rawURL, resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read %q: %w", rawURL, err)
	}
	if len(raw) > maxBytes {
		raw = raw[:maxBytes]
	}
	return string(raw), nil
}
