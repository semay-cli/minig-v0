# AGENT.md — minig Agent Specification

> **minig** is an ultra-lean, local-first coding agent engine engineered specifically for quantized local LLMs running on consumer GPUs with **8 GB VRAM** (e.g., RTX 3060/4060, Apple Silicon unified memory).
>
> By breaking high-level developer goals into an acyclic directed graph (DAG) of **atomic micro-tasks**, minig ensures that each reasoning step fits within tight local context windows (3K–4K tokens), enforces strict resource and step budgets, isolates execution environments, provides diagnostic error-healing loops, and connects seamlessly to advanced **MCP (Model Context Protocol)** servers for Git, PostgreSQL, Docker, and file manipulation.

---

## 1. Identity & 8 GB GPU Design Philosophy

### 1.1 The Local 8 GB GPU Challenge

Running coding agents on local consumer hardware (8 GB VRAM) presents severe constraints that cloud-hosted agent architectures ignore:

1. **Context Window Degradation**: Quantized 7B/8B models (e.g., Qwen 2.5 Coder 7B, Llama 3.1 8B, DeepSeek-R1-Distill-Qwen-7B at Q4_K_M or Q5_K_M) suffer significant degradation in instruction-following and tool-calling accuracy when context windows grow beyond 4,000–8,000 tokens.
2. **Attention & Tool Confusion**: Passing 20+ tool definitions simultaneously causes 7B models to hallucinate invalid arguments, confuse tool names, or emit broken JSON.
3. **Runaway Inference & Latency**: On an 8 GB GPU generating at 25–40 tokens/second, long reasoning chains or unbounded multi-turn loops freeze the developer's machine and quickly exhaust memory.
4. **Error Cascades**: Small models struggle to recover from errors when buried under 15 turns of prior conversational history.

### 1.2 The minig Solution: Micro-Decomposition

`minig` solves these problems through **micro-decomposition**:
- **Atomic Execution Units**: A complex goal is decomposed into a series of small, single-purpose micro-tasks (e.g., "Find config definition", "Read migration schema", "Apply surgical edit", "Run verification test").
- **Context Isolation**: Each micro-task begins with a clean, minimal context window (~1.5K–3K tokens). Only the compact summaries of direct dependencies are forwarded.
- **Dynamic Tool Scoping**: Each micro-task is provisioned *only* with the specific tools required for its atomic goal. A "read schema" task only sees read tools; it is physically impossible for it to trigger a write tool.
- **Micro-Repair & Self-Correction**: When a tool returns an error, targeted diagnostic feedback is injected immediately, allowing the model to repair arguments within its compact context without conversational pollution.
- **Dual Interface**: Operates both as an interactive/scriptable CLI coding agent and as an HTTP REST/SSE API server for IDE integrations.
- **Zero External Dependencies**: Pure Go standard library. No Python runtime requirements, no Node.js dependencies, no fragile orchestration frameworks.

### 1.3 Target Hardware Profile

```
Hardware Target:   8 GB VRAM (NVIDIA RTX 3060/4060, AMD RX 7600, Apple M1/M2/M3 16GB unified)
Model Tier:        7B–8B parameter open weights (GGUF format)
Quantization:      Q4_K_M or Q5_K_M (~4.5 GB – 5.8 GB VRAM allocation)
Inference Engine:  llama.cpp server (or Ollama / vLLM / local OpenAI-compatible endpoint)
Context Allocated: 4,096 to 16,384 tokens (micro-tasks capped at 3,072–4,096)
Host Memory:       16 GB RAM recommended
Storage:           Fast NVMe SSD for fast GGUF weight paging
```

### 1.4 Architectural Principles

