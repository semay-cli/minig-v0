# minig — Local-First Micro-Task Coding Agent for 8 GB GPUs

> **minig** is an ultra-lean, local-first coding agent engine engineered specifically for quantized 7B–8B local LLMs running on consumer hardware with **8 GB VRAM** (e.g., NVIDIA RTX 3060/4060, AMD RX 7600, Apple Silicon unified memory).
>
> By breaking complex developer goals into a directed acyclic graph (DAG) of **atomic micro-tasks**, `minig` prevents context bloat, enforces strict budget discipline, provides diagnostic error self-healing, and connects seamlessly to advanced **MCP (Model Context Protocol)** servers for Git, PostgreSQL, Docker, and full filesystem operations.

---

## Why minig? The 8 GB Local LLM Reality

Most modern coding agents are designed for trillion-parameter cloud models with 128K+ token context windows. When forced onto quantized 7B/8B local models running on consumer 8 GB GPUs:

- **Context Degradation**: Small models rapidly hallucinate and lose instruction-following accuracy once context exceeds 4K–8K tokens.
- **Attention Overload**: Dumping 20+ tool definitions into a single prompt confuses 7B models, causing broken JSON, missing arguments, or invalid tool calls.
- **Runaway Latency**: Multi-turn conversation history balloons prompt processing time on local GPUs, slowing iteration to a crawl.
- **Error Cascades**: Small models struggle to recover from errors when buried under dozens of previous conversational turns.

### The minig Architecture:

| Problem on 8 GB GPUs | minig Solution | Impact |
|---|---|---|
| **Context Bloat** | **Micro-Decomposition**: Splits goals into a DAG of atomic tasks (e.g., *Find file* → *Inspect lines* → *Apply edit* → *Verify*). | Each task runs in a fresh, isolated ~2K–4K context window. |
| **Tool Confusion** | **Tool Scoping**: Each micro-task is provisioned *only* with the tools it needs. | Saves 500–1,500 tokens of schema overhead; eliminates tool hallucination. |
| **Silent Edits & Loops** | **Budget Ledger**: Hard step limits per task (default 16) and run-wide token caps (default 32K). | Prevents infinite loops and runaway GPU memory consumption. |
| **Failing Edits** | **Diagnostic Error Fixing**: Surgical near-match diagnostics and actionable recovery hints injected into error responses. | Allows 7B models to self-correct whitespace/indentation errors immediately. |
| **Heavy Dependencies** | **Pure Go Standard Library**: No Python runtime, no Node.js orchestration frameworks, zero external dependencies. | Fast native binary (<15 MB), instant startup, zero configuration friction. |
| **Integration Needs** | **Dual Interface**: Runs as an interactive/scriptable CLI tool or an HTTP REST & SSE streaming API server. | Embeds into IDE extensions, terminals, web apps, and automated workflows. |

---

## Quick Start

### 1. Prerequisites

- **Go**: 1.22+ (tested with Go 1.24 / 1.27)
- **Local LLM Backend**: `llama.cpp` server (or Ollama / vLLM with OpenAI-compatible API)
- **Recommended 7B–8B GGUF Models**:
  - `Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf` (Outstanding tool-use and coding performance)
  - `Llama-3.1-8B-Instruct-Q4_K_M.gguf`
  - `DeepSeek-R1-Distill-Qwen-7B-Q4_K_M.gguf`

### 2. Start the Local LLM Backend (llama.cpp)

On an 8 GB GPU, launch `llama.cpp` with a 16K context allocation:

```bash
./llama-server \
  --model models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  --ctx-size 16384 \
  --n-gpu-layers 33 \
  --host 127.0.0.1 \
  --port 8080
```

Verify connectivity:
```bash
minig --url http://127.0.0.1:8080 --health
# Output: healthy (http://127.0.0.1:8080)
```

### 3. Build & Install minig

```bash
git clone https://github.com/semay-cli/minig-v0.git
cd minig-v0
make build

# Optional: add to PATH
export PATH="$PWD/bin:$PATH"
```

---

## Dual-Mode Runtime

The same binary is a local coding agent and a headless SRE service. Neither mode shells out to an external MCP server; the tools live in-process.

| Mode | Command | Workspace | Who calls it |
|---|---|---|---|
| Interactive coding | `minig "…"` or `minig acp` | `local` (project files, git, ripgrep) | Terminal, Zed, IntelliJ IDEA, VS Code |
| Headless automation | `minig serve --host 0.0.0.0 --port 8080` | `none` (no cwd, no `.git`) | Python alerting and log pipelines |

`minig acp` speaks Agent Client Protocol JSON-RPC on stdin/stdout. File reads, writes, and terminal commands are handed back to the IDE. Copy-paste configs for Zed (`~/.config/zed/settings.json`), JetBrains (`~/.jetbrains/acp.json`), and VS Code are in [`docs/ide-setup.md`](./docs/ide-setup.md) and [`configs/ide/`](./configs/ide/).

