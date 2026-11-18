package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

const (
	defaultTerminalTimeout = 30 * time.Second
	maxTerminalTimeout     = 120 * time.Second
	maxTerminalOutputBytes = 32 * 1024
)

type workspaceTerminalTool struct {
	ws *safety.Workspace
}

func (t *workspaceTerminalTool) Name() string   { return "workspace_terminal" }
func (t *workspaceTerminalTool) ReadOnly() bool { return false }
func (t *workspaceTerminalTool) Description() string {
	return "Execute a shell command within the workspace directory. Only active in workspace_mode='local' when allow_write is enabled."
}

func (t *workspaceTerminalTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["command"],
		"properties": {
			"command": {"type": "string", "description": "Shell command line to execute"},
			"timeout_seconds": {"type": "integer", "description": "Command execution timeout in seconds (default 30, max 120)"}
		}
	}`)
}

func (t *workspaceTerminalTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse workspace_terminal arguments: %w", err)
	}

	cmdStr := strings.TrimSpace(args.Command)
	if cmdStr == "" {
		return "", errors.New("command must not be empty")
	}

	// Safety check: block destructive commands that target system root or hardware
	lower := strings.ToLower(cmdStr)
	dangerous := []string{"rm -rf /", "rm -rf /*", "mkfs", "dd if=/dev", ":(){ :|:& };:", "shutdown", "reboot"}
	for _, d := range dangerous {
		if strings.Contains(lower, d) {
			return "", fmt.Errorf("command rejected: dangerous pattern %q detected", d)
		}
	}

	if del := EditorDelegateFrom(ctx); del != nil && del.SupportsTerminal() {
		return del.CreateTerminal(ctx, cmdStr, t.ws.Root())
	}

	timeout := defaultTerminalTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
		if timeout > maxTerminalTimeout {
			timeout = maxTerminalTimeout
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "sh", "-c", cmdStr)
	cmd.Dir = t.ws.Root()

	output, runErr := cmd.CombinedOutput()
	exitCode := 0
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}

	outStr := textutil.TruncateBytes(string(output), maxTerminalOutputBytes)

	if errors.Is(cmdCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("Command timed out after %s\nPartial output:\n%s", timeout, outStr),
			errors.New("command execution timed out")
	}

	if runErr != nil {
		return fmt.Sprintf("Exit code: %d (FAILED)\n%s", exitCode, outStr),
			fmt.Errorf("command exited with code %d: %s", exitCode, strings.TrimSpace(outStr))
	}

	if strings.TrimSpace(outStr) == "" {
		return fmt.Sprintf("Exit code: %d (completed with no output)", exitCode), nil
	}

	return fmt.Sprintf("Exit code: %d\n%s", exitCode, strings.TrimRight(outStr, "\n")), nil
}
