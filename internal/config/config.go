package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config holds bounded runtime settings for a micro-task agent run.
type Config struct {
	LlamaURL           string        `json:"llama_url"`
	LlamaAPIKey        string        `json:"llama_api_key"`
	Model              string        `json:"model"`
	WorkspaceRoot      string        `json:"workspace_root"`
	WorkspaceMode      string        `json:"workspace_mode"` // "none" | "ephemeral" | "local"
	ServerHost         string        `json:"server_host"`
	ServerPort         int           `json:"server_port"`
	APIKey             string        `json:"api_key"`
	MCPServers         []string      `json:"mcp_servers"`
	MaxMicroTasks      int           `json:"max_micro_tasks"`
	MaxStepsPerTask    int           `json:"max_steps_per_task"`
	TaskContextTokens  int           `json:"task_context_tokens"`
	TotalRunTokens     int           `json:"total_run_tokens"`
	MaxToolOutputBytes int           `json:"max_tool_output_bytes"`
	LLMTimeout         time.Duration `json:"llm_timeout"`
	ToolTimeout        time.Duration `json:"tool_timeout"`
	Temperature        float64       `json:"temperature"`
	AllowWrite         bool          `json:"allow_write"`
	AllowRemoteLLM     bool          `json:"allow_remote_llm"`
	MaxRetries         int           `json:"max_retries"`
}

// Default returns conservative, resource-efficient defaults tuned for local GGUF models.
func Default() Config {
	return Config{
		LlamaURL:           "http://127.0.0.1:8080",
		LlamaAPIKey:        "",
		Model:              "local",
		WorkspaceRoot:      ".",
		WorkspaceMode:      "local",
		ServerHost:         "0.0.0.0",
		ServerPort:         8080,
		APIKey:             "",
		MaxMicroTasks:      24,
		MaxStepsPerTask:    16,
		TaskContextTokens:  16384,
		TotalRunTokens:     32768,
		MaxToolOutputBytes: 8 * 1024,
		LLMTimeout:         45 * time.Second,
		ToolTimeout:        10 * time.Second,
		Temperature:        0.1,
		AllowWrite:         false,
		AllowRemoteLLM:     false,
		MaxRetries:         2,
	}
}