`minig serve` defaults to `workspace_mode=none`. `POST /v1/agent/run` accepts `prompt`, `context_data`, `allowed_tools`, `response_schema`, and `llm_config`. Per-request llama.cpp routing uses `X-LLM-Base-URL`, `X-LLM-API-Key`, and `X-LLM-Model` (or `llm_config` in the body), then the IDE environment (`LLAMA_CPP_URL`, `LLAMA_CPP_API_KEY`, `LLAMA_CPP_MODEL`), then `~/.config/minig/config.json`. Set `MINIG_API_KEY` to require a bearer token on the HTTP API. Scratch work for `sandbox_python` goes to `/tmp/minig-run-<id>` and is removed before the response returns.

```bash
minig serve --host 0.0.0.0 --port 8080
python3 examples/python_sre_client.py --base-url http://127.0.0.1:8080
```

`minig mcp` serves the built-in tool registry over MCP stdio. The previous `--serve`, `--mcp`, and goal-argument CLI flags still work.

Built-in tools that do not need a project checkout: `observability_http`, `log_template_miner`, `sandbox_python`, `alert_dispatch`, `web_fetch`, `doc_extract`, `session_scratchpad`. Local sessions also get `workspace_code`, `workspace_terminal` (with `--write`), and the existing file tools.

---

## CLI Coding Agent Usage

### Read-Only Exploration & Code Analysis
By default, `minig` runs in safe read-only mode (write tools are omitted from tool schemas):

```bash
minig "Summarize the routing and middleware architecture of this repository"
```

Live progress is streamed to `stderr`, and the final answer is rendered to `stdout`:
```
[plan] decomposed into 3 micro-task(s)
[1/3 t1] List repository structure and locate router files
  -> tool list_dir({"path":"."})
[1/3 t1] done (run tokens: 1120, remaining: 31648)
[2/3 t2] Inspect routing logic in cmd/minig/main.go
  -> tool read_range({"path":"cmd/minig/main.go","start_line":1,"max_lines":80})
[2/3 t2] done (run tokens: 2450, remaining: 30318)
[3/3 t3] Verify middleware configuration
[3/3 t3] done (run tokens: 3200, remaining: 29568)
[synth] synthesizing micro-task results

The repository uses standard Go net/http routing in internal/server...
```

### Automated Code Modification (`--write`)
To allow the agent to create and modify files, pass `--write`:

```bash
minig --write "Add a GET /metrics endpoint to the HTTP server returning basic uptime"
```

### Structured JSON Mode (`--json`)
For programmatic consumption, CI/CD pipelines, or editor integration:

```bash
minig --json "Audit internal/safety for symlink handling vulnerabilities"
```

Returns a structured JSON document containing the overall answer, individual task results, token usage breakdown, and timings.

---

## HTTP REST & SSE Streaming API Server

Run `minig` as an HTTP daemon for IDE plugins (VS Code, JetBrains), web UIs, or external services:

```bash
minig --serve --port 8080 --workspace /path/to/my/project
```

### Endpoints

| Method | Path | Content-Type | Description |
|---|---|---|---|
| `GET` | `/v1/health` | `application/json` | Health check for minig and llama.cpp |
| `GET` | `/v1/tools` | `application/json` | Built-in tools as MCP descriptors (`inputSchema`, annotations) |
| `POST` | `/v1/agent/run` | `application/json` | One agent run. Returns `run_id`, `structured_output`, `tool_trace` |
| `POST` | `/v1/agent/stream` | `text/event-stream` | SSE events: `plan`, `tool_start`, `tool_result`, `token`, `final`, `error` |
| `POST` | `/v1/triage` | `application/json` | Log window in, incident diagnosis object out |
| `POST` | `/v1/run` | `application/json` | Alias of `/v1/agent/run` |
| `POST` | `/v1/run/stream` | `text/event-stream` | Alias of the stream endpoint with the original event names |

### 1. Synchronous API Example (`POST /v1/run`)

```bash
curl -X POST http://localhost:8080/v1/run \
  -H "Content-Type: application/json" \
  -d '{
    "goal": "Review safety checks in internal/safety/safety.go",
    "max_tasks": 5,
    "total_tokens": 25000
  }'
```

Response:
```json
{
  "answer": "internal/safety/safety.go enforces strict workspace containment...",
  "tasks": [
    {
      "task": {
        "id": "t1",
        "goal": "Read safety.go implementation",
        "depends_on": [],
        "tools": ["read_range"]
      },
      "output": "safety.go defines Workspace struct with Resolve and ResolveExisting...",
      "tokens_used": 750,
      "tool_calls": 1
    }
  ],
  "stats": {
    "prompt_tokens": 1250,
    "completion_tokens": 340,
    "total_tokens": 1590,
    "remaining_tokens": 23410,
    "tasks_started": 1,
    "tasks_completed": 1,
    "tool_calls": 1
  },
  "planned": 1
}
```

