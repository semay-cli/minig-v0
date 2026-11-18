package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/textutil"
)

var (
	scriptTagRegex  = regexp.MustCompile(`(?is)<script.*?</script>`)
	styleTagRegex   = regexp.MustCompile(`(?is)<style.*?</style>`)
	htmlTagRegex    = regexp.MustCompile(`<[^>]+>`)
	blankLinesRegex = regexp.MustCompile(`\n{3,}`)
)

type webFetchTool struct{}

func (t *webFetchTool) Name() string   { return "web_fetch" }
func (t *webFetchTool) ReadOnly() bool { return true }
func (t *webFetchTool) Description() string {
	return "Fetch an external web page, runbook, or OpenAPI/API doc over HTTP and strip HTML into clean Markdown/text for context grounding."
}

func (t *webFetchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["url"],
		"properties": {
			"url": {"type": "string", "description": "Target HTTP/HTTPS URL"},
			"max_bytes": {"type": "integer", "description": "Maximum bytes to fetch (default 32768, max 131072)"}
		}
	}`)
}

func (t *webFetchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		URL      string `json:"url"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse web_fetch arguments: %w", err)
	}

	rawURL := strings.TrimSpace(args.URL)
	if rawURL == "" {
		return "", errors.New("url is required")
	}
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

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "minig/0.1.0 (web_fetch)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain,application/json;q=0.9")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %q: %w", rawURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("web_fetch %q returned HTTP %d %s", rawURL, resp.StatusCode, http.StatusText(resp.StatusCode))
	}

	rawContent, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read response from %q: %w", rawURL, err)
	}

	truncated := false
	if len(rawContent) > maxBytes {
		rawContent = rawContent[:maxBytes]
		truncated = true
	}

	contentType := resp.Header.Get("Content-Type")
	isHTML := strings.Contains(strings.ToLower(contentType), "html") || strings.Contains(string(rawContent[:min(len(rawContent), 256)]), "<html")

	var result string
	if isHTML {
		result = cleanHTML(string(rawContent))
	} else {
		result = string(rawContent)
	}

	result = textutil.CompactWhitespace(result)
	if truncated {
		result += "\n...[truncated]"
	}
	return result, nil
}

func cleanHTML(raw string) string {
	s := scriptTagRegex.ReplaceAllString(raw, "")
	s = styleTagRegex.ReplaceAllString(s, "")
	s = htmlTagRegex.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = blankLinesRegex.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
