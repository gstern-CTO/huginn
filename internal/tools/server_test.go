package tools

import (
	"github.com/mark3labs/mcp-go/server"

	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestServer builds a server over a temporary workspace with metrics off.
func newTestServer(t *testing.T, enableLocal bool) (*Server, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)

	cfg := config.Defaults()
	cfg.EnableLocal = enableLocal
	cfg.WorkspaceRoot = root
	cfg.MetricsEnabled = false
	cfg.CacheDir = filepath.Join(t.TempDir(), "cache")
	cfg.LargeFileBytes = 1024 // small threshold keeps the fixture small

	srv, err := NewServer(cfg, discardLogger())
	require.NoError(t, err)
	t.Cleanup(srv.Close)
	return srv, root
}

func call(t *testing.T, srv *Server, name string, args map[string]any) *protocol.Envelope {
	t.Helper()
	var handler toolFunc
	for _, registered := range srv.tools() {
		if registered.tool.Name == name {
			handler = registered.handler
			break
		}
	}
	require.NotNil(t, handler, "tool %s is not registered", name)

	env := handler(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name, Arguments: args},
	})
	require.NotNil(t, env)
	return env
}

// Local tools are registered only when local access is enabled: their absence
// is a deliberate security posture the agent should be able to observe.
func TestToolRegistrationDependsOnLocalAccess(t *testing.T) {
	remoteOnly, _ := newTestServer(t, false)
	names := toolNames(remoteOnly)
	require.Contains(t, names, "github_search_code")
	require.Contains(t, names, "databricks_query")
	require.NotContains(t, names, "local_file_content")
	require.NotContains(t, names, "lsp_navigate")

	full, _ := newTestServer(t, true)
	names = toolNames(full)
	require.Contains(t, names, "local_search_code")
	require.Contains(t, names, "local_file_content")
	require.Contains(t, names, "local_find_files")
	require.Contains(t, names, "local_directory_structure")
	require.Contains(t, names, "lsp_navigate")
	require.Len(t, names, 11, "5 GitHub + 4 local + LSP + Databricks")
}

func toolNames(s *Server) []string {
	var names []string
	for _, registered := range s.tools() {
		names = append(names, registered.tool.Name)
	}
	return names
}

func TestEveryToolHasADescriptionAndSchema(t *testing.T) {
	srv, _ := newTestServer(t, true)
	for _, registered := range srv.tools() {
		require.NotEmpty(t, registered.tool.Description, "%s has no description", registered.tool.Name)
		require.Equal(t, "object", registered.tool.InputSchema.Type, "%s has no object schema", registered.tool.Name)
	}
}

// A GitHub tool without a token must explain what to set rather than failing
// opaquely.
func TestGitHubToolsReportMissingConfiguration(t *testing.T) {
	srv, _ := newTestServer(t, false)

	env := call(t, srv, "github_search_code", map[string]any{
		"queries": []any{map[string]any{"keywords": []any{"http"}}},
	})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeNotConfigured, env.Error.Code)
	require.False(t, env.Error.Retryable)
	require.Contains(t, env.Error.Hint, "GITHUB_TOKEN")
}

// The large-file error must carry the actual size, the threshold and a concrete
// next step: an agent that is merely told "too large" is stuck.
func TestLargeFileErrorIsActionable(t *testing.T) {
	srv, root := newTestServer(t, true)
	big := filepath.Join(root, "big.log")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("line of log output\n", 500)), 0o644))

	env := call(t, srv, "local_file_content", map[string]any{"path": "big.log"})

	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeFileTooLarge, env.Error.Code)
	require.Contains(t, env.Error.Details, "sizeBytes")
	require.Contains(t, env.Error.Details, "thresholdBytes")
	require.Contains(t, env.Error.Details, "suggestedAction")
	require.Contains(t, env.Error.Hint, "startLine")
	require.Contains(t, env.Error.Hint, "matchString")
}

// The same file becomes readable once the caller bounds the read, which is what
// the error told it to do.
func TestLargeFileIsReadableWithALineRange(t *testing.T) {
	srv, root := newTestServer(t, true)
	big := filepath.Join(root, "big.log")
	require.NoError(t, os.WriteFile(big, []byte(strings.Repeat("line of log output\n", 500)), 0o644))

	env := call(t, srv, "local_file_content", map[string]any{
		"path": "big.log", "startLine": 10, "endLine": 12,
	})
	require.Equal(t, protocol.StatusHasResults, env.Status)

	data := env.Data.(map[string]any)
	body := strings.TrimRight(data["content"].(string), "\n")
	require.Equal(t, 3, strings.Count(body, "\n")+1)
}

