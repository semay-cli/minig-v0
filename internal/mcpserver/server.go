package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"github.com/semay-cli/minig-v0/internal/tool"
)

const protocolVersion = "2024-11-05"

// Serve exposes reg as a stdio MCP server. It returns when stdin closes.
func Serve(ctx context.Context, reg *tool.Registry, in io.Reader, out io.Writer) error {
	if reg == nil {
		return errors.New("tool registry must not be nil")
	}
	var writeMu sync.Mutex
	write := func(v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_, err = out.Write(append(b, '\n'))
		return err
	}

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id,omitempty"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params,omitempty"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			_ = write(resultMsg{JSONRPC: "2.0", ID: json.RawMessage(`null`), Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		if msg.Method == "" {
			continue
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		switch msg.Method {
		case "initialize":
			_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Result: map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "minig", "version": "0.1.0"},
			}})
		case "tools/list":
			_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Result: map[string]any{
				"tools": reg.MCPDescriptors(),
			}})
		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(msg.Params, &params); err != nil || strings.TrimSpace(params.Name) == "" {
				_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: -32602, Message: "name is required"}})
				continue
			}
			outText, err := reg.Execute(ctx, nil, params.Name, params.Arguments)
			isErr := err != nil
			if err != nil {
				if outText == "" {
					outText = err.Error()
				} else {
					outText = err.Error() + "\n" + outText
				}
			}
			_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Result: map[string]any{
				"content": []map[string]string{{"type": "text", "text": outText}},
				"isError": isErr,
			}})
		case "ping":
			_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Result: map[string]any{}})
		default:
			_ = write(resultMsg{JSONRPC: "2.0", ID: msg.ID, Error: &rpcError{Code: -32601, Message: "method not found"}})
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type resultMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}
