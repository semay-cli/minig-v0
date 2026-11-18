package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/semay-cli/minig-v0/internal/llm"
	"github.com/semay-cli/minig-v0/internal/textutil"
)

// Tool defines a single bounded capability exposed to micro-tasks.
type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	ReadOnly() bool
	Execute(ctx context.Context, rawArgs json.RawMessage) (string, error)
}

// Registry stores tools and enforces per-micro-task allowlists, timeouts, and output byte caps.
type Registry struct {
	mu             sync.RWMutex
	tools          map[string]Tool
	maxOutputBytes int
	timeout        time.Duration
}

// NewRegistry creates a bounded tool registry.
func NewRegistry(maxOutputBytes int, timeout time.Duration) (*Registry, error) {
	if maxOutputBytes < 256 {
		return nil, fmt.Errorf("max output bytes must be at least 256, got %d", maxOutputBytes)
	}
	if timeout <= 0 {
		return nil, errors.New("tool timeout must be greater than zero")
	}
	return &Registry{
		tools:          make(map[string]Tool),
		maxOutputBytes: maxOutputBytes,
		timeout:        timeout,
	}, nil
}

// Register adds a tool to the registry, rejecting duplicates or invalid schemas.
func (r *Registry) Register(t Tool) error {
	if t == nil {
		return errors.New("tool must not be nil")
	}
	name := strings.TrimSpace(t.Name())
	if name == "" {
		return errors.New("tool name must not be empty")
	}
	params := t.Parameters()
	if len(params) > 0 && !json.Valid(params) {
		return fmt.Errorf("tool %q has invalid parameter schema JSON", name)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.tools[name]; exists {
		return fmt.Errorf("tool %q is already registered", name)
	}
	r.tools[name] = t
	return nil
}

// Names returns all registered tool names in sorted order.
func (r *Registry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Specs returns schemas for the requested allowlist (or all tools if allowed is nil).
// Unknown names in allowed are ignored so planner hallucinations degrade gracefully.
func (r *Registry) Specs(allowed []string) []llm.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if allowed == nil {
		names := make([]string, 0, len(r.tools))
		for name := range r.tools {
			names = append(names, name)
		}
		sort.Strings(names)

		specs := make([]llm.ToolSpec, 0, len(names))
		for _, name := range names {
			specs = append(specs, toSpec(r.tools[name]))
		}
		return specs
	}

	seen := make(map[string]struct{}, len(allowed))
	specs := make([]llm.ToolSpec, 0, len(allowed))
	for _, rawName := range allowed {
		name := strings.TrimSpace(rawName)
		if _, dup := seen[name]; dup {
			continue
		}
		t, ok := r.tools[name]
		if !ok {
			continue
		}
		seen[name] = struct{}{}
		specs = append(specs, toSpec(t))
	}
	return specs
}

// Execute runs a named tool if it is permitted by allowed (nil permits all registered tools),
// enforcing the registry timeout and output byte ceiling.
func (r *Registry) Execute(ctx context.Context, allowed []string, name string, rawArgs json.RawMessage) (string, error) {
	trimmedName := strings.TrimSpace(name)
	if allowed != nil && !containsName(allowed, trimmedName) {
		return "", fmt.Errorf("tool %q is not permitted for this micro-task", trimmedName)
	}

	r.mu.RLock()
	t, ok := r.tools[trimmedName]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", trimmedName)
	}

	args := rawArgs
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	} else if !json.Valid(args) {
		return "", fmt.Errorf("invalid JSON arguments for tool %q", trimmedName)
	}

	callCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	out, err := t.Execute(callCtx, args)
	if err != nil {
		return "", err
	}
	return textutil.TruncateBytes(out, r.maxOutputBytes), nil
}

func toSpec(t Tool) llm.ToolSpec {
	params := t.Parameters()
	if len(params) == 0 {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return llm.ToolSpec{
		Name:        t.Name(),
		Description: t.Description(),
		Parameters:  params,
	}
}

func containsName(list []string, target string) bool {
	for _, item := range list {
		if strings.TrimSpace(item) == target {
			return true
		}
	}
	return false
}
