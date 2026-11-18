package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/semay-cli/minig-v0/internal/budget"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

const maxPlanOutputTokens = 768

type planEnvelope struct {
	Tasks []MicroTask `json:"tasks"`
}

// decompose asks the LLM to split userGoal into atomic, dependency-ordered micro-tasks.
// No tool schemas are sent during planning to keep the prompt small and deterministic.
func (e *Engine) decompose(ctx context.Context, ledger *budget.Ledger, userGoal string) ([]MicroTask, error) {
	availableTools := e.tools.Names()
	toolSummary := "none"
	if len(availableTools) > 0 {
		toolSummary = strings.Join(availableTools, ", ")
	}

	sysPrompt := fmt.Sprintf(
		"You are a micro-task planner for local coding agents running on 8GB GPU RAM.\n"+
			"Decompose the user goal into atomic, ordered micro-tasks (up to %d).\n"+
			"Rules:\n"+
			"- If the goal has multiple phases (explore/find, modify/write, verify/test, error-fixing), create separate micro-tasks for each phase.\n"+
			"- For bug-fixing, refactoring, or database queries, plan an inspection task first, an edit/action task second, and a verification task last.\n"+
			"- Use a single micro-task ONLY for trivial direct questions.\n"+
			"- Assign only the minimum tools from [%s] needed for each micro-task.\n"+
			"- Use depends_on to chain prerequisite task IDs.\n"+
			"- Respond ONLY with valid JSON.\n"+
			"Example:\n"+
			"{\"tasks\":[{\"id\":\"t1\",\"goal\":\"List repository files\",\"depends_on\":[],\"tools\":[\"list_dir\"]},"+
			"{\"id\":\"t2\",\"goal\":\"Read key Go files to determine each package's role\",\"depends_on\":[\"t1\"],\"tools\":[\"read_range\"]}]}",
		e.cfg.MaxMicroTasks,
		toolSummary,
	)

	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: sysPrompt},
		{Role: llm.RoleUser, Content: "Goal: " + userGoal},
	}

	promptTokens, err := e.provider.CountTokens(ctx, e.cfg.Model, messages, nil)
	if err != nil {
		return nil, fmt.Errorf("count planning tokens: %w", err)
	}
	if err := ledger.CheckPrompt(promptTokens); err != nil {
		return nil, fmt.Errorf("planning prompt exceeds budget: %w", err)
	}

	maxOut := e.clampCompletionTokens(promptTokens, maxPlanOutputTokens)
	if maxOut < 64 {
		return nil, fmt.Errorf("%w: insufficient context room for planning", budget.ErrTaskContextExceeded)
	}

	resp, err := e.provider.Chat(ctx, llm.ChatRequest{
		Model:       e.cfg.Model,
		Messages:    messages,
		MaxTokens:   maxOut,
		Temperature: 0.0,
	})
	if err != nil {
		return nil, fmt.Errorf("planning inference failed: %w", err)
	}
	if err := ledger.RecordTokens(resp.PromptTokens, resp.CompletionTokens); err != nil {
		return nil, err
	}

	tasks, ok := parsePlanJSON(resp.Content, availableTools)
	if !ok {
		// Graceful fallback for tiny models on simple prompts: run as a single atomic micro-task.
		return []MicroTask{{
			ID:    "t1",
			Goal:  userGoal,
			Tools: availableTools,
		}}, nil
	}

	if len(tasks) > e.cfg.MaxMicroTasks {
		return nil, fmt.Errorf("planner produced %d micro-tasks, exceeding limit %d", len(tasks), e.cfg.MaxMicroTasks)
	}

	ordered, err := orderDAG(tasks)
	if err != nil {
		return nil, err
	}
	return ordered, nil
}

