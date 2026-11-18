package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

var (
	uuidRegex      = regexp.MustCompile(`(?i)[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
	ipv4Regex      = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	isoTimeRegex   = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?\b`)
	hexRegex       = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b|\b[0-9a-fA-F]{16,}\b`)
	numRegex       = regexp.MustCompile(`\b\d+\b`)
	stackLineRegex = regexp.MustCompile(`^\s*(at\s+|File\s+"|Traceback|\.\.\.)`)
)

type logTemplateMinerTool struct{}

func (t *logTemplateMinerTool) Name() string   { return "log_template_miner" }
func (t *logTemplateMinerTool) ReadOnly() bool { return true }
func (t *logTemplateMinerTool) Description() string {
	return "Normalize and compress raw log lines into frequency-counted templates. Replaces UUIDs, IPs, numbers, hex values, and timestamps with placeholders, extracting representative stack traces and grouping patterns."
}

func (t *logTemplateMinerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["logs"],
		"properties": {
			"logs": {"type": "string", "description": "Raw log text (newline-separated lines)"},
			"max_templates": {"type": "integer", "description": "Maximum unique templates to return (default 20, max 50)"}
		}
	}`)
}

type templateCount struct {
	template string
	count    int
	sample   string
}

func (t *logTemplateMinerTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		Logs         string `json:"logs"`
		MaxTemplates int    `json:"max_templates"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse log_template_miner arguments: %w", err)
	}

	trimmedLogs := strings.TrimSpace(args.Logs)
	if trimmedLogs == "" {
		return "", errors.New("logs must not be empty")
	}

	maxTemplates := args.MaxTemplates
	if maxTemplates <= 0 {
		maxTemplates = 20
	}
	if maxTemplates > 50 {
		maxTemplates = 50
	}

	rawLines := strings.Split(strings.ReplaceAll(trimmedLogs, "\r\n", "\n"), "\n")

	counts := make(map[string]*templateCount)
	var stackTraces []string
	seenStack := make(map[string]struct{})
	totalLines := 0

	var currentStack strings.Builder

	for _, line := range rawLines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		totalLines++

		// Check for stack trace line
		if stackLineRegex.MatchString(trimmed) || strings.HasPrefix(trimmed, "Exception in thread") || strings.Contains(trimmed, "Exception:") || strings.Contains(trimmed, "Error:") {
			currentStack.WriteString(trimmed + "\n")
			continue
		} else if currentStack.Len() > 0 {
			st := strings.TrimSpace(currentStack.String())
			currentStack.Reset()
			if _, exists := seenStack[st]; !exists && len(stackTraces) < 5 {
				seenStack[st] = struct{}{}
				stackTraces = append(stackTraces, st)
			}
		}

		// Normalize line into template
		tmpl := normalizeLogLine(trimmed)
		if tc, exists := counts[tmpl]; exists {
			tc.count++
		} else {
			counts[tmpl] = &templateCount{
				template: tmpl,
				count:    1,
				sample:   trimmed,
			}
		}
	}

	// Flush remaining stack trace
	if currentStack.Len() > 0 {
		st := strings.TrimSpace(currentStack.String())
		if _, exists := seenStack[st]; !exists && len(stackTraces) < 5 {
			seenStack[st] = struct{}{}
			stackTraces = append(stackTraces, st)
		}
	}

	// Sort templates by frequency descending
	list := make([]*templateCount, 0, len(counts))
	for _, tc := range counts {
		list = append(list, tc)
	}
	sort.Slice(list, func(i, j int) bool {
		return list[i].count > list[j].count
	})

	var out strings.Builder
	fmt.Fprintf(&out, "Log Template Summary: %d raw lines compressed into %d distinct template(s)\n\n", totalLines, len(list))

	limit := maxTemplates
	if limit > len(list) {
		limit = len(list)
	}

	for i := 0; i < limit; i++ {
		tc := list[i]
		pct := float64(tc.count) / float64(totalLines) * 100.0
		fmt.Fprintf(&out, "[%4dx, %5.1f%%] %s\n", tc.count, pct, tc.template)
		if tc.count == 1 && tc.sample != tc.template {
			fmt.Fprintf(&out, "              Sample: %s\n", tc.sample)
		}
	}

	if len(stackTraces) > 0 {
		out.WriteString("\n--- Extracted Stack Traces ---\n")
		for idx, st := range stackTraces {
			fmt.Fprintf(&out, "[Trace %d]\n%s\n\n", idx+1, st)
		}
	}

	return strings.TrimRight(out.String(), "\n"), nil
}

func normalizeLogLine(line string) string {
	res := line
	res = isoTimeRegex.ReplaceAllString(res, "<TIMESTAMP>")
	res = uuidRegex.ReplaceAllString(res, "<UUID>")
	res = ipv4Regex.ReplaceAllString(res, "<IP>")
	res = hexRegex.ReplaceAllString(res, "<HEX>")
	res = numRegex.ReplaceAllString(res, "<NUM>")
	return res
}
