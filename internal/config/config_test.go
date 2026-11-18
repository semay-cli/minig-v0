package config

import (
	"testing"
	"time"
)

func TestDefaultConfigIsValid(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Default().Validate() failed: %v", err)
	}
	if cfg.LlamaURL != "http://127.0.0.1:8080" {
		t.Errorf("LlamaURL = %q, want http://127.0.0.1:8080", cfg.LlamaURL)
	}
	if cfg.WorkspaceMode != "local" {
		t.Errorf("WorkspaceMode = %q, want local", cfg.WorkspaceMode)
	}
}

func TestValidateRejectsRemoteURLByDefault(t *testing.T) {
	cfg := Default()
	cfg.LlamaURL = "http://192.168.1.50:8080"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected remote URL to be rejected when AllowRemoteLLM is false")
	}

	cfg.AllowRemoteLLM = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected remote URL to pass when AllowRemoteLLM is true: %v", err)
	}
}

func TestWorkspaceModeValidation(t *testing.T) {
	cfg := Default()
	cfg.WorkspaceMode = "none"
	cfg.WorkspaceRoot = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("workspace_mode 'none' should not require workspace root: %v", err)
	}

	cfg.WorkspaceMode = "invalid_mode"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for invalid workspace mode")
	}
}

func TestLoadFromEnvAppliesOverrides(t *testing.T) {
	t.Setenv("LLAMA_CPP_URL", "http://localhost:11434/")
	t.Setenv("LLAMA_CPP_API_KEY", "secret-token")
	t.Setenv("MINIG_MODEL", "qwen3-4b")
	t.Setenv("MINIG_WORKSPACE_MODE", "none")
	t.Setenv("MINIG_API_KEY", "minig-secret")
	t.Setenv("MINIG_SERVER_PORT", "9090")
	t.Setenv("MINIG_MAX_MICRO_TASKS", "8")
	t.Setenv("MINIG_LLM_TIMEOUT", "30s")
	t.Setenv("MINIG_ALLOW_WRITE", "true")

	cfg, err := LoadFromEnv()
	if err != nil {
		t.Fatalf("LoadFromEnv() failed: %v", err)
	}
	if cfg.LlamaURL != "http://localhost:11434" {
		t.Errorf("LlamaURL = %q", cfg.LlamaURL)
	}
	if cfg.LlamaAPIKey != "secret-token" {
		t.Errorf("LlamaAPIKey = %q", cfg.LlamaAPIKey)
	}
	if cfg.APIKey != "minig-secret" {
		t.Errorf("APIKey = %q", cfg.APIKey)
	}
	if cfg.WorkspaceMode != "none" {
		t.Errorf("WorkspaceMode = %q", cfg.WorkspaceMode)
	}
	if cfg.ServerPort != 9090 {
		t.Errorf("ServerPort = %d", cfg.ServerPort)
	}
	if cfg.Model != "qwen3-4b" || cfg.MaxMicroTasks != 8 || cfg.LLMTimeout != 30*time.Second || !cfg.AllowWrite {
		t.Errorf("unexpected config values: %+v", cfg)
	}
}

func TestValidateRejectsInvalidBounds(t *testing.T) {
	cases := []func(*Config){
		func(c *Config) { c.Model = "" },
		func(c *Config) { c.MaxMicroTasks = 0 },
		func(c *Config) { c.TaskContextTokens = 128 },
		func(c *Config) { c.TotalRunTokens = 1024; c.TaskContextTokens = 2048 },
		func(c *Config) { c.Temperature = 2.5 },
	}
	for i, mutate := range cases {
		cfg := Default()
		mutate(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}
