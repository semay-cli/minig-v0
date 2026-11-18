package textutil

import (
	"strings"
	"unicode/utf8"
)

const truncationMarker = "\n...[truncated]"

// StripThink extracts <think>...</think> reasoning blocks so they do not bloat
// downstream micro-task prompts. Unclosed <think> tags are stripped to EOF.
func StripThink(raw string) (clean string, thought string) {
	var cleanBuf strings.Builder
	var thoughtBuf strings.Builder

	rest := raw
	for len(rest) > 0 {
		lower := strings.ToLower(rest)
		openIdx := strings.Index(lower, "<think>")
		if openIdx == -1 {
			cleanBuf.WriteString(rest)
			break
		}

		cleanBuf.WriteString(rest[:openIdx])
		afterOpen := rest[openIdx+len("<think>"):]
		lowerAfter := lower[openIdx+len("<think>"):]

		closeIdx := strings.Index(lowerAfter, "</think>")
		if closeIdx == -1 {
			if thoughtBuf.Len() > 0 {
				thoughtBuf.WriteByte('\n')
			}
			thoughtBuf.WriteString(strings.TrimSpace(afterOpen))
			break
		}

		if thoughtBuf.Len() > 0 {
			thoughtBuf.WriteByte('\n')
		}
		thoughtBuf.WriteString(strings.TrimSpace(afterOpen[:closeIdx]))
		rest = afterOpen[closeIdx+len("</think>"):]
	}

	return strings.TrimSpace(cleanBuf.String()), strings.TrimSpace(thoughtBuf.String())
}

// TruncateBytes caps s to maxBytes without splitting multi-byte UTF-8 runes.
func TruncateBytes(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes <= len(truncationMarker) {
		return sliceValidUTF8(s, maxBytes)
	}
	cutoff := maxBytes - len(truncationMarker)
	return sliceValidUTF8(s, cutoff) + truncationMarker
}

func sliceValidUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}

// SliceLines returns a 1-based line window [startLine, startLine+maxLines) from s.
func SliceLines(s string, startLine, maxLines int) (sliced string, totalLines int, truncated bool) {
	if startLine < 1 {
		startLine = 1
	}
	if maxLines < 1 {
		maxLines = 50
	}
	if s == "" {
		return "", 0, false
	}

	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(normalized, "\n")
	// Ignore trailing empty element caused by a final newline.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	totalLines = len(lines)
	if startLine > totalLines {
		return "", totalLines, false
	}

	end := startLine - 1 + maxLines
	if end >= totalLines {
		end = totalLines
		truncated = false
	} else {
		truncated = true
	}

	return strings.Join(lines[startLine-1:end], "\n"), totalLines, truncated
}

// CompactWhitespace trims trailing line spaces and collapses 3+ consecutive newlines
// into a single blank line while preserving leading indentation.
func CompactWhitespace(s string) string {
	normalized := strings.ReplaceAll(s, "\r\n", "\n")
	rawLines := strings.Split(normalized, "\n")
	out := make([]string, 0, len(rawLines))
	blankRun := 0

	for _, line := range rawLines {
		trimmedRight := strings.TrimRight(line, " \t")
		if trimmedRight == "" {
			blankRun++
			if blankRun > 1 {
				continue
			}
		} else {
			blankRun = 0
		}
		out = append(out, trimmedRight)
	}
	return strings.Trim(strings.Join(out, "\n"), "\n")
}

// EstimateTokens provides a conservative local token count fallback (~4 bytes/token).
func EstimateTokens(s string) int {
	if len(s) == 0 {
		return 0
	}
	return (len(s) + 3) / 4
}