func TestLargeFileMatchStringReturnsAWindow(t *testing.T) {
	srv, root := newTestServer(t, true)
	lines := strings.Repeat("filler\n", 200) + "NEEDLE here\n" + strings.Repeat("filler\n", 200)
	require.NoError(t, os.WriteFile(filepath.Join(root, "big.log"), []byte(lines), 0o644))

	env := call(t, srv, "local_file_content", map[string]any{
		"path": "big.log", "matchString": "NEEDLE", "contextLines": 2,
	})
	require.Equal(t, protocol.StatusHasResults, env.Status)

	data := env.Data.(map[string]any)
	require.Contains(t, data["content"], "NEEDLE here")
	// The anchor is structured data, not a comment inside the content, so
	// minification cannot strip it.
	require.Equal(t, 201, data["matchLine"])
	require.Equal(t, "NEEDLE", data["matchString"])
}

func TestBinaryFileIsRefused(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(root, "image.bin"), []byte{0x89, 0x50, 0x00, 0x01, 0x02}, 0o644))

	env := call(t, srv, "local_file_content", map[string]any{"path": "image.bin"})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeBinaryFile, env.Error.Code)
}

// The workspace boundary must hold when reached through a tool, not only in the
// guard's own unit tests.
func TestToolsEnforceWorkspaceBoundary(t *testing.T) {
	srv, _ := newTestServer(t, true)

	for _, tool := range []string{"local_file_content", "local_search_code", "local_directory_structure", "local_find_files"} {
		args := map[string]any{"path": "/etc/passwd"}
		if tool == "local_search_code" {
			args["pattern"] = "root"
		}
		env := call(t, srv, tool, args)
		require.Equal(t, protocol.StatusError, env.Status, tool)
		require.Equal(t, protocol.CodePathDenied, env.Error.Code, tool)
	}
}

func TestLocalToolsDisabledWithoutOptIn(t *testing.T) {
	srv, _ := newTestServer(t, false)
	// The tool is not registered at all, so the guard is checked directly.
	tErr := srv.requireLocal("local_file_content")
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeToolDisabled, tErr.Code)
	require.Contains(t, tErr.Hint, "ENABLE_LOCAL")
}

// Secrets must not leave the process even when the file the agent asked for is
// entirely legitimate.
func TestSecretsAreRedactedThroughTheTool(t *testing.T) {
	srv, root := newTestServer(t, true)
	source := "package main\n\nconst token = \"ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "creds.go"), []byte(source), 0o644))

	env := call(t, srv, "local_file_content", map[string]any{"path": "creds.go", "minify": "none"})
	require.Equal(t, protocol.StatusHasResults, env.Status)

	data := env.Data.(map[string]any)
	require.NotContains(t, data["content"], "ghp_016C7f6a9B2c3D4e5F6a7B8c9D0e1F2a3B4c5D")
	require.Positive(t, env.Metadata.RedactionCount, "the caller must be told redaction happened")
}

// A blocked credential file is refused even though it sits inside the workspace.
func TestBlockedFilesAreRefusedThroughTheTool(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1\n"), 0o600))

	env := call(t, srv, "local_file_content", map[string]any{"path": ".env"})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodePathDenied, env.Error.Code)
}

func TestDirectoryStructureDepthOne(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "pkg", "deep"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "pkg", "lib.go"), []byte("package pkg\n"), 0o644))

	env := call(t, srv, "local_directory_structure", map[string]any{"depth": 1})
	require.Equal(t, protocol.StatusHasResults, env.Status)

	data := env.Data.(map[string]any)
	entries := data["entries"].([]dirEntryOut)
	var paths []string
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	require.ElementsMatch(t, []string{"main.go", "pkg"}, paths,
		"depth 1 lists this directory only, via a single ReadDir")
}

func TestFindFilesByExtension(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "sub"), 0o755))
	for _, name := range []string{"a.go", "b.go", "c.txt", "sub/d.go"} {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("x"), 0o644))
	}

	env := call(t, srv, "local_find_files", map[string]any{"extensions": []any{"go"}})
	require.Equal(t, protocol.StatusHasResults, env.Status)
	require.Equal(t, 3, env.Metadata.ResultCount)

	env = call(t, srv, "local_find_files", map[string]any{"namePattern": "*.txt"})
	require.Equal(t, 1, env.Metadata.ResultCount)
}

// A refused SQL statement must never reach the network: there is no Databricks
// host configured here, so a network attempt would surface as a different error.
func TestDatabricksRejectsMutationBeforeAnyNetworkCall(t *testing.T) {
	srv, _ := newTestServer(t, false)

	env := call(t, srv, "databricks_query", map[string]any{"statement": "DROP TABLE telemetry.dns"})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeForbiddenSQL, env.Error.Code)
}

func TestDatabricksUnconfiguredEnvironmentIsReported(t *testing.T) {
	srv, _ := newTestServer(t, false)

	env := call(t, srv, "databricks_query", map[string]any{"statement": "SELECT 1"})
	require.Equal(t, protocol.StatusError, env.Status)
	require.Equal(t, protocol.CodeNotConfigured, env.Error.Code)
	require.Contains(t, env.Error.Hint, "DATABRICKS_DEV_HOST")
}

