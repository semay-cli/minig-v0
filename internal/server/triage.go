package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/llm/llamacpp"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
)

// TriageRequest defines the payload for POST /v1/triage.
type TriageRequest struct {
	ServiceName     string          `json:"service_name"`
	AnomalyWindow   string          `json:"anomaly_window"`
	Logs            string          `json:"logs"`
	Metrics         json.RawMessage `json:"metrics,omitempty"`
	AlertWebhookURL string          `json:"alert_webhook_url,omitempty"`
	LLMConfig       *LLMOverride    `json:"llm_config,omitempty"`
	ResponseSchema  json.RawMessage `json:"response_schema,omitempty"`
}

// TriageResponse defines the structured outcome from POST /v1/triage.
type TriageResponse struct {
	RunID            string                `json:"run_id"`
	Status           string                `json:"status"`
	ServiceName      string                `json:"service_name"`
	AnomalyWindow    string                `json:"anomaly_window,omitempty"`
	Diagnosis        string                `json:"diagnosis"`
	StructuredOutput json.RawMessage       `json:"structured_output,omitempty"`
	ToolTrace        []agent.ToolTraceItem `json:"tool_trace,omitempty"`
	DurationMs       int64                 `json:"duration_ms"`
}

func (s *Server) handleTriage(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	var req TriageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request JSON: "+err.Error())
		return
	}

	serviceName := strings.TrimSpace(req.ServiceName)
	if serviceName == "" {
		writeJSONError(w, http.StatusBadRequest, "service_name is required")
		return
	}

	runID := generateRunID()

	// Build goal prompt for triage
	var goal strings.Builder
	fmt.Fprintf(&goal, "Perform automated incident triage and root cause diagnosis for service %q.", serviceName)
	if strings.TrimSpace(req.AnomalyWindow) != "" {
		fmt.Fprintf(&goal, " Anomaly window: %s.", strings.TrimSpace(req.AnomalyWindow))
	}
	goal.WriteString(" Compress logs, analyze anomaly patterns, identify root cause and severity, and recommend remediation steps.")
	if strings.TrimSpace(req.AlertWebhookURL) != "" {
		fmt.Fprintf(&goal, " If the issue is high or critical severity, dispatch an alert via alert_dispatch to webhook %s.", strings.TrimSpace(req.AlertWebhookURL))
	}

	// Prepare context_data
	contextPayload := map[string]any{
		"service_name": serviceName,
	}
	if req.AnomalyWindow != "" {
		contextPayload["anomaly_window"] = req.AnomalyWindow
	}
	if req.Logs != "" {
		contextPayload["raw_logs"] = req.Logs
	}
	if len(req.Metrics) > 0 {
		contextPayload["metrics"] = req.Metrics
	}
	if req.AlertWebhookURL != "" {
		contextPayload["alert_webhook_url"] = req.AlertWebhookURL
	}
	contextJSON, _ := json.Marshal(contextPayload)

	// Create headless tool registry for triage
	headlessReg, err := tool.NewRegistry(s.cfg.MaxToolOutputBytes, s.cfg.ToolTimeout)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "initialize registry: "+err.Error())
		return
	}
	if err := builtin.RegisterHeadless(headlessReg); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "register headless tools: "+err.Error())
		return
	}

	// Dynamic LLM routing
	llmOverride := extractLLMConfig(r, req.LLMConfig)
	runCfg := s.cfg
	runCfg.WorkspaceMode = "none"

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
			writeJSONError(w, http.StatusBadRequest, "llm override: "+err.Error())
			return
		}
		defer overrideClient.Close()
		provider = overrideClient
	}
	if llmOverride.Model != "" {
		runCfg.Model = llmOverride.Model
	}
	if llmOverride.Temperature != nil {
		runCfg.Temperature = *llmOverride.Temperature
	}
	if llmOverride.MaxTokens > 0 {
		runCfg.MaxCompletionTokens = llmOverride.MaxTokens
	}

	ctx := builtin.WithScratchpadSession(r.Context(), runID)
	defer builtin.CleanupScratchpad(runID)

	eng, err := agent.NewEngine(runCfg, provider, headlessReg, nil)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "create engine: "+err.Error())
		return
	}

	// Default structured schema if none passed by user
	effectiveSchema := req.ResponseSchema
	if len(effectiveSchema) == 0 {
		effectiveSchema = json.RawMessage(`{
			"type": "object",
			"required": ["severity", "root_cause", "recommended_actions"],
			"properties": {
				"severity": {"type": "string", "enum": ["critical", "high", "medium", "low", "info"]},
				"root_cause": {"type": "string"},
				"affected_components": {"type": "array", "items": {"type": "string"}},
				"recommended_actions": {"type": "array", "items": {"type": "string"}},
				"should_alert": {"type": "boolean"},
				"alert_summary": {"type": "string"}
			}
		}`)
	}

	runResult, err := eng.RunWithConfig(ctx, goal.String(), agent.RunConfig{
		RunID:          runID,
		WorkspaceMode:  "none",
		ContextData:    contextJSON,
		ResponseSchema: effectiveSchema,
		MaxRetries:     runCfg.MaxRetries,
	})
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("triage run failed: %v", err))
		return
	}

	triageResp := TriageResponse{
		RunID:            runID,
		Status:           "completed",
		ServiceName:      serviceName,
		AnomalyWindow:    req.AnomalyWindow,
		Diagnosis:        runResult.Answer,
		StructuredOutput: runResult.StructuredOutput,
		ToolTrace:        runResult.ToolTrace,
		DurationMs:       time.Since(start).Milliseconds(),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(triageResp)
}
