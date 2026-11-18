package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
)

func setupTestEnv(t *testing.T) (config.Config, *tool.Registry) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "version.txt"), []byte("v0.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.WorkspaceRoot = root
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	ws, err := safety.NewWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := tool.NewRegistry(cfg.MaxToolOutputBytes, cfg.ToolTimeout)
	if err != nil {
		t.Fatal(err)
	}
	if err := builtin.RegisterAll(reg, ws, false); err != nil {
		t.Fatal(err)
	}
	return cfg, reg
}

func TestEngineMicroTaskDecompositionAndContextIsolation(t *testing.T) {
	cfg, reg := setupTestEnv(t)

	mock := &llm.MockProvider{
		Responses: []llm.ChatResponse{
			// 1. Planner response (wrapped in markdown fence to test extraction)
			{
				Content: "```json\n{\"tasks\":[" +
					"{\"id\":\"t1\",\"goal\":\"Find files in workspace\",\"depends_on\":[],\"tools\":[\"list_dir\"]}," +
					"{\"id\":\"t2\",\"goal\":\"Read version from version.txt\",\"depends_on\":[\"t1\"],\"tools\":[\"read_range\"]}" +
					"]}\n```",
			},
			// 2. t1 step 1 -> calls list_dir
			{
				ToolCalls: []llm.ToolCall{{
					ID:        "c1",
					Name:      "list_dir",
					Arguments: json.RawMessage(`{"path":"."}`),
				}},
			},
			// 3. t1 step 2 -> returns concise finding
			{
				Content: "Found version.txt in workspace root.",
			},
			// 4. t2 step 1 -> calls read_range
			{
				ToolCalls: []llm.ToolCall{{
					ID:        "c2",
					Name:      "read_range",
					Arguments: json.RawMessage(`{"path":"version.txt","start_line":1,"max_lines":5}`),
				}},
			},
			// 5. t2 step 2 -> returns concise finding
			{
				Content: "version.txt contains v0.1.0.",
			},
			// 6. Final synthesis
			{
				Content: "The workspace version is v0.1.0.",
			},
		},
	}

	var events []EventKind
	eng, err := NewEngine(cfg, mock, reg, ObserverFunc(func(ev Event) {
		events = append(events, ev.Kind)
	}))
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Run(context.Background(), "What version is in this workspace?")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Answer != "The workspace version is v0.1.0." {
		t.Errorf("Answer = %q", res.Answer)
	}
	if res.Planned != 2 || len(res.Tasks) != 2 {
		t.Fatalf("unexpected task count: planned=%d completed=%d", res.Planned, len(res.Tasks))
	}

	// Verify request 0 (planner) received zero tool schemas
	if len(mock.Requests[0].Tools) != 0 {
		t.Errorf("planner should receive 0 tool schemas, got %d", len(mock.Requests[0].Tools))
	}
	// Verify request 1 (t1) received ONLY list_dir
	if len(mock.Requests[1].Tools) != 1 || mock.Requests[1].Tools[0].Name != "list_dir" {
		t.Errorf("t1 expected only list_dir, got %+v", mock.Requests[1].Tools)
	}
	// Verify request 3 (t2) received ONLY read_range and fresh isolated messages
	t2Req := mock.Requests[3]
	if len(t2Req.Tools) != 1 || t2Req.Tools[0].Name != "read_range" {
		t.Errorf("t2 expected only read_range, got %+v", t2Req.Tools)
	}
	if len(t2Req.Messages) != 2 {
		t.Errorf("t2 should start with fresh 2-message context, got %d messages", len(t2Req.Messages))
	}
	if !strings.Contains(t2Req.Messages[1].Content, "Found version.txt in workspace root.") {
		t.Errorf("t2 prompt missing t1 compacted output: %s", t2Req.Messages[1].Content)
	}
	if len(events) == 0 {
		t.Error("expected observer to receive lifecycle events")
	}
}

func TestEngineSingleTaskFastPathAndFallbackPlan(t *testing.T) {
	cfg, reg := setupTestEnv(t)

	mock := &llm.MockProvider{
		Responses: []llm.ChatResponse{
			// Planner emits non-JSON -> falls back to single task t1
			{Content: "Just answer directly."},
			// t1 answers directly -> no synthesis call needed
			{Content: "Direct single-task answer."},
		},
	}

	eng, err := NewEngine(cfg, mock, reg, nil)
	if err != nil {
		t.Fatal(err)
	}

	res, err := eng.Run(context.Background(), "Say hi")
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if res.Answer != "Direct single-task answer." {
		t.Errorf("Answer = %q", res.Answer)
	}
	if len(mock.Requests) != 2 {
		t.Errorf("expected exactly 2 LLM calls (plan + 1 task, skipping synthesis), got %d", len(mock.Requests))
	}
}

func TestOrderDAGRejectsCycles(t *testing.T) {
	_, err := orderDAG([]MicroTask{
		{ID: "t1", Goal: "step 1", DependsOn: []string{"t2"}},
		{ID: "t2", Goal: "step 2", DependsOn: []string{"t1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("expected cycle error, got %v", err)
	}
}

func TestEngineRunWithConfigAndSchemaEnforcement(t *testing.T) {
	cfg, reg := setupTestEnv(t)

	mock := &llm.MockProvider{
		Responses: []llm.ChatResponse{
			// 1. Planner fallback: single task
			{
				Content: "{\"tasks\":[{\"id\":\"t1\",\"goal\":\"Triage anomaly\",\"tools\":[\"observability_http\"]}]}",
			},
			// 2. Task t1 step 1: call observability_http
			{
				ToolCalls: []llm.ToolCall{{
					ID:        "c1",
					Name:      "observability_http",
					Arguments: json.RawMessage(`{"url":"http://127.0.0.1:9090"}`),
				}},
			},
			// 3. Task t1 step 2: diagnosis output with JSON object inside
			{
				Content: "Root cause identified: database saturation.\n{\"severity\":\"critical\",\"root_cause\":\"connection pool exhausted\",\"should_alert\":true}",
			},
		},
	}

	eng, err := NewEngine(cfg, mock, reg, nil)
	if err != nil {
		t.Fatal(err)
	}

	schema := json.RawMessage(`{"type":"object","properties":{"severity":{"type":"string"},"root_cause":{"type":"string"}}}`)
	res, err := eng.RunWithConfig(context.Background(), "Triage incident", RunConfig{
		RunID:          "run-test-456",
		ResponseSchema: schema,
		MaxRetries:     1,
	})
	if err != nil {
		t.Fatalf("RunWithConfig failed: %v", err)
	}

	if res.RunID != "run-test-456" {
		t.Errorf("expected RunID 'run-test-456', got %q", res.RunID)
	}
	if len(res.StructuredOutput) == 0 {
		t.Error("expected non-empty StructuredOutput")
	}
	var parsed map[string]any
	if err := json.Unmarshal(res.StructuredOutput, &parsed); err != nil {
		t.Fatalf("StructuredOutput is not valid JSON: %v", err)
	}
	if parsed["severity"] != "critical" || parsed["root_cause"] != "connection pool exhausted" {
		t.Errorf("unexpected structured fields: %v", parsed)
	}
	if len(res.ToolTrace) != 1 {
		t.Errorf("expected 1 ToolTraceItem, got %d", len(res.ToolTrace))
	}
	if res.ToolTrace[0].Tool != "observability_http" {
		t.Errorf("expected ToolTrace[0].Tool to be 'observability_http', got %q", res.ToolTrace[0].Tool)
	}
}