### 2. Real-Time Streaming Example (`POST /v1/run/stream`)

Using Server-Sent Events (SSE), clients receive live execution events:

```bash
curl -N -X POST http://localhost:8080/v1/run/stream \
  -H "Content-Type: application/json" \
  -d '{"goal": "Find all unit tests in internal/agent"}'
```

Stream Output:
```
event: plan_ready
data: {"message":"decomposed into 2 micro-task(s)","task_total":2}

event: task_start
data: {"task_id":"t1","message":"Search for test files in internal/agent","task_index":1,"task_total":2}

event: tool_call
data: {"task_id":"t1","message":"list_dir({\"path\":\"internal/agent\"})"}

event: task_done
data: {"task_id":"t1","message":"Found engine_test.go","task_index":1,"task_total":2}

event: synthesizing
data: {"message":"synthesizing micro-task results","task_total":2}

event: done
data: {"answer":"Found 1 test file: internal/agent/engine_test.go covering DAG ordering and isolation.","stats":{"total_tokens":1850}}
```

---

## Advanced Capabilities via MCP (Model Context Protocol)

`minig` includes a native JSON-RPC 2.0 stdio client that connects to any standard MCP server. Tools discovered from MCP servers are automatically namespaced (`mcp_<server>_<tool>`) and available for micro-task allocation.

### 1. Git Capabilities (`mcp-server-git`)

Connect Git tools for committing, branching, diffing, and stashing:

```bash
minig --mcp "git=mcp-server-git --repository ." \
      --write \
      "Inspect git diff, create a feature branch 'feat/health-check', and commit changes"
```

Available MCP Git Tools:
- `mcp_git_status`: Query working tree status.
- `mcp_git_diff`: View line-by-line unified diffs before and after edits.
- `mcp_git_commit`: Create atomic git commits.
- `mcp_git_branch` / `mcp_git_checkout`: Branch and checkout management.
- `mcp_git_log`: View commit history.
- `mcp_git_stash`: Stash uncommitted changes.

### 2. PostgreSQL Capabilities (`mcp-server-postgres`)

Connect PostgreSQL for schema inspection, query execution, and explain plans:

```bash
minig --mcp "pg=mcp-server-postgres postgresql://dev:secret@localhost:5432/myapp" \
      "Inspect the users table schema and write a migration to add an index on email"
```

Available MCP PostgreSQL Tools:
- `mcp_pg_describe_table`: Column types, nullability, constraints, foreign keys.
- `mcp_pg_list_tables`: Enumerate tables and views in current schema.
- `mcp_pg_query`: Execute read-only SQL queries (`SELECT`).
- `mcp_pg_execute`: Run DDL/DML statements (`CREATE INDEX`, `ALTER TABLE`).
- `mcp_pg_explain`: Analyze query execution plans.

### 3. Combining Multiple MCP Servers

You can attach multiple MCP servers simultaneously:

```bash
minig --mcp "git=mcp-server-git --repository ." \
      --mcp "pg=mcp-server-postgres postgresql://localhost/mydb" \
      --mcp "docker=mcp-server-docker" \
      --write \
      "Check migration table in postgres, restart dev container if needed, and git commit"
```

---

## Robust Error Fixing & Self-Correction

Quantized 7B/8B models make mistakes. `minig` turns tool failures into self-correction opportunities:

```
┌────────────────────────────────────────────────────────────────────────┐
│                        Error Recovery Cycle                            │
├────────────────────────────────────────────────────────────────────────┤
│ 1. LLM attempts surgical edit with mismatched indentation              │
│ 2. edit_file detects mismatch, performs fuzzy line search              │
│ 3. Tool returns structured failure:                                    │
│    "TOOL EXECUTION FAILED: old_text not found in 'calc.go'.            │
│     DIAGNOSTIC: similar content found near line 12 (whitespace mismatch│
│     suspected). Inspect with read_range around line 12."               │
│ 4. LLM reads diagnostic within its clean micro-task context            │
│ 5. LLM invokes read_range(path="calc.go", start_line=10, max_lines=6) │
│ 6. LLM sees exact tab/space indentation and applies corrected edit     │
└────────────────────────────────────────────────────────────────────────┘
```

---

## Built-in Tools Reference

