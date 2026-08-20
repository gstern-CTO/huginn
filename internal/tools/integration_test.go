//go:build integration

// Integration tests make real network calls to GitHub and, where configured,
// to Databricks. They are gated behind a build tag so that `go test ./...`
// stays hermetic and offline.
//
// Run them with:
//
//	GITHUB_TOKEN=ghp_... go test -tags=integration ./internal/tools/
package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

func newIntegrationServer(t *testing.T) *Server {
	t.Helper()
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Skip("GITHUB_TOKEN is not set")
	}

	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.GitHubToken = token
	cfg.EnableLocal = true
	cfg.WorkspaceRoot = root
	cfg.MetricsEnabled = false
	cfg.CacheDir = filepath.Join(t.TempDir(), "cache")

	srv, err := NewServer(cfg, discardLogger())
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	return srv
}

func callLive(t *testing.T, srv *Server, name string, args map[string]any) *protocol.Envelope {
	t.Helper()
	var handler toolFunc
	for _, registered := range srv.tools() {
		if registered.tool.Name == name {
			handler = registered.handler
		}
	}
	require.NotNil(t, handler)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	return handler(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
}

func TestIntegrationCodeSearch(t *testing.T) {
	srv := newIntegrationServer(t)

	env := callLive(t, srv, "github_search_code", map[string]any{
		"queries": []any{
			map[string]any{"keywords": []any{"ServeStdio"}, "owner": "mark3labs", "repo": "mcp-go"},
		},
		"limit": 3,
	})
	require.NotEqual(t, protocol.StatusError, env.Status, "%+v", env.Error)
	require.NotEmpty(t, env.Hints)
}

// Several queries in one call must actually run concurrently, not one after
// another (WEAKNESSES.md #1).
func TestIntegrationBulkSearchRunsInParallel(t *testing.T) {
	srv := newIntegrationServer(t)

	queries := []any{}
	for _, kw := range []string{"ServeStdio", "NewMCPServer", "CallToolRequest", "ToolHandlerFunc"} {
		queries = append(queries, map[string]any{
			"keywords": []any{kw}, "owner": "mark3labs", "repo": "mcp-go",
		})
	}

	start := time.Now()
	env := callLive(t, srv, "github_search_code", map[string]any{"queries": queries, "concise": true, "limit": 3})
	elapsed := time.Since(start)

	require.NotEqual(t, protocol.StatusError, env.Status, "%+v", env.Error)
	// Four sequential code searches against GitHub reliably exceed this;
	// four concurrent ones do not.
	require.Less(t, elapsed, 20*time.Second, "bulk queries appear to be running sequentially")
}

func TestIntegrationFileContentIsCached(t *testing.T) {
	srv := newIntegrationServer(t)

	args := map[string]any{"owner": "mark3labs", "repo": "mcp-go", "path": "README.md", "minify": "none"}

	first := callLive(t, srv, "github_file_content", args)
	require.Equal(t, protocol.StatusHasResults, first.Status, "%+v", first.Error)
	require.False(t, first.Metadata.CacheHit, "the first fetch cannot be a cache hit")

	second := callLive(t, srv, "github_file_content", args)
	require.Equal(t, protocol.StatusHasResults, second.Status)
	require.True(t, second.Metadata.CacheHit, "the same file must not be fetched twice in a session")
}

func TestIntegrationRepoStructurePaginates(t *testing.T) {
	srv := newIntegrationServer(t)

	env := callLive(t, srv, "github_repo_structure", map[string]any{
		"owner": "mark3labs", "repo": "mcp-go", "depth": 2, "pageSize": 5, "page": 1,
	})
	require.Equal(t, protocol.StatusHasResults, env.Status, "%+v", env.Error)
	require.LessOrEqual(t, env.Metadata.ResultCount, 5)
	require.True(t, env.Metadata.HasMore, "a five-entry page of this repository must report more")
}

func TestIntegrationRepoSearchDeduplicates(t *testing.T) {
	srv := newIntegrationServer(t)

	env := callLive(t, srv, "github_search_repos", map[string]any{
		"keywords": []any{"model context protocol"},
		"topics":   []any{"mcp"},
		"language": "Go",
		"limit":    10,
	})
	require.NotEqual(t, protocol.StatusError, env.Status, "%+v", env.Error)

	data := env.Data.(map[string]any)
	repos := data["repositories"].([]repoResult)
	seen := map[string]bool{}
	for _, r := range repos {
		require.False(t, seen[r.FullName], "duplicate repository %s across queries", r.FullName)
		seen[r.FullName] = true
	}
}

func TestIntegrationPullRequestDeepRead(t *testing.T) {
	srv := newIntegrationServer(t)

	env := callLive(t, srv, "github_search_pull_requests", map[string]any{
		"owner": "mark3labs", "repo": "mcp-go", "state": "merged", "limit": 2, "deepRead": true,
	})
	require.NotEqual(t, protocol.StatusError, env.Status, "%+v", env.Error)
}

// A bad owner must produce a structured NOT_FOUND, not an opaque failure.
func TestIntegrationErrorsAreStructured(t *testing.T) {
	srv := newIntegrationServer(t)

	env := callLive(t, srv, "github_file_content", map[string]any{
		"owner": "gstern-CTO", "repo": "definitely-does-not-exist-xyz", "path": "README.md",
	})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeNotFound, env.Error.Code)
	require.False(t, env.Error.Retryable)
	require.NotEmpty(t, env.Error.Hint)
}

// Requires a language server on PATH; skipped otherwise.
func TestIntegrationLSPNavigation(t *testing.T) {
	srv := newIntegrationServer(t)
	if !srv.runner.Available("gopls") {
		t.Skip("gopls is not installed")
	}

	root := srv.guard.PrimaryRoot()
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/probe\n\ngo 1.22\n"), 0o644))
	source := "package probe\n\ntype Widget struct{}\n\nfunc (w Widget) Name() string { return \"w\" }\n\nfunc use() string { return Widget{}.Name() }\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "probe.go"), []byte(source), 0o644))

	env := callLive(t, srv, "lsp_navigate", map[string]any{
		"operation": "documentSymbol", "path": "probe.go",
	})
	require.Equal(t, protocol.StatusHasResults, env.Status, "%+v", env.Error)

	data := env.Data.(map[string]any)
	require.False(t, data["usedFallback"].(bool), "gopls is installed, so the fallback must not be used")
}

func TestIntegrationDatabricksQuery(t *testing.T) {
	if os.Getenv("DATABRICKS_DEV_HOST") == "" {
		t.Skip("DATABRICKS_DEV_HOST is not set")
	}
	cfg, _, err := config.Load("")
	require.NoError(t, err)
	cfg.MetricsEnabled = false

	srv, err := NewServer(cfg, discardLogger())
	require.NoError(t, err)
	t.Cleanup(srv.Close)

	env := callLive(t, srv, "databricks_query", map[string]any{"statement": "SELECT 1 AS probe"})
	require.NotEqual(t, protocol.StatusError, env.Status, "%+v", env.Error)
}
