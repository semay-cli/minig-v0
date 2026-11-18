# Using minig as an agent

`minig` is one local binary. It plans a goal into small tasks, calls tools, and answers from a `llama.cpp` server (OpenAI-compatible `/v1/chat/completions`). You do not start a separate MCP process for the built-in tools. They run inside `minig`.

Pick the interface that matches who is asking:

| You want to… | Run | Workspace |
|---|---|---|
| Ask a question in the terminal about this repo | `minig "…"` | `local`, read-only |
| Let it edit files in this repo | `minig --write "…"` | `local`, writes on |
| Use it inside Zed, IntelliJ IDEA, or VS Code | `minig acp` | `local`, writes on |
| Let a Python service send logs and get JSON back | `minig serve` | `none` (no project directory) |
| Expose the same tools to another MCP client | `minig mcp` | `local`, writes on |
| Keep a project-aware HTTP API for scripts | `minig --serve --workspace …` | `local` |

`llama-server` and `minig serve` both default to port `8080`. Run the model on `8080` and the minig HTTP API on another port, such as `8090`.

---

## 1. Install

Requirements:

- Go 1.22 or newer to build
- A running `llama.cpp` server (`llama-server`), or any OpenAI-compatible `/v1/chat/completions` endpoint
- `python3` only if the agent will use `sandbox_python`
- `git` and `rg` (ripgrep) are optional. `workspace_code` uses them when they are on `PATH` and falls back when they are not

```bash
git clone https://github.com/semay-cli/minig-v0.git
cd minig-v0
make build
export PATH="$PWD/bin:$PATH"
minig --version
```

Start the model. On an 8 GB GPU a 7B–8B Q4 model with a 16K context is the intended size:

```bash
./llama-server \
  --model models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  --ctx-size 16384 \
  --n-gpu-layers 33 \
  --host 127.0.0.1 \
  --port 8080
```

Check that minig can see it:

```bash
minig --url http://127.0.0.1:8080 --health
# healthy (http://127.0.0.1:8080)
```

If the model is not on localhost, add `--allow-remote` or set `MINIG_ALLOW_REMOTE_LLM=true`. A per-request URL on `POST /v1/agent/run` is allowed even when the server itself was started against localhost.

---

## 2. Where settings come from

Highest priority wins.

1. **This HTTP call** (serve mode only): headers `X-LLM-Base-URL`, `X-LLM-API-Key`, `X-LLM-Model`, or the JSON field `llm_config`.
2. **Process environment**, including the `env` block in an IDE config: `LLAMA_CPP_URL`, `LLAMA_CPP_API_KEY`, `LLAMA_CPP_MODEL`. The `MINIG_*` variables below override those when both are set.
3. **Config file** `~/.config/minig/config.json`.
4. **Built-in defaults**: model URL `http://127.0.0.1:8080`, model name `local`, workspace `.`, mode `local`, writes off.

Example config file:

```json
{
  "llama_url": "http://127.0.0.1:8080",
  "llama_api_key": "",
  "model": "local",
  "workspace_mode": "local",
  "allow_write": false,
  "allow_remote_llm": false,
  "server_host": "127.0.0.1",
  "server_port": 8090,
  "api_key": "",
  "max_micro_tasks": 24,
  "max_steps_per_task": 16,
  "task_context_tokens": 16384,
  "total_run_tokens": 32768,
  "temperature": 0.1,
  "max_retries": 2
}
```

Leave `llama_api_key` empty for a local `llama-server` with no token. minig then omits the `Authorization` header. When the key is set, every model request sends `Authorization: Bearer <token>`.

