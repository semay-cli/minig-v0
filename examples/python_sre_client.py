#!/usr/bin/env python3
"""Call a headless minig serve instance from a Python observability pipeline.

The watcher below is the shape of a Loki + IsolationForest loop: it already
has an anomaly window, and it asks minig for a schema-validated diagnosis
before paging anyone. Point LLAMA_CPP_* at the llama.cpp server the agent
should use for this call; the minig process does not need a restart.

    python3 examples/python_sre_client.py --base-url http://127.0.0.1:8080
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import urllib.error
import urllib.request


DIAGNOSIS_SCHEMA = {
    "type": "object",
    "required": ["severity", "root_cause", "recommended_actions", "should_alert"],
    "properties": {
        "severity": {"type": "string", "enum": ["critical", "high", "medium", "low", "info"]},
        "root_cause": {"type": "string"},
        "affected_components": {"type": "array", "items": {"type": "string"}},
        "recommended_actions": {"type": "array", "items": {"type": "string"}},
        "should_alert": {"type": "boolean"},
        "alert_summary": {"type": "string"},
    },
}


def _headers() -> dict[str, str]:
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    api_key = os.environ.get("MINIG_API_KEY", "").strip()
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    # Per-call llama.cpp routing. Headers outrank the server's config file.
    for env_name, header in (
        ("LLAMA_CPP_URL", "X-LLM-Base-URL"),
        ("LLAMA_CPP_API_KEY", "X-LLM-API-Key"),
        ("LLAMA_CPP_MODEL", "X-LLM-Model"),
    ):
        value = os.environ.get(env_name, "").strip()
        if value:
            headers[header] = value
    return headers


def _post(base_url: str, path: str, payload: dict) -> dict:
    req = urllib.request.Request(
        base_url.rstrip("/") + path,
        data=json.dumps(payload).encode(),
        headers=_headers(),
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            return json.loads(resp.read().decode())
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode(errors="replace")
        raise SystemExit(f"{path} failed: HTTP {exc.code} {detail}") from exc


def diagnose_logs(base_url: str, service: str, logs: str) -> dict:
    """POST /v1/agent/run with a response schema and the anomaly payload."""
    return _post(base_url, "/v1/agent/run", {
        "prompt": (
            f"You are triaging {service}. Compress the logs, identify the root cause, "
            "and decide whether to page. Use log_template_miner and sandbox_python if they help."
        ),
        "workspace_mode": "none",
        "allowed_tools": ["log_template_miner", "sandbox_python", "observability_http", "alert_dispatch"],
        "context_data": {
            "service": service,
            "source": "loki",
            "logs": logs,
        },
        "response_schema": DIAGNOSIS_SCHEMA,
        "llm_config": {
            "temperature": 0.1,
            "max_tokens": 800,
        },
    })


def triage_spike(base_url: str, service: str, logs: str, webhook: str) -> dict:
    """POST /v1/triage for the ready-made incident diagnosis object."""
    body = {
        "service_name": service,
        "anomaly_window": "5m",
        "logs": logs,
        "metrics": {"error_ratio": 0.18, "p99_ms": 2400},
        "response_schema": DIAGNOSIS_SCHEMA,
    }
    if webhook:
        body["alert_webhook_url"] = webhook
    return _post(base_url, "/v1/triage", body)


def main() -> None:
    parser = argparse.ArgumentParser(description="Call minig serve from a Python SRE pipeline")
    parser.add_argument("--base-url", default=os.environ.get("MINIG_URL", "http://127.0.0.1:8080"))
    parser.add_argument("--service", default="checkout")
    parser.add_argument("--webhook", default="")
    args = parser.parse_args()

    logs = "\n".join([
        "2026-11-18T10:00:01Z ERROR checkout request_id=11111111-1111-1111-1111-111111111111 pool exhausted",
        "2026-11-18T10:00:02Z ERROR checkout request_id=22222222-2222-2222-2222-222222222222 pool exhausted",
        "2026-11-18T10:00:03Z ERROR checkout request_id=33333333-3333-3333-3333-333333333333 upstream 10.1.2.3 timed out",
    ])

    diagnosis = diagnose_logs(args.base_url, args.service, logs)
    structured = diagnosis.get("structured_output") or {}
    print(json.dumps({
        "run_id": diagnosis.get("run_id"),
        "should_alert": structured.get("should_alert"),
        "severity": structured.get("severity"),
        "root_cause": structured.get("root_cause"),
        "recommended_actions": structured.get("recommended_actions"),
    }, indent=2))

    triage = triage_spike(args.base_url, args.service, logs, args.webhook)
    print(json.dumps({
        "triage_run_id": triage.get("run_id"),
        "service_name": triage.get("service_name"),
        "structured_output": triage.get("structured_output"),
    }, indent=2))

    if structured.get("should_alert"):
        print("caller would open a ticket or fire the local runbook here", file=sys.stderr)


if __name__ == "__main__":
    main()
