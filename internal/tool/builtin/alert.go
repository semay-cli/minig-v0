package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type alertDispatchTool struct{}

func (t *alertDispatchTool) Name() string   { return "alert_dispatch" }
func (t *alertDispatchTool) ReadOnly() bool { return false }
func (t *alertDispatchTool) Description() string {
	return "Send a formatted incident alert or webhook notification to Slack, PagerDuty, Opsgenie, or a custom Python endpoint."
}

func (t *alertDispatchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["webhook_url", "summary"],
		"properties": {
			"webhook_url": {"type": "string", "description": "Target HTTP/HTTPS webhook URL"},
			"target_type": {"type": "string", "description": "Platform type: slack, pagerduty, opsgenie, custom (default custom)"},
			"summary": {"type": "string", "description": "Short alert title / headline"},
			"description": {"type": "string", "description": "Detailed incident context, root cause, or diagnostic"},
			"severity": {"type": "string", "description": "Incident severity: critical, high, medium, low (default medium)"},
			"service_name": {"type": "string", "description": "Affected microservice or component name"},
			"dedup_key": {"type": "string", "description": "Deduplication key for incident de-bouncing"}
		}
	}`)
}

func (t *alertDispatchTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		WebhookURL  string `json:"webhook_url"`
		TargetType  string `json:"target_type"`
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Severity    string `json:"severity"`
		ServiceName string `json:"service_name"`
		DedupKey    string `json:"dedup_key"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse alert_dispatch arguments: %w", err)
	}

	targetURL := strings.TrimSpace(args.WebhookURL)
	if targetURL == "" {
		return "", errors.New("webhook_url is required")
	}
	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		return "", errors.New("webhook_url must be an http:// or https:// URL")
	}

	summary := strings.TrimSpace(args.Summary)
	if summary == "" {
		return "", errors.New("summary is required")
	}

	severity := strings.ToLower(strings.TrimSpace(args.Severity))
	if severity == "" {
		severity = "medium"
	}

	targetType := strings.ToLower(strings.TrimSpace(args.TargetType))
	if targetType == "" {
		targetType = "custom"
	}

	var payload any
	nowStr := time.Now().UTC().Format(time.RFC3339)

	switch targetType {
	case "slack":
		header := fmt.Sprintf("[%s] %s", strings.ToUpper(severity), summary)
		text := header
		if args.ServiceName != "" {
			text += fmt.Sprintf("\n*Service:* %s", args.ServiceName)
		}
		if args.Description != "" {
			text += fmt.Sprintf("\n*Details:* %s", args.Description)
		}
		payload = map[string]any{
			"text": text,
		}

	case "pagerduty":
		pdSeverity := "warning"
		switch severity {
		case "critical":
			pdSeverity = "critical"
		case "high":
			pdSeverity = "error"
		case "low":
			pdSeverity = "info"
		}
		payload = map[string]any{
			"event_action": "trigger",
			"dedup_key":    args.DedupKey,
			"payload": map[string]any{
				"summary":   summary,
				"severity":  pdSeverity,
				"source":    args.ServiceName,
				"timestamp": nowStr,
				"custom_details": map[string]any{
					"description": args.Description,
					"source":      "minig",
				},
			},
		}

	case "opsgenie":
		priority := "P3"
		switch severity {
		case "critical":
			priority = "P1"
		case "high":
			priority = "P2"
		case "low":
			priority = "P4"
		}
		payload = map[string]any{
			"message":     summary,
			"description": args.Description,
			"priority":    priority,
			"entity":      args.ServiceName,
			"alias":       args.DedupKey,
		}

	default: // custom
		payload = map[string]any{
			"summary":      summary,
			"description":  args.Description,
			"severity":     severity,
			"service_name": args.ServiceName,
			"dedup_key":    args.DedupKey,
			"timestamp":    nowStr,
			"source":       "minig",
		}
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode alert payload: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, targetURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return "", fmt.Errorf("create alert request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "minig/0.1.0 (alert_dispatch)")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("dispatch alert to %q: %w", targetURL, err)
	}
	defer resp.Body.Close()

	respSnippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Sprintf("Alert dispatch to %s failed (HTTP %d): %s", targetURL, resp.StatusCode, string(respSnippet)),
			fmt.Errorf("webhook responded with HTTP %d", resp.StatusCode)
	}

	return fmt.Sprintf("Alert successfully dispatched to %s (HTTP %d %s)", targetURL, resp.StatusCode, http.StatusText(resp.StatusCode)), nil
}