// Production must be an explicit choice: omitting env resolves to dev.
func TestDatabricksDefaultsToDevEnvironment(t *testing.T) {
	srv, _ := newTestServer(t, false)

	env := call(t, srv, "databricks_query", map[string]any{"statement": "SELECT 1"})
	require.Contains(t, env.Error.Message, `"dev"`)
}

// Every response carries the envelope contract, whatever happened.
func TestEnvelopeContractIsAlwaysHonoured(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644))

	success := call(t, srv, "local_file_content", map[string]any{"path": "main.go"})
	success.Finalize()
	require.Equal(t, protocol.StatusHasResults, success.Status)
	require.NotEmpty(t, success.Hints, "a successful response must still suggest what to do next")
	require.LessOrEqual(t, len(success.Hints), protocol.MaxHints)
	require.Positive(t, success.Metadata.EstimatedTokens)
	require.Nil(t, success.Error)

	failure := call(t, srv, "local_file_content", map[string]any{"path": "absent.go"})
	require.Equal(t, protocol.StatusError, failure.Status)
	require.NotNil(t, failure.Error)
	require.NotEmpty(t, failure.Error.Code)
	require.NotEmpty(t, failure.Error.Message)
	require.NotEmpty(t, failure.Error.Hint)
	require.NotEmpty(t, failure.Hints)
}

// The MCP layer must flag a failure as a protocol error as well as carrying the
// structured envelope, so clients that only inspect isError still notice.
func TestWrapMarksProtocolErrors(t *testing.T) {
	srv, _ := newTestServer(t, true)

	handler := srv.wrap("test_tool", func(context.Context, mcp.CallToolRequest) *protocol.Envelope {
		return protocol.Failure(protocol.ErrInvalidInput("bad argument"))
	})
	result, err := handler(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, result.IsError)

	handler = srv.wrap("test_tool", func(context.Context, mcp.CallToolRequest) *protocol.Envelope {
		return protocol.Success(map[string]any{"ok": true}, 1)
	})
	result, err = handler(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.False(t, result.IsError)
}

// A handler that returns nothing must still produce a well-formed envelope
// rather than a nil dereference somewhere downstream.
func TestWrapHandlesNilEnvelope(t *testing.T) {
	srv, _ := newTestServer(t, true)

	handler := srv.wrap("test_tool", func(context.Context, mcp.CallToolRequest) *protocol.Envelope { return nil })
	result, err := handler(context.Background(), mcp.CallToolRequest{})
	require.NoError(t, err)
	require.True(t, result.IsError)
}

func TestWrapEmitsValidJSON(t *testing.T) {
	srv, root := newTestServer(t, true)
	require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644))

	var handler server.ToolHandlerFunc
	for _, registered := range srv.tools() {
		if registered.tool.Name == "local_file_content" {
			handler = srv.wrap(registered.tool.Name, registered.handler)
		}
	}
	require.NotNil(t, handler)

	result, err := handler(context.Background(), mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: "local_file_content", Arguments: map[string]any{"path": "main.go"}},
	})
	require.NoError(t, err)
	require.NotEmpty(t, result.Content)

	text, ok := result.Content[0].(mcp.TextContent)
	require.True(t, ok)

	var decoded protocol.Envelope
	require.NoError(t, json.Unmarshal([]byte(text.Text), &decoded))
	require.Equal(t, protocol.StatusHasResults, decoded.Status)
}

// The MCP server assembles without error and exposes every registered tool.
func TestMCPServerAssembles(t *testing.T) {
	srv, _ := newTestServer(t, true)
	require.NotNil(t, srv.MCPServer())
	require.Equal(t, 11, srv.ToolCount())
}

// writeFile is a fixture helper for tests in this package.
func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0o644)
}

// When the language server cannot answer and the ripgrep fallback cannot run
// either, the response must be an error. Reporting that as an empty result
// would have the agent conclude the symbol has no references, which is a
// confidently wrong answer rather than a missing one.
func TestLSPDoubleFailureIsAnErrorNotAnEmptyResult(t *testing.T) {
	srv, root := newTestServer(t, true)
	if srv.runner.Available("rg") {
		t.Skip("ripgrep is installed, so the fallback can run")
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "notes.txt"), []byte("Anything appears here\n"), 0o644))

	// A .txt file has no configured language server, forcing the fallback.
	env := call(t, srv, "lsp_navigate", map[string]any{
		"operation": "definition", "path": "notes.txt", "symbol": "Anything",
	})

	require.Equal(t, protocol.StatusError, env.Status,
		"a double failure must not be reported as an empty result")
	require.Equal(t, protocol.CodeDependencyMiss, env.Error.Code)
	require.Contains(t, env.Error.Details, "fallbackError")
	require.NotEmpty(t, env.Error.Hint)
}