| Principle | Technical Implementation | Benefit for 8 GB Local LLM |
|---|---|---|
| **Micro-Decomposition** | Topological DAG generation via zero-tool planning prompt | Keeps each execution step focused on a single atomic deliverable |
| **Context Isolation** | Ephemeral context per task; only parent task artifacts passed | Eliminates history pollution; avoids model confusion and context drift |
| **Tool Scoping** | Dynamic filter on tool schemas per micro-task | Saves 500–1,500 tokens of schema overhead per step; prevents tool hallucination |
| **Budget Enforcement** | Two-tier ledger tracking tokens, steps, and task count | Prevents runaway inference loops and GPU memory bottlenecks |
| **Diagnostic Error Fixing** | Near-match line search and actionable correction hints | Turns failed tool calls into self-correction opportunities |
| **Workspace Sandboxing** | Canonical path resolution with strict symlink rejection | Prevents path traversal and unintended file modifications |
| **Pluggable MCP** | JSON-RPC 2.0 stdio subprocess communication | Unlocks advanced Git, PostgreSQL, and system tools without binary bloat |

---

## 2. Architecture Overview

```
                      ┌───────────────────────────────────────────┐
                      │            minig Engine / CLI / API       │
                      │  cmd/minig (CLI)  │  internal/server (API) │
                      └─────────────────────┬─────────────────────┘
                                            │
                                            ▼
                      ┌───────────────────────────────────────────┐
                      │              agent.Engine                 │
                      │  1. Ingestion & Budget Initialization     │
                      │  2. Micro-Decomposition & Validation      │
                      │  3. DAG Topological Ordering              │
                      │  4. Isolated Micro-Task Execution Loop    │
                      │  5. Diagnostic Error-Fixing Loop          │
                      │  6. Bounded Synthesis                     │
                      └─────┬──────────────┬──────────────┬───────┘
                            │              │              │
           ┌────────────────┘              │              └────────────────┐
           ▼                               ▼                               ▼
┌────────────────────┐          ┌────────────────────┐          ┌────────────────────┐
│   budget.Ledger    │          │  safety.Workspace  │          │   tool.Registry    │
│  - Total Run Limit │          │  - Path Sandboxing │          │  - Tool Scoping    │
│  - Task Ctx Limit  │          │  - Symlink Defense │          │  - Timeout Wrapper │
│  - Step Clamping   │          │  - Read-Only Mode  │          │  - Output Bounding │
└────────────────────┘          └────────────────────┘          └─────────┬──────────┘
                                                                          │
                                       ┌──────────────────────────────────┴──────────────┐
                                       ▼                                                 ▼
                          ┌─────────────────────────┐                       ┌─────────────────────────┐
                          │   internal/tool/builtin │                       │    internal/tool/mcp    │
                          │   - list_dir            │                       │  JSON-RPC 2.0 stdio     │
                          │   - read_range          │                       │  ┌────────────────────┐ │
                          │   - search_text         │                       │  │ mcp-server-git     │ │
                          │   - write_file          │                       │  │ mcp-server-postgres│ │
                          │   - edit_file           │                       │  │ mcp-server-docker  │ │
                          │   - append_file         │                       │  │ mcp-server-fs      │ │
                          │   - file_info           │                       │  │ custom MCP servers │ │
                          │   - delete_file         │                       │  └────────────────────┘ │
                          └─────────────────────────┘                       └─────────────────────────┘
                                       │                                                 │
                                       └────────────────┬────────────────────────────────┘
                                                        ▼
                                       ┌──────────────────────────────────┐
                                       │        llm.Provider Client       │
                                       │  - llama.cpp /v1/chat/completions│
                                       │  - Token Counting & Clamping     │
                                       │  - Loopback Guard Protection     │
                                       └──────────────────────────────────┘
```

---

## 3. The Micro-Task Decomposition Engine

### 3.1 The Decomposition Lifecycle

When a user submits a goal (e.g., *"Refactor the auth middleware to check PostgreSQL revoked_tokens and update tests"*):

