package tools

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/gstern-CTO/huginn/internal/cache"
	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/databricks"
	"github.com/gstern-CTO/huginn/internal/ghclient"
	"github.com/gstern-CTO/huginn/internal/lsp"
	"github.com/gstern-CTO/huginn/internal/metrics"
	"github.com/gstern-CTO/huginn/internal/protocol"
	"github.com/gstern-CTO/huginn/internal/security"
)

const ServerName = "huginn"

// ServerVersion is a var, not a const, so a release build can inject the tag
// with -ldflags "-X .../internal/tools.ServerVersion=1.2.3". A const cannot be
// overridden that way, and a binary that misreports its own version is a
// support problem waiting to happen (Design Log #4).
var ServerVersion = "0.1.0-dev"

// Server holds every collaborator a tool might need. Tools receive it rather
// than reaching for globals, which keeps them testable.
type Server struct {
	cfg      *config.Config
	logger   *slog.Logger
	metrics  *metrics.Metrics
	cache    *cache.Cache
	redactor *security.Redactor
	runner   *security.Runner

	// guard is nil when local tools are disabled. Every local tool checks it
	// before touching the filesystem.
	guard *security.PathGuard
	// gh is nil when no GitHub token was resolved.
	gh  *ghclient.Client
	lsp *lsp.Manager
	dbx *databricks.Client
}

// toolFunc is the internal handler shape: it always returns an envelope,
// never a bare error, so every failure path is structured.
type toolFunc func(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope

// NewServer constructs the server and its collaborators. Failures to build an
// optional collaborator (GitHub without a token, Databricks without a host)
// disable the corresponding tools rather than aborting startup.
func NewServer(cfg *config.Config, logger *slog.Logger) (*Server, error) {
	metrics := metrics.NewMetrics(cfg.MetricsEnabled, cfg.MetricsPort)

	s := &Server{
		cfg:      cfg,
		logger:   logger,
		metrics:  metrics,
		cache:    cache.NewCache(cfg.CacheDir, cfg.CacheTTL, metrics),
		redactor: security.DefaultRedactor,
		runner:   security.NewRunner(cfg.MaxSubprocessOutput),
	}

	if cfg.EnableLocal {
		guard, err := security.NewPathGuard(cfg.WorkspaceRoot, cfg.AllowedPaths)
		if err != nil {
			return nil, err
		}
		s.guard = guard
		s.lsp = lsp.NewManager(s.runner, guard, metrics, logger)
	}

	if cfg.HasGitHub() {
		gh, err := ghclient.New(cfg, s.cache, metrics, logger)
		if err != nil {
			return nil, err
		}
		s.gh = gh
	}

	s.dbx = databricks.New(cfg, logger)
	return s, nil
}

// MCPServer builds the protocol server with every tool registered.
func (s *Server) MCPServer() *server.MCPServer {
	mcpSrv := server.NewMCPServer(
		ServerName,
		ServerVersion,
		server.WithToolCapabilities(true),
		server.WithRecovery(),
	)
	for _, t := range s.tools() {
		mcpSrv.AddTool(t.tool, s.wrap(t.tool.Name, t.handler))
	}
	return mcpSrv
}

type registeredTool struct {
	tool    mcp.Tool
	handler toolFunc
}

// tools returns every tool definition. GitHub tools are always registered so
// that an agent asking for them gets an actionable configuration error rather
// than "unknown tool"; local tools are registered only when local access is on,
// because their absence is a deliberate security posture the agent should see.
func (s *Server) tools() []registeredTool {
	tools := []registeredTool{
		{toolGitHubSearchCode(), s.handleGitHubSearchCode},
		{toolGitHubFileContent(), s.handleGitHubFileContent},
		{toolGitHubRepoStructure(), s.handleGitHubRepoStructure},
		{toolGitHubSearchRepos(), s.handleGitHubSearchRepos},
		{toolGitHubSearchPullRequests(), s.handleGitHubSearchPullRequests},
		{toolDatabricksQuery(), s.handleDatabricksQuery},
	}
	if s.cfg.EnableLocal {
		tools = append(tools,
			registeredTool{toolLocalSearchCode(), s.handleLocalSearchCode},
			registeredTool{toolLocalFileContent(), s.handleLocalFileContent},
			registeredTool{toolLocalFindFiles(), s.handleLocalFindFiles},
			registeredTool{toolLocalDirectoryStructure(), s.handleLocalDirectoryStructure},
			registeredTool{toolLSPNavigate(), s.handleLSPNavigate},
		)
	}
	return tools
}

// wrap adapts an internal toolFunc to the MCP handler signature, adding
// timeout, metrics, redaction accounting and envelope finalisation in one place
// so no individual tool can forget them.
func (s *Server) wrap(name string, fn toolFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		start := time.Now()

		ctx, cancel := context.WithTimeout(ctx, s.toolTimeout())
		defer cancel()

		env := fn(ctx, req)
		if env == nil {
			env = protocol.Failure(protocol.ErrInternal(context.Canceled))
		}
		env.Finalize()

		s.metrics.RecordToolCall(name, env.Status, time.Since(start))
		s.metrics.RecordRedactions(env.Metadata.RedactionCount)

		if env.Status == protocol.StatusError {
			s.logger.Warn("tool call failed",
				"tool", name,
				"code", env.Error.Code,
				"message", env.Error.Message,
				"retryable", env.Error.Retryable,
			)
		} else {
			s.logger.Debug("tool call complete",
				"tool", name,
				"status", env.Status,
				"results", env.Metadata.ResultCount,
				"tokens", env.Metadata.EstimatedTokens,
				"cacheHit", env.Metadata.CacheHit,
				"duration", time.Since(start).String(),
			)
		}

		payload, err := json.MarshalIndent(env, "", "  ")
		if err != nil {
			return mcp.NewToolResultError("failed to encode response: " + err.Error()), nil
		}

		result := mcp.NewToolResultStructured(env, string(payload))
		// Surfacing the protocol-level error flag as well as the envelope
		// status means clients that only look at isError still notice, while
		// clients that read the payload get the code, retry flag and hint.
		if env.Status == protocol.StatusError {
			result.IsError = true
		}
		return result, nil
	}
}

