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
	"github.com/semay-cli/minig-v0/internal/tool"
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

// Handler returns the underlying http.Handler.
func (s *Server) Handler() http.Handler {
	return s.mux
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("GET /v1/health", s.handleHealth)
	s.mux.HandleFunc("GET /v1/tools", s.handleTools)
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
		"status":    status,
		"url":       s.cfg.LlamaURL,
		"model":     s.cfg.Model,
		"workspace": s.cfg.WorkspaceRoot,
	}
	if checkErr != "" {
		resp["error"] = checkErr
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	specs := s.tools.Specs(nil)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"tools": specs,
		"count": len(specs),
	})
}

// RunRequest defines the JSON body for /v1/run and /v1/run/stream.
type RunRequest struct {
	Goal              string   `json:"goal"`
	MaxTasks          int      `json:"max_tasks,omitempty"`
	MaxStepsPerTask   int      `json:"max_steps,omitempty"`
	TaskContextTokens int      `json:"task_context_tokens,omitempty"`
	TotalRunTokens    int      `json:"total_tokens,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
}

func (s *Server) buildRunConfig(req RunRequest) (config.Config, error) {
	runCfg := s.cfg
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
	return runCfg, runCfg.Validate()
}

func (s *Server) handleRun(w http.ResponseWriter, r *http.Request) {
	var req RunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Goal) == "" {
		writeJSONError(w, http.StatusBadRequest, "goal is required")
		return
	}

	runCfg, err := s.buildRunConfig(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	eng, err := agent.NewEngine(runCfg, s.provider, s.tools, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	res, err := eng.Run(r.Context(), req.Goal)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(res)
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
	if strings.TrimSpace(req.Goal) == "" {
		writeJSONError(w, http.StatusBadRequest, "goal is required")
		return
	}

	runCfg, err := s.buildRunConfig(req)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

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
		emitSSE(string(ev.Kind), ev)
	})

	eng, err := agent.NewEngine(runCfg, s.provider, s.tools, observer)
	if err != nil {
		emitSSE("error", map[string]string{"error": err.Error()})
		return
	}

	res, err := eng.Run(r.Context(), req.Goal)
	if err != nil {
		emitSSE("error", map[string]string{"error": err.Error()})
		return
	}

	emitSSE("done", res)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
