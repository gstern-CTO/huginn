package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// clearConfigEnv removes every variable the loader reads, so a test starts from
// a known state regardless of the developer's shell.
func clearConfigEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"OCTOCODE_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GITHUB_API_URL",
		"ENABLE_LOCAL", "WORKSPACE_ROOT", "ALLOWED_PATHS",
		"REQUEST_TIMEOUT_SECONDS", "MAX_RETRIES", "MAX_RESPONSE_TOKENS",
		"GITHUB_CONCURRENCY", "CACHE_DIR", "CACHE_TTL_SECONDS",
		"METRICS_ENABLED", "METRICS_PORT", "DATABRICKS_MAX_ROWS",
		"DATABRICKS_HOST", "DATABRICKS_TOKEN", "DATABRICKS_WAREHOUSE_ID",
		"DATABRICKS_DEV_HOST", "DATABRICKS_DEV_TOKEN", "DATABRICKS_DEV_WAREHOUSE_ID",
		"DATABRICKS_PROD_HOST", "DATABRICKS_PROD_TOKEN", "DATABRICKS_PROD_WAREHOUSE_ID",
		"GO_RESEARCH_MCP_CONFIG", "MAX_SUBPROCESS_OUTPUT_BYTES", "LARGE_FILE_BYTES",
		"FIND_RESULT_LIMIT",
	} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	// Keep the gh CLI out of the picture: it would otherwise supply a token
	// on a developer machine and change what these tests observe.
	t.Setenv("PATH", "")
}

func writeConfigFile(t *testing.T, contents map[string]any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	raw, err := json.Marshal(contents)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
	return path
}

func TestConfigDefaultsApplyWhenNothingIsSet(t *testing.T) {
	clearConfigEnv(t)

	cfg, warnings, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Equal(t, DefaultGitHubAPIURL, cfg.GitHubAPIURL)
	require.False(t, cfg.EnableLocal, "local access must be off unless explicitly enabled")
	require.Equal(t, protocol.DefaultMaxTokens, cfg.MaxResponseTokens)
	require.Equal(t, defaultGitHubConcurrency, cfg.GitHubConcurrency)
	require.NotEmpty(t, warnings, "a missing GitHub token must warn rather than fail")
}

// Environment variables always win over the config file, which always wins over
// the built-in defaults.
func TestConfigEnvironmentOverridesFile(t *testing.T) {
	clearConfigEnv(t)

	path := writeConfigFile(t, map[string]any{
		"github_token":        "file-token",
		"max_response_tokens": 1234,
		"github_concurrency":  2,
		"metrics_port":        7000,
	})

	t.Setenv("GITHUB_TOKEN", "env-token")
	t.Setenv("MAX_RESPONSE_TOKENS", "9999")

	cfg, _, err := Load(path)
	require.NoError(t, err)
	require.Equal(t, "env-token", cfg.GitHubToken, "env must beat the file")
	require.Equal(t, 9999, cfg.MaxResponseTokens, "env must beat the file")
	require.Equal(t, 2, cfg.GitHubConcurrency, "the file must beat the default")
	require.Equal(t, 7000, cfg.MetricsPort, "the file must beat the default")
}

func TestConfigTokenResolutionOrder(t *testing.T) {
	t.Run("OCTOCODE_TOKEN wins", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("OCTOCODE_TOKEN", "octocode")
		t.Setenv("GH_TOKEN", "gh")
		t.Setenv("GITHUB_TOKEN", "github")

		cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
		require.NoError(t, err)
		require.Equal(t, "octocode", cfg.GitHubToken)
	})

	t.Run("GH_TOKEN beats GITHUB_TOKEN", func(t *testing.T) {
		clearConfigEnv(t)
		t.Setenv("GH_TOKEN", "gh")
		t.Setenv("GITHUB_TOKEN", "github")

		cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
		require.NoError(t, err)
		require.Equal(t, "gh", cfg.GitHubToken)
	})

	t.Run("file supplies the token when no env var does", func(t *testing.T) {
		clearConfigEnv(t)
		path := writeConfigFile(t, map[string]any{"github_token": "from-file"})

		cfg, warnings, err := Load(path)
		require.NoError(t, err)
		require.Equal(t, "from-file", cfg.GitHubToken)
		require.Empty(t, warnings)
	})
}

// Local tools without a valid workspace root is a misconfiguration that must be
// caught at startup, not on the first tool call.
func TestConfigFailsFastOnMissingWorkspaceRoot(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ENABLE_LOCAL", "true")
	t.Setenv("WORKSPACE_ROOT", filepath.Join(t.TempDir(), "does-not-exist"))

	_, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "workspace root")
}

func TestConfigRejectsLocalToolsWithoutWorkspaceRoot(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("ENABLE_LOCAL", "true")

	_, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "WORKSPACE_ROOT")
}

func TestConfigAcceptsValidLocalSetup(t *testing.T) {
	clearConfigEnv(t)
	root := t.TempDir()
	extra := t.TempDir()
	t.Setenv("ENABLE_LOCAL", "true")
	t.Setenv("WORKSPACE_ROOT", root)
	t.Setenv("ALLOWED_PATHS", extra)

	cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.True(t, cfg.EnableLocal)
	require.Equal(t, filepath.Clean(root), cfg.WorkspaceRoot)
	require.Equal(t, []string{filepath.Clean(extra)}, cfg.AllowedPaths)
}

func TestConfigDatabricksEnvironments(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABRICKS_DEV_HOST", "https://dev.cloud.databricks.com")
	t.Setenv("DATABRICKS_DEV_TOKEN", "dev-token")
	t.Setenv("DATABRICKS_DEV_WAREHOUSE_ID", "wh-dev")
	t.Setenv("DATABRICKS_PROD_HOST", "https://prod.cloud.databricks.com")
	t.Setenv("DATABRICKS_PROD_TOKEN", "prod-token")
	t.Setenv("DATABRICKS_PROD_WAREHOUSE_ID", "wh-prod")

	cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.True(t, cfg.Databricks["dev"].Configured())
	require.True(t, cfg.Databricks["prod"].Configured())
	require.Equal(t, "wh-prod", cfg.Databricks["prod"].WarehouseID)
}

func TestConfigRejectsNonHTTPSDatabricksHost(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("DATABRICKS_DEV_HOST", "http://insecure.example.com")

	_, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "https")
}

func TestConfigDurationParsing(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("REQUEST_TIMEOUT_SECONDS", "45")
	t.Setenv("CACHE_TTL_SECONDS", "2h")

	cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Equal(t, 45*time.Second, cfg.RequestTimeout, "a bare integer means seconds")
	require.Equal(t, 2*time.Hour, cfg.CacheTTL, "a Go duration string is also accepted")
}

func TestConfigMalformedFileIsAnError(t *testing.T) {
	clearConfigEnv(t)
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte("{not json"), 0o600))

	_, _, err := Load(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "parse")
}

func TestConfigNormalisesAPIURL(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("GITHUB_API_URL", "https://github.example.com/api/v3")

	cfg, _, err := Load(filepath.Join(t.TempDir(), "absent.json"))
	require.NoError(t, err)
	require.Equal(t, "https://github.example.com/api/v3/", cfg.GitHubAPIURL,
		"go-github requires a trailing slash on the base URL")
}
