package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

const (
	defaultPythonTimeout = 30 * time.Second
	maxPythonTimeout     = 120 * time.Second
	maxPythonOutputBytes = 32 * 1024 // 32KB
)

type sandboxPythonTool struct{}

func (t *sandboxPythonTool) Name() string   { return "sandbox_python" }
func (t *sandboxPythonTool) ReadOnly() bool { return true }
func (t *sandboxPythonTool) Description() string {
	return "Execute a standalone Python 3 script in an ephemeral sandbox. If context_data is provided, it is automatically written to context.json in the working directory for the script to load. The sandbox directory is wiped with zero disk residue when execution finishes."
}

func (t *sandboxPythonTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["script"],
		"properties": {
			"script": {"type": "string", "description": "Complete Python 3 script source code to run"},
			"context_data": {"type": "string", "description": "Optional JSON payload written as context.json in the sandbox"},
			"timeout_seconds": {"type": "integer", "description": "Maximum script execution time in seconds (default 30, max 120)"}
		}
	}`)
}

func (t *sandboxPythonTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		Script         string `json:"script"`
		ContextData    string `json:"context_data"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse sandbox_python arguments: %w", err)
	}

	trimmedScript := strings.TrimSpace(args.Script)
	if trimmedScript == "" {
		return "", errors.New("script must not be empty")
	}

	// Create ephemeral sandbox directory /tmp/minig-run-<id> and always remove it.
	sb, err := safety.NewSandbox()
	if err != nil {
		return "", fmt.Errorf("create python sandbox: %w", err)
	}
	defer sb.Cleanup()
	tmpDir := sb.Dir()

	// Mount explicit context_data, or the per-run payload when the model omits it.
	if strings.TrimSpace(args.ContextData) == "" {
		args.ContextData = RunContextData(ctx)
	}
	// Write context.json if context_data is provided
	if trimmedContext := strings.TrimSpace(args.ContextData); trimmedContext != "" {
		contextPath := filepath.Join(tmpDir, "context.json")
		if err := os.WriteFile(contextPath, []byte(trimmedContext), 0o600); err != nil {
			return "", fmt.Errorf("write context.json: %w", err)
		}
	}

	// Write script.py
	scriptPath := filepath.Join(tmpDir, "script.py")
	if err := os.WriteFile(scriptPath, []byte(trimmedScript), 0o600); err != nil {
		return "", fmt.Errorf("write script.py: %w", err)
	}

	timeout := defaultPythonTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > maxPythonTimeout {
			timeout = maxPythonTimeout
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "python3", "script.py")
	cmd.Dir = tmpDir
	// Prevent interactive prompts or buffer hangs
	cmd.Env = append(os.Environ(), "PYTHONUNBUFFERED=1", "PYTHONDONTWRITEBYTECODE=1")

	output, runErr := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	outStr := textutil.TruncateBytes(string(output), maxPythonOutputBytes)

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("Python execution timed out after %s\nPartial output:\n%s", timeout, outStr),
			errors.New("python script execution timed out")
	}

	if runErr != nil {
		return fmt.Sprintf("Exit code: %d (FAILED)\n%s", exitCode, outStr),
			fmt.Errorf("python script failed with exit code %d: %s", exitCode, strings.TrimSpace(outStr))
	}

	if strings.TrimSpace(outStr) == "" {
		return fmt.Sprintf("Exit code: %d (completed with no stdout/stderr)", exitCode), nil
	}

	return fmt.Sprintf("Exit code: %d\n%s", exitCode, strings.TrimRight(outStr, "\n")), nil
}
