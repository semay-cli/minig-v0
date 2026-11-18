package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/budget"
)

func TestRendererTextAndJSONModes(t *testing.T) {
	var stdout, stderr bytes.Buffer
	r := NewRenderer(&stdout, &stderr, false, false)

	r.OnEvent(agent.Event{
		Kind:      agent.EventPlanReady,
		Message:   "decomposed into 1 micro-task(s)",
		TaskTotal: 1,
	})
	r.OnEvent(agent.Event{
		Kind:      agent.EventTaskStart,
		TaskID:    "t1",
		Message:   "Inspect repository",
		TaskIndex: 1,
		TaskTotal: 1,
	})

	res := agent.RunResult{
		Answer:  "All clear.",
		Planned: 1,
		Stats: budget.Stats{
			TasksStarted:     1,
			TasksCompleted:   1,
			PromptTokens:     30,
			CompletionTokens: 10,
			TotalTokens:      40,
			RemainingTokens:  960,
		},
	}
	if err := r.RenderResult(res); err != nil {
		t.Fatalf("RenderResult failed: %v", err)
	}

	if strings.TrimSpace(stdout.String()) != "All clear." {
		t.Errorf("stdout = %q, want %q", stdout.String(), "All clear.")
	}
	if !strings.Contains(stderr.String(), "[plan]") || !strings.Contains(stderr.String(), "[stats] tasks=1/1") {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	// JSON mode
	stdout.Reset()
	stderr.Reset()
	jr := NewRenderer(&stdout, &stderr, true, true)
	jr.OnEvent(agent.Event{Kind: agent.EventPlanReady, Message: "ignored when quiet"})
	if err := jr.RenderResult(res); err != nil {
		t.Fatalf("JSON RenderResult failed: %v", err)
	}
	if stderr.Len() != 0 {
		t.Errorf("expected no stderr in quiet mode, got %q", stderr.String())
	}
	var decoded agent.RunResult
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("invalid JSON output: %v", err)
	}
	if decoded.Answer != "All clear." || decoded.Stats.TotalTokens != 40 {
		t.Errorf("unexpected decoded JSON: %+v", decoded)
	}
}