| Environment variable | Flag | Meaning |
|---|---|---|
| `LLAMA_CPP_URL`, `MINIG_LLAMA_URL` | `--url` | Model base URL |
| `LLAMA_CPP_API_KEY`, `MINIG_LLAMA_API_KEY` | | Bearer token for llama.cpp |
| `LLAMA_CPP_MODEL`, `MINIG_MODEL` | `--model` | Model name sent in the chat request |
| `MINIG_WORKSPACE` | `--workspace` | Project root for `local` mode |
| `MINIG_WORKSPACE_MODE` | `--workspace-mode` | `local`, `none`, or `ephemeral` |
| `MINIG_ALLOW_WRITE` | `--write` | Allow file edits and `workspace_terminal` |
| `MINIG_ALLOW_REMOTE_LLM` | `--allow-remote` | Allow a non-loopback model URL |
| `MINIG_API_KEY` | | Bearer token callers must send to `minig serve` |
| `MINIG_SERVER_HOST` | `--host` | Bind address for the HTTP API |
| `MINIG_SERVER_PORT` | `--port` | HTTP port (default 8080) |
| `MINIG_MAX_MICRO_TASKS` | `--max-tasks` | Cap on planned tasks (1–64, default 24) |
| `MINIG_MAX_STEPS_PER_TASK` | `--max-steps` | Tool turns inside one task (1–16, default 16) |
| `MINIG_TASK_CONTEXT_TOKENS` | `--ctx-tokens` | Context budget for one task |
| `MINIG_TOTAL_RUN_TOKENS` | `--total-tokens` | Token budget for the whole run |
| `MINIG_MCP_SERVERS` | `--mcp` | Extra external MCP servers, separated by `;` |

`--health` checks the model and exits. `--json` prints the run as JSON on stdout. `--quiet` hides the progress lines on stderr. `--version` prints `minig v0.1.0`.

---

## 3. What a run does

Every goal, whether it comes from the terminal, an IDE, or HTTP, follows the same loop:

1. **Plan.** The model splits the goal into a small directed graph of tasks. Each task names the few tools it needs. If the model returns junk, minig runs the whole goal as one task.
2. **Execute.** Each task starts with a fresh prompt: the goal, short notes from the tasks it depends on, and only those tool schemas. The model calls one tool, reads the result, and continues until it answers or hits the step limit.
3. **Recover.** A failed tool call is sent back as the next tool result, with a short hint (bad edit, missing file, HTTP 4xx/5xx, Python exception). The model can retry up to `max_retries` (default 2). After that it is told to stop retrying and summarize.
4. **Fit the context.** Huge log-like tool output is compressed with the same normalizer as `log_template_miner` before it is shown to the model.
5. **Answer.** One task returns its own answer. Several tasks are summarized into one answer. If you passed `response_schema`, the answer is checked against that JSON Schema and reformatted once if it does not match. Read `structured_output` for the object.

Progress on the terminal looks like this (stderr). The answer is the only thing on stdout, unless you passed `--json`.

```text
[plan] decomposed into 2 micro-task(s)
[1/2 t1] Find the router
  -> tool search_text({"query":"HandleFunc"})
[1/2 t1] done (run tokens: 900, remaining: 31868)
[2/2 t2] Read the match
[synth] synthesizing micro-task results
```

---

## 4. Scenario: terminal coding agent

Use this when you are in a project checkout and you want a one-shot answer.

### Read the repo, change nothing

Writes are off unless you pass `--write`. `write_file`, `edit_file`, `append_file`, `delete_file`, and `workspace_terminal` are not offered to the model.

```bash
cd /path/to/your/project
minig "Summarize how HTTP routes are registered"
```

Point at another checkout or another model without editing the config file:

```bash
minig \
  --url http://127.0.0.1:8080 \
  --model qwen2.5-coder \
  --workspace /path/to/your/project \
  "Where is the workspace path check?"
```

### Edit the repo

```bash
minig --write "Add a one-line comment above NewServer explaining the HTTP API"
```

Edits stay inside `--workspace`. Paths that escape the root, and symlinks that would, are rejected. `edit_file` only replaces a snippet that occurs once. If indentation does not match, the tool names a nearby line and the model can read that range and try again.

### Machine-readable output

```bash
minig --json --quiet "List the exported functions in internal/safety"
```

stdout is a single JSON object: `answer`, `tasks`, `stats`, `planned`. stderr stays empty because of `--quiet`.

### Throwaway directory

