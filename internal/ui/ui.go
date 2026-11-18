package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

// Renderer streams micro-task progress to stderr and writes final output to stdout.
type Renderer struct {
	mu      sync.Mutex
	out     io.Writer
	errOut  io.Writer
	quiet   bool
	jsonOut bool
}

// NewRenderer creates a thread-safe terminal renderer.
func NewRenderer(out, errOut io.Writer, quiet, jsonOut bool) *Renderer {
	return &Renderer{
		out:     out,
		errOut:  errOut,
		quiet:   quiet,
		jsonOut: jsonOut,
	}
}

// OnEvent implements agent.Observer to display live micro-task execution milestones.
func (r *Renderer) OnEvent(ev agent.Event) {
	if r.quiet || r.errOut == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	switch ev.Kind {
	case agent.EventPlanReady:
		fmt.Fprintf(r.errOut, "[plan] %s\n", ev.Message)
	case agent.EventTaskStart:
		fmt.Fprintf(r.errOut, "[%d/%d %s] %s\n",
			ev.TaskIndex, ev.TaskTotal, ev.TaskID, singleLine(ev.Message, 100))
	case agent.EventToolCall:
		fmt.Fprintf(r.errOut, "  -> tool %s\n", singleLine(ev.Message, 100))
	case agent.EventTaskDone:
		fmt.Fprintf(r.errOut, "[%d/%d %s] done (run tokens: %d, remaining: %d)\n",
			ev.TaskIndex, ev.TaskTotal, ev.TaskID, ev.Stats.TotalTokens, ev.Stats.RemainingTokens)
	case agent.EventSynthesizing:
		fmt.Fprintf(r.errOut, "[synth] %s\n", ev.Message)
	}
}

// RenderResult writes the final answer (or structured JSON) and resource summary.
func (r *Renderer) RenderResult(res agent.RunResult) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.jsonOut {
		enc := json.NewEncoder(r.out)
		enc.SetIndent("", "  ")
		return enc.Encode(res)
	}

	if _, err := fmt.Fprintln(r.out, res.Answer); err != nil {
		return err
	}
	if !r.quiet && r.errOut != nil {
		fmt.Fprintf(r.errOut, "\n[stats] tasks=%d/%d tools=%d prompt_tok=%d completion_tok=%d total_tok=%d remaining_tok=%d\n",
			res.Stats.TasksCompleted,
			res.Planned,
			res.Stats.ToolCalls,
			res.Stats.PromptTokens,
			res.Stats.CompletionTokens,
			res.Stats.TotalTokens,
			res.Stats.RemainingTokens,
		)
	}
	return nil
}

func singleLine(s string, maxBytes int) string {
	collapsed := strings.Join(strings.Fields(s), " ")
	return textutil.TruncateBytes(collapsed, maxBytes)
}
