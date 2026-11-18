package safety

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Sandbox manages an ephemeral, isolated temporary directory that is automatically
// or explicitly cleaned up after execution with zero residue left on disk.
type Sandbox struct {
	dir     string
	mu      sync.Mutex
	cleaned bool
}

// NewSandbox creates an ephemeral directory /tmp/minig-run-<hex> with 0700 permissions.
func NewSandbox() (*Sandbox, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("generate sandbox id: %w", err)
	}
	id := hex.EncodeToString(b)
	dir := filepath.Join(os.TempDir(), "minig-run-"+id)

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create sandbox directory %q: %w", dir, err)
	}

	return &Sandbox{
		dir: dir,
	}, nil
}

// Dir returns the root path of the ephemeral sandbox.
func (s *Sandbox) Dir() string {
	return s.dir
}

// WriteFile writes data to a named file inside the sandbox.
// Path traversal outside the sandbox is strictly rejected.
func (s *Sandbox) WriteFile(name string, data []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cleaned {
		return "", errors.New("sandbox has already been cleaned up")
	}

	cleaned := filepath.Clean(name)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("invalid sandbox file path %q", name)
	}

	target := filepath.Join(s.dir, cleaned)
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return "", fmt.Errorf("create sandbox parent dirs: %w", err)
	}

	if err := os.WriteFile(target, data, 0o600); err != nil {
		return "", fmt.Errorf("write sandbox file %q: %w", name, err)
	}

	return target, nil
}

// ReadFile reads the contents of a named file inside the sandbox.
func (s *Sandbox) ReadFile(name string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cleaned {
		return nil, errors.New("sandbox has already been cleaned up")
	}

	cleaned := filepath.Clean(name)
	if strings.HasPrefix(cleaned, "..") || filepath.IsAbs(cleaned) {
		return nil, fmt.Errorf("invalid sandbox file path %q", name)
	}

	target := filepath.Join(s.dir, cleaned)
	return os.ReadFile(target)
}

// Cleanup removes the sandbox directory and all its contents. Safe to call multiple times.
func (s *Sandbox) Cleanup() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cleaned {
		return nil
	}
	s.cleaned = true

	if s.dir != "" && s.dir != "/" && s.dir != "." {
		return os.RemoveAll(s.dir)
	}
	return nil
}

// NewNullWorkspace returns a Workspace pointing to a designated null workspace directory.
// This is used when workspace_mode="none" so no project directory is scanned or modified.
func NewNullWorkspace() (*Workspace, error) {
	nullDir := filepath.Join(os.TempDir(), "minig-null-workspace")
	if err := os.MkdirAll(nullDir, 0o700); err != nil {
		return nil, fmt.Errorf("create null workspace dir: %w", err)
	}
	return NewWorkspace(nullDir)
}
