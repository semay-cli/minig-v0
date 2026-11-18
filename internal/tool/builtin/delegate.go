package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type editorDelegateKey struct{}
type runContextDataKey struct{}

// EditorDelegate routes file and terminal operations to the hosting IDE over ACP.
type EditorDelegate interface {
	SupportsRead() bool
	SupportsWrite() bool
	SupportsTerminal() bool
	ReadTextFile(ctx context.Context, path string, line, limit int) (string, error)
	WriteTextFile(ctx context.Context, path, content string) (string, error)
	CreateTerminal(ctx context.Context, command, cwd string) (string, error)
}

// WithEditorDelegate attaches an IDE delegate to the tool execution context.
func WithEditorDelegate(ctx context.Context, del EditorDelegate) context.Context {
	if del == nil {
		return ctx
	}
	return context.WithValue(ctx, editorDelegateKey{}, del)
}

// EditorDelegateFrom returns the IDE delegate stored on ctx, if any.
func EditorDelegateFrom(ctx context.Context) EditorDelegate {
	del, _ := ctx.Value(editorDelegateKey{}).(EditorDelegate)
	return del
}

// WithRunContextData stores the per-run JSON payload so sandbox_python can mount it.
func WithRunContextData(ctx context.Context, data json.RawMessage) context.Context {
	if len(data) == 0 {
		return ctx
	}
	cloned := append(json.RawMessage(nil), data...)
	return context.WithValue(ctx, runContextDataKey{}, cloned)
}

// RunContextData returns the per-run JSON payload, or an empty string.
func RunContextData(ctx context.Context) string {
	raw, _ := ctx.Value(runContextDataKey{}).(json.RawMessage)
	return string(raw)
}

func editViaEditor(ctx context.Context, del EditorDelegate, path, oldText, newText string) (string, error) {
	content, err := del.ReadTextFile(ctx, path, 1, 0)
	if err != nil {
		return "", err
	}
	matches := strings.Count(content, oldText)
	if matches == 0 {
		diag := findEditMismatchDiagnostic(content, oldText)
		return "", fmt.Errorf("old_text not found in %q; %s", path, diag)
	}
	if matches > 1 {
		return "", fmt.Errorf("old_text matched %d locations in %q; include more surrounding context lines", matches, path)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if _, err := del.WriteTextFile(ctx, path, updated); err != nil {
		return "", err
	}
	oldLines := strings.Count(oldText, "\n") + 1
	newLines := strings.Count(newText, "\n") + 1
	return fmt.Sprintf("edited %s via IDE (replaced %d line(s) with %d line(s))", path, oldLines, newLines), nil
}