func parsePlanJSON(raw string, availableTools []string) ([]MicroTask, bool) {
	jsonBlob, ok := extractJSONObject(raw)
	if !ok {
		return nil, false
	}

	var env planEnvelope
	if err := json.Unmarshal([]byte(jsonBlob), &env); err != nil || len(env.Tasks) == 0 {
		return nil, false
	}

	allowedSet := make(map[string]struct{}, len(availableTools))
	for _, name := range availableTools {
		allowedSet[name] = struct{}{}
	}

	seenIDs := make(map[string]struct{}, len(env.Tasks))
	cleaned := make([]MicroTask, 0, len(env.Tasks))

	for i, rawTask := range env.Tasks {
		goal := textutil.CompactWhitespace(rawTask.Goal)
		if goal == "" {
			continue
		}
		id := strings.TrimSpace(rawTask.ID)
		if id == "" {
			id = fmt.Sprintf("t%d", i+1)
		}
		if _, exists := seenIDs[id]; exists {
			id = fmt.Sprintf("%s_%d", id, i+1)
		}
		seenIDs[id] = struct{}{}

		filteredTools := make([]string, 0, len(rawTask.Tools))
		seenTools := make(map[string]struct{}, len(rawTask.Tools))
		for _, toolName := range rawTask.Tools {
			t := strings.TrimSpace(toolName)
			if _, valid := allowedSet[t]; !valid {
				continue
			}
			if _, dup := seenTools[t]; dup {
				continue
			}
			seenTools[t] = struct{}{}
			filteredTools = append(filteredTools, t)
		}

		deps := make([]string, 0, len(rawTask.DependsOn))
		seenDeps := make(map[string]struct{}, len(rawTask.DependsOn))
		for _, dep := range rawTask.DependsOn {
			d := strings.TrimSpace(dep)
			if d == "" || d == id {
				continue
			}
			if _, dup := seenDeps[d]; dup {
				continue
			}
			seenDeps[d] = struct{}{}
			deps = append(deps, d)
		}

		cleaned = append(cleaned, MicroTask{
			ID:        id,
			Goal:      goal,
			DependsOn: deps,
			Tools:     filteredTools,
		})
	}

	if len(cleaned) == 0 {
		return nil, false
	}
	return cleaned, true
}

// extractJSONObject locates the outermost balanced JSON object in s,
// ignoring markdown fences or conversational text.
func extractJSONObject(s string) (string, bool) {
	start := strings.IndexByte(s, '{')
	if start == -1 {
		return "", false
	}
	depth := 0
	inString := false
	escaped := false

	for i := start; i < len(s); i++ {
		ch := s[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inString = false
			}
			continue
		}

		switch ch {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[start : i+1], true
			}
		}
	}
	return "", false
}

// orderDAG validates dependency references, rejects cycles, and returns a topological execution order.
func orderDAG(tasks []MicroTask) ([]MicroTask, error) {
	byID := make(map[string]MicroTask, len(tasks))
	inDegree := make(map[string]int, len(tasks))
	dependents := make(map[string][]string, len(tasks))

	for _, t := range tasks {
		byID[t.ID] = t
		inDegree[t.ID] = 0
	}

	for _, t := range tasks {
		validDeps := make([]string, 0, len(t.DependsOn))
		for _, dep := range t.DependsOn {
			if _, exists := byID[dep]; !exists {
				return nil, fmt.Errorf("micro-task %q depends on unknown task %q", t.ID, dep)
			}
			validDeps = append(validDeps, dep)
			inDegree[t.ID]++
			dependents[dep] = append(dependents[dep], t.ID)
		}
		t.DependsOn = validDeps
		byID[t.ID] = t
	}

	queue := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if inDegree[t.ID] == 0 {
			queue = append(queue, t.ID)
		}
	}

	ordered := make([]MicroTask, 0, len(tasks))
	for len(queue) > 0 {
		curr := queue[0]
		queue = queue[1:]
		ordered = append(ordered, byID[curr])

		for _, next := range dependents[curr] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(ordered) != len(tasks) {
		return nil, errors.New("micro-task plan contains a dependency cycle")
	}
	return ordered, nil
}