`ephemeral` creates `/tmp/minig-run-<id>`, uses that as the workspace, and deletes it when the process exits. Your real checkout is not modified.

```bash
minig --workspace-mode ephemeral "Draft a small Go function that formats a duration, and write it to draft.go"
```

### Investigate without a checkout

`none` does not open the current directory and does not register file or git tools. This is the same tool set as `minig serve`.

```bash
minig --workspace-mode none \
  "Compress these logs and state the most common error: ERROR disk full"$'\n'"ERROR disk full"$'\n'"WARN retry"
```

---

## 5. Scenario: IDE agent (`minig acp`)

The editor starts `minig acp` and speaks JSON-RPC 2.0 over stdin and stdout. Do not type this command in a terminal and expect a chat UI. The IDE is the UI.

Supported methods: `initialize`, `session/new`, `session/prompt`, `session/cancel`. While a prompt runs, minig sends `session/update` notifications (plan, thoughts, tool calls, text chunks).

When the editor advertises the capability, minig does not touch the file itself:

- `fs/read_text_file` for `read_range`
- `fs/write_text_file` for `write_file` and `edit_file`
- `terminal/create` for `workspace_terminal`

The diff and the permission prompt then belong to the editor. `minig acp` turns writes on so those tools exist. Set `MINIG_ALLOW_WRITE=false` in the IDE `env` block if you want a read-only session.

Each developer sets their own model in the IDE config. Restarting minig is not required when you change that file and start a new chat. Ready-made fragments live in [`configs/ide/`](../configs/ide/).

### Zed

Merge [`configs/ide/zed.settings.json`](../configs/ide/zed.settings.json) into `~/.config/zed/settings.json`:

```json
{
  "agent_servers": {
    "minig": {
      "type": "custom",
      "command": "minig",
      "args": ["acp"],
      "env": {
        "LLAMA_CPP_URL": "http://127.0.0.1:8080",
        "LLAMA_CPP_API_KEY": "",
        "LLAMA_CPP_MODEL": "local"
      }
    }
  }
}
```

`command` can be an absolute path if `minig` is not on the PATH Zed sees.

### IntelliJ IDEA and other JetBrains IDEs

Copy [`configs/ide/jetbrains.acp.json`](../configs/ide/jetbrains.acp.json) to `~/.jetbrains/acp.json`. `command` must be the absolute binary path:

```json
{
  "agent_servers": {
    "minig": {
      "command": "/home/you/go/bin/minig",
      "args": ["acp"],
      "env": {
        "LLAMA_CPP_URL": "http://127.0.0.1:8080",
        "LLAMA_CPP_API_KEY": "",
        "LLAMA_CPP_MODEL": "local"
      }
    }
  }
}
```

Open the project, start an ACP agent session named `minig`, and prompt as usual. The session `cwd` is the project the IDE sends in `session/new`.

### VS Code

Merge [`configs/ide/vscode.settings.json`](../configs/ide/vscode.settings.json) into your user or workspace `settings.json`. This matches an ACP client that reads `acp.agentServers`:

```json
{
  "acp.agentServers": {
    "minig": {
      "command": "minig",
      "args": ["acp"],
      "env": {
        "LLAMA_CPP_URL": "http://127.0.0.1:8080",
        "LLAMA_CPP_API_KEY": "",
        "LLAMA_CPP_MODEL": "local"
      }
    }
  }
}
```

Use the absolute path in `command` when the VS Code process does not inherit your shell PATH.

### Shared model, different tokens

Two people can point at the same `llama-server` and still use different API keys. Put the key only in that user's IDE `env`. An empty `LLAMA_CPP_API_KEY` means "do not send a bearer token".

---

## 6. Scenario: headless service for Python (`minig serve`)

Use this when another program, not a person, owns the loop: anomaly watchers, log tailers, ticket bots.

```bash
export LLAMA_CPP_URL="http://127.0.0.1:8080"
export LLAMA_CPP_MODEL="local"
export MINIG_API_KEY="change-me"     # omit to leave the API open
minig serve --host 127.0.0.1 --port 8090
```

