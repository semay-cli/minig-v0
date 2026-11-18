package config

import (
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
	LlamaURL           string
	Model              string
	WorkspaceRoot      string
	MCPServers         []string
	MaxMicroTasks      int
	MaxStepsPerTask    int
	TaskContextTokens  int
	TotalRunTokens     int
	MaxToolOutputBytes int
	LLMTimeout         time.Duration
	ToolTimeout        time.Duration
	Temperature        float64
	AllowWrite         bool
	AllowRemoteLLM     bool
}

// Default returns conservative, resource-efficient defaults tuned for local GGUF models.
func Default() Config {
	return Config{
		LlamaURL:           "http://127.0.0.1:8080",
		Model:              "local",
		WorkspaceRoot:      ".",
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
	}
}

// LoadFromEnv starts from Default() and applies MINIG_* environment overrides.
func LoadFromEnv() (Config, error) {
	cfg := Default()

	if v := strings.TrimSpace(os.Getenv("MINIG_LLAMA_URL")); v != "" {
		cfg.LlamaURL = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_MODEL")); v != "" {
		cfg.Model = v
	}
	if v := strings.TrimSpace(os.Getenv("MINIG_WORKSPACE")); v != "" {
		cfg.WorkspaceRoot = v
	}

	if v := strings.TrimSpace(os.Getenv("MINIG_MCP_SERVERS")); v != "" {
		for item := range strings.SplitSeq(v, ";") {
			if trimmed := strings.TrimSpace(item); trimmed != "" {
				cfg.MCPServers = append(cfg.MCPServers, trimmed)
			}
		}
	}

	var err error
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
	if strings.TrimSpace(c.WorkspaceRoot) == "" {
		return errors.New("workspace root must not be empty")
	}
	absRoot, err := filepath.Abs(c.WorkspaceRoot)
	if err != nil {
		return fmt.Errorf("resolve workspace root: %w", err)
	}
	c.WorkspaceRoot = absRoot

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
