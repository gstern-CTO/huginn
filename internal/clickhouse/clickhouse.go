// Package clickhouse runs read-only queries against ClickHouse Cloud over the
// HTTP interface.
//
// Two findings from Design Log #5, both verified against ClickHouse 24.10,
// shape this package:
//
//   - The server's readonly setting is not sufficient. readonly=1 blocks
//     url() and file(), but readonly=2 — common because it permits settings
//     changes — blocks writes while still executing them. A SELECT can
//     therefore reach the network. The callee denylist in the dialect below is
//     the enforcement; readonly=1 on the wire is only a backstop.
//   - max_result_rows does not cap a small result. A 100-row SELECT with
//     max_result_rows=2 returned all 100 under every readonly value, because
//     the limit applies at block granularity. The row cap is therefore
//     enforced here while decoding.
package clickhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/content"
	"github.com/gstern-CTO/huginn/internal/protocol"
	"github.com/gstern-CTO/huginn/internal/sqlguard"
)

// Dialect is ClickHouse's notion of a read-only statement.
//
// The forbidden list is wider than Databricks': ClickHouse adds cluster and
// storage administration verbs. WATCH is there for a different reason — it is
// not a mutation, it is a live query that never returns, and would hold a tool
// call open until the deadline.
var Dialect = sqlguard.Dialect{
	Name: "ClickHouse",
	AllowedLeading: map[string]bool{
		"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true,
		"DESC": true, "EXPLAIN": true, "EXISTS": true, "CHECK": true,
	},
	Forbidden: []string{
		"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER", "CREATE",
		"RENAME", "EXCHANGE", "ATTACH", "DETACH", "OPTIMIZE", "SYSTEM", "KILL",
		"GRANT", "REVOKE", "SET", "USE", "MOVE", "BACKUP", "RESTORE", "FREEZE",
		"UNFREEZE", "WATCH", "REPLACE", "MERGE",
	},
	// Table functions that reach outside the database. These run inside an
	// ordinary SELECT, so no verb check can catch them, and a readonly=2
	// server executes them happily.
	ForbiddenCallees: []string{
		"url", "urls", "file", "s3", "s3cluster", "remote", "remotesecure",
		"mysql", "postgresql", "jdbc", "odbc", "hdfs", "hdfscluster",
		"azureblobstorage", "deltalake", "iceberg", "hudi", "executable",
		"input", "sqlite", "mongodb", "redis", "gcs", "deltalakecluster",
		"icebergs3", "urlcluster", "filecluster",
	},
	DocsURL:     protocol.DocsReadOnlySQL,
	AllowedHint: "Only read queries are permitted. Rewrite this as a SELECT, SHOW, DESCRIBE, EXPLAIN or EXISTS.",
}

// ValidateReadOnlySQL refuses anything that is not a single read-only
// ClickHouse statement, before it reaches the network.
func ValidateReadOnlySQL(statement string) *protocol.ToolError {
	return Dialect.Validate(statement)
}

// QueryResult and QueryColumn are the shared SQL response shape.
type QueryResult = sqlguard.QueryResult
type QueryColumn = sqlguard.QueryColumn

// Client talks to ClickHouse Cloud's HTTP interface.
type Client struct {
	cfg    *config.Config
	http   *http.Client
	logger *slog.Logger
}

func New(cfg *config.Config, logger *slog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		http:   &http.Client{Timeout: cfg.RequestTimeout},
		logger: logger,
	}
}

