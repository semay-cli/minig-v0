# Technical documentation

This is the map of the `minig` codebase: how a run moves through the process, which package owns which decision, and how to change the code without breaking the two modes (local agent and headless service). Operator setup lives in [`agent-guide.md`](./agent-guide.md). Prompt wording and the original safety contract live in [`../AGENT.md`](../AGENT.md).

The module path is `github.com/semay-cli/minig-v0`. The Go version in `go.mod` is the language version this tree builds with. There are no third-party Go dependencies. Everything under `internal/` is unreachable from other modules.

---

## 1. How to read the code

Read in this order. Each step is one package and answers one question.

1. `cmd/minig/main.go` — which command was typed, and which objects get built before any model call.
2. `internal/config/config.go` — which knobs exist and which values are illegal.
3. `internal/agent/types.go`, then `engine.go`, then `planner.go` — the only place a goal becomes tool calls and a final answer.
4. `internal/tool/tool.go` — the `Tool` interface and the registry that times out and truncates every call.
5. `internal/tool/builtin/builtin.go` — `RegisterHeadless` and `RegisterAll`, the switch between “no project” and “this checkout”.
6. `internal/llm/llm.go` and `internal/llm/llamacpp/client.go` — the HTTP contract with `llama-server`.
7. `internal/server/server.go` — one HTTP request becomes one `Engine.RunWithConfig`.
8. `internal/acp/server.go` and `internal/mcpserver/server.go` — the two stdin/stdout protocols.
9. `internal/safety/safety.go` and `sandbox.go` — path containment and `/tmp/minig-run-<id>`.

Tests sit next to the file they lock down. When a comment and a test disagree, trust the test and the function it calls.

Dependency direction is one way:

```text
cmd/minig
  → acp, mcpserver, server, agent, config, llm/llamacpp, tool, tool/builtin, tool/mcp, safety, ui

server, acp
  → agent, config, llm, tool, tool/builtin, safety

agent
  → budget, config, jsonschema, llm, textutil, tool, tool/builtin

tool/builtin
  → safety, textutil, tool

tool, tool/mcp
  → llm, textutil

llm/llamacpp
  → llm, textutil
```

`agent` imports `tool/builtin` only for `CompressLogs`, `WithRunContextData`, and the IDE delegate context key. New agent logic should stay on the `tool.Tool` interface. Do not import `server`, `acp`, or `cmd` from `internal/`.

---

## 2. Repository map

```text
minig-v0/
├── cmd/minig/                 # process entry: main.go, CLI tests, dual-mode tests
├── internal/
│   ├── agent/                 # plan → DAG → isolated tasks → synthesis
│   ├── budget/                # run-wide token ledger and per-task step counter
│   ├── config/                # defaults, file, environment, Validate
│   ├── jsonschema/            # subset of JSON Schema used on structured_output
│   ├── llm/                   # Provider, Message, ToolCall; mock.go for tests
│   │   └── llamacpp/          # /v1/chat/completions, /health, /props, tool-call fallback
│   ├── safety/                # Workspace path jail; Sandbox temp dirs
│   ├── server/                # HTTP routes, auth, triage, per-request LLM override
│   ├── acp/                   # Agent Client Protocol JSON-RPC on stdin/stdout
│   ├── mcpserver/             # built-in tools exposed as an MCP server
│   ├── textutil/              # truncation, token estimate, line slice, <think> strip
│   ├── tool/
│   │   ├── tool.go            # Tool interface, Registry, MCP descriptors
│   │   ├── builtin/           # in-process tools
│   │   └── mcp/               # MCP client (minig launches an external server)
│   └── ui/                    # stderr progress and stdout answer / JSON
├── docs/                      # this file, agent-guide.md, ide-setup.md
├── examples/python_sre_client.py
├── configs/ide/               # Zed, JetBrains, VS Code fragments
├── AGENT.md                   # prompt and safety design notes
├── ARCHITECTURE.md            # earlier stage write-up; this file is the current map
└── Makefile
```

