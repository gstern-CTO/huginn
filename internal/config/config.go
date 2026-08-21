package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gstern-CTO/huginn/internal/protocol"
	"github.com/gstern-CTO/huginn/internal/security"
)

// Defaults. Every one of these is overridable through the environment or the
// JSON config file; they are chosen so that the zero-configuration case is the
// safe case (no local filesystem access, dev Databricks only).
const (
	DefaultGitHubAPIURL      = "https://api.github.com/"
	defaultRequestTimeout    = 30 * time.Second
	defaultMaxRetries        = 3
	defaultGitHubConcurrency = 5
	defaultLargeFileBytes    = 1 << 20 // 1MB
	defaultCacheTTL          = 6 * time.Hour
	defaultMetricsPort       = 9090
	defaultDatabricksMaxRows = 1000
	defaultFindResultLimit   = 200
)

// DatabricksEnv is one addressable Databricks workspace. Environments are keyed
// by name ("dev", "prod"); prod is never the default (WEAKNESSES.md #9).
type DatabricksEnv struct {
	Host        string `json:"host"`
	Token       string `json:"token"`
	WarehouseID string `json:"warehouse_id"`
}

func (d DatabricksEnv) Configured() bool {
	return d.Host != "" && d.Token != "" && d.WarehouseID != ""
}

// Config is the fully resolved runtime configuration.
type Config struct {
	GitHubToken  string
	GitHubAPIURL string

	EnableLocal   bool
	WorkspaceRoot string
	AllowedPaths  []string

	RequestTimeout    time.Duration
	MaxRetries        int
	MaxResponseTokens int

	GitHubConcurrency   int
	MaxSubprocessOutput int
	LargeFileBytes      int64
	FindResultLimit     int

	CacheDir string
	CacheTTL time.Duration

	MetricsEnabled bool
	MetricsPort    int

	Databricks        map[string]DatabricksEnv
	DatabricksMaxRows int
}

// fileConfig mirrors Config for the on-disk JSON representation. Durations are
// expressed as plain seconds so the file stays hand-editable.
type fileConfig struct {
	GitHubToken       *string                  `json:"github_token"`
	GitHubAPIURL      *string                  `json:"github_api_url"`
	EnableLocal       *bool                    `json:"enable_local"`
	WorkspaceRoot     *string                  `json:"workspace_root"`
	AllowedPaths      []string                 `json:"allowed_paths"`
	RequestTimeoutSec *int                     `json:"request_timeout_seconds"`
	MaxRetries        *int                     `json:"max_retries"`
	MaxResponseTokens *int                     `json:"max_response_tokens"`
	GitHubConcurrency *int                     `json:"github_concurrency"`
	CacheDir          *string                  `json:"cache_dir"`
	CacheTTLSec       *int                     `json:"cache_ttl_seconds"`
	MetricsEnabled    *bool                    `json:"metrics_enabled"`
	MetricsPort       *int                     `json:"metrics_port"`
	Databricks        map[string]DatabricksEnv `json:"databricks"`
	DatabricksMaxRows *int                     `json:"databricks_max_rows"`
}

// DefaultConfigPath is the fallback JSON config location.
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".go-research-mcp", "config.json")
}

func defaultCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "go-research-mcp")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".go-research-mcp", "cache")
	}
	return filepath.Join(os.TempDir(), "go-research-mcp")
}

func defaultConfig() *Config {
	return &Config{
		GitHubAPIURL:        DefaultGitHubAPIURL,
		RequestTimeout:      defaultRequestTimeout,
		MaxRetries:          defaultMaxRetries,
		MaxResponseTokens:   protocol.DefaultMaxTokens,
		GitHubConcurrency:   defaultGitHubConcurrency,
		MaxSubprocessOutput: security.DefaultMaxOutput,
		LargeFileBytes:      defaultLargeFileBytes,
		FindResultLimit:     defaultFindResultLimit,
		CacheDir:            defaultCacheDir(),
		CacheTTL:            defaultCacheTTL,
		MetricsEnabled:      true,
		MetricsPort:         defaultMetricsPort,
		Databricks:          map[string]DatabricksEnv{},
		DatabricksMaxRows:   defaultDatabricksMaxRows,
	}
}