`minig serve` sets `workspace_mode=none` unless you also pass `--workspace` or `--workspace-mode`. It does not read `.git` and it does not register `write_file` or the other project tools. `GET /v1/health` stays open even when `MINIG_API_KEY` is set. Every other route then requires `Authorization: Bearer change-me`.

Bind `0.0.0.0` only on a network you trust. The default host in config is `0.0.0.0`. Prefer `127.0.0.1` on a laptop.

### Confirm it is up

```bash
curl -s http://127.0.0.1:8090/v1/health
curl -s http://127.0.0.1:8090/v1/tools | head
```

`/v1/health` reports `status`, the configured model URL, and `workspace_mode`. `/v1/tools` lists each built-in tool with an MCP `inputSchema`.

### One-shot diagnosis

`prompt` and `goal` are the same field. `context_data` is any JSON object. It is appended to the prompt and, when the model runs `sandbox_python` without its own payload, written to `context.json` inside the scratch directory.

```bash
curl -s http://127.0.0.1:8090/v1/agent/run \
  -H "Authorization: Bearer change-me" \
  -H "Content-Type: application/json" \
  -H "X-LLM-Base-URL: http://127.0.0.1:8080" \
  -H "X-LLM-API-Key: llama-token" \
  -H "X-LLM-Model: qwen2.5-coder" \
  -d '{
    "prompt": "Decide if this spike should page someone.",
    "workspace_mode": "none",
    "allowed_tools": ["log_template_miner", "sandbox_python"],
    "context_data": {
      "service": "checkout",
      "logs": "ERROR pool exhausted\nERROR pool exhausted\nWARN retry"
    },
    "response_schema": {
      "type": "object",
      "required": ["severity", "root_cause", "should_alert"],
      "properties": {
        "severity": {"type": "string", "enum": ["critical", "high", "medium", "low", "info"]},
        "root_cause": {"type": "string"},
        "should_alert": {"type": "boolean"},
        "recommended_actions": {"type": "array", "items": {"type": "string"}}
      }
    },
    "llm_config": {"temperature": 0.1, "max_tokens": 800}
  }'
```

Response shape:

```json
{
  "run_id": "run-…",
  "status": "completed",
  "output": "…prose the model wrote…",
  "answer": "…same text, kept for older clients…",
  "structured_output": {
    "severity": "high",
    "root_cause": "connection pool exhausted",
    "should_alert": true
  },
  "tool_trace": [
    {"task_id": "t1", "tool": "log_template_miner", "args": "…", "output": "…", "error": ""}
  ],
  "usage": {"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "tool_calls": 1},
  "duration_ms": 1200
}
```

Trust `structured_output` when you passed `response_schema`. The Python caller should branch on `should_alert` and ignore the prose.

`llm_config` fields are `base_url`, `api_key`, `model`, `temperature`, `max_tokens`. Headers override the body. The body overrides the process environment.

`allowed_tools` is optional. When it is set, tools outside the list are not registered for that call. Unknown names are ignored. An empty match is a 400.

`workspace_mode` on the request:

| Value | What that call can touch |
|---|---|
| `none` | HTTP, logs, Python sandbox, web, alerts, scratchpad. No project files. |
| `ephemeral` | Same tools plus file tools inside a new `/tmp/minig-run-<id>` that is deleted when the response finishes. |
| `local` | The workspace the server was started with. A `minig serve` that has no workspace rejects this. |

`POST /v1/run` is the same handler as `/v1/agent/run`.

### Stream the steps

`POST /v1/agent/stream` is `text/event-stream`. Events: `plan`, `tool_start`, `tool_result`, `token`, `final`, `error`. `final` is the same object as `/v1/agent/run`.

```bash
curl -N http://127.0.0.1:8090/v1/agent/stream \
  -H "Authorization: Bearer change-me" \
  -H "Content-Type: application/json" \
  -d '{"prompt":"Summarize the log line: ERROR disk full","workspace_mode":"none"}'
```