| Package | Owns | Does not own |
|---|---|---|
| `main` | flag parse, signal context, which registry to build, process lifetime | task loop, tool implementations |
| `config` | bounds and URL shape | reading files inside a workspace |
| `agent` | planning, task isolation, retries, schema retry, events | HTTP, stdin framing |
| `budget` | counters and the errors the loop treats as fatal | pricing or GPU memory |
| `llm` | chat request/response types | which model binary is installed |
| `llamacpp` | wire format, bearer header, textual tool-call fallback | the agent loop |
| `tool` | allowlist, timeout, output cap, MCP JSON shape | what a tool does |
| `builtin` | each tool’s arguments and side effects | when the model decides to call it |
| `safety` | canonical paths, symlink rejection, temp sandbox | git policy |
| `server` | routes, auth middleware, per-request overrides, SSE names | planning |
| `acp` | IDE session and client RPC (`fs/*`, `terminal/create`) | file parsing of Go |
| `mcpserver` | MCP `initialize`, `tools/list`, `tools/call` | namespacing remote tools |
| `tool/mcp` | spawning and calling an external MCP server | serving MCP |
| `jsonschema` | `type`, `required`, `properties`, `items`, `enum` | the full JSON Schema spec |
| `ui` | human and `--json` rendering | event production |
| `textutil` | byte-safe string cuts | log template mining (`builtin.CompressLogs`) |

---

## 3. Process entry

`main` installs a signal context (`Interrupt`, `SIGTERM`) and calls `run`. Tests call `run` or `runIO` with buffers so they never need the real stdin of the test process.

`runIO` (`cmd/minig/main.go`):

1. `config.LoadFromEnv()`.
2. If the first argument is `serve`, `acp`, or `mcp`, that word is the subcommand and the rest is flags. A goal that starts with those words must be quoted after a flag, or it will be taken as the subcommand.
3. Parse flags. `--help` returns nil.
4. Adjust mode:
   - `serve` with no `--workspace`, no `--workspace-mode`, and no `MINIG_WORKSPACE_MODE` forces `workspace_mode=none` and clears the workspace root.
   - `acp` and `mcp` turn writes on unless `--write` was passed or `MINIG_ALLOW_WRITE` is set.
5. `cfg.Validate()`.
6. Dispatch.

```text
argv
  ├─ mcp  → buildToolRegistry → optional external MCP clients → mcpserver.Serve
  ├─ acp  → llamacpp client → acp.Server.Serve
  ├─ --health → client.CheckHealth, print, exit
  ├─ serve or --serve → buildToolRegistry(forServe=true) → HTTP server until ctx cancel
  └─ goal text → buildToolRegistry → agent.Engine.Run → ui.Renderer
```

`buildToolRegistry`:

- `workspace_mode=none`, or a serve process in `ephemeral` mode: `builtin.RegisterHeadless` only. No `safety.NewWorkspace`, so startup does not stat the current directory.
- `ephemeral` on a one-shot CLI run: `safety.NewSandbox`, then `RegisterAll` rooted at that directory. The cleanup func removes it when `runIO` returns.
- `local`: `safety.NewWorkspace(cfg.WorkspaceRoot)` then `RegisterAll`. Write tools and `workspace_terminal` are added only when `AllowWrite` is true.

`attachExternalMCP` parses each `--mcp` spec (`name=command args`), spawns `tool/mcp.Client`, and registers remote tools as `mcp_<server>_<tool>`. The returned cleanup closes those processes.

The HTTP server listens on `net.JoinHostPort(host, port)`. Shutdown on context cancel uses a 3 second `http.Server.Shutdown`.

---

## 4. Configuration logic

`config.Default` sets the conservative numbers (16 task steps, 24 tasks, 16384 task tokens, 32768 run tokens, 8 KiB tool output, 45s model timeout, 10s tool timeout, temperature 0.1, two retries, writes off, loopback model URL only).

`LoadFromEnv` overlays three layers, later winning:

1. `~/.config/minig/config.json` unmarshaled into `Config`. A missing file is ignored. Invalid JSON is ignored at this step because the error from `json.Unmarshal` is discarded. `Validate` still runs afterward and rejects an empty model or a bad URL.
2. `LLAMA_CPP_*`, then `MINIG_*` (the `MINIG_` name wins when both exist).
3. Flags in `runIO`, applied by `flag` on top of the struct.

`Validate` is the gate every engine and every server request repeats:

- Model URL must be `http` or `https`, with a host, and with no userinfo, query, or fragment. Non-loopback hosts fail until `AllowRemoteLLM` is true. Per-request overrides in the HTTP server call `llamacpp.NewClientWithAuth(..., allowRemote=true)` so a caller can name a remote llama.cpp without flipping the process flag.
- `workspace_mode` is `local`, `none`, or `ephemeral`. `none` may have an empty root. The other modes require a directory that `filepath.Abs` can resolve.
- Numeric bounds are hard: tasks 1–64, steps 1–16, tool output 256–1 MiB, temperature 0–2, retries 0–5, optional `max_completion_tokens` 16–4096 when set.

