package safety

import (
	"os"
	"testing"
)

func TestSandboxLifecycle(t *testing.T) {
	sb, err := NewSandbox()
	if err != nil {
		t.Fatalf("NewSandbox failed: %v", err)
	}
	dir := sb.Dir()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("sandbox dir does not exist on disk: %v", err)
	}

	_, err = sb.WriteFile("test.txt", []byte("hello world"))
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	data, err := sb.ReadFile("test.txt")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(data) != "hello world" {
		t.Errorf("got %q, want 'hello world'", string(data))
	}

	// Reject path traversal
	if _, err := sb.WriteFile("../escape.txt", []byte("bad")); err == nil {
		t.Error("expected error for path traversal, got nil")
	}

	if err := sb.Cleanup(); err != nil {
		t.Fatalf("Cleanup failed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("sandbox dir still exists after cleanup: %v", err)
	}
	// Idempotent
	if err := sb.Cleanup(); err != nil {
		t.Errorf("second Cleanup call failed: %v", err)
	}
}

func TestNewNullWorkspace(t *testing.T) {
	ws, err := NewNullWorkspace()
	if err != nil {
		t.Fatalf("NewNullWorkspace failed: %v", err)
	}
	if ws.Root() == "" {
		t.Error("expected non-empty root for null workspace")
	}
}