// jsonCompactResponse is ClickHouse's FORMAT JSONCompact payload. `data` is
// already rows-as-arrays, which is exactly the shape QueryResult wants.
type jsonCompactResponse struct {
	Meta []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"meta"`
	Data       [][]any `json:"data"`
	Rows       int64   `json:"rows"`
	Statistics struct {
		Elapsed  float64 `json:"elapsed"`
		RowsRead int64   `json:"rows_read"`
	} `json:"statistics"`
}

// Execute runs an already-validated read-only statement against the named
// environment.
func (c *Client) Execute(ctx context.Context, envName, statement, database string, maxRows int) (*QueryResult, *protocol.ToolError) {
	env, ok := c.cfg.ClickHouse[envName]
	if !ok || !env.Configured() {
		upper := strings.ToUpper(envName)
		return nil, protocol.ErrNotConfigured(
			fmt.Sprintf("the ClickHouse %q environment", envName),
			fmt.Sprintf("Set CLICKHOUSE_%s_URL, CLICKHOUSE_%s_USER and CLICKHOUSE_%s_PASSWORD, then restart the server.",
				upper, upper, upper),
		)
	}
	if maxRows <= 0 || maxRows > c.cfg.ClickHouseMaxRows {
		maxRows = c.cfg.ClickHouseMaxRows
	}
	if database == "" {
		database = env.Database
	}

	// FORMAT is appended rather than expected from the caller: a statement
	// carrying its own FORMAT would produce a body this code cannot decode.
	body := strings.TrimRight(strings.TrimSpace(statement), ";") + "\nFORMAT JSONCompact"

	endpoint, err := c.endpoint(env, database, maxRows)
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}
	// Credentials go in headers, never in the query string, which lands in
	// server access logs.
	req.Header.Set("X-ClickHouse-User", env.User)
	req.Header.Set("X-ClickHouse-Key", env.Password)
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeNetwork, true,
			"Check the ClickHouse URL is reachable from this machine, then retry.",
			"ClickHouse request failed: %v", err)
	}
	defer resp.Body.Close()

	raw, err := content.ReadAllLimited(resp.Body, 64<<20)
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}

	if tErr := classify(resp.StatusCode, raw); tErr != nil {
		return nil, tErr
	}

	var parsed jsonCompactResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, protocol.ErrInternal(fmt.Errorf("decode ClickHouse response: %w", err))
	}

	result := &QueryResult{}
	for _, m := range parsed.Meta {
		result.Columns = append(result.Columns, QueryColumn{Name: m.Name, Type: m.Type})
	}

	// The row cap is applied here because the server's is unreliable at this
	// granularity — see the package comment.
	rows := parsed.Data
	if len(rows) > maxRows {
		rows = rows[:maxRows]
		result.Truncated = true
	}
	result.Rows = rows
	result.RowCount = len(rows)
	return result, nil
}

// endpoint builds the request URL with the settings that act as a backstop to
// the in-process validation.
func (c *Client) endpoint(env config.ClickHouseEnv, database string, maxRows int) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(env.URL, "/") + "/")
	if err != nil {
		return "", err
	}
	q := u.Query()
	if database != "" {
		q.Set("database", database)
	}
	// readonly=1 is the strictest mode: it refuses writes and, unlike
	// readonly=2, also refuses url() and file(). It is a backstop, not the
	// enforcement.
	q.Set("readonly", "1")
	// Coarse, and known not to bite at small values, but it still prevents a
	// ten-million-row accident from being streamed in full.
	q.Set("max_result_rows", strconv.Itoa(maxRows*10))
	q.Set("result_overflow_mode", "break")
	q.Set("max_execution_time", strconv.Itoa(int(c.cfg.RequestTimeout.Seconds())))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// classify turns a non-200 into a structured, actionable error. ClickHouse
// reports its own errors as `Code: N. DB::Exception: …` in the body, which is
// far more useful than the status alone.
func classify(status int, raw []byte) *protocol.ToolError {
	if status == http.StatusOK {
		return nil
	}
	message := strings.TrimSpace(content.TruncateToTokens(string(raw), 120))

	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return protocol.NewError(protocol.CodeAuth, false,
			"Check CLICKHOUSE_<ENV>_USER and CLICKHOUSE_<ENV>_PASSWORD for this environment.",
			"ClickHouse rejected the credentials (HTTP %d)", status)
	case status == http.StatusTooManyRequests:
		return protocol.NewError(protocol.CodeRateLimited, true,
			"The service is throttling. Wait and retry, or reduce query concurrency.",
			"ClickHouse rate limited the request")
	case status >= 500:
		return protocol.NewError(protocol.CodeUpstream, true,
			"ClickHouse is failing server-side; retrying shortly is reasonable.",
			"ClickHouse server error (HTTP %d): %s", status, message)
	}

	// A readonly refusal means our own validation let something through that
	// the server caught. Worth naming explicitly so the gap gets closed.
	if strings.Contains(message, "READONLY") || strings.Contains(message, "Cannot execute query in readonly mode") {
		return protocol.NewError(protocol.CodeForbiddenSQL, false,
			"This tool is read-only. Express the query as a SELECT.",
			"ClickHouse refused the statement in readonly mode: %s", message).
			WithDocs(protocol.DocsReadOnlySQL)
	}
	if strings.Contains(message, "TIMEOUT_EXCEEDED") || strings.Contains(message, "max_execution_time") {
		return protocol.NewError(protocol.CodeTimeout, true,
			"Narrow the time range, add a LIMIT, or aggregate in SQL to make the query cheaper.",
			"ClickHouse exceeded its execution time: %s", message)
	}

	return protocol.NewError(protocol.CodeUpstream, false,
		"ClickHouse rejected the query. Check the table name with SHOW TABLES and the columns with DESCRIBE.",
		"ClickHouse returned HTTP %d: %s", status, message)
}