Do not bypass `Validate` to “just try” a request. The HTTP layer turns a validation error into HTTP 400.

---

## 5. Agent loop

`agent.Engine` holds `config.Config`, an `llm.Provider`, a `tool.Registry`, and an optional `Observer`. `NewEngine` validates the config and rejects a nil provider or registry.

### 5.1 `Run`

```text
goal
  → textutil.CompactWhitespace          empty goal is an error
  → budget.NewLedger
  → Engine.decompose                    one model call, no tool schemas
  → event plan_ready
  → for task in topological order:
        ledger.StartTask
        event task_start
        executeMicroTask                fresh message list
        event task_done
  → if one task: its output is the answer
  → else: event synthesizing, Engine.synthesize
        if synthesis cannot fit the budget: assembleFallbackAnswer (no model call)
```

`RunWithConfig` is the HTTP and schema entry. It appends `context_data` to the goal text, stores that JSON on the context with `builtin.WithRunContextData` (so `sandbox_python` can write `context.json`), and if `allowed_tools` is non-empty replaces the registry with `Registry.Subset`. After `Run`, it concatenates each task’s `ToolTrace` and, when `response_schema` is set, calls `enforceSchema`.

### 5.2 Planning (`planner.go`)

`decompose` sends a system prompt that lists tool **names** only, not JSON schemas. The model must return:

```json
{"tasks":[{"id":"t1","goal":"…","depends_on":[],"tools":["read_range"]}]}
```

`parsePlanJSON` uses `extractJSONObject`, a brace scanner that skips strings, so a `<think>` block or a markdown fence around the object still yields the object. Hallucinated tool names are dropped. Duplicate ids are rewritten. Self-dependencies are dropped. Unknown dependency ids fail in `orderDAG`.

`orderDAG` is Kahn’s algorithm: in-degree from `depends_on`, queue of zeros, emit, decrement. If the emitted count is not the task count, the error is `micro-task plan contains a dependency cycle`.

If parsing fails, the fallback is a single task `t1` whose tool list is every registered name. That is deliberate. A small model that cannot plan still gets one isolated attempt.

### 5.3 One micro-task (`executeMicroTask`)

The prompt contains the overall goal (truncated), the text outputs of `depends_on` tasks only, and the current goal. It does not contain the other tasks’ raw tool transcripts. `Registry.Specs(task.Tools)` is the only schema block sent. `nil` means “all tools”; a non-nil list means “these names, ignore unknown”.

Each loop iteration:

1. `TaskBudget.NextStep`. Past `MaxStepsPerTask` the task returns `ErrMaxTaskStepsExceeded`.
2. `Provider.CountTokens`. `TaskBudget.CheckPrompt` rejects a prompt over the task window.
3. `clampCompletionTokens` leaves room inside `TaskContextTokens` and, when set, `MaxCompletionTokens`.
4. `Provider.Chat` with `tool_choice` equivalent of auto (the llama.cpp client sets that when tools are present).
5. Non-empty assistant text emits `EventToken`.
6. Zero tool calls: compact the text, cap it at `maxArtifactBytes` (1200), return `TaskResult`.
7. Otherwise append the assistant message and run each call through `Registry.Execute(ctx, task.Tools, name, args)`.

Failure handling inside the task, before the text is shown to the model:

- If the tool returned success but the body starts with `HTTP 4xx/5xx` (`observability_http` and the other HTTP-shaped tools), the loop treats it as an error so the model must correct the call.
- `formatToolErrorDiagnostic` adds a short recovery line for edit mismatches, missing files, and SQL-ish errors.
- The same tool name may fail `MaxRetries` times (default 2). The next failure is prefixed `TOOL RETRIES EXHAUSTED` and tells the model to stop calling it the same way.
- Successful output that looks like a long log (`observability_http`, or many lines containing `ERROR` / `Exception`, over 1500 bytes) is replaced by `builtin.CompressLogs` under the heading `COMPRESSED LOG SUMMARY`.
- The result is compacted and cut to the per-tool byte cap (the smaller of `MaxToolOutputBytes` and twice the task token budget).

`EventToolCall` fires before the tool. `EventToolResult` fires after truncation. The `ui.Renderer` prints plan, task, and tool-call lines and ignores token and tool-result events, so the terminal stays short.

