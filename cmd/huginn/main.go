package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/tools"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", tools.ServerName, err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath  = flag.String("config", "", "path to a JSON config file (default ~/.go-research-mcp/config.json)")
		logLevel    = flag.String("log-level", "info", "log verbosity: debug, info, warn, error")
		showVersion = flag.Bool("version", false, "print the version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Fprintf(os.Stderr, "%s %s\n", tools.ServerName, tools.ServerVersion)
		return nil
	}

	// stdout carries the MCP protocol and nothing else. Every log line, including
	// startup diagnostics, goes to stderr.
	logger := newLogger(*logLevel)

	cfg, warnings, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		logger.Warn(w)
	}

	srv, err := tools.NewServer(cfg, logger)
	if err != nil {
		return err
	}
	defer srv.Close()

	srv.Metrics().Serve(logger)

	logger.Info("starting",
		"version", tools.ServerVersion,
		"github", cfg.HasGitHub(),
		"localTools", cfg.EnableLocal,
		"workspaceRoot", cfg.WorkspaceRoot,
		"cacheDir", cfg.CacheDir,
		"tools", srv.ToolCount(),
	)

	// Shut down cleanly on a signal so language server child processes are not
	// orphaned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		errCh <- server.ServeStdio(srv.MCPServer())
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		if err != nil {
			return err
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Metrics().Shutdown(shutdownCtx)
	return nil
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
