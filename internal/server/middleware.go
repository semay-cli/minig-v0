package server

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// LLMOverride captures per-request LLM backend configuration.
type LLMOverride struct {
	BaseURL     string   `json:"base_url,omitempty"`
	APIKey      string   `json:"api_key,omitempty"`
	Model       string   `json:"model,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   int      `json:"max_tokens,omitempty"`
}

// extractLLMConfig extracts LLM overrides from HTTP headers, with fallback to body fields.
func extractLLMConfig(r *http.Request, bodyCfg *LLMOverride) LLMOverride {
	out := LLMOverride{}
	if bodyCfg != nil {
		out = *bodyCfg
	}

	// HTTP headers take highest precedence
	if h := strings.TrimSpace(r.Header.Get("X-LLM-Base-URL")); h != "" {
		out.BaseURL = h
	}
	if h := strings.TrimSpace(r.Header.Get("X-LLM-API-Key")); h != "" {
		out.APIKey = h
	}
	if h := strings.TrimSpace(r.Header.Get("X-LLM-Model")); h != "" {
		out.Model = h
	}

	return out
}

// authMiddleware enforces bearer token authentication when apiKey is configured.
// GET /v1/health is always permitted without auth for liveness probes.
func authMiddleware(apiKey string, next http.Handler) http.Handler {
	requiredKey := strings.TrimSpace(apiKey)
	if requiredKey == "" {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Allow health check without credentials
		if r.Method == http.MethodGet && r.URL.Path == "/v1/health" {
			next.ServeHTTP(w, r)
			return
		}

		authHeader := strings.TrimSpace(r.Header.Get("Authorization"))
		expectedBearer := "Bearer " + requiredKey

		if authHeader != expectedBearer {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "unauthorized: invalid or missing Bearer token",
			})
			return
		}

		next.ServeHTTP(w, r)
	})
}

// generateRunID produces a unique 16-byte hex identifier for request tracing.
func generateRunID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "run-fallback"
	}
	return "run-" + hex.EncodeToString(b)
}