### 5.4 Synthesis and schema

`synthesize` sends the goal plus each task’s short output. If token counting fails, the remaining budget cannot hold the prompt, or the completion would be under 64 tokens, `assembleFallbackAnswer` joins the task outputs with no model call. A single-task run never synthesizes.

`enforceSchema` (`jsonschema.Validate`):

- Accept the first balanced JSON object in the answer when it satisfies the schema.
- Otherwise ask the model, at temperature 0, up to `max_retries` times, including the previous validation error in the user message.
- On success the bytes are `RunResult.StructuredOutput`. On failure the prose answer is kept and `StructuredOutput` stays empty. Callers that need a hard failure should check that field; the HTTP handler still returns 200 with `status=completed` when the agent run itself succeeded.

`jsonschema` implements object/array/string/boolean/number/integer/null, `required`, `properties`, `items`, and `enum`. Unknown schema types fail validation. Extra properties are allowed.

### 5.5 Events

`agent.EventKind` values are the in-process names. HTTP SSE renames some of them.

| Kind | SSE on `/v1/agent/stream` | SSE on `/v1/run/stream` | Terminal |
|---|---|---|---|
| `plan_ready` | `plan` | `plan_ready` | `[plan]` |
| `task_start` | `task_start` | `task_start` | `[i/n id]` |
| `tool_call` | `tool_start` | `tool_call` | `-> tool` |
| `tool_result` | `tool_result` | `tool_result` | (not printed) |
| `token` | `token` | `token` | (not printed) |
| `task_done` | `task_done` | `task_done` | `done (tokens)` |
| `synthesizing` | `synthesizing` | `synthesizing` | `[synth]` |

`/v1/agent/stream` ends with `final`. `/v1/run/stream` ends with `done`. Failures after the headers are `error`. ACP maps the same events onto `session/update` (`plan`, `agent_thought_chunk`, `tool_call`, `tool_call_update`, `agent_message_chunk`).

---

## 6. Budget

`budget.Ledger` is mutex-guarded and shared by planning, every task, and synthesis. `StartTask` increments the task count and returns a `TaskBudget` whose step counter is independent. `RecordTokens` adds prompt and completion tokens to the run total. Exceeding `MaxTotalTokens` returns `ErrRunTokenBudgetExceeded`.

`TaskBudget.CheckPrompt` compares the counted prompt to `MaxTaskContextTokens`, not to the remaining run budget. A task can be refused for its own window while the run still has tokens left. `NextStep` is the loop ceiling (`ErrMaxTaskStepsExceeded`).

Token counts prefer llama.cpp `POST /v1/chat/completions/input_tokens`. A 404 or 405 falls back to `estimatePromptTokens` (a character heuristic in the llama.cpp client, and `textutil.EstimateTokens` in the mock). Tests that use `llm.MockProvider` never open a socket. `MockProvider.Responses` is a queue: the next `Chat` pops one scripted response.

---

## 7. Model client

`llm.Provider`:

```go
CheckHealth(ctx) error
CountTokens(ctx, model, messages, tools) (int, error)
Chat(ctx, ChatRequest) (ChatResponse, error)
```

`llamacpp.Client` is built with `NewClient` or `NewClientWithAuth`. Empty API key means the `Authorization` header is not set. A non-empty key sends `Bearer`.

`CheckHealth` tries `GET /health`, then `GET /props` when `/health` is 404.

`Chat` posts `/v1/chat/completions` with `stream: false`. A non-empty `ChatRequest.BaseURL` or `APIKey` overrides the client defaults for that call. Native `tool_calls` are decoded first. If the choice has tools but no native calls, `parseFallbackToolCalls` looks for `<tool_call>{json}</tool_call>` and for fenced JSON of the form `{"name","arguments"}`. `textutil.StripThink` removes `<think>` blocks from the assistant text before that fallback runs.

The HTTP transport disables compression, refuses redirects, and caps response bodies (`MaxResponseBytes`). Timeouts come from `Config.LLMTimeout`.

Adding a backend means a new type that implements `Provider`, constructed in `runIO` (and in `server.prepareExecution` if requests may target it). Keep `agent.Engine` unaware of URLs.

---

## 8. Tools

### 8.1 Interface and registry

```go
type Tool interface {
    Name() string
    Description() string
    Parameters() json.RawMessage // JSON Schema object
    ReadOnly() bool
    Execute(ctx context.Context, rawArgs json.RawMessage) (string, error)
}
```

