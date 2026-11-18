package tool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type stubTool struct {
	name     string
	readOnly bool
	fn       func(ctx context.Context, args json.RawMessage) (string, error)
}

func (s stubTool) Name() string                { return s.name }
func (s stubTool) Description() string         { return s.name + " description" }
func (s stubTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (s stubTool) ReadOnly() bool              { return s.readOnly }
func (s stubTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return s.fn(ctx, args)
}

func TestRegistryAllowlistAndTruncation(t *testing.T) {
	reg, err := NewRegistry(256, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	_ = reg.Register(stubTool{
		name:     "read_range",
		readOnly: true,
		fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return strings.Repeat("x", 600), nil
		},
	})
	_ = reg.Register(stubTool{
		name:     "write_file",
		readOnly: false,
		fn: func(_ context.Context, _ json.RawMessage) (string, error) {
			return "written", nil
		},
	})

	// Subset schema exposure
	specs := reg.Specs([]string{"read_range", "nonexistent", "read_range"})
	if len(specs) != 1 || specs[0].Name != "read_range" {
		t.Fatalf("unexpected specs: %+v", specs)
	}

	// Disallowed tool execution blocked
	if _, err := reg.Execute(context.Background(), []string{"read_range"}, "write_file", nil); err == nil {
		t.Fatal("expected disallowed tool call to fail")
	}

	// Empty allowlist blocks all tools
	if _, err := reg.Execute(context.Background(), []string{}, "read_range", nil); err == nil {
		t.Fatal("expected empty allowlist to block tool execution")
	}

	// Output capped to 256 bytes
	out, err := reg.Execute(context.Background(), []string{"read_range"}, "read_range", nil)
	if err != nil {
		t.Fatalf("Execute failed: %v", err)
	}
	if len(out) > 256 {
		t.Fatalf("output len = %d, want <= 256", len(out))
	}
}
