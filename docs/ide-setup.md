# IDE and Python service setup

The full walkthrough of every mode (terminal, IDE, HTTP, triage, MCP) is [`agent-guide.md`](./agent-guide.md). This page is only the config you paste into an editor or a service.

`minig` is one binary with two modes.

- `minig acp` speaks the Agent Client Protocol on stdin/stdout. Zed, IntelliJ IDEA, and VS Code start it and pass `LLAMA_CPP_URL`, `LLAMA_CPP_API_KEY`, and `LLAMA_CPP_MODEL` in the process environment. File reads, writes, and terminal commands are delegated back to the IDE (`fs/read_text_file`, `fs/write_text_file`, `terminal/create`) so diffs and permission prompts stay in the editor.
- `minig serve` is a working-directory-free HTTP service (`workspace_mode=none`) for Python automation. Each request can override the llama.cpp server with `X-LLM-Base-URL`, `X-LLM-API-Key`, and `X-LLM-Model`.

Resolution order for the model endpoint, highest first:

1. HTTP headers or the request body's `llm_config` (serve only).
2. `LLAMA_CPP_URL`, `LLAMA_CPP_API_KEY`, `LLAMA_CPP_MODEL` from the IDE or the process environment.
3. `~/.config/minig/config.json`, then the built-in defaults (`http://127.0.0.1:8080`, model `local`).

Copy the JSON fragments from [`configs/ide/`](../configs/ide/) into the files named below. Replace the binary path and token with your own.

## Zed

Merge this into `~/.config/zed/settings.json`. The matching fragment is [`configs/ide/zed.settings.json`](../configs/ide/zed.settings.json).

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

Leave `LLAMA_CPP_API_KEY` empty when `llama-server` has no token. Set it when the server expects `Authorization: Bearer`.

## IntelliJ IDEA and other JetBrains IDEs

Write [`configs/ide/jetbrains.acp.json`](../configs/ide/jetbrains.acp.json) to `~/.jetbrains/acp.json`. `command` has to be the absolute path of the `minig` binary (`args` is `["acp"]`).

```json
{
  "agent_servers": {
    "minig": {
      "command": "/absolute/path/to/minig",
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

## VS Code

Merge [`configs/ide/vscode.settings.json`](../configs/ide/vscode.settings.json) into VS Code `settings.json` for an ACP client that launches an agent server:

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

## Python services

Start the headless service from any directory. It does not scan `.git` or the working tree.

```bash
export MINIG_API_KEY="change-me"          # optional service-to-service bearer token
export LLAMA_CPP_URL="http://127.0.0.1:8080"
export LLAMA_CPP_MODEL="local"
minig serve --host 0.0.0.0 --port 8080
```

A caller can still point a single request at a different llama.cpp server:

```bash
curl -s http://127.0.0.1:8080/v1/triage \
  -H "Authorization: Bearer change-me" \
  -H "Content-Type: application/json" \
  -H "X-LLM-Base-URL: http://10.0.0.8:8080" \
  -H "X-LLM-API-Key: llama-token" \
  -H "X-LLM-Model: qwen2.5-coder" \
  -d '{
    "service_name": "checkout",
    "anomaly_window": "5m",
    "logs": "ERROR pool exhausted",
    "alert_webhook_url": "https://hooks.example/alerts"
  }'
```

`examples/python_sre_client.py` shows the same flow with `POST /v1/agent/run` and `POST /v1/triage`, including `response_schema` so `structured_output` is ready for the caller's ticket or paging code.

`POST /v1/agent/stream` emits `text/event-stream` events named `plan`, `tool_start`, `tool_result`, `token`, `final`, and `error`.

`minig mcp` exposes the same built-in tools over MCP stdio (`initialize`, `tools/list`, `tools/call`) without starting an external MCP server.
