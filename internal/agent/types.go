package agent

import "github.com/semay-cli/minig-v0/internal/budget"

// MicroTask represents one atomic unit of work with scoped tool access.
type MicroTask struct {
	ID        string   `json:"id"`
	Goal      string   `json:"goal"`
	DependsOn []string `json:"depends_on,omitempty"`
	Tools     []string `json:"tools,omitempty"`
}

// TaskResult stores the compacted outcome of a completed micro-task.
type TaskResult struct {
	Task       MicroTask
	Output     string
	TokensUsed int
	ToolCalls  int
}

// RunResult summarizes a completed agent run.
type RunResult struct {
	Answer  string
	Tasks   []TaskResult
	Stats   budget.Stats
	Planned int
}

// EventKind identifies lifecycle milestones emitted during a run.
type EventKind string

const (
	EventPlanReady    EventKind = "plan_ready"
	EventTaskStart    EventKind = "task_start"
	EventToolCall     EventKind = "tool_call"
	EventTaskDone     EventKind = "task_done"
	EventSynthesizing EventKind = "synthesizing"
)

// Event carries progress telemetry for UI/logging consumers.
type Event struct {
	Kind      EventKind
	TaskID    string
	Message   string
	TaskIndex int
	TaskTotal int
	Stats     budget.Stats
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
