package safety

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceResolveContainment(t *testing.T) {
	root := t.TempDir()
	subDir := filepath.Join(root, "pkg")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	existingFile := filepath.Join(subDir, "main.go")
	if err := os.WriteFile(existingFile, []byte("package pkg\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatalf("NewWorkspace failed: %v", err)
	}

	// Existing relative path
	got, err := ws.ResolveExisting("pkg/main.go")
	if err != nil || got != filepath.Join(ws.Root(), "pkg", "main.go") {
		t.Fatalf("ResolveExisting(pkg/main.go) = (%q, %v)", got, err)
	}

	// Non-existent new file inside real directory
	newPath, err := ws.Resolve("pkg/new_file.go")
	if err != nil || newPath != filepath.Join(ws.Root(), "pkg", "new_file.go") {
		t.Fatalf("Resolve(pkg/new_file.go) = (%q, %v)", newPath, err)
	}
	if _, err := ws.ResolveExisting("pkg/new_file.go"); err == nil {
		t.Fatal("ResolveExisting should fail on non-existent file")
	}

	// Traversal escape attempts
	for _, bad := range []string{"../outside.txt", "pkg/../../outside.txt", "/etc/passwd"} {
		if _, err := ws.Resolve(bad); err == nil {
			t.Errorf("expected escape path %q to be rejected", bad)
		}
	}
}

func TestWorkspaceRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(outsideFile, filepath.Join(root, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "linked_dir")); err != nil {
		t.Fatal(err)
	}

	ws, err := NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := ws.Resolve("leak.txt"); err == nil {
		t.Fatal("expected symlink file to be rejected")
	}
	if _, err := ws.Resolve("linked_dir/new.txt"); err == nil {
		t.Fatal("expected path traversing symlink directory to be rejected")
	}
}