`Registry.Register` rejects an empty name, a duplicate, and an invalid parameter schema. `Execute`:

- If `allowed` is non-nil, the name must be in it (`nil` allows every registered tool).
- Arguments must be valid JSON. Empty arguments become `{}`.
- The call runs under `context.WithTimeout(ctx, registry.timeout)`.
- A non-nil error drops the tool’s string return. The error text is what the agent sees. Put any stderr you need into the error (see `sandbox_python`).
- Success output is truncated to `maxOutputBytes`.

`MCPDescriptors` is the shape `GET /v1/tools` and `minig mcp` return: `name`, `description`, `inputSchema`, `parameters` (same bytes), and `annotations.readOnlyHint` / `destructiveHint` from `ReadOnly()`.

`Subset` copies pointers to the named tools into a new registry. It is how `allowed_tools` and `RunWithConfig` narrow a call. The tool implementations are shared and must be safe for concurrent runs. Per-run state goes in `context.Context` (`session_scratchpad` session id, `context_data`, IDE delegate), not in package globals except the scratchpad map, which is keyed by that session id and deleted in `CleanupScratchpad`.

### 8.2 Registration

`RegisterHeadless` (`builtin.go`): `observability_http`, `log_template_miner`, `sandbox_python`, `alert_dispatch`, `web_fetch`, `doc_extract`, `session_scratchpad`.

`RegisterAll` calls `RegisterHeadless`, then `workspace_code`, `list_dir`, `read_range`, `search_text`, `file_info`. With `allowWrite`: `write_file`, `edit_file`, `append_file`, `delete_file`, `workspace_terminal`.

`workspace_code` actions are `search`, `symbols`, `git_diff`, `git_log`, `read`, and `patch`. `patch` returns an error when `allowWrite` is false even though the tool is registered. Search prefers `rg` and otherwise walks files, skipping `.git`, `node_modules`, and `vendor`.

### 8.3 How to add a built-in tool

1. Add `internal/tool/builtin/<name>.go` with a struct and the five methods. Keep argument structs local to `Execute`. Return `fmt.Errorf("…: %w", err)` for wrapped I/O errors.
2. Register it in `RegisterHeadless` if it must work with no project directory. Register it in `RegisterAll` if it needs `*safety.Workspace`. Gate mutations on `allowWrite`.
3. Bound every output. Reuse `textutil.TruncateBytes`. Do not return unbounded command output.
4. If the tool can be asked to touch the filesystem, resolve paths with `Workspace.Resolve` or `ResolveExisting`. Do not use `filepath.Join` on user input alone.
5. Add a test in `builtin_test.go` or `new_tools_test.go` that runs `Execute` without a model.
6. If `workspace_mode=none` must not see the tool, do not add it to `RegisterHeadless`. The headless test in `cmd/minig/dualmode_test.go` fails if `write_file` appears on `GET /v1/tools` for `minig serve`.

File tools consult `EditorDelegateFrom(ctx)` first. When an ACP session advertised `fs.readTextFile`, `fs.writeTextFile`, or `terminal`, the matching tool calls the IDE and does not read or write the disk itself. `editViaEditor` in `delegate.go` reads the whole file, applies one exact replacement, and writes it back. CLI runs have no delegate, so the same tools use `safety.Workspace`.

### 8.4 External MCP client

`internal/tool/mcp` is the opposite direction of `internal/mcpserver`. `ParseServerSpec` splits `name=command arg…`. `Client` speaks newline JSON-RPC, performs `initialize` and `tools/list`, and `RegisterTools` wraps each remote tool in a `bridgedTool` named `mcp_<server>_<tool>`. `Execute` calls `tools/call` and joins text content blocks. A non-zero `isError` becomes a Go error.

`internal/mcpserver.Serve` is the server side for `minig mcp`. It answers `initialize` (`protocolVersion` `2024-11-05`), `tools/list` from `MCPDescriptors`, `tools/call` via `Registry.Execute`, and `ping`. Notifications (no `id`) are ignored.

---

## 9. Safety and scratch space

`safety.Workspace` stores one canonical root. `NewWorkspace` runs `Abs`, `EvalSymlinks`, and `Stat`, and requires a directory. `Resolve` joins a relative path, rejects `..` that escapes the root, and `Lstat`s each existing segment so a symlink cannot point outside. A missing leaf is allowed so `write_file` can create it. `ResolveExisting` is the read path and fails when the leaf is absent.

