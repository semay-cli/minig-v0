package agent

import (
	"encoding/json"

	"github.com/semay-cli/minig-v0/internal/budget"
)

// MicroTask represents one atomic unit of work with scoped tool access.
type MicroTask struct {
	ID        string   `json:"id"`
	Goal      string   `json:"goal"`
	DependsOn []string `json:"depends_on,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// ToolTraceItem records a single tool invocation for telemetry and microservice consumers.
type ToolTraceItem struct {
	TaskID string `json:"task_id"`
	Tool   string `json:"tool"`
	Args   string `json:"args"`
	Output string `json:"output"`
	Error  string `json:"error,omitempty"`
}

// TaskResult stores the compacted outcome of a completed micro-task.
type TaskResult struct {
	Task       MicroTask       `json:"task"`
	Output     string          `json:"output"`
	TokensUsed int             `json:"tokens_used"`
	ToolCalls  int             `json:"tool_calls"`
	ToolTrace  []ToolTraceItem `json:"tool_trace,omitempty"`
}

// RunResult summarizes a completed agent run.
type RunResult struct {
	RunID            string          `json:"run_id,omitempty"`
	Status           string          `json:"status,omitempty"`
	Answer           string          `json:"answer"`
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
	Tasks            []TaskResult    `json:"tasks"`
	Stats            budget.Stats    `json:"stats"`
	Planned          int             `json:"planned"`
	ToolTrace        []ToolTraceItem `json:"tool_trace,omitempty"`
	DurationMs       int64           `json:"duration_ms,omitempty"`
}

// RunConfig contains per-request execution parameters.
type RunConfig struct {
	RunID          string          `json:"run_id,omitempty"`
	WorkspaceMode  string          `json:"workspace_mode,omitempty"`
	AllowedTools   []string        `json:"allowed_tools,omitempty"`
	ContextData    json.RawMessage `json:"context_data,omitempty"`
	ResponseSchema json.RawMessage `json:"response_schema,omitempty"`
	MaxRetries     int             `json:"max_retries,omitempty"`
}

// EventKind identifies lifecycle milestones emitted during a run.
type EventKind string

const (
	EventPlanReady    EventKind = "plan_ready"
	EventTaskStart    EventKind = "task_start"
	EventToolCall     EventKind = "tool_call"
	EventToolResult   EventKind = "tool_result"
	EventToken        EventKind = "token"
	EventTaskDone     EventKind = "task_done"
	EventSynthesizing EventKind = "synthesizing"
	EventError        EventKind = "error"
)

// Event carries progress telemetry for UI/logging consumers.
type Event struct {
	Kind      EventKind    `json:"kind"`
	TaskID    string       `json:"task_id,omitempty"`
	Message   string       `json:"message"`
	TaskIndex int          `json:"task_index,omitempty"`
	TaskTotal int          `json:"task_total,omitempty"`
	Stats     budget.Stats `json:"stats"`
}

// Observer receives optional progress events during Engine.Run.
type Observer interface {
	OnEvent(Event)
}

// ObserverFunc adapts a plain function to Observer.
type ObserverFunc func(Event)

func (f ObserverFunc) OnEvent(e Event) {
	if f != nil {
		f(e)
	}
}
