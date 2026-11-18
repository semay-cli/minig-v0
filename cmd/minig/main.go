package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/semay-cli/minig-v0/internal/acp"
	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm/llamacpp"
	"github.com/semay-cli/minig-v0/internal/mcpserver"
	"github.com/semay-cli/minig-v0/internal/safety"
	"github.com/semay-cli/minig-v0/internal/server"
	"github.com/semay-cli/minig-v0/internal/tool"
	"github.com/semay-cli/minig-v0/internal/tool/builtin"
	"github.com/semay-cli/minig-v0/internal/tool/mcp"
	"github.com/semay-cli/minig-v0/internal/ui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "minig: %v\n", err)
		os.Exit(1)
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, "; ") }
func (m *multiFlag) Set(v string) error {
	if trimmed := strings.TrimSpace(v); trimmed != "" {
		*m = append(*m, trimmed)
	}
	return nil
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runIO(ctx, args, os.Stdin, stdout, stderr)
}

func runIO(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	cmd, rest := splitCommand(args)

	fs := flag.NewFlagSet("minig", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		healthOnly  bool
		versionFlag bool
		serveMode   bool
		quiet       bool
		jsonOut     bool
	)
	port := cfg.ServerPort

	fs.StringVar(&cfg.LlamaURL, "url", cfg.LlamaURL, "llama.cpp server base URL")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model identifier")
	fs.StringVar(&cfg.WorkspaceRoot, "workspace", cfg.WorkspaceRoot, "workspace root directory")
	fs.StringVar(&cfg.WorkspaceMode, "workspace-mode", cfg.WorkspaceMode, "workspace mode: local, none, or ephemeral")
	fs.StringVar(&cfg.ServerHost, "host", cfg.ServerHost, "bind address for minig serve")
	fs.IntVar(&cfg.MaxMicroTasks, "max-tasks", cfg.MaxMicroTasks, "maximum micro-tasks per run")
	fs.IntVar(&cfg.MaxStepsPerTask, "max-steps", cfg.MaxStepsPerTask, "maximum reasoning/tool steps per micro-task")
	fs.IntVar(&cfg.TaskContextTokens, "ctx-tokens", cfg.TaskContextTokens, "token budget per isolated micro-task")
	fs.IntVar(&cfg.TotalRunTokens, "total-tokens", cfg.TotalRunTokens, "total token budget across the entire run")
	fs.BoolVar(&cfg.AllowWrite, "write", cfg.AllowWrite, "enable workspace file writes (write_file tool)")
	fs.BoolVar(&cfg.AllowRemoteLLM, "allow-remote", cfg.AllowRemoteLLM, "allow non-loopback llama.cpp server URLs")
	fs.BoolVar(&healthOnly, "health", false, "check llama.cpp server health and exit")
	fs.BoolVar(&versionFlag, "version", false, "print minig version and exit")
	fs.BoolVar(&serveMode, "serve", false, "run as an HTTP REST and SSE API server")
	fs.IntVar(&port, "port", port, "HTTP API server port (default 8080)")
	fs.BoolVar(&quiet, "quiet", false, "suppress micro-task progress output on stderr")
	fs.BoolVar(&jsonOut, "json", false, "emit structured JSON output on stdout")
	mcpSpecs := multiFlag(append([]string(nil), cfg.MCPServers...))
	fs.Var(&mcpSpecs, "mcp", "stdio MCP server spec as name='command args...' (repeatable)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage:\n  minig [flags] <goal>\n  minig serve [--host 0.0.0.0 --port 8080]\n  minig acp\n  minig mcp\n  minig --serve [--port 8080] [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(rest); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if versionFlag {
		fmt.Fprintf(stdout, "minig v0.1.0\n")
		return nil
	}

	modeFlag, workspaceFlag, writeFlag := visitedFlags(fs)
	if cmd == "serve" && !modeFlag && os.Getenv("MINIG_WORKSPACE_MODE") == "" && !workspaceFlag {
		cfg.WorkspaceMode = "none"
		cfg.WorkspaceRoot = ""
	}
	if (cmd == "acp" || cmd == "mcp") && !writeFlag && os.Getenv("MINIG_ALLOW_WRITE") == "" {
		cfg.AllowWrite = true
	}
	cfg.ServerPort = port

	if err := cfg.Validate(); err != nil {
		return err
	}

	switch cmd {
	case "mcp":
		reg, cleanup, err := buildToolRegistry(cfg, false)
		if err != nil {
			return err
		}
		defer cleanup()
		closeMCP, err := attachExternalMCP(ctx, cfg, reg, mcpSpecs)
		if err != nil {
			return err
		}
		defer closeMCP()
		return mcpserver.Serve(ctx, reg, stdin, stdout)
	case "acp":
		client, err := llamacpp.NewClientWithAuth(cfg.LlamaURL, cfg.LlamaAPIKey, cfg.LLMTimeout, cfg.AllowRemoteLLM)
		if err != nil {
			return err
		}
		defer client.Close()
		return acp.New(cfg, client, stdin, stdout).Serve(ctx)
	}

	client, err := llamacpp.NewClientWithAuth(cfg.LlamaURL, cfg.LlamaAPIKey, cfg.LLMTimeout, cfg.AllowRemoteLLM)
	if err != nil {
		return err
	}
	defer client.Close()

	if healthOnly {
		if err := client.CheckHealth(ctx); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "healthy (%s)\n", cfg.LlamaURL)
		return nil
	}

	reg, cleanup, err := buildToolRegistry(cfg, cmd == "serve" || serveMode)
	if err != nil {
		return err
	}
	defer cleanup()
	closeMCP, err := attachExternalMCP(ctx, cfg, reg, mcpSpecs)
	if err != nil {
		return err
	}
	defer closeMCP()

	if cmd == "serve" || serveMode {
		srv, err := server.NewServer(cfg, client, reg)
		if err != nil {
			return err
		}

		addr := net.JoinHostPort(cfg.ServerHost, strconv.Itoa(port))
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}

		httpServer := &http.Server{Handler: srv.Handler()}
		fmt.Fprintf(stderr, "minig API server listening on http://%s (workspace_mode: %s, model: %s)\n", listener.Addr().String(), cfg.WorkspaceMode, cfg.Model)

		serverErr := make(chan error, 1)
		go func() {
			if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverErr <- err
			}
		}()

		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			return httpServer.Shutdown(shutdownCtx)
		case err := <-serverErr:
			return err
		}
	}

	goal := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if goal == "" {
		return errors.New("goal is required (example: minig \"summarize the Go files in this repo\")")
	}

	renderer := ui.NewRenderer(stdout, stderr, quiet, jsonOut)
	eng, err := agent.NewEngine(cfg, client, reg, renderer)
	if err != nil {
		return err
	}

	res, err := eng.Run(ctx, goal)
	if err != nil {
		return err
	}
	return renderer.RenderResult(res)
}

func splitCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "", args
	}
	switch args[0] {
	case "serve", "acp", "mcp":
		return args[0], args[1:]
	default:
		return "", args
	}
}

func visitedFlags(fs *flag.FlagSet) (mode, workspace, write bool) {
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "workspace-mode":
			mode = true
		case "workspace":
			workspace = true
		case "write":
			write = true
		}
	})
	return mode, workspace, write
}

func buildToolRegistry(cfg config.Config, forServe bool) (*tool.Registry, func(), error) {
	cleanup := func() {}
	reg, err := tool.NewRegistry(cfg.MaxToolOutputBytes, cfg.ToolTimeout)
	if err != nil {
		return nil, cleanup, err
	}
	if cfg.WorkspaceMode == "none" || (forServe && cfg.WorkspaceMode == "ephemeral") {
		if err := builtin.RegisterHeadless(reg); err != nil {
			return nil, cleanup, err
		}
		return reg, cleanup, nil
	}

	root := cfg.WorkspaceRoot
	if cfg.WorkspaceMode == "ephemeral" {
		sb, err := safety.NewSandbox()
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { _ = sb.Cleanup() }
		root = sb.Dir()
	}
	ws, err := safety.NewWorkspace(root)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if err := builtin.RegisterAll(reg, ws, cfg.AllowWrite); err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return reg, cleanup, nil
}

func attachExternalMCP(ctx context.Context, cfg config.Config, reg *tool.Registry, specs []string) (func(), error) {
	var closers []func()
	cleanup := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	for _, spec := range specs {
		srvCfg, err := mcp.ParseServerSpec(spec)
		if err != nil {
			cleanup()
			return func() {}, err
		}
		mcpClient, err := mcp.NewStdioClient(ctx, srvCfg)
		if err != nil {
			cleanup()
			return func() {}, err
		}
		closers = append(closers, func() { _ = mcpClient.Close() })
		initCtx, initCancel := context.WithTimeout(ctx, cfg.ToolTimeout)
		err = mcpClient.Initialize(initCtx)
		if err == nil {
			_, err = mcp.RegisterTools(initCtx, reg, mcpClient)
		}
		initCancel()
		if err != nil {
			cleanup()
			return func() {}, err
		}
	}
	return cleanup, nil
}