`safety.Sandbox` creates `<os.TempDir()>/minig-run-<32 hex chars>` mode `0700`. `WriteFile` rejects absolute names and `..`. `Cleanup` is `RemoveAll` and is safe to call twice. `sandbox_python` creates one sandbox per execution and defers `Cleanup`. The HTTP `ephemeral` workspace mode creates another sandbox for the whole request and registers file tools against it. Both must be gone when the response returns. `TestHeadlessServeStructuredRun` snapshots `minig-run-*` before and after to lock that.

`NewNullWorkspace` exists for callers that need a `Workspace` value without a project. The serve path does not use it. Headless startup simply never calls `NewWorkspace`.

---

## 10. HTTP server

`server.Server` holds the process config, the process-level provider, the process-level registry, and a `ServeMux`. `Handler` wraps the mux with `authMiddleware`. When `Config.APIKey` is empty there is no check. When it is set, every route except `GET /v1/health` requires `Authorization: Bearer <key>` compared as a full string.

Routes:

| Route | Handler |
|---|---|
| `GET /v1/health` | `handleHealth` — provider `CheckHealth` with a 5s timeout |
| `GET /v1/tools` | `handleTools` — `MCPDescriptors` of the process registry |
| `POST /v1/agent/run`, `POST /v1/run` | `handleRun` |
| `POST /v1/agent/stream`, `POST /v1/run/stream` | `handleRunStream` |
| `POST /v1/triage` | `handleTriage` |

`prepareExecution` is the per-request isolate:

1. Require `goal` or `prompt`.
2. Copy `s.cfg`. Apply `workspace_mode`, task limits, `temperature`.
3. `extractLLMConfig`: body `llm_config`, then headers `X-LLM-Base-URL`, `X-LLM-API-Key`, `X-LLM-Model`.
4. Build a registry:
   - `none`: new registry, `RegisterHeadless`.
   - `ephemeral`: `NewSandbox`, `RegisterAll` with writes inside that directory only, cleanup removes it.
   - anything else: the process registry. If the process was started in `none`, this returns HTTP 400 (`this server has no local workspace`).
5. `Subset` when `allowed_tools` is set.
6. `Validate`.
7. If a base URL or API key override is present, `NewClientWithAuth` replaces the provider for this call only. Cleanup closes that client.

`handleRun` puts `builtin.WithScratchpadSession(ctx, runID)` on the context and `CleanupScratchpad` in a defer. The engine observer is nil (no SSE). The JSON body is `RunResponse`: `run_id`, `status`, `output`, `answer` (same string, so older clients that decode `agent.RunResult` still see `answer`), `structured_output`, `tool_trace`, `usage`, `duration_ms`.

`handleTriage` always forces `workspace_mode=none` and a headless registry. It builds a prompt from `service_name`, `anomaly_window`, and optional `alert_webhook_url`, and it puts logs and metrics into `context_data`. A missing `response_schema` is replaced with the incident object (`severity`, `root_cause`, `recommended_actions`, `affected_components`, `should_alert`, `alert_summary`).

Streaming writes `event: <name>\ndata: <json>\n\n` and flushes. The observer and the writer share `streamMu`.

---

## 11. ACP session flow

`acp.Server.Serve` scans stdin for one JSON object per line. A line with a method and an `id` is a request and is handled on its own goroutine. That split is required: `session/prompt` blocks on the model and on `fs/read_text_file`, and the read loop must stay free to deliver the IDE’s response and to observe `session/cancel`.

```text
IDE                         minig acp                      llama.cpp
 | initialize                |                                |
 |-------------------------->| store client capabilities      |
 |<--------------------------| protocolVersion, agentInfo     |
 | session/new {cwd}         |                                |
 |-------------------------->| session map                    |
 |<--------------------------| {sessionId}                    |
 | session/prompt            |                                |
 |-------------------------->| NewWorkspace(cwd)              |
 |                           | RegisterAll(allowWrite)        |
 |                           | Engine.Run                     |
 |                           |---- chat / tools ------------->|
 |<-- session/update --------|                                |
 | fs/read_text_file (if cap)|                                |
 |<--------------------------|                                |
 |-------------------------->| tool result back to the model  |
 |<--------------------------| {stopReason: end_turn}         |
```

