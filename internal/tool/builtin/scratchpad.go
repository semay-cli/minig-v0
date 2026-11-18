package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type scratchpadKeyType struct{}

// ScratchpadSessionKey is the context key for attaching a session/run ID to an execution context.
var ScratchpadSessionKey = scratchpadKeyType{}

// Global thread-safe session map: sessionID -> map[string]string
var (
	scratchpadMu sync.RWMutex
	scratchpads  = make(map[string]map[string]string)
)

// WithScratchpadSession returns a new context containing the session ID.
func WithScratchpadSession(ctx context.Context, sessionID string) context.Context {
	return context.WithValue(ctx, ScratchpadSessionKey, sessionID)
}

// CleanupScratchpad drops all in-memory keys for the specified session ID.
func CleanupScratchpad(sessionID string) {
	if sessionID == "" {
		return
	}
	scratchpadMu.Lock()
	delete(scratchpads, sessionID)
	scratchpadMu.Unlock()
}

type sessionScratchpadTool struct{}

func (t *sessionScratchpadTool) Name() string   { return "session_scratchpad" }
func (t *sessionScratchpadTool) ReadOnly() bool { return false }
func (t *sessionScratchpadTool) Description() string {
	return "Request-scoped in-memory key-value store for caching intermediate findings, query results, and hypotheses across multi-step reasoning turns. Automatically cleaned when the run finishes."
}

func (t *sessionScratchpadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"required": ["operation"],
		"properties": {
			"operation": {"type": "string", "description": "Action: set, get, list, delete"},
			"key": {"type": "string", "description": "Cache key name"},
			"value": {"type": "string", "description": "Value to store (required for set)"}
		}
	}`)
}

func (t *sessionScratchpadTool) Execute(ctx context.Context, rawArgs json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	var args struct {
		Operation string `json:"operation"`
		Key       string `json:"key"`
		Value     string `json:"value"`
	}
	if err := json.Unmarshal(rawArgs, &args); err != nil {
		return "", fmt.Errorf("parse session_scratchpad arguments: %w", err)
	}

	sessionID, _ := ctx.Value(ScratchpadSessionKey).(string)
	if sessionID == "" {
		sessionID = "default"
	}

	op := strings.ToLower(strings.TrimSpace(args.Operation))
	key := strings.TrimSpace(args.Key)

	scratchpadMu.Lock()
	defer scratchpadMu.Unlock()

	sessionMap, exists := scratchpads[sessionID]
	if !exists {
		sessionMap = make(map[string]string)
		scratchpads[sessionID] = sessionMap
	}

	switch op {
	case "set":
		if key == "" {
			return "", errors.New("key is required for set operation")
		}
		if len(args.Value) > 8*1024 {
			return "", errors.New("value exceeds 8KB limit")
		}
		if len(sessionMap) >= 100 {
			return "", errors.New("session scratchpad key limit (100) reached")
		}
		sessionMap[key] = args.Value
		return fmt.Sprintf("stored key %q (%d bytes)", key, len(args.Value)), nil

	case "get":
		if key == "" {
			return "", errors.New("key is required for get operation")
		}
		val, found := sessionMap[key]
		if !found {
			return fmt.Sprintf("key %q not found in scratchpad", key), nil
		}
		return val, nil

	case "list":
		if len(sessionMap) == 0 {
			return "(scratchpad is empty)", nil
		}
		keys := make([]string, 0, len(sessionMap))
		for k := range sessionMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var out strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&out, "%s (%d bytes)\n", k, len(sessionMap[k]))
		}
		return strings.TrimRight(out.String(), "\n"), nil

	case "delete":
		if key == "" {
			return "", errors.New("key is required for delete operation")
		}
		delete(sessionMap, key)
		return fmt.Sprintf("deleted key %q", key), nil

	default:
		return "", fmt.Errorf("unknown operation %q: must be set, get, list, or delete", op)
	}
}
