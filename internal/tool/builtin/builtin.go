package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/textutil"
	"github.com/semay-cli/minig-v0/internal/tool"
)

const (
	maxInspectFileBytes = 512 * 1024
	defaultReadLines    = 80
	maxReadLines        = 200
	defaultListEntries  = 60
	maxListEntries      = 200
	defaultSearchHits   = 25
	maxSearchHits       = 50
)

// RegisterAll registers built-in workspace tools into reg.
// write_file is only registered when allowWrite is true.
func RegisterAll(reg *tool.Registry, ws *safety.Workspace, allowWrite bool) error {
	if reg == nil {
		return errors.New("tool registry must not be nil")
	}
	if ws == nil {
		return errors.New("workspace must not be nil")
	}

	tools := []tool.Tool{
		&listDirTool{ws: ws},
		&readRangeTool{ws: ws},
		&searchTextTool{ws: ws},
	}
	if allowWrite {
		tools = append(tools,
			&writeFileTool{ws: ws},
			&editFileTool{ws: ws},
		)
	}
	for _, t := range tools {
		if err := reg.Register(t); err != nil {
			return err
		}
	}
	return nil
}

// --- list_dir ---

type listDirTool struct {
	ws *safety.Workspace
}

func (t *listDirTool) Name() string   { return "list_dir" }
func (t *listDirTool) ReadOnly() bool { return true }
func (t *listDirTool) Description() string {
	return "List files and subdirectories in a workspace directory."
}

func (t *listDirTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Workspace-relative directory path (default '.')"},
			"depth": {"type": "integer", "description": "Directory traversal depth (default 3, max 4)"},
			"limit": {"type": "integer", "description": "Maximum entries to return (default 80, max 200)"}
		}
	}`)
}

func (t *listDirTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse list_dir arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	if args.Limit <= 0 {
		args.Limit = defaultListEntries
	}
	if args.Limit > maxListEntries {
		args.Limit = maxListEntries
	}

	dirPath, err := t.ws.ResolveExisting(args.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", fmt.Errorf("read directory %q: %w", args.Path, err)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var buf strings.Builder
	shown := 0
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if shown >= args.Limit {
			fmt.Fprintf(&buf, "...(%d more entries)\n", len(entries)-shown)
			break
		}
		if entry.IsDir() {
			fmt.Fprintf(&buf, "%s/\n", name)
		} else {
			info, err := entry.Info()
			if err != nil {
				continue
			}
			fmt.Fprintf(&buf, "%s (%dB)\n", name, info.Size())
		}
		shown++
	}
	if shown == 0 {
		return "(empty directory)", nil
	}
	return strings.TrimRight(buf.String(), "\n"), nil
}

// --- read_range ---

type readRangeTool struct {
	ws *safety.Workspace
}

func (t *readRangeTool) Name() string   { return "read_range" }
func (t *readRangeTool) ReadOnly() bool { return true }
func (t *readRangeTool) Description() string {
	return "Read a bounded 1-based line slice from a text file in the workspace."
}
func (t *readRangeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["path"],
		"properties": {
			"path": {"type": "string", "description": "Workspace-relative file path"},
			"start_line": {"type": "integer", "description": "1-based starting line number (default 1)"},
			"max_lines": {"type": "integer", "description": "Maximum lines to read (default 80, max 200)"}
		}
	}`)
}

func (t *readRangeTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Path      string `json:"path"`
		StartLine int    `json:"start_line"`
		MaxLines  int    `json:"max_lines"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse read_range arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", errors.New("path is required")
	}
	if args.StartLine < 1 {
		args.StartLine = 1
	}
	if args.MaxLines <= 0 {
		args.MaxLines = defaultReadLines
	}
	if args.MaxLines > maxReadLines {
		args.MaxLines = maxReadLines
	}

	resolved, err := t.ws.ResolveExisting(args.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%q is a directory; use list_dir instead", args.Path)
	}
	if info.Size() > maxInspectFileBytes {
		return "", fmt.Errorf("file %q is %d bytes (limit %d); use search_text to locate relevant lines",
			args.Path, info.Size(), maxInspectFileBytes)
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", args.Path, err)
	}
	if bytes.IndexByte(raw, 0) != -1 {
		return "", fmt.Errorf("file %q appears to be binary", args.Path)
	}

	sliced, totalLines, truncated := textutil.SliceLines(string(raw), args.StartLine, args.MaxLines)
	if totalLines == 0 {
		return fmt.Sprintf("%s (empty file)", args.Path), nil
	}
	if sliced == "" {
		return fmt.Sprintf("%s: start_line %d exceeds total lines (%d)", args.Path, args.StartLine, totalLines), nil
	}

	lineCount := strings.Count(sliced, "\n") + 1
	endLine := args.StartLine + lineCount - 1
	header := fmt.Sprintf("%s (lines %d-%d of %d", args.Path, args.StartLine, endLine, totalLines)
	if truncated {
		header += ", more available"
	}
	header += "):\n"
	return header + sliced, nil
}

// --- search_text ---

type searchTextTool struct {
	ws *safety.Workspace
}

func (t *searchTextTool) Name() string   { return "search_text" }
func (t *searchTextTool) ReadOnly() bool { return true }
func (t *searchTextTool) Description() string {
	return "Search for a case-insensitive text substring across workspace files and return matching file:line snippets."
}
func (t *searchTextTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["query"],
		"properties": {
			"query": {"type": "string", "description": "Substring to search for"},
			"path": {"type": "string", "description": "Optional workspace-relative file or directory scope (default '.')"},
			"max_hits": {"type": "integer", "description": "Maximum matching lines to return (default 25, max 50)"}
		}
	}`)
}