Capabilities are read from `clientCapabilities.fs.readTextFile`, `writeTextFile`, and `terminal` (bool or object). The `ideDelegate` implements `builtin.EditorDelegate` and sends those methods with `sessionId` on stdout, then waits on a pending map keyed by the numeric id. `session/cancel` cancels the prompt context. The prompt result uses `stopReason` `end_turn` or `cancelled`.

ACP does not run `CheckHealth` at startup. The first `Chat` fails if the model URL is wrong, and that error is a `session/update` with `sessionUpdate: error` plus a prompt result.

---

## 12. Concurrency and cancellation

- The signal context cancels the HTTP server, the ACP read loop, and the MCP read loop.
- Each HTTP request uses `r.Context()`, so a disconnect cancels the model call and the tool timeout derived from it.
- Registries and the scratchpad map use their own mutexes. Tool `Execute` methods must not assume they run one at a time across requests. A single task’s tool calls are sequential.
- ACP writes stdout under `writeMu`. Pending IDE responses use `pendMu`. Do not hold `writeMu` while waiting for a reply.
- `budget.Ledger` methods lock. Do not call back into the ledger from a hook that already holds other locks in a different order. Today the engine calls the ledger only from the task goroutine.

---

## 13. Tests and the commands that maintain them

| Command | What it guarantees |
|---|---|
| `gofmt -s -w .` | The only format. `make fmt` does this. |
| `go test ./...` | Unit and in-process HTTP tests. `make test`. |
| `go test -race -count=1 ./...` | Data races. `make race`. |
| `go vet ./...` | `make vet`. |

There is no third-party test framework. Use the standard `testing` package and `httptest`.

| Test | File | What it locks |
|---|---|---|
| CLI health, one real micro-run, `--serve` shutdown | `cmd/minig/main_test.go` | flag compatibility and a scripted llama.cpp |
| Headless serve, schema, API key header, no leftover sandbox | `cmd/minig/dualmode_test.go` | `TestHeadlessServeStructuredRun` |
| ACP initialize → session/new → session/prompt and `fs/read_text_file` | `cmd/minig/dualmode_test.go` | IDE delegation |
| `minig mcp` tools/list and tools/call | `cmd/minig/dualmode_test.go` | MCP schemas for every built-in headless and workspace tool |
| DAG cycle, context isolation, schema enforcement | `internal/agent/engine_test.go` | planner and `RunWithConfig` |
| Path escape and symlink | `internal/safety/safety_test.go` | workspace jail |
| Sandbox cleanup | `internal/safety/sandbox_test.go` | temp dir removal |
| Registry allowlist and truncation | `internal/tool/tool_test.go` | `Execute` rules |
| File tools and edit diagnostics | `internal/tool/builtin/builtin_test.go` | read-only vs write registration |
| HTTP, logs, sandbox, alerts, headless names | `internal/tool/builtin/new_tools_test.go` | new tools |
| llama.cpp wire format and fallback parser | `internal/llm/llamacpp/client_test.go` | bearer header and `<tool_call>` |
| Config bounds and env overlay | `internal/config/config_test.go` | `none` may omit the workspace |
| REST and legacy SSE `done` | `internal/server/server_test.go` | `/v1/run` still returns `answer` |
| External MCP client against a fake stdio server | `internal/tool/mcp/mcp_test.go` | handshake and `tools/call` |

`llm.MockProvider` is the stand-in inside package tests. The CLI tests start `httptest.Server` and speak the real llama.cpp JSON because they go through `runIO`. When you change a response field, update both the mock-based test and the scripted HTTP test if both decode it.

`make build` writes `bin/minig` and also `/home/bushu/go/bin/minig`. The second path is this checkout’s install location. Change it if you are not that machine, or build with `go build -o bin/minig ./cmd/minig` and ignore the Makefile line.

---

## 14. How to contribute

Work in a branch. Keep a change inside the package that owns the decision (section 2). A behavior change needs a test that fails before the change and passes after it.

Match the surrounding style:

- Standard library only. A new import in `go.mod` is a project decision, not a drive-by.
- Exported names are the types and functions other packages in this repo call. Tests in `package foo` can use unexported identifiers. CLI tests are `package main` and call `runIO`.
- Errors that callers branch on are `var Err…` in `budget`. Everything else is a formatted error with `%w` when wrapping.
- Context is the first argument. Honor `ctx.Err()` at the start of `Execute` and do not start work after cancellation.
- Cap bytes before they enter a prompt or an HTTP response. Use the constants already next to the tool (`maxInspectFileBytes`, `maxReadLines`, `MaxResponseBytes`).
- Do not log secrets. `LlamaAPIKey` and `APIKey` must not appear in event messages or tool traces.
- Do not add a new CLI flag that changes the default of an existing flag. `minig "goal"`, `minig --serve`, and `minig --json` are covered by `main_test.go`.

