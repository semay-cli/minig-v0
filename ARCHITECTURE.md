# Technical Architecture & Stages Implementation Guide

> Current package map, run loop, and contribution rules: [`docs/technical.md`](./docs/technical.md). This file is the earlier stage-by-stage narrative. Where the two differ, follow `docs/technical.md` and the code.

> This document details the internal technical structure of **minig**, the step-by-step implementation of its execution stages, and exhaustive developer guides for extending the engine with custom agentic behaviors, built-in Go tools, MCP (Model Context Protocol) servers, and LLM providers.

---

## 1. Technical Structure & Component Topology

```
minig-v0/
├── cmd/minig/               # Binary entrypoint: CLI driver & HTTP server lifecycle
│   ├── main.go              # Flag parsing, signal traps, daemon startup
│   └── main_test.go         # End-to-end integration tests & HTTP tests
├── internal/
│   ├── agent/               # Agent orchestration & DAG execution engine
│   │   ├── engine.go        # Main execution loop, synthesis, error diagnostics
│   │   ├── planner.go       # Schema-less decomposition & Kahn's DAG topo-sort
│   │   ├── types.go         # MicroTask, TaskResult, RunResult, Events
│   │   └── engine_test.go   # Context isolation & DAG unit tests
│   ├── budget/              # Two-tier resource ledger
│   │   ├── budget.go        # Ledger, TaskBudget, token & step counters
│   │   └── budget_test.go   # Thread-safety and budget boundary tests
│   ├── safety/              # Workspace sandbox & boundary containment
│   │   ├── safety.go        # Canonical path resolution & symlink rejection
│   │   └── safety_test.go   # Directory traversal and symlink attack tests
│   ├── server/              # HTTP REST & SSE streaming server
│   │   ├── server.go        # /v1/health, /v1/tools, /v1/run, /v1/run/stream
│   │   └── server_test.go   # REST and SSE streaming unit tests
│   ├── tool/                # Tool abstraction layer & registry
│   │   ├── tool.go          # Tool & Spec interfaces, execution registry
│   │   ├── builtin/         # Built-in workspace tools
│   │   │   ├── builtin.go   # list_dir, read_range, search_text, file_info, write_file, edit_file, append_file, delete_file
│   │   │   └── builtin_test.go
│   │   └── mcp/             # MCP stdio client & subprocess manager
│   │       ├── mcp.go       # JSON-RPC 2.0 stdio client, tool registration
│   │       └── mcp_test.go  # Mock stdio MCP subprocess tests
│   ├── llm/                 # LLM provider contracts & implementations
│   │   ├── llm.go           # Message, ToolCall, ChatRequest, Provider interface
│   │   ├── mock.go          # Test double with mock responses & request recording
│   │   └── llamacpp/        # llama.cpp HTTP client (/v1/chat/completions)
│   ├── textutil/            # Lean text processing & prompt compaction
│   │   ├── textutil.go      # Line slicing, UTF-8 truncation, whitespace compaction
│   │   └── textutil_test.go
│   ├── config/              # Configuration loading & validation
│   │   ├── config.go        # Defaults, env-var bindings, bounds checking
│   │   └── config_test.go
│   └── ui/                  # Progress rendering & output formatting
│       ├── ui.go            # Live stderr events, stdout JSON/text renderer
│       └── ui_test.go
└── Makefile                 # Build, test, race, and vet automation
```

---

## 2. Deep Dive: Execution Stages

The `agent.Engine` coordinates 6 sequential stages to transform a high-level user goal into a verified solution while keeping memory consumption bounded for an 8 GB GPU.

```
Goal Ingestion ──► Stage 1: Budget Init ──► Stage 2: Schema-less Decompose
                                                         │
                                                         ▼
Final Answer  ◄── Stage 6: Synthesis    ◄── Stage 3: DAG Ordering (Kahn)
      ▲                     ▲                            │
      │                     │                            ▼
      └────────── Stage 5: Error Fixing  ◄── Stage 4: Isolated Execution Loop
```

---