// LoadFromEnv starts from Default(), loads optional config file, and applies environment overrides.
func LoadFromEnv() (Config, error) {
	cfg := Default()

	// Load file config from ~/.config/minig/config.json if present
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cfgPath := filepath.Join(home, ".config", "minig", "config.json")
		if data, err := os.ReadFile(cfgPath); err == nil {
			_ = json.Unmarshal(data, &cfg)
		}
	}

	// Environment variable overrides: LLAMA_CPP_* and MINIG_*
	if v := strings.TrimSpace(os.Getenv("LLAMA_CPP_URL")); v != "" {
		cfg.LlamaURL = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_LLAMA_URL")); v != "" {
		cfg.LlamaURL = v
	}

	if v := strings.TrimSpace(os.Getenv("LLAMA_CPP_API_KEY")); v != "" {
		cfg.LlamaAPIKey = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_LLAMA_API_KEY")); v != "" {
		cfg.LlamaAPIKey = v
	}

	if v := strings.TrimSpace(os.Getenv("LLAMA_CPP_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_MODEL")); v != "" {
		cfg.Model = v
	}

	if v := strings.TrimSpace(os.Getenv("MINIG_WORKSPACE")); v != "" {
		cfg.WorkspaceRoot = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_WORKSPACE_MODE")); v != "" {
		cfg.WorkspaceMode = v
	}

	if v := strings.TrimSpace(os.Getenv("MINIG_API_KEY")); v != "" {
		cfg.APIKey = v
	}

	if v := strings.TrimSpace(os.Getenv("MINIG_SERVER_HOST")); v != "" {
		cfg.ServerHost = v
	}
	var err error
	if cfg.ServerPort, err = envInt("MINIG_SERVER_PORT", cfg.ServerPort); err != nil {
		return Config{}, err
	}

	if v := strings.TrimSpace(os.Getenv("MINIG_MCP_SERVERS")); v != "" {
		for item := range strings.SplitSeq(v, ";") {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				cfg.MCPServers = append(cfg.MCPServers, trimmed)
			}
		}
	}

	if cfg.MaxMicroTasks, err = envInt("MINIG_MAX_MICRO_TASKS", cfg.MaxMicroTasks); err != nil {
		return Config{}, err
	}
	if cfg.MaxStepsPerTask, err = envInt("MINIG_MAX_STEPS_PER_TASK", cfg.MaxStepsPerTask); err != nil {
		return Config{}, err
	}
	if cfg.TaskContextTokens, err = envInt("MINIG_TASK_CONTEXT_TOKENS", cfg.TaskContextTokens); err != nil {
		return Config{}, err
	}
	if cfg.TotalRunTokens, err = envInt("MINIG_TOTAL_RUN_TOKENS", cfg.TotalRunTokens); err != nil {
		return Config{}, err
	}
	if cfg.MaxToolOutputBytes, err = envInt("MINIG_MAX_TOOL_OUTPUT_BYTES", cfg.MaxToolOutputBytes); err != nil {
		return Config{}, err
	}
	if cfg.LLMTimeout, err = envDuration("MINIG_LLM_TIMEOUT", cfg.LLMTimeout); err != nil {
		return Config{}, err
	}
	if cfg.ToolTimeout, err = envDuration("MINIG_TOOL_TIMEOUT", cfg.ToolTimeout); err != nil {
		return Config{}, err
	}
	if cfg.AllowWrite, err = envBool("MINIG_ALLOW_WRITE", cfg.AllowWrite); err != nil {
		return Config{}, err
	}
	if cfg.AllowRemoteLLM, err = envBool("MINIG_ALLOW_REMOTE_LLM", cfg.AllowRemoteLLM); err != nil {
		return Config{}, err
	}
	if cfg.MaxRetries, err = envInt("MINIG_MAX_RETRIES", cfg.MaxRetries); err != nil {
		return Config{}, err
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate verifies bounds and normalizes URL/path fields.
func (c *Config) Validate() error {
	normalizedURL, err := validateLlamaURL(c.LlamaURL, c.AllowRemoteLLM)
	if err != nil {
		return err
	}
	c.LlamaURL = normalizedURL

	if strings.TrimSpace(c.Model) == "" {
		return errors.New("model must not be empty")
	}

	if strings.TrimSpace(c.WorkspaceMode) == "" {
		c.WorkspaceMode = "local"
	}
	c.WorkspaceMode = strings.ToLower(strings.TrimSpace(c.WorkspaceMode))
	switch c.WorkspaceMode {
	case "none", "ephemeral", "local":
		// valid
	default:
		return fmt.Errorf("invalid workspace mode %q: expected 'none', 'ephemeral', or 'local'", c.WorkspaceMode)
	}

	if c.WorkspaceMode != "none" {
		if strings.TrimSpace(c.WorkspaceRoot) == "" {
			return errors.New("workspace root must not be empty when workspace mode is not none")
		}
		absRoot, err := filepath.Abs(c.WorkspaceRoot)
		if err != nil {
			return fmt.Errorf("resolve workspace root: %w", err)
		}
		c.WorkspaceRoot = absRoot
	}

	if c.MaxMicroTasks < 1 || c.MaxMicroTasks > 64 {
		return fmt.Errorf("max micro-tasks must be between 1 and 64, got %d", c.MaxMicroTasks)
	}
	if c.MaxStepsPerTask < 1 || c.MaxStepsPerTask > 16 {
		return fmt.Errorf("max steps per task must be between 1 and 16, got %d", c.MaxStepsPerTask)
	}
	if c.TaskContextTokens < 256 {
		return fmt.Errorf("task context tokens must be at least 256, got %d", c.TaskContextTokens)
	}
	if c.TotalRunTokens < c.TaskContextTokens {
		return fmt.Errorf("total run tokens (%d) must be >= task context tokens (%d)", c.TotalRunTokens, c.TaskContextTokens)
	}
	if c.MaxToolOutputBytes < 256 || c.MaxToolOutputBytes > 1<<20 {
		return fmt.Errorf("max tool output bytes must be between 256 and 1048576, got %d", c.MaxToolOutputBytes)
	}
	if c.LLMTimeout <= 0 {
		return errors.New("LLM timeout must be greater than zero")
	}
	if c.ToolTimeout <= 0 {
		return errors.New("tool timeout must be greater than zero")
	}
	if c.Temperature < 0 || c.Temperature > 2 {
		return fmt.Errorf("temperature must be between 0 and 2, got %v", c.Temperature)
	}
	if c.MaxRetries < 0 || c.MaxRetries > 5 {
		return fmt.Errorf("max retries must be between 0 and 5, got %d", c.MaxRetries)
	}
	return nil
}

func validateLlamaURL(raw string, allowRemote bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parse llama URL: %w", err)
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("llama URL must be http/https with a host and no credentials, query, or fragment")
	}
	if !allowRemote {
		host := parsed.Hostname()
		if !strings.EqualFold(host, "localhost") {
			ip := net.ParseIP(host)
			if ip == nil || !ip.IsLoopback() {
				return "", errors.New("llama URL must use localhost or a loopback IP unless AllowRemoteLLM is enabled")
			}
		}
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return strings.TrimRight(parsed.String(), "/"), nil
}

func envInt(key string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	val, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", key, err)
	}
	return val, nil
}

func envDuration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	val, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", key, err)
	}
	return val, nil
}

func envBool(key string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	val, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", key, err)
	}
	return val, nil
}