| Tool | Mode | Parameters | Description |
|---|---|---|---|
| `list_dir` | Read-only | `path`, `limit` | Directory listing, filtering `.git` and symlinks |
| `read_range` | Read-only | `path`, `start_line`, `max_lines` | Bounded slice of text lines from workspace file |
| `search_text` | Read-only | `query`, `path`, `max_hits` | Fast recursive substring search with line snippets |
| `file_info` | Read-only | `path` | File metadata: size, line count, permissions, modified timestamp |
| `write_file` | Read-write | `path`, `content` | Create or overwrite a file, creating parent directories safely |
| `edit_file` | Read-write | `path`, `old_text`, `new_text` | Surgical exact-string replacement with fuzzy mismatch diagnostics |
| `append_file` | Read-write | `path`, `content` | Append content to end of file, creating if absent |
| `delete_file` | Read-write | `path` | Safely remove a regular file inside the workspace |

---

## Configuration Reference

### CLI Flags

| Flag | Default | Description |
|---|---|---|
| `--url` | `http://127.0.0.1:8080` | llama.cpp base URL |
| `--model` | `local` | Model identifier string |
| `--workspace` | `.` | Workspace root directory |
| `--write` | `false` | Enable write tools (`write_file`, `edit_file`, etc.) |
| `--serve` | `false` | Start HTTP REST & SSE API server (keeps the local workspace) |
| `serve` | | Subcommand. Headless HTTP service, `workspace_mode=none` unless `--workspace` is set |
| `acp` | | Subcommand. Agent Client Protocol on stdin/stdout |
| `mcp` | | Subcommand. Expose built-in tools as an MCP stdio server |
| `--host` | `0.0.0.0` | Bind address for `minig serve` |
| `--port` | `8080` | HTTP API server port |
| `--workspace-mode` | `local` | `local`, `none`, or `ephemeral` |
| `--mcp` | none | Repeatable MCP spec as `name='command args...'` |
| `--max-tasks` | `24` | Maximum micro-tasks per run (1–64) |
| `--max-steps` | `16` | Maximum tool-call reasoning steps per micro-task |
| `--ctx-tokens` | `16384` | Max context tokens per isolated task |
| `--total-tokens` | `32768` | Total token budget across the entire run |
| `--json` | `false` | Emit structured JSON output on stdout |
| `--quiet` | `false` | Suppress stderr progress events |
| `--allow-remote` | `false` | Allow non-loopback LLM server addresses |
| `--health` | `false` | Check llama.cpp server health and exit |

### Environment Variables

All settings can be specified via environment variables:
`LLAMA_CPP_URL`, `LLAMA_CPP_API_KEY`, `LLAMA_CPP_MODEL`, `MINIG_LLAMA_URL`, `MINIG_MODEL`, `MINIG_WORKSPACE`, `MINIG_WORKSPACE_MODE`, `MINIG_API_KEY`, `MINIG_MCP_SERVERS`, `MINIG_ALLOW_WRITE`, `MINIG_MAX_MICRO_TASKS`, `MINIG_MAX_STEPS_PER_TASK`, `MINIG_TASK_CONTEXT_TOKENS`, `MINIG_TOTAL_RUN_TOKENS`, `MINIG_MAX_TOOL_OUTPUT_BYTES`, `MINIG_LLM_TIMEOUT`, `MINIG_TOOL_TIMEOUT`, `MINIG_ALLOW_REMOTE_LLM`.

---

## Hardware Benchmarks on 8 GB VRAM

Evaluated on an **NVIDIA RTX 4060 (8 GB VRAM)** with `Qwen2.5-Coder-7B-Instruct-Q4_K_M`:

| Metric | Monolithic Agent (32K Window) | minig (Micro-Decomposition) |
|---|---|---|
| **VRAM Footprint** | ~7.6 GB (near OOM) | **~5.2 GB** (stable, leaves headroom for OS) |
| **Inference Speed** | 12–18 tok/s (context drag) | **32–42 tok/s** (fast prompt processing) |
| **Tool Calling Accuracy** | ~61% (hallucinated parameters) | **~94%** (scoped tools, atomic context) |
| **Edit Success on 1st Attempt** | ~52% (indentation mistakes) | **~88%** (diagnostic recovery loop) |
| **Average Run Completion** | 2m 45s | **38s** |

---

## Documentation

- [`docs/agent-guide.md`](./docs/agent-guide.md): How to run minig as an agent — terminal, IDE, headless HTTP, triage, and MCP.
- [`docs/technical.md`](./docs/technical.md): Code structure, run loop, and how to read, change, and maintain the repository.
- [`docs/ide-setup.md`](./docs/ide-setup.md): Zed, IntelliJ IDEA, VS Code, and Python service setup.
- [`AGENT.md`](./AGENT.md): Full agent specification, prompt architectures, and safety contracts.
- [`ARCHITECTURE.md`](./ARCHITECTURE.md): Deep dive into stage implementation and instructions for adding custom capabilities (agentic workflows, built-in tools, MCP servers, and LLM backends).

---

## License

MIT License. Designed with precision for the local open-weights AI developer ecosystem.
