package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/semay-cli/minig-v0/internal/tool"
)

func TestParseServerSpec(t *testing.T) {
	cfg, err := ParseServerSpec(`docs=python3 -m mcp_docs --root "/tmp/my docs"`)
	if err != nil {
		t.Fatalf("ParseServerSpec failed: %v", err)
	}
	if cfg.Name != "docs" || cfg.Command != "python3" {
		t.Fatalf("unexpected cfg: %+v", cfg)
	}
	if len(cfg.Args) != 4 || cfg.Args[3] != "/tmp/my docs" {
		t.Fatalf("unexpected args: %+v", cfg.Args)
	}

	if _, err := ParseServerSpec("missing_equals"); err == nil {
		t.Fatal("expected error for invalid spec")
	}
}

func TestStdioSubprocessMCPServerEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewStdioClient(ctx, ServerConfig{
		Name:    "demo",
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperMCPServerProcess"},
		Env:     []string{"MINIG_TEST_MCP_HELPER=1"},
	})
	if err != nil {
		t.Fatalf("NewStdioClient failed: %v", err)
	}
	defer client.Close()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}

	reg, err := tool.NewRegistry(4096, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	count, err := RegisterTools(ctx, reg, client)
	if err != nil {
		t.Fatalf("RegisterTools failed: %v", err)
	}
	if count != 1 {
		t.Fatalf("registered count = %d, want 1", count)
	}

	// Verify namespaced tool appears in registry and executes via JSON-RPC
	out, err := reg.Execute(ctx, []string{"mcp_demo_echo"}, "mcp_demo_echo", json.RawMessage(`{"text":"hello mcp"}`))
	if err != nil {
		t.Fatalf("Execute MCP tool failed: %v", err)
	}
	if out != "echo: hello mcp" {
		t.Errorf("out = %q, want %q", out, "echo: hello mcp")
	}
}

// TestHelperMCPServerProcess acts as a minimal JSON-RPC 2.0 MCP server over stdin/stdout
// when invoked with MINIG_TEST_MCP_HELPER=1.
func TestHelperMCPServerProcess(t *testing.T) {
	if os.Getenv("MINIG_TEST_MCP_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal([]byte(line), &req); err != nil || req.ID == nil {
			continue
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]string{"name": "helper", "version": "1.0"},
			}
		case "tools/list":
			result = map[string]any{
				"tools": []map[string]any{{
					"name":        "echo",
					"description": "Echo back input text",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"text": map[string]any{"type": "string"},
						},
					},
				}},
			}
		case "tools/call":
			var call struct {
				Name      string `json:"name"`
				Arguments struct {
					Text string `json:"text"`
				} `json:"arguments"`
			}
			_ = json.Unmarshal(req.Params, &call)
			result = map[string]any{
				"content": []map[string]string{{
					"type": "text",
					"text": "echo: " + call.Arguments.Text,
				}},
			}
		default:
			_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32601,"message":"method not found"}}`+"\n", *req.ID)
			continue
		}

		resBytes, _ := json.Marshal(result)
		_, _ = fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", *req.ID, string(resBytes))
	}
	_ = io.Discard
	os.Exit(0)
}