// toolTimeout allows a tool call somewhat longer than a single upstream
// request, since a bulk call issues several of them.
func (s *Server) toolTimeout() time.Duration {
	return s.cfg.RequestTimeout * 4
}

// requireGitHub is the guard every GitHub tool calls first.
func (s *Server) requireGitHub() *protocol.ToolError {
	if s.gh == nil {
		return protocol.NewError(protocol.CodeNotConfigured, false,
			"Check that GITHUB_TOKEN is set and has repo + read:org scopes.",
			"no GitHub token is configured, so GitHub tools are unavailable")
	}
	return nil
}

// requireLocal is the guard every filesystem-touching tool calls first.
func (s *Server) requireLocal(tool string) *protocol.ToolError {
	if !s.cfg.EnableLocal || s.guard == nil {
		return protocol.ErrToolDisabled(tool)
	}
	return nil
}

// budget returns a fresh token budget for one response.
func (s *Server) budget() *protocol.TokenBudget {
	return protocol.NewTokenBudget(s.cfg.MaxResponseTokens)
}

// redact scrubs a string and accumulates the count into the envelope metadata.
func (s *Server) redact(text string, meta *protocol.Metadata) string {
	cleaned, n := s.redactor.Redact(text)
	meta.RedactionCount += n
	return cleaned
}

// Close releases long-lived resources: language servers in particular hold
// child processes that must not outlive the server.
func (s *Server) Close() {
	if s.lsp != nil {
		s.lsp.Shutdown()
	}
}

// Metrics exposes the collector set so the entry point can start and stop the
// metrics endpoint without reaching into the server's internals.
func (s *Server) Metrics() *metrics.Metrics { return s.metrics }

// ToolCount reports how many tools are registered, for the startup log line.
func (s *Server) ToolCount() int { return len(s.tools()) }
