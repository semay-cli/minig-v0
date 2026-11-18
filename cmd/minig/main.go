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
	"strings"
	"syscall"
	"time"

	"github.com/semay-cli/minig-v0/internal/agent"
	"github.com/semay-cli/minig-v0/internal/config"
	"github.com/semay-cli/minig-v0/internal/llm/llamacpp"
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
	cfg, err := config.LoadFromEnv()
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("minig", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var (
		healthOnly  bool
		versionFlag bool
		serveMode   bool
		port        int = 8080
		quiet       bool
		jsonOut     bool
	)

	fs.StringVar(&cfg.LlamaURL, "url", cfg.LlamaURL, "llama.cpp server base URL")
	fs.StringVar(&cfg.Model, "model", cfg.Model, "model identifier")
	fs.StringVar(&cfg.WorkspaceRoot, "workspace", cfg.WorkspaceRoot, "workspace root directory")
	fs.IntVar(&cfg.MaxMicroTasks, "max-tasks", cfg.MaxMicroTasks, "maximum micro-tasks per run")
	fs.IntVar(&cfg.MaxStepsPerTask, "max-steps", cfg.MaxStepsPerTask, "maximum reasoning/tool steps per micro-task")
	fs.IntVar(&cfg.TaskContextTokens, "ctx-tokens", cfg.TaskContextTokens, "token budget per isolated micro-task")
	fs.IntVar(&cfg.TotalRunTokens, "total-tokens", cfg.TotalRunTokens, "total token budget across the entire run")
	fs.BoolVar(&cfg.AllowWrite, "write", cfg.AllowWrite, "enable workspace file writes (write_file tool)")
	fs.BoolVar(&cfg.AllowRemoteLLM, "allow-remote", cfg.AllowRemoteLLM, "allow non-loopback llama.cpp server URLs")
	fs.BoolVar(&healthOnly, "health", false, "check llama.cpp server health and exit")
	fs.BoolVar(&versionFlag, "version", false, "print minig version and exit")
	fs.BoolVar(&serveMode, "serve", false, "run as an HTTP REST and SSE API server")
	fs.IntVar(&port, "port", 8080, "HTTP API server port (default 8080)")
	fs.BoolVar(&quiet, "quiet", false, "suppress micro-task progress output on stderr")
	fs.BoolVar(&jsonOut, "json", false, "emit structured JSON output on stdout")
	mcpSpecs := multiFlag(append([]string(nil), cfg.MCPServers...))
	fs.Var(&mcpSpecs, "mcp", "stdio MCP server spec as name='command args...' (repeatable)")

	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: minig [flags] <goal>\n       minig --serve [--port 8080] [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}

	if versionFlag {
		fmt.Fprintf(stdout, "minig v0.1.0\n")
		return nil
	}

	if err := cfg.Validate(); err != nil {
		return err
	}

	client, err := llamacpp.NewClient(cfg.LlamaURL, cfg.LLMTimeout, cfg.AllowRemoteLLM)
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

	ws, err := safety.NewWorkspace(cfg.WorkspaceRoot)
	if err != nil {
		return err
	}

	reg, err := tool.NewRegistry(cfg.MaxToolOutputBytes, cfg.ToolTimeout)
	if err != nil {
		return err
	}
	if err := builtin.RegisterAll(reg, ws, cfg.AllowWrite); err != nil {
		return err
	}

	for _, spec := range mcpSpecs {
		srvCfg, err := mcp.ParseServerSpec(spec)
		if err != nil {
			return err
		}
		mcpClient, err := mcp.NewStdioClient(ctx, srvCfg)
		if err != nil {
			return err
		}
		defer mcpClient.Close()

		initCtx, initCancel := context.WithTimeout(ctx, cfg.ToolTimeout)
		err = mcpClient.Initialize(initCtx)
		if err == nil {
			_, err = mcp.RegisterTools(initCtx, reg, mcpClient)
		}
		initCancel()
		if err != nil {
			return err
		}
	}

	if serveMode {
		srv, err := server.NewServer(cfg, client, reg)
		if err != nil {
			return err
		}

		addr := fmt.Sprintf(":%d", port)
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen on %s: %w", addr, err)
		}

		httpServer := &http.Server{
			Handler: srv.Handler(),
		}
		fmt.Fprintf(stderr, "minig API server listening on http://127.0.0.1:%d (workspace: %s, model: %s)\n", port, cfg.WorkspaceRoot, cfg.Model)

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
