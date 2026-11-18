package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/semay-cli/minig-v0/internal/budget"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/textutil"
	"github.com/semay-cli/minig-v0/internal/tool"
)

const (
	maxArtifactBytes      = 1200
	maxWorkerOutputTokens = 768
	maxSynthOutputTokens  = 1024
)

// Engine orchestrates micro-task decomposition, isolated execution, and bounded synthesis.
type Engine struct {
	cfg      config.Config
	provider llm.Provider
	tools    *tool.Registry
	observer Observer
}

// NewEngine constructs an Engine after validating dependencies.
func NewEngine(cfg config.Config, provider llm.Provider, tools *tool.Registry, observer Observer) (*Engine, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if provider == nil {
		return nil, errors.New("llm provider must not be nil")
	}
	if tools == nil {
		return nil, errors.New("tool registry must not be nil")
	}
	return &Engine{
		cfg:      cfg,
		provider: provider,
		tools:    tools,
		observer: observer,
	}, nil
}

// Run executes the full micro-task pipeline for userGoal under strict token and step budgets.
func (e *Engine) Run(ctx context.Context, userGoal string) (RunResult, error) {
	goal := textutil.CompactWhitespace(userGoal)
	if goal == "" {
		return RunResult{}, errors.New("user goal must not be empty")
	}

	ledger, err := budget.NewLedger(budget.Limits{
		MaxTotalTokens:       e.cfg.TotalRunTokens,
		MaxTaskContextTokens: e.cfg.TaskContextTokens,
		MaxMicroTasks:        e.cfg.MaxMicroTasks,
		MaxStepsPerTask:      e.cfg.MaxStepsPerTask,
	})
	if err != nil {
		return RunResult{}, err
	}

	tasks, err := e.decompose(ctx, ledger, goal)
	if err != nil {
		return RunResult{}, err
	}
	e.emit(Event{
		Kind:      EventPlanReady,
		Message:   fmt.Sprintf("decomposed into %d micro-task(s)", len(tasks)),
		TaskTotal: len(tasks),
		Stats:     ledger.Stats(),
	})

	completed := make(map[string]TaskResult, len(tasks))
	results := make([]TaskResult, 0, len(tasks))

	for idx, task := range tasks {
		if err := ctx.Err(); err != nil {
			return RunResult{}, err
		}

		taskBudget, err := ledger.StartTask(task.ID)
		if err != nil {
			return RunResult{}, err
		}

		e.emit(Event{
			Kind:      EventTaskStart,
			TaskID:    task.ID,
			Message:   task.Goal,
			TaskIndex: idx + 1,
			TaskTotal: len(tasks),
			Stats:     ledger.Stats(),
		})

		res, err := e.executeMicroTask(ctx, taskBudget, goal, task, completed)
		if err != nil {
			return RunResult{}, fmt.Errorf("micro-task %q failed: %w", task.ID, err)
		}
		taskBudget.Complete()

		completed[task.ID] = res
		results = append(results, res)

		e.emit(Event{
			Kind:      EventTaskDone,
			TaskID:    task.ID,
			Message:   res.Output,
			TaskIndex: idx + 1,
			TaskTotal: len(tasks),
			Stats:     ledger.Stats(),
		})
	}

	// Fast path: single micro-task output is already the final answer.
	if len(results) == 1 {
		return RunResult{
			Answer:  results[0].Output,
			Tasks:   results,
			Stats:   ledger.Stats(),
			Planned: len(tasks),
		}, nil
	}

	e.emit(Event{
		Kind:      EventSynthesizing,
		Message:   "synthesizing micro-task results",
		TaskTotal: len(tasks),
		Stats:     ledger.Stats(),
	})

	answer, err := e.synthesize(ctx, ledger, goal, results)
	if err != nil {
		return RunResult{}, err
	}

	return RunResult{
		Answer:  answer,
		Tasks:   results,
		Stats:   ledger.Stats(),
		Planned: len(tasks),
	}, nil
}