`POST /v1/run/stream` is the older stream. Its event names stay `plan_ready`, `task_start`, `tool_call`, `task_done`, and `done`.

### Incident shortcut

`POST /v1/triage` builds the prompt for you and always runs with `workspace_mode=none`. If you omit `response_schema`, minig asks for `severity`, `root_cause`, `recommended_actions`, `affected_components`, `should_alert`, and `alert_summary`.

```bash
curl -s http://127.0.0.1:8090/v1/triage \
  -H "Authorization: Bearer change-me" \
  -H "Content-Type: application/json" \
  -d '{
    "service_name": "checkout",
    "anomaly_window": "5m",
    "logs": "ERROR pool exhausted",
    "metrics": {"error_ratio": 0.18, "p99_ms": 2400},
    "alert_webhook_url": "https://hooks.example/alerts"
  }'
```

The model may call `alert_dispatch` when `alert_webhook_url` is set and it judges the severity high or critical. Your service can also ignore that and page from `structured_output` itself. Doing it in the caller is the more predictable option.

### Python client

[`examples/python_sre_client.py`](../examples/python_sre_client.py) calls both `/v1/agent/run` and `/v1/triage`.

```bash
export MINIG_URL="http://127.0.0.1:8090"
export MINIG_API_KEY="change-me"
export LLAMA_CPP_URL="http://127.0.0.1:8080"
export LLAMA_CPP_API_KEY=""          # omit or leave empty if llama-server has no token
export LLAMA_CPP_MODEL="local"
python3 examples/python_sre_client.py --service checkout
```

The script prints `should_alert`, `severity`, and `root_cause`. A watcher would open a ticket when `should_alert` is true.

### Project-aware HTTP API

`--serve` (the flag, not the `serve` subcommand) keeps `workspace_mode=local` and the `--workspace` you pass. Use this when a script should ask questions about a real checkout. Add `--write` if that script may edit files.

```bash
minig --serve --host 127.0.0.1 --port 8090 \
  --workspace /path/to/your/project \
  --write \
  --url http://127.0.0.1:8080
```

```bash
curl -s http://127.0.0.1:8090/v1/agent/run \
  -H "Content-Type: application/json" \
  -d '{"goal":"What does safety.NewWorkspace reject?","max_tasks":4}'
```

`minig serve --workspace /path/to/your/project` also keeps that directory, because passing `--workspace` cancels the headless default.

---

## 7. Scenario: built-in tools over MCP (`minig mcp`)

`minig mcp` is an MCP server on stdin/stdout (`initialize`, `notifications/initialized`, `tools/list`, `tools/call`). Another agent, or `minig --mcp`, can list and call the same tools. Writes are on unless `MINIG_ALLOW_WRITE=false`.

```bash
minig mcp --workspace /path/to/your/project
```

A client session looks like this (one JSON object per line):

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{}}}
```

```json
{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}
```

```json
{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"log_template_miner","arguments":{"logs":"ERROR a\nERROR a\nWARN b"}}}
```

To attach a different MCP server *into* a minig run (git, Postgres, and so on), minig is the client:

```bash
minig --mcp "git=mcp-server-git --repository ." \
      --write \
      "Show git status and summarize the dirty files"