// Load resolves configuration from, in increasing order of precedence:
// built-in defaults, the JSON config file, then environment variables.
// Environment variables always win over the file.
func Load(configPath string) (*Config, []string, error) {
	cfg := defaultConfig()
	var warnings []string

	if configPath == "" {
		configPath = envStr("GO_RESEARCH_MCP_CONFIG", DefaultConfigPath())
	}
	if configPath != "" {
		if err := applyFileConfig(cfg, configPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, nil, fmt.Errorf("config file %s: %w", configPath, err)
		}
	}

	applyEnvConfig(cfg)

	// Token resolution order is explicit: OCTOCODE_TOKEN, GH_TOKEN,
	// GITHUB_TOKEN, then whatever the config file held, then the gh CLI.
	if tok := firstNonEmpty(os.Getenv("OCTOCODE_TOKEN"), os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); tok != "" {
		cfg.GitHubToken = tok
	}
	if cfg.GitHubToken == "" {
		if tok := ghCLIToken(); tok != "" {
			cfg.GitHubToken = tok
		}
	}
	if cfg.GitHubToken == "" {
		// Not fatal: local-only mode is a legitimate way to run the server.
		warnings = append(warnings, "no GitHub token found (checked OCTOCODE_TOKEN, GH_TOKEN, GITHUB_TOKEN, config file, gh CLI); GitHub tools will be unavailable")
	}

	if err := cfg.normalize(); err != nil {
		return nil, warnings, err
	}
	if err := cfg.validate(); err != nil {
		return nil, warnings, err
	}
	if cfg.EnableLocal && len(cfg.Databricks) == 0 {
		// Purely informational; Databricks is optional.
		warnings = append(warnings, "no Databricks environments configured; databricks_query will report a configuration error")
	}
	return cfg, warnings, nil
}

func applyFileConfig(cfg *Config, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var fc fileConfig
	if err := json.Unmarshal(raw, &fc); err != nil {
		return fmt.Errorf("parse: %w", err)
	}
	setIf(&cfg.GitHubToken, fc.GitHubToken)
	setIf(&cfg.GitHubAPIURL, fc.GitHubAPIURL)
	setIf(&cfg.EnableLocal, fc.EnableLocal)
	setIf(&cfg.WorkspaceRoot, fc.WorkspaceRoot)
	setIf(&cfg.MaxRetries, fc.MaxRetries)
	setIf(&cfg.MaxResponseTokens, fc.MaxResponseTokens)
	setIf(&cfg.GitHubConcurrency, fc.GitHubConcurrency)
	setIf(&cfg.CacheDir, fc.CacheDir)
	setIf(&cfg.MetricsEnabled, fc.MetricsEnabled)
	setIf(&cfg.MetricsPort, fc.MetricsPort)
	setIf(&cfg.DatabricksMaxRows, fc.DatabricksMaxRows)
	if fc.AllowedPaths != nil {
		cfg.AllowedPaths = fc.AllowedPaths
	}
	if fc.RequestTimeoutSec != nil {
		cfg.RequestTimeout = time.Duration(*fc.RequestTimeoutSec) * time.Second
	}
	if fc.CacheTTLSec != nil {
		cfg.CacheTTL = time.Duration(*fc.CacheTTLSec) * time.Second
	}
	if fc.Databricks != nil {
		cfg.Databricks = fc.Databricks
	}
	return nil
}

func applyEnvConfig(cfg *Config) {
	cfg.GitHubAPIURL = envStr("GITHUB_API_URL", cfg.GitHubAPIURL)
	cfg.EnableLocal = envBool("ENABLE_LOCAL", cfg.EnableLocal)
	cfg.WorkspaceRoot = envStr("WORKSPACE_ROOT", cfg.WorkspaceRoot)
	cfg.RequestTimeout = envDur("REQUEST_TIMEOUT_SECONDS", cfg.RequestTimeout)
	cfg.MaxRetries = envInt("MAX_RETRIES", cfg.MaxRetries)
	cfg.MaxResponseTokens = envInt("MAX_RESPONSE_TOKENS", cfg.MaxResponseTokens)
	cfg.GitHubConcurrency = envInt("GITHUB_CONCURRENCY", cfg.GitHubConcurrency)
	cfg.MaxSubprocessOutput = envInt("MAX_SUBPROCESS_OUTPUT_BYTES", cfg.MaxSubprocessOutput)
	cfg.LargeFileBytes = int64(envInt("LARGE_FILE_BYTES", int(cfg.LargeFileBytes)))
	cfg.FindResultLimit = envInt("FIND_RESULT_LIMIT", cfg.FindResultLimit)
	cfg.CacheDir = envStr("CACHE_DIR", cfg.CacheDir)
	cfg.CacheTTL = envDur("CACHE_TTL_SECONDS", cfg.CacheTTL)
	cfg.MetricsEnabled = envBool("METRICS_ENABLED", cfg.MetricsEnabled)
	cfg.MetricsPort = envInt("METRICS_PORT", cfg.MetricsPort)
	cfg.DatabricksMaxRows = envInt("DATABRICKS_MAX_ROWS", cfg.DatabricksMaxRows)

	if v := os.Getenv("ALLOWED_PATHS"); v != "" {
		cfg.AllowedPaths = splitList(v)
	}

	// Databricks environments come from DATABRICKS_<ENV>_{HOST,TOKEN,WAREHOUSE_ID}.
	// The unsuffixed DATABRICKS_HOST form is treated as "dev" for convenience.
	if cfg.Databricks == nil {
		cfg.Databricks = map[string]DatabricksEnv{}
	}
	mergeDatabricksEnv(cfg, "dev", "DATABRICKS_HOST", "DATABRICKS_TOKEN", "DATABRICKS_WAREHOUSE_ID")
	for _, name := range []string{"dev", "prod"} {
		prefix := "DATABRICKS_" + strings.ToUpper(name)
		mergeDatabricksEnv(cfg, name, prefix+"_HOST", prefix+"_TOKEN", prefix+"_WAREHOUSE_ID")
	}
}