### Stage 1: Ingestion & Ledger Initialization

1. **Goal Sanitization**: The user's input string is compacted using `textutil.CompactWhitespace`, stripping redundant line breaks and extra spaces to conserve tokens.
2. **Resource Boundaries Allocation**: A thread-safe `budget.Ledger` is instantiated with hard constraints:
   - `MaxTotalTokens`: Total token allowance across all tasks (default 32,768).
   - `MaxTaskContextTokens`: Context window allocated to any single micro-task (default 16,384, or clamped to 3,072–4,096 on small models).
   - `MaxMicroTasks`: Ceiling on the number of decomposed steps (default 24).
   - `MaxStepsPerTask`: Maximum reasoning/tool iterations within a single micro-task (default 16).
3. **Workspace Initialization**: The canonical absolute path of the target directory is resolved. Symlinks at the workspace root are evaluated once and sealed.

---

### Stage 2: Micro-Decomposition & Schema-less Planning

A quantized 7B model degrades rapidly if forced to digest 20 tool schemas during initial planning. `minig` uses **schema-less planning**:

1. **Lightweight Tool Summary**: The engine extracts tool names from the registry (`list_dir, read_range, edit_file...`) and formats them as a single comma-separated string (~30 tokens).
2. **Decomposition Prompt**:
   - Instructs the model to split the goal into atomic, single-deliverable tasks.
   - For bug-fixing or refactoring, mandates: (a) explore/find, (b) apply edit, (c) verify/diff.
   - Demands strict JSON with `tasks: [{id, goal, depends_on, tools}]`.
3. **Robust JSON Extraction (`extractJSONObject`)**:
   - Small models often wrap output in conversational chatter ("Here is the plan:") or markdown fences (````json ... ````) or `<think>` tags.
   - `extractJSONObject` uses a state machine to scan for balanced `{ ... }` brackets, completely ignoring surrounding commentary.
4. **Resilient Fallback**:
   - If the local model fails to produce valid JSON, `minig` falls back to a single atomic task `t1` containing the entire goal and all available tools, ensuring the agent never crashes on planning.

---

### Stage 3: DAG Topological Ordering & Cycle Detection

Once micro-tasks are extracted, their dependencies must be verified and sequenced:

1. **ID Normalization & Tool Filtering**:
   - Strips any hallucinated tool names that do not exist in the active registry.
   - Strips self-referencing dependencies.
2. **Kahn's Topological Sort**:
   - Calculates in-degree (number of prerequisite tasks) for every node.
   - Enqueues nodes with `in_degree == 0` into a processing queue.
   - Pops each node, decrements in-degree of its dependents, and appends nodes reaching 0 to the ordered execution list.
3. **Cycle Detection**:
   - If `len(ordered) != len(tasks)`, a circular dependency exists (e.g., `t1 -> t2 -> t1`).
   - The engine aborts with `micro-task plan contains a dependency cycle`.

---

### Stage 4: Isolated Micro-Task Execution Loop

This is the core innovation for 8 GB GPU inference. Each micro-task runs in a **stateless, clean room**:

```
                       Task t3 System Prompt
┌──────────────────────────────────────────────────────────────────┐
│ [Role] Execute ONLY the Current Micro-Task.                     │
│ [Tools] ONLY schemas for declared tools (e.g. read_range, edit) │
│                                                                  │
│ Overall Goal: Add revoked token verification                     │
│                                                                  │
│ Prerequisite Findings:                                           │
│ - [t1] (Locate middleware): Found auth.go at line 42             │
│ - [t2] (Check DB schema): Table 'revoked_tokens' has col 'token' │
│                                                                  │
│ Current Micro-Task [t3]:                                         │
│ Edit auth.go to check token against PostgreSQL revoked_tokens    │
└──────────────────────────────────────────────────────────────────┘
```

1. **Tool Allowlisting (`Registry.Specs(task.Tools)`)**:
   - The system prompt contains JSON schemas *only* for the tools declared for this task.
   - A read task cannot call write tools; a git task cannot call database tools.