1. **Schema-less Planning Prompt**: The engine queries the local LLM with *zero* tool schemas, providing only a lightweight comma-separated list of tool names. This minimizes token consumption during planning (~150 tokens) and avoids distracting the 7B model.
2. **JSON Extraction**: The response is parsed with a robust, balanced-delimiter JSON parser (`extractJSONObject`) that automatically ignores conversational prose, markdown code blocks (````json ... ````), and thinking tags (`<think>...</think>`).
3. **Micro-Task Normalization**:
   - Every task is assigned a unique alphanumeric ID (e.g., `t1`, `t2`, `t3`).
   - Tools are validated against the active tool registry; invalid or hallucinated tools are stripped.
   - Dependencies are checked for self-cycles and non-existent IDs.
4. **DAG Topological Sort (Kahn's Algorithm)**:
   - Evaluates in-degree counters for all micro-tasks.
   - Verifies the dependency graph is acyclic.
   - Computes the optimal linear execution order.
5. **Fallback Plan**: If a tiny model outputs invalid JSON or unstructured text, the engine automatically falls back to a single atomic micro-task `t1` containing the entire goal and all available tools.

### 3.2 MicroTask Schema Definition

```json
{
  "tasks": [
    {
      "id": "t1",
      "goal": "Read auth/middleware.go to locate token validation logic",
      "depends_on": [],
      "tools": ["read_range", "search_text"]
    },
    {
      "id": "t2",
      "goal": "Inspect PostgreSQL schema for revoked_tokens table definition",
      "depends_on": ["t1"],
      "tools": ["mcp_postgres_describe_table", "mcp_postgres_query"]
    },
    {
      "id": "t3",
      "goal": "Edit auth/middleware.go to add revoked token check against DB",
      "depends_on": ["t1", "t2"],
      "tools": ["read_range", "edit_file"]
    },
    {
      "id": "t4",
      "goal": "Verify git diff to ensure changes are accurate and clean",
      "depends_on": ["t3"],
      "tools": ["mcp_git_diff", "mcp_git_status"]
    }
  ]
}
```

### 3.3 Context Sandboxing & Artifact Forwarding

When micro-task `t3` executes:
- It **does not** receive the raw conversation history or multi-turn tool dumps from `t1` or `t2`.
- It receives a clean prompt containing:
  1. The overall goal (truncated to 512 bytes).
  2. Prerequisite findings: compact, truncated summary artifacts from its direct dependencies (`t1` and `t2`).
  3. The current micro-task goal.
  4. Only the JSON schemas for `read_range` and `edit_file`.
- Total starting prompt size: **< 800 tokens**, leaving over **2,500 tokens** of clean space for tool calls and reasoning.

---

## 4. Advanced Error Recovery & Diagnostic Loops

Quantized 7B models frequently make subtle errors: minor indentation mismatches in file editing, invalid SQL table names, or missing file paths. `minig` provides multi-tiered error recovery tailored to local models.

### 4.1 Layer 1: In-Task Diagnostic Error Feedback

Rather than halting on a failed tool execution, the error message is fed directly into the tool response message with actionable guidance:

```
Tool Output on Failure:
"TOOL EXECUTION FAILED: old_text not found in 'pkg/auth/token.go'
DIAGNOSTIC HINT: The exact text snippet did not match. Similar content was found near lines 42-50.
RECOVERY ACTION: Call read_range(path='pkg/auth/token.go', start_line=35, max_lines=25) to verify exact whitespace and indentation, then re-call edit_file."
```

Because the context window is small, the local LLM immediately reads the diagnostic hint and executes `read_range`, followed by a corrected `edit_file`.

### 4.2 Layer 2: Surgical Fuzzy & Near-Match Diagnostics in `edit_file`

The built-in `edit_file` tool does not simply fail with `strings.Index`. It performs intelligent diagnostics:
1. **Uniqueness Verification**: Checks how many times `old_text` appears. If `matches > 1`, it reports the count and advises providing more surrounding context lines.
2. **Fuzzy Line Search**: If `matches == 0`, it normalizes trailing whitespace and scans for line subsets. If lines match with different indentation, it explicitly informs the LLM:
   ```
   "old_text not found in auth.go. Notice: Found similar text with different indentation on line 84. Inspect with read_range before editing."
   ```
3. **Safety Protection**: Prevents binary file edits and enforces a maximum file size limit (512 KB) to prevent context buffer overflow.

### 4.3 Layer 3: Micro-Repair Tasks & Verification Phases

When the agent performs modifications, the planner is instructed to generate distinct verification micro-tasks:
1. **Exploration Task**: Find relevant files and lines.
2. **Modification Task**: Apply surgical changes using `edit_file` or `write_file`.
3. **Verification/Error-Fixing Task**: Inspect `git_diff`, run compilation/lint, or check database constraints. If an error is detected during verification, the verification task performs the targeted repair or reports exact remediation steps.

### 4.4 Layer 4: Hard Budget Safety Net

If a model enters a hallucination loop:
- **MaxStepsPerTask** (default 16) caps the number of tool iterations within a single micro-task. Once reached, the task exits cleanly with its accumulated partial findings.
- **MaxTotalTokens** (default 32,768) ensures the overall run never exceeds memory or compute thresholds.
- **Synthesis Fallback**: If the token budget is exhausted before the synthesis step, `minig` concatenates the task artifacts directly without invoking an extra LLM call, guaranteeing a deterministic result.

---

## 5. Tool Ecosystem

### 5.1 Built-in Workspace Tools

Built-in tools run in-process with zero external runtime requirements. All file operations are strictly confined to the validated workspace root.

| Tool | Mode | Parameters | Purpose |
|---|---|---|---|
| `list_dir` | Read-only | `path` (str), `limit` (int) | List directories and files with size metadata, automatically ignoring `.git`, `node_modules`, and symlinks |
| `read_range` | Read-only | `path` (str), `start_line` (int), `max_lines` (int) | Read a bounded 1-based slice of lines (default 80, max 200) from any text file |
| `search_text` | Read-only | `query` (str), `path` (str), `max_hits` (int) | Fast, recursive substring search returning `file:line: snippet` entries across the workspace |
| `file_info` | Read-only | `path` (str) | Retrieve file metadata: size, line count, permissions, modification time, and binary detection |
| `write_file` | Read-write | `path` (str), `content` (str) | Write or overwrite a file, automatically creating parent directories safely |
| `edit_file` | Read-write | `path` (str), `old_text` (str), `new_text` (str) | Surgically replace a unique, exact block of text with diagnostic error guidance on mismatch |
| `append_file` | Read-write | `path` (str), `content` (str) | Append text to the end of a file or create the file if it does not exist |
| `delete_file` | Read-write | `path` (str) | Safely remove a file inside the workspace (directories cannot be deleted via this tool) |

### 5.2 Advanced Git Capabilities via MCP

Git operations are delegated to standard MCP servers (such as `mcp-server-git`) over stdio JSON-RPC 2.0. Tools are dynamically registered under the `mcp_<server>_<tool>` namespace.

| MCP Tool | Arguments | Purpose & Best Practice |
|---|---|---|
| `mcp_git_status` | `repository` | Inspect dirty files, untracked files, and staged changes |
| `mcp_git_diff` | `repository`, `staged`, `file_path` | View line-by-line unified diffs before and after edits |
| `mcp_git_log` | `repository`, `max_count` | View recent commit messages, authors, and hashes |
| `mcp_git_add` | `repository`, `files` | Stage specific files or patterns |
| `mcp_git_commit` | `repository`, `message` | Create atomic git commits with formatted commit messages |
| `mcp_git_branch` | `repository`, `list`, `name` | List or create local feature branches |
| `mcp_git_checkout` | `repository`, `branch` | Switch branches or restore files |
| `mcp_git_reset` | `repository`, `mode`, `target` | Unstage changes or reset HEAD safely |
| `mcp_git_stash` | `repository`, `action` | Stash temporary work during complex multi-task refactorings |
| `mcp_git_show` | `repository`, `revision` | Inspect specific commits, tags, or tree objects |

### 5.3 Advanced PostgreSQL / SQL Capabilities via MCP

PostgreSQL integration is provided via `mcp-server-postgres`. Small local models excel at targeted database tasks when schemas are pre-inspected.

| MCP Tool | Arguments | Purpose & Error Recovery Pattern |
|---|---|---|
| `mcp_pg_describe_table` | `table_name`, `schema` | Inspect column types, nullability, primary keys, and foreign keys |
| `mcp_pg_list_tables` | `schema` | Discover available relations without dumping massive schemas into context |
| `mcp_pg_list_schemas` | none | Discover user and system schemas (`public`, `auth`, etc.) |
| `mcp_pg_query` | `sql` | Execute read-only SQL queries (`SELECT`) with automatic row limit clamping |
| `mcp_pg_execute` | `sql` | Execute DDL/DML statements (`INSERT`, `UPDATE`, `CREATE TABLE`, `ALTER TABLE`) |
| `mcp_pg_explain` | `sql`, `analyze` | Generate query execution plans for query optimization |

#### SQL Error Self-Correction Loop:
```
1. LLM calls mcp_pg_query("SELECT id, user_id FROM orders WHERE created_at > NOW()")
2. Database returns error: "column 'user_id' does not exist in relation 'orders'"
3. Engine feeds error back to LLM.
4. LLM calls mcp_pg_describe_table("orders")
5. Tool returns columns: id, customer_id, total_amount, created_at.
6. LLM immediately retries with corrected query: "SELECT id, customer_id FROM orders..."
```

### 5.4 Additional Advanced MCP Servers

`minig` supports any compliant MCP server configured via `--mcp` flags or environment variables:
- **`mcp-server-docker`**: Container management (`docker_ps`, `docker_logs`, `docker_build`, `docker_exec`).
- **`mcp-server-filesystem`**: Extended file operations outside workspace boundaries when explicitly permitted.
- **`mcp-server-fetch`**: Secure HTTP client for inspecting remote API endpoints, OpenAPI specs, and webhooks.
- **`mcp-server-shell`**: Bounded subprocess command execution for compiling code and running tests (`go test`, `pytest`, `npm test`).

---

## 6. HTTP API Server Specification

`minig` can run as an HTTP daemon (`minig --serve --port 8080`) to provide agentic intelligence to IDE extensions, web interfaces, and CI/CD pipelines.

### 6.1 Endpoints Overview

| Method | Path | Content-Type | Description |
|---|---|---|---|
| `GET` | `/v1/health` | `application/json` | Health check for minig and upstream llama.cpp server |
| `GET` | `/v1/tools` | `application/json` | List all available built-in and MCP tool schemas |
| `POST` | `/v1/run` | `application/json` | Execute a goal synchronously; returns complete `RunResult` |
| `POST` | `/v1/run/stream` | `text/event-stream` | Execute a goal with Server-Sent Events (SSE) streaming milestones |

### 6.2 Synchronous Run: `POST /v1/run`

#### Request Payload:
```json
{
  "goal": "Find all exported functions in pkg/auth and check if tests exist",
  "workspace": "/home/user/workspace/myapp",
  "read_only": true,
  "max_tasks": 8,
  "task_context_tokens": 4096,
  "total_tokens": 30000
}
```

#### Response Payload:
```json
{
  "answer": "pkg/auth exports 3 functions: Authenticate, ValidateToken, and HashPassword. Unit tests in pkg/auth/auth_test.go cover Authenticate and ValidateToken; HashPassword lacks coverage.",
  "tasks": [
    {
      "task": {
        "id": "t1",
        "goal": "Search exported functions in pkg/auth",
        "depends_on": [],
        "tools": ["search_text", "read_range"]
      },
      "output": "Found Authenticate (line 15), ValidateToken (line 42), HashPassword (line 88).",
      "tokens_used": 620,
      "tool_calls": 2
    },
    {
      "task": {
        "id": "t2",
        "goal": "Check test coverage in pkg/auth/auth_test.go",
        "depends_on": ["t1"],
        "tools": ["read_range"]
      },
      "output": "TestAuthenticate and TestValidateToken found. No test for HashPassword.",
      "tokens_used": 480,
      "tool_calls": 1
    }
  ],
  "stats": {
    "prompt_tokens": 1640,
    "completion_tokens": 420,
    "total_tokens": 2060,
    "remaining_tokens": 27940,
    "tasks_started": 2,
    "tasks_completed": 2,
    "tool_calls": 3
  },
  "planned": 2
}
```

### 6.3 Streaming Run: `POST /v1/run/stream` (SSE)

Clients receive real-time JSON events as the micro-task DAG progresses:

```http
POST /v1/run/stream HTTP/1.1
Host: localhost:8080
Content-Type: application/json

{"goal": "Add health check endpoint to router"}

HTTP/1.1 200 OK
Content-Type: text/event-stream
Cache-Control: no-cache
Connection: keep-alive

event: plan_ready
data: {"message":"decomposed into 3 micro-task(s)","task_total":3}

event: task_start
data: {"task_id":"t1","message":"Read router setup in cmd/api/router.go","task_index":1,"task_total":3}

event: tool_call
data: {"task_id":"t1","message":"read_range({\"path\":\"cmd/api/router.go\"})"}

event: task_done
data: {"task_id":"t1","message":"Found router configuration on line 25","task_index":1,"task_total":3}

event: task_start
data: {"task_id":"t2","message":"Edit router.go to add /healthz route","task_index":2,"task_total":3}

event: synthesizing
data: {"message":"synthesizing micro-task results","task_total":3}

event: done
data: {"answer":"Successfully added /healthz endpoint to cmd/api/router.go.","stats":{"total_tokens":3120}}
```

---

## 7. System Prompt Engineering for 8 GB Local Models

### 7.1 Rules of Prompt Compression

To conserve precious VRAM and compute tokens:
1. **No Superfluous Personas**: Skip lengthy motivational preambles. State roles and strict execution rules directly.
2. **One Tool at a Time**: Small models struggle with multi-tool calling. The prompt commands: *"Call one tool at a time. Await the output before proceeding."*
3. **No Hallucinated Knowledge**: *"ALWAYS inspect files using tools. NEVER assume or guess file contents."*
4. **Stripping Whitespace & Fences**: Prompts pass through `textutil.CompactWhitespace` to remove unnecessary carriage returns and repeated spaces.

### 7.2 Micro-Task System Prompt Template

```
Execute ONLY the Current Micro-Task.
Allowed Tools: [read_range, edit_file]
Rules:
1. Call allowed tools only when necessary to inspect or modify files.
2. If a tool fails, read the error diagnostic and fix arguments immediately.
3. Once the micro-task goal is satisfied, respond with a concise, factual summary of findings or modifications.

Overall Goal: <compact goal>
Prerequisite Findings:
- [t1] (Read router): Located router definition in cmd/api/router.go at line 42.

Current Micro-Task [t2]: Add GET /healthz route handler
```

---

## 8. Budget Ledger & Safety System

### 8.1 Resource Budget Hierarchy

```
                            Run-Level Ledger
                (MaxTotalTokens: default 32,768, ceiling 500,000)
                                    │
          ┌─────────────────────────┴─────────────────────────┐
          ▼                                                   ▼
   MaxMicroTasks                                     TaskContextTokens
(default 24, max 64)                                (default 16,384, clamped)
                                                              │
                                                              ▼
                                                       MaxStepsPerTask
                                                    (default 16, max 32)
```

### 8.2 Safety & Sandboxing

1. **Path Traversal Shield**: All paths resolve via `filepath.Abs` and `filepath.Rel` against the workspace canonical path. Attempts to traverse outside (`../../etc/passwd`) immediately return `path escapes workspace root`.
2. **Symlink Defense**: Symlinks are evaluated at every path segment. If any parent directory or file is a symlink, operations are aborted to prevent symlink race escapes.
3. **Read-Only Mode**: When `AllowWrite = false`, write and edit tools (`write_file`, `edit_file`, `append_file`, `delete_file`) are completely omitted from the tool registry. The LLM is never presented with write options.
4. **Loopback LLM Guard**: By default, the LLM endpoint must resolve to `127.0.0.1` or `localhost`. External URLs require explicit opt-in via `--allow-remote` or `MINIG_ALLOW_REMOTE=true`.

---

## 9. Extending minig

### 9.1 Adding a Custom Built-in Tool

Implement the `tool.Tool` interface in Go:

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() json.RawMessage
    Execute(ctx context.Context, args json.RawMessage) (string, error)
    ReadOnly() bool
}
```

Register it in `internal/tool/builtin/builtin.go`:
```go
reg.Register(&myCustomTool{ws: ws})
```

### 9.2 Connecting a Custom MCP Server

Any subprocess that implements the Model Context Protocol over stdio JSON-RPC 2.0 can be plugged in immediately:

```bash
minig --mcp "git=mcp-server-git --repository ." \
      --mcp "postgres=mcp-server-postgres postgresql://dev:secret@localhost:5432/mydb" \
      "Check migration status and create missing index"
