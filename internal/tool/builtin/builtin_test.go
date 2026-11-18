package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
)

func TestBuiltinReadOnlyAndWriteTools(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "alpha.go"), []byte("package main\n\nfunc Hello() string {\n\treturn \"hi\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := safety.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}

	// Read-only mode must omit write_file
	roReg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterAll(roReg, ws, false); err != nil {
		t.Fatal(err)
	}
	for _, name := range roReg.Names() {
		if name == "write_file" {
			t.Fatal("write_file should not be registered when allowWrite is false")
		}
	}

	// list_dir
	listed, err := roReg.Execute(context.Background(), nil, "list_dir", json.RawMessage(`{"path":"."}`))
	if err != nil || !strings.Contains(listed, "alpha.go") {
		t.Fatalf("list_dir = (%q, %v)", listed, err)
	}

	// read_range
	slice, err := roReg.Execute(context.Background(), nil, "read_range", json.RawMessage(`{"path":"alpha.go","start_line":3,"max_lines":2}`))
	if err != nil || !strings.Contains(slice, "lines 3-4 of 5") || !strings.Contains(slice, "func Hello()") {
		t.Fatalf("read_range = (%q, %v)", slice, err)
	}

	// search_text
	hits, err := roReg.Execute(context.Background(), nil, "search_text", json.RawMessage(`{"query":"hello"}`))
	if err != nil || !strings.Contains(hits, "alpha.go:3:") {
		t.Fatalf("search_text = (%q, %v)", hits, err)
	}

	// Read-write mode allows write_file inside workspace and blocks escapes
	rwReg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterAll(rwReg, ws, true); err != nil {
		t.Fatal(err)
	}

	res, err := rwReg.Execute(context.Background(), nil, "write_file", json.RawMessage(`{"path":"sub/out.txt","content":"ok"}`))
	if err != nil || !strings.Contains(res, "wrote 2 bytes") {
		t.Fatalf("write_file = (%q, %v)", res, err)
	}
	if _, err := rwReg.Execute(context.Background(), nil, "write_file", json.RawMessage(`{"path":"../escape.txt","content":"bad"}`)); err == nil {
		t.Fatal("expected write_file path escape to fail")
	}
}

func TestEditFileSurgicalReplacement(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "calc.go")
	initial := "package calc\n\nfunc Add(a, b int) int {\n\treturn a - b\n}\n"
	if err := os.WriteFile(target, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	ws, err := safety.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterAll(reg, ws, true); err != nil {
		t.Fatal(err)
	}

	// Ambiguous match ("int") must fail
	if _, err := reg.Execute(context.Background(), nil, "edit_file", json.RawMessage(`{
		"path": "calc.go",
		"old_text": "int",
		"new_text": "int64"
	}`)); err == nil {
		t.Fatal("expected ambiguous old_text to be rejected")
	}

	// Unique match succeeds
	out, err := reg.Execute(context.Background(), nil, "edit_file", json.RawMessage(`{
		"path": "calc.go",
		"old_text": "func Add(a, b int) int {\n\treturn a - b\n}",
		"new_text": "func Add(a, b int) int {\n\treturn a + b\n}"
	}`))
	if err != nil {
		t.Fatalf("edit_file failed: %v", err)
	}
	if !strings.Contains(out, "edited calc.go") {
		t.Errorf("unexpected output: %q", out)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "return a + b") {
		t.Errorf("file not updated as expected:\n%s", string(got))
	}

	// Mismatched whitespace produces diagnostic guidance
	_, err = reg.Execute(context.Background(), nil, "edit_file", json.RawMessage(`{
		"path": "calc.go",
		"old_text": "    return a + b",
		"new_text": "    return a * b"
	}`))
	if err == nil || !strings.Contains(err.Error(), "DIAGNOSTIC") {
		t.Fatalf("expected diagnostic mismatch error, got: %v", err)
	}
}

func TestFileInfoAppendDeleteTools(t *testing.T) {
	root := t.TempDir()
	ws, err := safety.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := RegisterAll(reg, ws, true); err != nil {
		t.Fatal(err)
	}

	// append_file creates file if absent
	appOut, err := reg.Execute(context.Background(), nil, "append_file", json.RawMessage(`{
		"path": "notes.txt",
		"content": "line 1\n"
	}`))
	if err != nil || !strings.Contains(appOut, "appended 7 bytes") {
		t.Fatalf("append_file = (%q, %v)", appOut, err)
	}

	// append_file appends to existing file
	_, err = reg.Execute(context.Background(), nil, "append_file", json.RawMessage(`{
		"path": "notes.txt",
		"content": "line 2\n"
	}`))
	if err != nil {
		t.Fatalf("second append_file failed: %v", err)
	}

	// file_info inspects file
	infoOut, err := reg.Execute(context.Background(), nil, "file_info", json.RawMessage(`{
		"path": "notes.txt"
	}`))
	if err != nil || !strings.Contains(infoOut, "lines: 2") || !strings.Contains(infoOut, "type: text") {
		t.Fatalf("file_info = (%q, %v)", infoOut, err)
	}

	// delete_file deletes the file
	delOut, err := reg.Execute(context.Background(), nil, "delete_file", json.RawMessage(`{
		"path": "notes.txt"
	}`))
	if err != nil || !strings.Contains(delOut, "deleted file notes.txt") {
		t.Fatalf("delete_file = (%q, %v)", delOut, err)
	}

	// Verify file is gone
	if _, err := os.Stat(filepath.Join(root, "notes.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected notes.txt to be deleted, stat err: %v", err)
	}
}