2. **Ephemeral Conversation History**:
   - Does NOT inherit the multi-turn conversational history or raw tool dumps from `t1` or `t2`.
   - Only compact summary artifacts from direct `depends_on` tasks are injected.
   - Starting context is **< 800 tokens**, leaving maximum compute room for local inference.
3. **Token Clamping**:
   - The engine counts prompt tokens using the LLM's token counting endpoint (or character heuristics).
   - Dynamically clamps `max_tokens` so prompt + completion never exceeds the context window.
4. **Step Budget Check**:
   - Each tool invocation increments the task step counter.
   - If `step >= MaxStepsPerTask` (default 16), the loop terminates cleanly with partial results.

---

### Stage 5: Diagnostic Error Fixing & Micro-Repair Loops

When a tool fails, small models need actionable, immediate guidance:

```
                          Tool Error Interception
                                    │
                                    ▼
                     Is it an edit_file mismatch?
                     ├── YES ──► Scan file lines for nearest content match
                     │           Return line number & whitespace warning
                     │
                     Is it a missing file/path?
                     ├── YES ──► Advise using list_dir or search_text
                     │
                     Is it a database relation/syntax error?
                     └── YES ──► Advise using describe_table or list_tables
```

1. **`edit_file` Diagnostic Matching**:
   - If `old_text` does not match, `findEditMismatchDiagnostic` checks if lines match with different indentation.
   - If found, returns: `"DIAGNOSTIC: similar content found near line 84 (whitespace/indentation mismatch suspected). Inspect with read_range around line 84 before editing."`
2. **Actionable Context Injection (`formatToolErrorDiagnostic`)**:
   - The tool execution error is formatted with a clear `RECOVERY GUIDANCE` block.
   - The local model reads this in its compact context and immediately executes the suggested recovery action on the next turn.

---

### Stage 6: Bounded Synthesis & Fallback Assembly

1. **Single-Task Fast Path**:
   - If decomposition produced only 1 task, its output is returned directly as the final answer. No synthesis LLM call is wasted.
2. **Bounded Synthesis Prompt**:
   - Aggregates the compact outputs of all completed micro-tasks.
   - Asks the model to synthesize a direct, unified answer to the overall user goal.
3. **Deterministic Zero-LLM Fallback**:
   - If remaining tokens in the run-level ledger are insufficient to perform synthesis, `assembleFallbackAnswer` directly formats the micro-task outputs into a structured summary without invoking the LLM, guaranteeing deterministic completion.

---

## 3. Developer Guide: Adding Custom Capabilities

---

### 3.1 Adding Custom Built-in Tools (in Go)

Built-in tools run in-process inside `minig`, giving them maximum execution speed and direct access to the sandboxed workspace.

#### Step 1: Implement the `tool.Tool` Interface

In `internal/tool/tool.go`, the interface is defined as:

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() json.RawMessage
    Execute(ctx context.Context, args json.RawMessage) (string, error)
    ReadOnly() bool
}
```

#### Step 2: Create Your Tool Struct

Create a new file or add to `internal/tool/builtin/builtin.go`:

```go
package builtin

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "strings"

    "github.com/semay-cli/minig-v0/internal/safety"
)

type countLinesTool struct {
    ws *safety.Workspace
}

func (t *countLinesTool) Name() string   { return "count_lines" }
func (t *countLinesTool) ReadOnly() bool { return true }
func (t *countLinesTool) Description() string {
    return "Count the total number of lines in a workspace file."
}

func (t *countLinesTool) Parameters() json.RawMessage {
    return json.RawMessage(`{
        "type": "object",
        "required": ["path"],
        "properties": {
            "path": {"type": "string", "description": "Workspace-relative file path"}
        }
    }`)
}