```

The engine automatically:
1. Spawns the subprocess on startup.
2. Sends the `initialize` handshake and `notifications/initialized`.
3. Calls `tools/list` to register tools under `mcp_<name>_<tool>`.
4. Reaps the subprocess cleanly on shutdown.

---

## 10. Configuration & CLI Reference

### 10.1 CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--url` | `http://127.0.0.1:8080` | llama.cpp base URL |
| `--model` | `local` | Model identifier string |
| `--workspace` | `.` | Root directory for file operations |
| `--write` | `false` | Enable write tools (`write_file`, `edit_file`, etc.) |
| `--mcp` | none | MCP server spec as `name='command args...'` (repeatable) |
| `--serve` | `false` | Run as an HTTP REST & SSE API server |
| `--port` | `8080` | Port for the HTTP API server |
| `--max-tasks` | `24` | Maximum micro-tasks per run (1–64) |
| `--max-steps` | `16` | Maximum tool-call reasoning steps per micro-task |
| `--ctx-tokens` | `16384` | Max context tokens per isolated task |
| `--total-tokens` | `32768` | Total token budget across the entire run |
| `--json` | `false` | Emit structured JSON output on stdout |
| `--quiet` | `false` | Suppress stderr progress events |
| `--allow-remote` | `false` | Permit non-loopback LLM server addresses |
| `--health` | `false` | Check llama.cpp server health and exit |

### 10.2 Environment Variables

All flags can be configured via environment variables:
`MINIG_LLAMA_URL`, `MINIG_MODEL`, `MINIG_WORKSPACE`, `MINIG_MCP_SERVERS`, `MINIG_ALLOW_WRITE`, `MINIG_MAX_MICRO_TASKS`, `MINIG_MAX_STEPS_PER_TASK`, `MINIG_TASK_CONTEXT_TOKENS`, `MINIG_TOTAL_RUN_TOKENS`, `MINIG_MAX_TOOL_OUTPUT_BYTES`, `MINIG_LLM_TIMEOUT`, `MINIG_TOOL_TIMEOUT`, `MINIG_ALLOW_REMOTE_LLM`.

---

*minig is engineered for developers who believe that powerful, private, and deterministic coding agents should run locally on accessible hardware.*