func (e *Engine) executeMicroTask(
	ctx context.Context,
	tb *budget.TaskBudget,
	overallGoal string,
	task MicroTask,
	completed map[string]TaskResult,
) (TaskResult, error) {
	// Expose ONLY the tools allowed for this micro-task.
	allowedSpecs := e.tools.Specs(task.Tools)

	// Build a minimal, isolated prompt containing only direct dependency outputs.
	var prompt strings.Builder
	fmt.Fprintf(&prompt, "Overall Goal: %s\n", textutil.TruncateBytes(overallGoal, 512))
	if len(task.DependsOn) > 0 {
		prompt.WriteString("\nPrerequisite Findings:\n")
		for _, depID := range task.DependsOn {
			if depRes, ok := completed[depID]; ok {
				fmt.Fprintf(&prompt, "- [%s] (%s): %s\n",
					depID,
					textutil.TruncateBytes(depRes.Task.Goal, 120),
					textutil.TruncateBytes(depRes.Output, maxArtifactBytes),
				)
			}
		}
	}
	fmt.Fprintf(&prompt, "\nCurrent Micro-Task [%s]: %s\n", task.ID, task.Goal)

	messages := []llm.Message{
		{
			Role: llm.RoleSystem,
			Content: "Execute ONLY the Current Micro-Task. Call allowed tools only when needed to inspect or modify files. " +
				"If a tool call fails, inspect the error diagnostic and self-correct immediately. " +
				"Once complete, respond with a concise, factual summary of what you found or changed.",
		},
		{
			Role:    llm.RoleUser,
			Content: prompt.String(),
		},
	}

	toolCallsCount := 0

	for {
		if _, err := tb.NextStep(); err != nil {
			return TaskResult{}, err
		}

		promptTokens, err := e.provider.CountTokens(ctx, e.cfg.Model, messages, allowedSpecs)
		if err != nil {
			return TaskResult{}, fmt.Errorf("count task tokens: %w", err)
		}
		if err := tb.CheckPrompt(promptTokens); err != nil {
			return TaskResult{}, err
		}

		maxOut := e.clampCompletionTokens(promptTokens, maxWorkerOutputTokens)
		if maxOut < 64 {
			return TaskResult{}, fmt.Errorf("%w: prompt (%d tokens) leaves insufficient completion room",
				budget.ErrTaskContextExceeded, promptTokens)
		}

		resp, err := e.provider.Chat(ctx, llm.ChatRequest{
			Model:       e.cfg.Model,
			Messages:    messages,
			Tools:       allowedSpecs,
			MaxTokens:   maxOut,
			Temperature: e.cfg.Temperature,
		})
		if err != nil {
			return TaskResult{}, err
		}
		if err := tb.RecordTokens(resp.PromptTokens, resp.CompletionTokens); err != nil {
			return TaskResult{}, err
		}

		// No tool calls means this micro-task reached its terminal answer.
		if len(resp.ToolCalls) == 0 {
			output := textutil.CompactWhitespace(resp.Content)
			if output == "" {
				output = "(no output)"
			}
			output = textutil.TruncateBytes(output, maxArtifactBytes)
			return TaskResult{
				Task:       task,
				Output:     output,
				TokensUsed: tb.TokensUsed(),
				ToolCalls:  toolCallsCount,
			}, nil
		}

		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Keep tool responses compact so the ephemeral micro-task window stays under TaskContextTokens.
		perToolCap := e.cfg.MaxToolOutputBytes
		if ctxByteCap := (e.cfg.TaskContextTokens * 2); ctxByteCap < perToolCap {
			perToolCap = ctxByteCap
		}

		for _, tc := range resp.ToolCalls {
			toolCallsCount++
			tb.RecordToolCall()

			e.emit(Event{
				Kind:    EventToolCall,
				TaskID:  task.ID,
				Message: fmt.Sprintf("%s(%s)", tc.Name, textutil.TruncateBytes(string(tc.Arguments), 120)),
			})

			toolOut, execErr := e.tools.Execute(ctx, task.Tools, tc.Name, tc.Arguments)
			if execErr != nil {
				toolOut = formatToolErrorDiagnostic(tc.Name, execErr)
			}
			toolOut = textutil.TruncateBytes(textutil.CompactWhitespace(toolOut), perToolCap)

			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Name:       tc.Name,
				ToolCallID: tc.ID,
				Content:    toolOut,
			})
		}
	}
}

