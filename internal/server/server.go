package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/llm/llamacpp"
	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
)

// Server provides an HTTP REST and SSE API for the minig agent.
type Server struct {
	cfg      config.Config
	provider llm.Provider
	tools    *tool.Registry
	mux      *http.ServeMux
}

// NewServer constructs an HTTP Server configured with the agent runtime components.
func NewServer(cfg config.Config, provider llm.Provider, tools *tool.Registry) (*Server, error) {
	if provider == nil {
		return nil, errors.New("llm provider must not be nil")
	}
	if tools == nil {
		return nil, errors.New("tool registry must not be nil")
	}

	s := &Server{
		cfg:      cfg,
		provider: provider,
		tools:    tools,
		mux:      http.NewServeMux(),
	}
	s.registerRoutes()
	return s, nil
}

// Handler returns the underlying http.Handler wrapped with authentication middleware.
func (s *Server) Handler() http.Handler {
	return authMiddleware(s.cfg.APIKey, s.mux)
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/tools", s.handleTools)

	// Modern API endpoints
	s.mux.HandleFunc("POST /v1/agent/run", s.handleRun)
	s.mux.HandleFunc("POST /v1/agent/stream", s.handleRunStream)
	s.mux.HandleFunc("POST /v1/triage", s.handleTriage)

	// Backward-compatible aliases
	s.mux.HandleFunc("POST /v1/run", s.handleRun)
	s.mux.HandleFunc("POST /v1/run/stream", s.handleRunStream)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	status := "healthy"
	httpStatus := http.StatusOK
	var checkErr string

	if err := s.provider.CheckHealth(ctx); err != nil {
		status = "unhealthy"
		httpStatus = http.StatusServiceUnavailable
		checkErr = err.Error()
	}

	resp := map[string]any{
		"status":         status,
		"url":            s.cfg.LlamaURL,
		"model":          s.cfg.Model,
		"workspace":      s.cfg.WorkspaceRoot,
		"workspace_mode": s.cfg.WorkspaceMode,
	}
	if checkErr != "" {
		resp["error"] = checkErr
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	specs := s.tools.MCPDescriptors()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tools": specs,
		"count": len(specs),
	})
}

// RunRequest defines the JSON body for agent execution endpoints.
type RunRequest struct {
	Goal              string          `json:"goal,omitempty"`
	Prompt            string          `json:"prompt,omitempty"` // alias for goal
	WorkspaceMode     string          `json:"workspace_mode,omitempty"`
	AllowedTools      []string        `json:"allowed_tools,omitempty"`
	ContextData       json.RawMessage `json:"context_data,omitempty"`
	ResponseSchema    json.RawMessage `json:"response_schema,omitempty"`
	LLMConfig         *LLMOverride    `json:"llm_config,omitempty"`
	MaxTasks          int             `json:"max_tasks,omitempty"`
	MaxStepsPerTask   int             `json:"max_steps,omitempty"`
	TaskContextTokens int             `json:"task_context_tokens,omitempty"`
	TotalRunTokens    int             `json:"total_tokens,omitempty"`
	Temperature       *float64        `json:"temperature,omitempty"`
}

// RunResponse defines the structured outcome from POST /v1/agent/run.
type RunResponse struct {
	RunID            string                `json:"run_id"`
	Status           string                `json:"status"`
	Output           string                `json:"output"`
	Answer           string                `json:"answer,omitempty"`
	StructuredOutput json.RawMessage       `json:"structured_output,omitempty"`
	ToolTrace        []agent.ToolTraceItem `json:"tool_trace,omitempty"`
	Usage            map[string]int        `json:"usage"`
	DurationMs       int64                 `json:"duration_ms"`
}

