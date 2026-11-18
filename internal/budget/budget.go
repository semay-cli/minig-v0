package budget

import (
	"errors"
	"fmt"
	"sync"
)

var (
	ErrRunTokenBudgetExceeded = errors.New("total run token budget exceeded")
	ErrTaskContextExceeded    = errors.New("micro-task context token limit exceeded")
	ErrMaxMicroTasksExceeded  = errors.New("maximum micro-task count exceeded")
	ErrMaxTaskStepsExceeded   = errors.New("maximum steps per micro-task exceeded")
)

// Limits defines hard resource boundaries for an agent run.
type Limits struct {
	MaxTotalTokens       int
	MaxTaskContextTokens int
	MaxMicroTasks        int
	MaxStepsPerTask      int
}

// Stats captures a point-in-time snapshot of resource consumption.
type Stats struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	RemainingTokens  int
	TasksStarted     int
	TasksCompleted   int
	ToolCalls        int
}

// Ledger tracks thread-safe token, task, and step usage across an entire run.
type Ledger struct {
	limits Limits

	mu               sync.Mutex
	promptTokens     int
	completionTokens int
	tasksStarted     int
	tasksCompleted   int
	toolCalls        int
}

// NewLedger validates limits and initializes a run-wide resource ledger.
func NewLedger(limits Limits) (*Ledger, error) {
	if limits.MaxTaskContextTokens < 1 {
		return nil, fmt.Errorf("max task context tokens must be >= 1, got %d", limits.MaxTaskContextTokens)
	}
	if limits.MaxTotalTokens < limits.MaxTaskContextTokens {
		return nil, fmt.Errorf("max total tokens (%d) must be >= max task context tokens (%d)",
			limits.MaxTotalTokens, limits.MaxTaskContextTokens)
	}
	if limits.MaxMicroTasks < 1 {
		return nil, fmt.Errorf("max micro-tasks must be >= 1, got %d", limits.MaxMicroTasks)
	}
	if limits.MaxStepsPerTask < 1 {
		return nil, fmt.Errorf("max steps per task must be >= 1, got %d", limits.MaxStepsPerTask)
	}
	return &Ledger{limits: limits}, nil
}

// StartTask allocates a scoped budget for one isolated micro-task.
func (l *Ledger) StartTask(taskID string) (*TaskBudget, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.tasksStarted >= l.limits.MaxMicroTasks {
		return nil, fmt.Errorf("%w: limit %d", ErrMaxMicroTasksExceeded, l.limits.MaxMicroTasks)
	}
	used := l.promptTokens + l.completionTokens
	if used >= l.limits.MaxTotalTokens {
		return nil, fmt.Errorf("%w: used %d/%d", ErrRunTokenBudgetExceeded, used, l.limits.MaxTotalTokens)
	}

	l.tasksStarted++
	return &TaskBudget{
		taskID: taskID,
		ledger: l,
	}, nil
}

// CheckPrompt verifies whether promptTokens fits inside both the single-task
// context window and the remaining run-wide token allowance.
func (l *Ledger) CheckPrompt(promptTokens int) error {
	if promptTokens < 0 {
		return fmt.Errorf("prompt tokens must not be negative, got %d", promptTokens)
	}
	if promptTokens > l.limits.MaxTaskContextTokens {
		return fmt.Errorf("%w: prompt %d > limit %d",
			ErrTaskContextExceeded, promptTokens, l.limits.MaxTaskContextTokens)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	used := l.promptTokens + l.completionTokens
	if used+promptTokens > l.limits.MaxTotalTokens {
		return fmt.Errorf("%w: used %d + prompt %d > limit %d",
			ErrRunTokenBudgetExceeded, used, promptTokens, l.limits.MaxTotalTokens)
	}
	return nil
}

// RecordTokens commits actual prompt and completion token counts after an LLM call.
func (l *Ledger) RecordTokens(promptTokens, completionTokens int) error {
	if promptTokens < 0 || completionTokens < 0 {
		return fmt.Errorf("token counts must not be negative: prompt=%d completion=%d",
			promptTokens, completionTokens)
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.promptTokens += promptTokens
	l.completionTokens += completionTokens
	used := l.promptTokens + l.completionTokens
	if used > l.limits.MaxTotalTokens {
		return fmt.Errorf("%w: used %d > limit %d",
			ErrRunTokenBudgetExceeded, used, l.limits.MaxTotalTokens)
	}
	return nil
}

// RecordToolCall increments the global tool invocation counter.
func (l *Ledger) RecordToolCall() {
	l.mu.Lock()
	l.toolCalls++
	l.mu.Unlock()
}

// MarkTaskCompleted increments the completed micro-task counter.
func (l *Ledger) MarkTaskCompleted() {
	l.mu.Lock()
	l.tasksCompleted++
	l.mu.Unlock()
}

// Stats returns a consistent snapshot of current usage and remaining tokens.
func (l *Ledger) Stats() Stats {
	l.mu.Lock()
	defer l.mu.Unlock()

	total := l.promptTokens + l.completionTokens
	remaining := l.limits.MaxTotalTokens - total
	if remaining < 0 {
		remaining = 0
	}
	return Stats{
		PromptTokens:     l.promptTokens,
		CompletionTokens: l.completionTokens,
		TotalTokens:      total,
		RemainingTokens:  remaining,
		TasksStarted:     l.tasksStarted,
		TasksCompleted:   l.tasksCompleted,
		ToolCalls:        l.toolCalls,
	}
}

// TaskBudget tracks step and token consumption for a single micro-task.
type TaskBudget struct {
	taskID string
	ledger *Ledger

	mu               sync.Mutex
	stepsUsed        int
	promptTokens     int
	completionTokens int
	completed        bool
}

// TaskID returns the identifier of the micro-task owning this budget.
func (tb *TaskBudget) TaskID() string {
	return tb.taskID
}

// NextStep consumes one reasoning/action turn inside the micro-task.
func (tb *TaskBudget) NextStep() (int, error) {
	tb.mu.Lock()
	defer tb.mu.Unlock()

	if tb.stepsUsed >= tb.ledger.limits.MaxStepsPerTask {
		return tb.stepsUsed, fmt.Errorf("%w: task %q reached limit %d",
			ErrMaxTaskStepsExceeded, tb.taskID, tb.ledger.limits.MaxStepsPerTask)
	}
	tb.stepsUsed++
	return tb.stepsUsed, nil
}

// CheckPrompt delegates pre-flight token validation to the run ledger.
func (tb *TaskBudget) CheckPrompt(promptTokens int) error {
	return tb.ledger.CheckPrompt(promptTokens)
}

// RecordTokens records token usage on both the micro-task and parent ledger.
func (tb *TaskBudget) RecordTokens(promptTokens, completionTokens int) error {
	if err := tb.ledger.RecordTokens(promptTokens, completionTokens); err != nil {
		return err
	}
	tb.mu.Lock()
	tb.promptTokens += promptTokens
	tb.completionTokens += completionTokens
	tb.mu.Unlock()
	return nil
}

// RecordToolCall records a tool call in the run ledger.
func (tb *TaskBudget) RecordToolCall() {
	tb.ledger.RecordToolCall()
}

// Complete marks the micro-task as finished once.
func (tb *TaskBudget) Complete() {
	tb.mu.Lock()
	if tb.completed {
		tb.mu.Unlock()
		return
	}
	tb.completed = true
	tb.mu.Unlock()

	tb.ledger.MarkTaskCompleted()
}

// TokensUsed returns total prompt + completion tokens consumed by this micro-task.
func (tb *TaskBudget) TokensUsed() int {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	return tb.promptTokens + tb.completionTokens
}