Pull request contents that reviewers expect:

- What user-visible mode changed (CLI, ACP, serve, mcp).
- Which registry the new tool is in (`RegisterHeadless`, `RegisterAll`, or both).
- `go test ./...` output, and `go test -race` if you touched a mutex, the scratchpad, or the ACP write path.

### 14.1 Change recipes

**New tool.** Section 8.3. Then run `go test ./internal/tool/builtin/`.

**New HTTP field.** Add it to `RunRequest` or `TriageRequest` in `server`. Thread it through `prepareExecution` into `config.Config` or `agent.RunConfig`. Extend `Validate` if the value has a range. Add a `server` test with `httptest.NewRecorder` and `MockProvider`. Do not parse the body again in the handler.

**New model endpoint behavior.** Change `llamacpp.Client` and `client_test.go`. Keep `Provider` stable unless every caller (`Engine`, server override, ACP) is updated in the same change.

**New SSE event.** Add the `EventKind`, emit it from the engine, map it in `streamEventName`, and ignore it in `ui.Renderer` unless the terminal should show it. Legacy `/v1/run/stream` must still emit `done` (`TestServerRunStreamSSEEndpoint`).

**Workspace rule.** If a mode must not touch the project, enforce it by which tools are registered, not by hoping the model refrains. Registration is the security boundary. `Execute` checks are the second line (path jail, dangerous shell patterns).

### 14.2 Invariants that should stay true

- `workspace_mode=none` never calls `safety.NewWorkspace` on the process current directory at startup.
- Write tools are absent from the registry when `AllowWrite` is false, except `workspace_code`, which refuses `patch` itself.
- Tool schemas in the planning prompt are names only. Full schemas appear only on the task that listed that tool.
- One micro-task does not receive another task’s raw tool transcript. Only the truncated `TaskResult.Output`.
- A tool error is data for the next model turn until `max_retries` is exhausted. It does not abort the HTTP request by itself.
- `structured_output` is either valid against `response_schema` or absent.
- Sandbox directories use the `minig-run-` prefix and are removed on every return path, including tool errors (`defer Cleanup`).
- ACP stdout is JSON-RPC only. Progress for humans goes to stderr, and `minig acp` does not use the terminal renderer.
- `GET /v1/health` does not require `MINIG_API_KEY`.

---

## 15. Maintenance

When you upgrade Go, change the `go` line in `go.mod` only after `go test ./...` and `go vet ./...` pass with that toolchain. This tree uses `strings.SplitSeq` and `testing.T.Chdir`, which need a recent Go.

When a llama.cpp release changes `/health`, `/props`, or `input_tokens`, update `llamacpp.Client` and `client_test.go`. Keep the 404 fallback so older servers still count tokens by heuristic.

When you rename a tool, update:

- the `Name()` method and its registration
- `TestRegisterHeadless` and `TestMCPExposesBuiltinTools` (they list required names)
- `docs/agent-guide.md` tool table
- any planner examples in `planner.go` that mention the old name

Docs to touch together:

| Change | Also edit |
|---|---|
| Flag, env var, endpoint, workspace mode | `docs/agent-guide.md`, `README.md` |
| IDE config shape | `docs/ide-setup.md`, `configs/ide/` |
| Package boundaries or the run loop | this file |
| Prompt rules or safety story | `AGENT.md` |

`ARCHITECTURE.md` is the earlier stage-by-stage narrative. Where it disagrees with this file or the code, the code and this file win. Update this file in the same change as a structural move so the reading order in section 1 stays true.

Before you call a maintenance change done:

```bash
gofmt -s -w .
go test ./...
go vet ./...
```

Run `go test -race -count=1 ./...` when the diff touches `server`, `acp`, `budget`, `tool.Registry`, or `scratchpad.go`.

A local smoke check that does not need a GPU is the scripted tests themselves. A check that does need a model is:

```bash
minig --url http://127.0.0.1:8080 --health
minig --workspace . "Name the packages under internal/"
```

Do not point smoke tests at a production llama.cpp that shares a port with `minig serve`. The agent guide uses `8080` for the model and `8090` for minig.