```

Those remote tools are registered as `mcp_<server>_<tool>`. Repeat `--mcp` for more servers. `MINIG_MCP_SERVERS` is the same list, separated by `;`.

---

## 8. Tools

Headless tools are always available in `none` and `ephemeral` mode, and also in a local session.

| Tool | Use it for |
|---|---|
| `observability_http` | `GET`/`POST`/`PUT` with headers, bearer or basic auth, query params, a `loki` or `prometheus` path helper, JSONPath, or a regex extract. HTTP 4xx/5xx is fed back so the model can fix the query. |
| `log_template_miner` | Turn raw logs into `<UUID>`, `<IP>`, `<NUM>`, `<TIMESTAMP>` templates with counts and a few stack traces. |
| `sandbox_python` | Run a Python script in `/tmp/minig-run-<id>`. `context_data` is `context.json`. The directory is removed when the script finishes. |
| `alert_dispatch` | POST a Slack, PagerDuty, Opsgenie, or custom webhook. Requires `webhook_url` and `summary`. |
| `web_fetch` | Download a URL and strip HTML to text. |
| `doc_extract` | Same idea for a runbook or OpenAPI doc, or for HTML you already have in `content`. |
| `session_scratchpad` | `set` / `get` / `list` / `delete` keys for this run only. Dropped when the HTTP request or process ends. |

Local tools, only when `workspace_mode` is `local` or `ephemeral`:

| Tool | When it is registered | Use it for |
|---|---|---|
| `list_dir` | always | List a directory inside the workspace |
| `read_range` | always | Read a line slice (default 80 lines, max 200) |
| `search_text` | always | Case-insensitive substring search |
| `file_info` | always | Size, line count, mode, mtime |
| `workspace_code` | always | `search` (ripgrep or a walker), `symbols`, `git_diff`, `git_log`, `read`. `patch` only if writes are on |
| `write_file`, `edit_file`, `append_file`, `delete_file` | `--write`, or `acp` / `mcp` | Create, patch, append, or remove a file inside the workspace |
| `workspace_terminal` | same as writes | Run a shell command with the workspace as cwd. Commands such as `rm -rf /` are refused |

`acp` routes the file and terminal tools through the IDE when the IDE says it can handle them.

---

## 9. Limits and safety

Defaults are aimed at a 7B model on a 16K context:

- 24 tasks per run, 16 tool steps per task
- 16,384 tokens of context per task, 32,768 tokens for the whole run
- Tool output capped (default 8 KiB) and then truncated again to fit the task window
- Model HTTP timeout 45s, tool timeout 10s
- Temperature 0.1

Lower `--ctx-tokens` if the model was started with a smaller `--ctx-size`. A prompt that does not fit fails that task instead of overflowing the GPU context.

File tools resolve every path under the workspace root and reject symlink escapes. `none` never opens that root. Python and ephemeral work use a private directory under the system temp dir and delete it afterward.

`MINIG_API_KEY` protects minig's own HTTP API. `LLAMA_CPP_API_KEY` protects the model server. They are different secrets.

---

## 10. When something fails

| What you see | What to change |
|---|---|
| `llama URL must use localhost` | The URL is not loopback. Pass `--allow-remote`, or use `127.0.0.1`. |
| `address already in use` on serve | `llama-server` is already on that port. Start minig with `--port 8090`. |
| HTTP 401 from minig | Send `Authorization: Bearer` with the same value as `MINIG_API_KEY`. `/v1/health` does not need it. |
| `goal or prompt is required` | The JSON body needs `prompt` or `goal`. |
| `this server has no local workspace` | The request asked for `workspace_mode=local` but the process was started with `minig serve` and no `--workspace`. |
| `allowed_tools matched no registered tools` | The names are not in this mode. `write_file` does not exist in `none`. |
| `structured_output` missing | The model never produced JSON that matched `response_schema`. Read `output` and the `error` field, then tighten the prompt or schema. |
| Tool trace `error` on `sandbox_python` | `python3` is missing, or the script exited non-zero. The stderr is in the error string. |
| IDE session never answers | `LLAMA_CPP_URL` in the IDE `env` is wrong, or `minig` is not on the PATH the editor uses. Run `minig --url … --health` with the same URL. |
| Edits do not appear in `minig acp` | The IDE client did not advertise `fs.writeTextFile`, or it denied the write. Check the editor permission prompt. |

---

## Related pages

- [`technical.md`](./technical.md) — package map, run loop, and how to change the code
- [`ide-setup.md`](./ide-setup.md) — the IDE and `curl` snippets alone
- [`../examples/python_sre_client.py`](../examples/python_sre_client.py) — a runnable Python caller
- [`../README.md`](../README.md) — build, flags, and the HTTP examples
- [`../AGENT.md`](../AGENT.md) — prompt and safety design
- [`../ARCHITECTURE.md`](../ARCHITECTURE.md) — how to add a tool or a model backend