func (s *Server) prepareExecution(r *http.Request, req RunRequest) (config.Config, llm.Provider, *tool.Registry, string, agent.RunConfig, func(), error) {
	nop := func() {}
	goal := strings.TrimSpace(req.Goal)
	if goal == "" {
		goal = strings.TrimSpace(req.Prompt)
	}
	if goal == "" {
		return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, errors.New("goal or prompt is required")
	}

	runCfg := s.cfg
	if req.WorkspaceMode != "" {
		runCfg.WorkspaceMode = req.WorkspaceMode
	}
	if req.MaxTasks > 0 {
		runCfg.MaxMicroTasks = req.MaxTasks
	}
	if req.MaxStepsPerTask > 0 {
		runCfg.MaxStepsPerTask = req.MaxStepsPerTask
	}
	if req.TaskContextTokens > 0 {
		runCfg.TaskContextTokens = req.TaskContextTokens
	}
	if req.TotalRunTokens > 0 {
		runCfg.TotalRunTokens = req.TotalRunTokens
	}
	if req.Temperature != nil {
		runCfg.Temperature = *req.Temperature
	}

	llmOverride := extractLLMConfig(r, req.LLMConfig)
	if llmOverride.Model != "" {
		runCfg.Model = llmOverride.Model
	}
	if llmOverride.Temperature != nil {
		runCfg.Temperature = *llmOverride.Temperature
	}
	if llmOverride.MaxTokens > 0 {
		runCfg.MaxCompletionTokens = llmOverride.MaxTokens
	}

	cleanup := nop
	var reg *tool.Registry
	switch strings.ToLower(strings.TrimSpace(runCfg.WorkspaceMode)) {
	case "none":
		hReg, err := tool.NewRegistry(s.cfg.MaxToolOutputBytes, s.cfg.ToolTimeout)
		if err != nil {
			return config.Config{}, nil, nil, "", agent.RunConfig{}, cleanup, err
		}
		if err := builtin.RegisterHeadless(hReg); err != nil {
			return config.Config{}, nil, nil, "", agent.RunConfig{}, cleanup, err
		}
		reg = hReg
	case "ephemeral":
		sb, err := safety.NewSandbox()
		if err != nil {
			return config.Config{}, nil, nil, "", agent.RunConfig{}, cleanup, err
		}
		cleanup = func() { _ = sb.Cleanup() }
		runCfg.WorkspaceRoot = sb.Dir()
		ws, err := safety.NewWorkspace(sb.Dir())
		if err != nil {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, err
		}
		eReg, err := tool.NewRegistry(s.cfg.MaxToolOutputBytes, s.cfg.ToolTimeout)
		if err != nil {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, err
		}
		if err := builtin.RegisterAll(eReg, ws, true); err != nil {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, err
		}
		reg = eReg
	default:
		if s.cfg.WorkspaceMode == "none" {
			return config.Config{}, nil, nil, "", agent.RunConfig{}, cleanup, errors.New("this server has no local workspace; use workspace_mode none or ephemeral")
		}
		reg = s.tools
	}

	if len(req.AllowedTools) > 0 {
		filtered, err := reg.Subset(req.AllowedTools)
		if err != nil {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, err
		}
		if len(filtered.Names()) == 0 {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, errors.New("allowed_tools matched no registered tools")
		}
		reg = filtered
	}

	if err := runCfg.Validate(); err != nil {
		cleanup()
		return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, err
	}

	provider := s.provider
	if llmOverride.BaseURL != "" || llmOverride.APIKey != "" {
		targetURL := s.cfg.LlamaURL
		if llmOverride.BaseURL != "" {
			targetURL = llmOverride.BaseURL
		}
		targetKey := s.cfg.LlamaAPIKey
		if llmOverride.APIKey != "" {
			targetKey = llmOverride.APIKey
		}
		overrideClient, err := llamacpp.NewClientWithAuth(targetURL, targetKey, s.cfg.LLMTimeout, true)
		if err != nil {
			cleanup()
			return config.Config{}, nil, nil, "", agent.RunConfig{}, nop, fmt.Errorf("llm override: %w", err)
		}
		provider = overrideClient
		prev := cleanup
		cleanup = func() {
			overrideClient.Close()
			prev()
		}
	}

	runID := generateRunID()
	rc := agent.RunConfig{
		RunID:          runID,
		WorkspaceMode:  runCfg.WorkspaceMode,
		AllowedTools:   req.AllowedTools,
		ContextData:    req.ContextData,
		ResponseSchema: req.ResponseSchema,
		MaxRetries:     runCfg.MaxRetries,
	}

	return runCfg, provider, reg, goal, rc, cleanup, nil
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error())
		return
	}

	runCfg, provider, reg, goal, rc, cleanup, err := s.prepareExecution(r, req)
	defer cleanup()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := builtin.WithScratchpadSession(r.Context(), rc.RunID)
	defer builtin.CleanupScratchpad(rc.RunID)

	eng, err := agent.NewEngine(runCfg, provider, reg, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	runRes, err := eng.RunWithConfig(ctx, goal, rc)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	resp := RunResponse{
		RunID:            rc.RunID,
		Status:           "completed",
		Output:           runRes.Answer,
		Answer:           runRes.Answer,
		StructuredOutput: runRes.StructuredOutput,
		ToolTrace:        runRes.ToolTrace,
		Usage: map[string]int{
			"prompt_tokens":     runRes.Stats.PromptTokens,
			"completion_tokens": runRes.Stats.CompletionTokens,
			"total_tokens":      runRes.Stats.TotalTokens,
			"tool_calls":        runRes.Stats.ToolCalls,
		},
		DurationMs: time.Since(start).Milliseconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleRunStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONError(w, http.StatusInternalServerError, "streaming unsupported by response writer")
		return
	}

	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error())
		return
	}

	runCfg, provider, reg, goal, rc, cleanup, err := s.prepareExecution(r, req)
	defer cleanup()
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	legacyStream := r.URL.Path == "/v1/run/stream"

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	var streamMu sync.Mutex
	emitSSE := func(event string, data any) {
		streamMu.Lock()
		defer streamMu.Unlock()
		b, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(b))
		flusher.Flush()
	}

	observer := agent.ObserverFunc(func(ev agent.Event) {
		emitSSE(streamEventName(ev.Kind, legacyStream), ev)
	})

	ctx := builtin.WithScratchpadSession(r.Context(), rc.RunID)
	defer builtin.CleanupScratchpad(rc.RunID)

	eng, err := agent.NewEngine(runCfg, provider, reg, observer)
	if err != nil {
		emitSSE("error", map[string]string{"error": err.Error()})
		return
	}

	res, err := eng.RunWithConfig(ctx, goal, rc)
	if err != nil {
		emitSSE("error", map[string]string{"error": err.Error()})
		return
	}

	finalEvent := "final"
	if legacyStream {
		finalEvent = "done"
	}
	emitSSE(finalEvent, RunResponse{
		RunID:            rc.RunID,
		Status:           "completed",
		Output:           res.Answer,
		Answer:           res.Answer,
		StructuredOutput: res.StructuredOutput,
		ToolTrace:        res.ToolTrace,
		Usage: map[string]int{
			"prompt_tokens":     res.Stats.PromptTokens,
			"completion_tokens": res.Stats.CompletionTokens,
			"total_tokens":      res.Stats.TotalTokens,
			"tool_calls":        res.Stats.ToolCalls,
		},
	})
}

func streamEventName(kind agent.EventKind, legacy bool) string {
	if legacy {
		return string(kind)
	}
	switch kind {
	case agent.EventPlanReady:
		return "plan"
	case agent.EventToolCall:
		return "tool_start"
	case agent.EventToolResult:
		return "tool_result"
	case agent.EventToken:
		return "token"
	case agent.EventError:
		return "error"
	default:
		return string(kind)
	}
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
