package safety

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Workspace confines file operations to a resolved root directory and blocks symlink escapes.
type Workspace struct {
	root string
}

// NewWorkspace validates and resolves the workspace root directory.
func NewWorkspace(root string) (*Workspace, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("workspace root must not be empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace path: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("resolve workspace symlinks: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %q is not a directory", resolved)
	}
	return &Workspace{root: resolved}, nil
}

// Root returns the canonical absolute path of the workspace.
func (w *Workspace) Root() string {
	return w.root
}

// Resolve maps a relative (or workspace-contained absolute) path inside the workspace.
// It rejects directory traversal and any symlink along the path, while allowing
// non-existent leaf paths for file creation inside real workspace directories.
func (w *Workspace) Resolve(target string) (string, error) {
	trimmed := strings.TrimSpace(target)
	if trimmed == "" {
		return "", errors.New("path must not be empty")
	}

	var candidate string
	if filepath.IsAbs(trimmed) {
		candidate = filepath.Clean(trimmed)
	} else {
		candidate = filepath.Clean(filepath.Join(w.root, trimmed))
	}

	rel, err := filepath.Rel(w.root, candidate)
	if err != nil {
		return "", fmt.Errorf("compute relative path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes workspace root", target)
	}
	if rel == "." {
		return w.root, nil
	}

	// Walk every segment from root downward so symlinks cannot bypass containment,
	// even if the final file does not exist yet.
	current := w.root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				// Remaining tail does not exist on disk yet; containment is already verified.
				return candidate, nil
			}
			return "", fmt.Errorf("inspect path %q: %w", rel, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("symlink not allowed at %q", rel)
		}
	}

	return candidate, nil
}

// ResolveExisting resolves a path inside the workspace and verifies it exists on disk.
func (w *Workspace) ResolveExisting(target string) (string, error) {
	resolved, err := w.Resolve(target)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(resolved); err != nil {
		return "", fmt.Errorf("access path %q: %w", target, err)
	}
	return resolved, nil
}