func formatToolErrorDiagnostic(toolName string, err error) string {
	msg := err.Error()
	if strings.Contains(msg, "old_text not found") || strings.Contains(msg, "DIAGNOSTIC") {
		return fmt.Sprintf("TOOL EXECUTION FAILED: %s\nRECOVERY GUIDANCE: Inspect the exact lines with read_range to verify whitespace and indentation, then retry edit_file with precise context.", msg)
	}
	if strings.Contains(msg, "does not exist") || strings.Contains(msg, "no such file") || strings.Contains(msg, "access path") {
		return fmt.Sprintf("TOOL EXECUTION FAILED: %s\nRECOVERY GUIDANCE: File or path was not found. Use list_dir or search_text to locate the correct file path before retrying.", msg)
	}
	if strings.Contains(strings.ToLower(msg), "syntax") || strings.Contains(strings.ToLower(msg), "relation") || strings.Contains(strings.ToLower(msg), "column") {
		return fmt.Sprintf("TOOL EXECUTION FAILED: %s\nRECOVERY GUIDANCE: Database or syntax error. Inspect schema using describe_table or list_tables before executing SQL.", msg)
	}
	return "error: " + msg
}

func (e *Engine) synthesize(ctx context.Context, ledger *budget.Ledger, userGoal string, results []TaskResult) (string, error) {
	var body strings.Builder
	fmt.Fprintf(&body, "User Goal: %s\n\nCompleted Micro-Task Results:\n", userGoal)
	for _, r := range results {
		fmt.Fprintf(&body, "- [%s] %s => %s\n", r.Task.ID, r.Task.Goal, r.Output)
	}

	messages := []llm.Message{
		{
			Role:    llm.RoleSystem,
			Content: "Synthesize the completed micro-task results into a direct, concise final answer for the user goal.",
		},
		{
			Role:    llm.RoleUser,
			Content: textutil.TruncateBytes(body.String(), e.cfg.TaskContextTokens*3),
		},
	}

	promptTokens, err := e.provider.CountTokens(ctx, e.cfg.Model, messages, nil)
	if err != nil {
		return assembleFallbackAnswer(results), nil
	}
	if err := ledger.CheckPrompt(promptTokens); err != nil {
		// If remaining token budget is tight, return the assembled micro-task outputs instead of failing.
		return assembleFallbackAnswer(results), nil
	}

	maxOut := e.clampCompletionTokens(promptTokens, maxSynthOutputTokens)
	if maxOut < 64 {
		return assembleFallbackAnswer(results), nil
	}

	resp, err := e.provider.Chat(ctx, llm.ChatRequest{
		Model:       e.cfg.Model,
		Messages:    messages,
		MaxTokens:   maxOut,
		Temperature: e.cfg.Temperature,
	})
	if err != nil {
		return "", fmt.Errorf("synthesis inference failed: %w", err)
	}
	if err := ledger.RecordTokens(resp.PromptTokens, resp.CompletionTokens); err != nil {
		return "", err
	}

	answer := textutil.CompactWhitespace(resp.Content)
	if answer == "" {
		return assembleFallbackAnswer(results), nil
	}
	return answer, nil
}

func (e *Engine) clampCompletionTokens(promptTokens, desiredMax int) int {
	remainingInWindow := e.cfg.TaskContextTokens - promptTokens
	if remainingInWindow < desiredMax {
		return remainingInWindow
	}
	return desiredMax
}

func (e *Engine) emit(ev Event) {
	if e.observer != nil {
		e.observer.OnEvent(ev)
	}
}

func assembleFallbackAnswer(results []TaskResult) string {
	var buf strings.Builder
	for i, r := range results {
		if i > 0 {
			buf.WriteByte('\n')
		}
		fmt.Fprintf(&buf, "[%s] %s", r.Task.ID, r.Output)
	}
	return buf.String()
}
