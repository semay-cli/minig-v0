package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

var symbolRE = regexp.MustCompile(`(?m)^(?:func|type|class|def|fn|struct|interface)\s+([A-Za-z_][\w.]*)`)

type workspaceCodeTool struct {
	ws         *safety.Workspace
	allowWrite bool
}

func (t *workspaceCodeTool) Name() string { return "workspace_code" }
func (t *workspaceCodeTool) ReadOnly() bool {
	return !t.allowWrite
}
func (t *workspaceCodeTool) Description() string {
	return "Search the local workspace with ripgrep or a built-in walker, find symbols, inspect git diff/log, read a file, or apply a unique text patch. Active only in workspace_mode=local."
}

func (t *workspaceCodeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["action"],
		"properties": {
			"action": {"type": "string", "description": "search, symbols, git_diff, git_log, read, or patch"},
			"path": {"type": "string", "description": "Workspace-relative path (default '.')"},
			"query": {"type": "string", "description": "Search text or symbol name"},
			"old_text": {"type": "string", "description": "Exact text to replace when action is patch"},
			"new_text": {"type": "string", "description": "Replacement text when action is patch"},
			"max_hits": {"type": "integer", "description": "Maximum matches (default 25, max 50)"}
		}
	}`)
}

func (t *workspaceCodeTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Action  string `json:"action"`
		Path    string `json:"path"`
		Query   string `json:"query"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
		MaxHits int    `json:"max_hits"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse workspace_code arguments: %w", err)
	}
	action := strings.ToLower(strings.TrimSpace(args.Action))
	if args.Path == "" {
		args.Path = "."
	}
	if args.MaxHits <= 0 {
		args.MaxHits = 25
	}
	if args.MaxHits > 50 {
		args.MaxHits = 50
	}

	switch action {
	case "search":
		if strings.TrimSpace(args.Query) == "" {
			return "", errors.New("query is required for search")
		}
		return t.search(ctx, args.Path, args.Query, args.MaxHits)
	case "symbols":
		return t.symbols(ctx, args.Path, args.Query, args.MaxHits)
	case "git_diff":
		return t.git(ctx, "diff", "--stat", "HEAD")
	case "git_log":
		return t.git(ctx, "log", "-n", "20", "--oneline")
	case "read":
		return t.readFile(args.Path)
	case "patch":
		if !t.allowWrite {
			return "", errors.New("patch is disabled unless workspace writes are enabled")
		}
		return t.patch(args.Path, args.OldText, args.NewText)
	default:
		return "", fmt.Errorf("unsupported workspace_code action %q", args.Action)
	}
}

func (t *workspaceCodeTool) search(ctx context.Context, rel, query string, maxHits int) (string, error) {
	root, err := t.ws.ResolveExisting(rel)
	if err != nil {
		return "", err
	}
	if path, lookErr := exec.LookPath("rg"); lookErr == nil {
		cmdCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		cmd := exec.CommandContext(cmdCtx, path, "-n", "-i", "--max-count", fmt.Sprint(maxHits), "--glob", "!.git/**", "--glob", "!node_modules/**", query, ".")
		cmd.Dir = root
		out, runErr := cmd.CombinedOutput()
		text := strings.TrimSpace(string(out))
		if runErr != nil && text == "" {
			if cmdCtx.Err() != nil {
				return "", fmt.Errorf("ripgrep timed out: %w", cmdCtx.Err())
			}
		}
		if text != "" {
			return textutil.TruncateBytes(text, 8*1024), nil
		}
	}
	return t.walkSearch(root, rel, query, maxHits)
}

func (t *workspaceCodeTool) walkSearch(root, rel, query string, maxHits int) (string, error) {
	needle := strings.ToLower(query)
	var buf strings.Builder
	hits := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || hits >= maxHits {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > maxInspectFileBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(raw, 0) >= 0 {
			return nil
		}
		relPath, err := filepath.Rel(t.ws.Root(), path)
		if err != nil {
			relPath = path
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(strings.ToLower(line), needle) {
				fmt.Fprintf(&buf, "%s:%d:%s\n", relPath, i+1, textutil.TruncateBytes(strings.TrimSpace(line), 160))
				hits++
				if hits >= maxHits {
					return nil
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if buf.Len() == 0 {
		return fmt.Sprintf("no matches for %q under %s", query, rel), nil
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func (t *workspaceCodeTool) symbols(ctx context.Context, rel, query string, maxHits int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := t.ws.ResolveExisting(rel)
	if err != nil {
		return "", err
	}
	q := strings.ToLower(strings.TrimSpace(query))
	var buf strings.Builder
	hits := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || hits >= maxHits {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(raw, 0) >= 0 || len(raw) > maxInspectFileBytes {
			return nil
		}
		relPath, _ := filepath.Rel(t.ws.Root(), path)
		lines := strings.Split(string(raw), "\n")
		for i, line := range lines {
			m := symbolRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if q != "" && !strings.Contains(strings.ToLower(m[1]), q) && !strings.Contains(strings.ToLower(line), q) {
				continue
			}
			fmt.Fprintf(&buf, "%s:%d: %s\n", relPath, i+1, strings.TrimSpace(line))
			hits++
			if hits >= maxHits {
				return nil
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if buf.Len() == 0 {
		return "no symbols found", nil
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

func (t *workspaceCodeTool) git(ctx context.Context, args ...string) (string, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", append([]string{"-C", t.ws.Root()}, args...)...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			text = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), text)
	}
	if text == "" {
		return "(no git output)", nil
	}
	return textutil.TruncateBytes(text, 8*1024), nil
}

func (t *workspaceCodeTool) readFile(rel string) (string, error) {
	resolved, err := t.ws.ResolveExisting(rel)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory", rel)
	}
	if info.Size() > maxInspectFileBytes {
		return "", fmt.Errorf("file %q exceeds read limit", rel)
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	if bytes.IndexByte(raw, 0) >= 0 {
		return "", fmt.Errorf("file %q appears to be binary", rel)
	}
	sliced, total, truncated := textutil.SliceLines(string(raw), 1, defaultReadLines)
	header := fmt.Sprintf("%s (%d lines", rel, total)
	if truncated {
		header += ", truncated"
	}
	return header + "):\n" + sliced, nil
}

func (t *workspaceCodeTool) patch(rel, oldText, newText string) (string, error) {
	if oldText == "" {
		return "", errors.New("old_text must not be empty")
	}
	if oldText == newText {
		return "", errors.New("old_text and new_text are identical")
	}
	resolved, err := t.ws.ResolveExisting(rel)
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	content := string(raw)
	matches := strings.Count(content, oldText)
	if matches == 0 {
		return "", fmt.Errorf("old_text not found in %q; %s", rel, findEditMismatchDiagnostic(content, oldText))
	}
	if matches > 1 {
		return "", fmt.Errorf("old_text matched %d locations in %q", matches, rel)
	}
	updated := strings.Replace(content, oldText, newText, 1)
	if err := os.WriteFile(resolved, []byte(updated), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("patched %s", rel), nil
}