func (t *searchTextTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Query   string `json:"query"`
		Path    string `json:"path"`
		MaxHits int    `json:"max_hits"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse search_text arguments: %w", err)
	}
	query := strings.ToLower(strings.TrimSpace(args.Query))
	if query == "" {
		return "", errors.New("query must not be empty")
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	if args.MaxHits <= 0 {
		args.MaxHits = defaultSearchHits
	}
	if args.MaxHits > maxSearchHits {
		args.MaxHits = maxSearchHits
	}

	startPath, err := t.ws.ResolveExisting(args.Path)
	if err != nil {
		return "", err
	}

	var hits []string
	stopWalk := errors.New("max hits reached")

	walkErr := filepath.WalkDir(startPath, func(path string, d fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}

		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > maxInspectFileBytes {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(raw, 0) != -1 {
			return nil
		}

		rel, err := filepath.Rel(t.ws.Root(), path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)

		normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
		lines := strings.Split(normalized, "\n")
		for idx, line := range lines {
			if strings.Contains(strings.ToLower(line), query) {
				snippet := strings.TrimSpace(line)
				if len(snippet) > 160 {
					snippet = snippet[:160] + "..."
				}
				hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, idx+1, snippet))
				if len(hits) >= args.MaxHits {
					return stopWalk
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, stopWalk) {
		return "", walkErr
	}

	if len(hits) == 0 {
		return fmt.Sprintf("no matches found for %q", args.Query), nil
	}
	return strings.Join(hits, "\n"), nil
}

// --- write_file ---

type writeFileTool struct {
	ws *safety.Workspace
}

func (t *writeFileTool) Name() string   { return "write_file" }
func (t *writeFileTool) ReadOnly() bool { return false }
func (t *writeFileTool) Description() string {
	return "Write UTF-8 text content to a file inside the workspace, creating parent directories if needed."
}
func (t *writeFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["path", "content"],
		"properties": {
			"path": {"type": "string", "description": "Workspace-relative target file path"},
			"content": {"type": "string", "description": "Complete text content to write"}
		}
	}`)
}

func (t *writeFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse write_file arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", errors.New("path is required")
	}

	resolved, err := t.ws.Resolve(args.Path)
	if err != nil {
		return "", err
	}
	if resolved == t.ws.Root() {
		return "", errors.New("cannot overwrite workspace root directory")
	}
	if info, err := os.Stat(resolved); err == nil && info.IsDir() {
		return "", fmt.Errorf("cannot overwrite directory %q", args.Path)
	}

	if err := os.MkdirAll(filepath.Dir(resolved), 0o755); err != nil {
		return "", fmt.Errorf("create parent directories: %w", err)
	}
	if err := os.WriteFile(resolved, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write %q: %w", args.Path, err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), args.Path), nil
}

// --- edit_file ---

type editFileTool struct {
	ws *safety.Workspace
}

func (t *editFileTool) Name() string   { return "edit_file" }
func (t *editFileTool) ReadOnly() bool { return false }
func (t *editFileTool) Description() string {
	return "Replace a unique exact text block in an existing workspace file without rewriting the whole file."
}
func (t *editFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["path", "old_text", "new_text"],
		"properties": {
			"path": {"type": "string", "description": "Workspace-relative target file path"},
			"old_text": {"type": "string", "description": "Exact existing text snippet to replace (must match once)"},
			"new_text": {"type": "string", "description": "Replacement text"}
		}
	}`)
}

func (t *editFileTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var args struct {
		Path    string `json:"path"`
		OldText string `json:"old_text"`
		NewText string `json:"new_text"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse edit_file arguments: %w", err)
	}
	if strings.TrimSpace(args.Path) == "" {
		return "", errors.New("path is required")
	}
	if args.OldText == "" {
		return "", errors.New("old_text must not be empty")
	}
	if args.OldText == args.NewText {
		return "", errors.New("old_text and new_text are identical")
	}

	resolved, err := t.ws.ResolveExisting(args.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %q: %w", args.Path, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("cannot edit directory %q", args.Path)
	}
	if info.Size() > maxInspectFileBytes {
		return "", fmt.Errorf("file %q exceeds %d byte edit limit", args.Path, maxInspectFileBytes)
	}

	raw, err := os.ReadFile(resolved)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", args.Path, err)
	}
	if bytes.IndexByte(raw, 0) != -1 {
		return "", fmt.Errorf("file %q appears to be binary", args.Path)
	}

	content := string(raw)
	matches := strings.Count(content, args.OldText)
	if matches == 0 {
		return "", fmt.Errorf("old_text not found in %q; inspect exact lines with read_range first", args.Path)
	}
	if matches > 1 {
		return "", fmt.Errorf("old_text matched %d locations in %q; include more surrounding context lines", matches, args.Path)
	}

	updated := strings.Replace(content, args.OldText, args.NewText, 1)
	if err := os.WriteFile(resolved, []byte(updated), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("write %q: %w", args.Path, err)
	}

	oldLines := strings.Count(args.OldText, "\n") + 1
	newLines := strings.Count(args.NewText, "\n") + 1
	return fmt.Sprintf("edited %s (replaced %d line(s) with %d line(s))", args.Path, oldLines, newLines), nil
}