func mergeDatabricksEnv(cfg *Config, name, hostKey, tokenKey, warehouseKey string) {
	env := cfg.Databricks[name]
	env.Host = envStr(hostKey, env.Host)
	env.Token = envStr(tokenKey, env.Token)
	env.WarehouseID = envStr(warehouseKey, env.WarehouseID)
	if env.Host != "" || env.Token != "" || env.WarehouseID != "" {
		cfg.Databricks[name] = env
	}
}

// normalize expands and absolutizes paths so that later comparisons operate on
// canonical values.
func (c *Config) normalize() error {
	var err error
	if c.WorkspaceRoot != "" {
		if c.WorkspaceRoot, err = security.ExpandPath(c.WorkspaceRoot); err != nil {
			return fmt.Errorf("workspace root: %w", err)
		}
	}
	for i, p := range c.AllowedPaths {
		if c.AllowedPaths[i], err = security.ExpandPath(p); err != nil {
			return fmt.Errorf("allowed path %q: %w", p, err)
		}
	}
	if c.CacheDir != "" {
		if c.CacheDir, err = security.ExpandPath(c.CacheDir); err != nil {
			return fmt.Errorf("cache dir: %w", err)
		}
	}
	if !strings.HasSuffix(c.GitHubAPIURL, "/") {
		c.GitHubAPIURL += "/"
	}
	return nil
}

// validate fails fast on configurations that would misbehave at request time.
func (c *Config) validate() error {
	if c.EnableLocal {
		if c.WorkspaceRoot == "" {
			return errors.New("ENABLE_LOCAL is set but WORKSPACE_ROOT is empty: local tools need a workspace boundary")
		}
		info, err := os.Stat(c.WorkspaceRoot)
		if err != nil {
			return fmt.Errorf("workspace root %s is not accessible: %w", c.WorkspaceRoot, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("workspace root %s is not a directory", c.WorkspaceRoot)
		}
		for _, p := range c.AllowedPaths {
			if _, err := os.Stat(p); err != nil {
				return fmt.Errorf("allowed path %s is not accessible: %w", p, err)
			}
		}
	}
	if c.MaxResponseTokens <= 0 {
		return fmt.Errorf("max response tokens must be positive, got %d", c.MaxResponseTokens)
	}
	if c.GitHubConcurrency <= 0 {
		return fmt.Errorf("github concurrency must be positive, got %d", c.GitHubConcurrency)
	}
	if c.RequestTimeout <= 0 {
		return fmt.Errorf("request timeout must be positive, got %s", c.RequestTimeout)
	}
	if c.MetricsEnabled && (c.MetricsPort < 1 || c.MetricsPort > 65535) {
		return fmt.Errorf("metrics port %d out of range", c.MetricsPort)
	}
	for name, env := range c.Databricks {
		if env.Host != "" && !strings.HasPrefix(env.Host, "https://") {
			return fmt.Errorf("databricks %s host must be an https URL, got %q", name, env.Host)
		}
	}
	return nil
}

// HasGitHub reports whether GitHub-backed tools can operate.
func (c *Config) HasGitHub() bool { return c.GitHubToken != "" }

// ghCLIToken is the last-resort token source. It is best-effort: any failure
// (gh not installed, not logged in) simply yields no token.
func ghCLIToken() string {
	if _, err := exec.LookPath("gh"); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func splitList(v string) []string {
	parts := strings.Split(v, string(os.PathListSeparator))
	if len(parts) == 1 {
		parts = strings.Split(v, ",")
	}
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func envStr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func envBool(key string, def bool) bool {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return b
}

func envInt(key string, def int) int {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return def
	}
	return n
}

func envDur(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	v = strings.TrimSpace(v)
	if n, err := strconv.Atoi(v); err == nil {
		return time.Duration(n) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

// Defaults returns a configuration with only the built-in defaults applied.
// Tests and callers that construct a server programmatically start here.
func Defaults() *Config { return defaultConfig() }

// DefaultGitHubConcurrency caps in-flight GitHub requests so a bulk call cannot
// burn through the rate limit in one burst.
const DefaultGitHubConcurrency = defaultGitHubConcurrency