func (t *countLinesTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
    if err := ctx.Err(); err != nil {
        return "", err
    }
    var args struct {
        Path string `json:"path"`
    }
    if err := json.Unmarshal(rawArgs, &args); err != nil {
        return "", fmt.Errorf("parse arguments: %w", err)
    }
    if strings.TrimSpace(args.Path) == "" {
        return "", errors.New("path is required")
    }

    resolved, err := t.ws.ResolveExisting(args.Path)
    if err != nil {
        return "", err
    }

    raw, err := os.ReadFile(resolved)
    if err != nil {
        return "", err
    }

    lines := strings.Count(string(raw), "\n")
    if len(raw) > 0 && !strings.HasSuffix(string(raw), "\n") {
        lines++
    }
    return fmt.Sprintf("%s has %d lines", args.Path, lines), nil
}
```

#### Step 3: Register in `RegisterAll`

In `internal/tool/builtin/builtin.go`:

```go
func RegisterAll(reg *tool.Registry, ws *safety.Workspace, allowWrite bool) error {
    tools := []tool.Tool{
        &listDirTool{ws: ws},
        &readRangeTool{ws: ws},
        &searchTextTool{ws: ws},
        &fileInfoTool{ws: ws},
        &countLinesTool{ws: ws}, // <-- Add here
    }
    // ...
}
```

#### Step 4: Add Unit Tests

Add a test case in `internal/tool/builtin/builtin_test.go` and run:
```bash
go test -v ./internal/tool/builtin/...
```

---

### 3.2 Adding & Connecting Custom MCP Servers

MCP (Model Context Protocol) allows you to expose external tools to `minig` without modifying the `minig` codebase. `minig` launches the MCP server as a subprocess and speaks JSON-RPC 2.0 over standard I/O.

#### The MCP Handshake Flow

```
minig (Client)                                MCP Server (Subprocess)
      │                                                  │
      ├────── jsonrpc: "2.0", method: "initialize" ─────►│
      │◄───── result: { protocolVersion, capabilities } ─┤
      ├────── notifications/initialized ────────────────►│
      │                                                  │
      ├────── method: "tools/list" ─────────────────────►│
      │◄───── result: { tools: [...] } ──────────────────┤
      │                                                  │
      │       (Registered as mcp_<server>_<tool>)        │
      │                                                  │
      ├────── method: "tools/call", params: {...} ──────►│
      │◄───── result: { content: [{ text: "..." }] } ────┤
```

---

#### Example A: Writing a Custom Python MCP Server

Create `my_mcp_server.py`:

```python
#!/usr/bin/env python3
import sys
import json

def send_response(response):
    sys.stdout.write(json.dumps(response) + "\n")
    sys.stdout.flush()

def main():
    for line in sys.stdin:
        if not line.strip():
            continue
        req = json.loads(line)
        method = req.get("method")
        msg_id = req.get("id")

        if method == "initialize":
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "protocolVersion": "2024-11-05",
                    "capabilities": {"tools": {}},
                    "serverInfo": {"name": "custom-tools", "version": "1.0.0"}
                }
            })
        elif method == "notifications/initialized":
            continue
        elif method == "tools/list":
            send_response({
                "jsonrpc": "2.0",
                "id": msg_id,
                "result": {
                    "tools": [
                        {
                            "name": "system_info",
                            "description": "Get current operating system and Python version",
                            "inputSchema": {
                                "type": "object",
                                "properties": {}
                            }
                        }
                    ]
                }
            })
        elif method == "tools/call":
            params = req.get("params", {})
            name = params.get("name")
            if name == "system_info":
                send_response({
                    "jsonrpc": "2.0",
                    "id": msg_id,
                    "result": {
                        "content": [
                            {"type": "text", "text": f"Platform: {sys.platform}, Python: {sys.version}"}
                        ],
                        "isError": False
                    }
                })

if __name__ == "__main__":
    main()
```

Make it executable:
```bash
chmod +x my_mcp_server.py
```

Run with `minig`:
```bash
minig --mcp "sys=python3 my_mcp_server.py" "Check system info"
```
The tool is automatically registered as `mcp_sys_system_info`!

---

#### Example B: Writing a Custom Go MCP Server

Create `cmd/mcp-linter/main.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
)

type RPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type RPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func main() {
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		var req RPCRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			send(RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"protocolVersion": "2024-11-05",
					"capabilities":    map[string]any{"tools": map[string]any{}},
					"serverInfo":      map[string]string{"name": "go-linter", "version": "1.0.0"},
				},
			})
		case "notifications/initialized":
			// Handshake complete
		case "tools/list":
			send(RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"tools": []map[string]any{
						{
							"name":        "run_vet",
							"description": "Run go vet on workspace packages",
							"inputSchema": map[string]any{
								"type":       "object",
								"properties": map[string]any{},
							},
						},
					},
				},
			})
		case "tools/call":
			out, err := exec.Command("go", "vet", "./...").CombinedOutput()
			msg := string(out)
			if err != nil && msg == "" {
				msg = err.Error()
			}
			if msg == "" {
				msg = "go vet passed with zero warnings"
			}
			send(RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]any{
					"content": []map[string]string{
						{"type": "text", "text": msg},
					},
					"isError": false,
				},
			})
		}
	}
}

func send(resp RPCResponse) {
	b, _ := json.Marshal(resp)
	fmt.Println(string(b))
}
```

Build and run:
```bash
go build -o bin/mcp-linter ./cmd/mcp-linter
minig --mcp "linter=./bin/mcp-linter" "Run static analysis with go vet"
```

---

### 3.3 Adding Custom LLM Providers

`minig` abstracts inference backends behind the `llm.Provider` interface:

```go
type Provider interface {
    CheckHealth(ctx context.Context) error
    CountTokens(ctx context.Context, model string, messages []Message, tools []ToolSpec) (int, error)
    Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}
```

To add support for Ollama, vLLM, or Aphrodite:

1. Create `internal/llm/ollama/client.go`.
2. Implement `CheckHealth` (query `/api/tags` or `/health`).
3. Implement `CountTokens` (use `/api/embeddings` or token count heuristic).
4. Implement `Chat` (POST `/api/chat` mapping `messages` and `tools`).
5. In `cmd/minig/main.go`, instantiate your client based on a `--provider` flag or URL format:
   ```go
   if strings.Contains(cfg.LlamaURL, "11434") {
       client = ollama.NewClient(...)
   } else {
       client = llamacpp.NewClient(...)
   }
   ```

---

### 3.4 Adding Custom Agentic Workflows & Planners

To alter the decomposition strategy (e.g., adding specialized Test-Driven Development loops or Multi-Agent debate):

1. **Custom Planner Prompts**:
   In `internal/agent/planner.go`, you can define alternative decomposition strategies:
   ```go
   func (e *Engine) DecomposeTDD(ctx context.Context, ledger *budget.Ledger, goal string) ([]MicroTask, error) {
       // Instruct the model to strictly generate:
       // 1. Write failing test
       // 2. Implement minimal code
       // 3. Run test and verify pass
       // 4. Refactor
   }
   ```
2. **Dynamic Task Insertion / Self-Healing Pipeline**:
   In `internal/agent/engine.go`, you can evaluate the output of verification tasks:
   ```go
   if task.IsVerification && strings.Contains(res.Output, "FAIL") {
       // Dynamically insert an error-repair task into the execution list!
       repairTask := MicroTask{
           ID:        fmt.Sprintf("%s_fix", task.ID),
           Goal:      "Fix failing test diagnosed in " + task.ID,
           Tools:     []string{"read_range", "edit_file"},
           DependsOn: []string{task.ID},
       }
       // Inject into task queue
   }
   ```
3. **Custom Event Observers**:
   Implement the `agent.Observer` interface to stream events to WebSockets, OpenTelemetry, or custom TUI dashboards:
   ```go
   type Observer interface {
       OnEvent(ev Event)
   }
   ```

---

## 4. Verification & Testing

Every component in `minig` is backed by unit and integration tests. Run the full verification suite before committing:

```bash
# Run all unit tests
make test

# Run race detector to guarantee concurrent safety
make race

# Run static analysis
make vet

# Format code
make fmt
```

---

*Engineered with precision to deliver robust, predictable, and local-first AI software engineering.*
