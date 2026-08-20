package databricks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/content"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

// forbiddenStatements are the SQL verbs that can modify state. The check runs
// before the statement leaves this process — it is not delegated to a
// read-only credential, a warehouse setting, or Databricks itself
// (WEAKNESSES.md #9).
var forbiddenStatements = []string{
	"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER", "CREATE",
	"MERGE", "REPLACE", "GRANT", "REVOKE", "REFRESH", "RESTORE", "VACUUM",
	"OPTIMIZE", "COPY", "UPSERT", "SET", "RESET", "USE", "CALL", "EXECUTE",
	"ANALYZE", "COMMENT", "MSCK", "CACHE", "UNCACHE", "CLEAR",
}

// allowedLeadingKeywords are the verbs a statement may begin with.
var allowedLeadingKeywords = map[string]bool{
	"SELECT": true, "WITH": true, "SHOW": true, "DESCRIBE": true, "DESC": true,
	"EXPLAIN": true, "VALUES": true, "TABLE": true,
}

var wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

// sanitizeSQL removes comments and blanks the contents of string and identifier
// literals, leaving only the parts of the statement that carry SQL syntax.
//
// This has to be one pass rather than two regex passes: a comment marker can
// appear inside a literal ('--') and a quote can appear inside a comment
// (-- don't), so stripping either one first corrupts the other. It reports
// ok=false for an unterminated literal or comment, which is refused rather than
// guessed at.
func sanitizeSQL(s string) (string, bool) {
	var out strings.Builder
	out.Grow(len(s))

	for i := 0; i < len(s); {
		if strings.HasPrefix(s[i:], "--") {
			for i < len(s) && s[i] != '\n' {
				i++
			}
			out.WriteByte(' ')
			continue
		}
		if strings.HasPrefix(s[i:], "/*") {
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return out.String(), false
			}
			i += 2 + end + 2
			out.WriteByte(' ')
			continue
		}

		c := s[i]
		if c != '\'' && c != '"' && c != '`' {
			out.WriteByte(c)
			i++
			continue
		}

		// A literal: emit the delimiters but none of the content, so a
		// keyword appearing inside it is correctly treated as data.
		quote := c
		out.WriteByte(quote)
		i++
		closed := false
		for i < len(s) {
			if s[i] == '\\' && quote != '`' && i+1 < len(s) {
				i += 2 // backslash escape
				continue
			}
			if s[i] == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i += 2 // SQL escapes a quote by doubling it
					continue
				}
				closed = true
				out.WriteByte(quote)
				i++
				break
			}
			i++
		}
		if !closed {
			return out.String(), false
		}
	}
	return out.String(), true
}

// ValidateReadOnlySQL rejects anything that is not a read. It returns a
// structured error naming the offending keyword so the agent can rewrite the
// query rather than guess why it was refused.
func ValidateReadOnlySQL(statement string) *protocol.ToolError {
	trimmed := strings.TrimSpace(statement)
	if trimmed == "" {
		return protocol.ErrInvalidInput("statement must not be empty")
	}

	// Everything below scans the sanitised form: comments removed and literal
	// contents blanked, so no keyword can hide inside either.
	stripped, ok := sanitizeSQL(trimmed)
	if !ok {
		return protocol.ErrInvalidInput("statement has an unterminated string literal or block comment")
	}
	stripped = strings.TrimSpace(stripped)
	if stripped == "" || strings.Trim(stripped, "'\"`  \t\n") == "" {
		return protocol.ErrInvalidInput("statement contains only comments")
	}

	// Multiple statements are refused outright: allowing them would mean
	// validating each one and still risking a parser disagreement with
	// Databricks over where the boundaries are.
	if hasMultipleStatements(stripped) {
		return protocol.NewError(protocol.CodeForbiddenSQL, false,
			"Send exactly one SELECT statement per call.",
			"multiple SQL statements in one call are not allowed")
	}

	upperWords := wordRe.FindAllString(strings.ToUpper(stripped), -1)
	if len(upperWords) == 0 {
		return protocol.ErrInvalidInput("statement contains no SQL keywords")
	}

	// The statement must start with a read verb.
	if !allowedLeadingKeywords[upperWords[0]] {
		return protocol.NewError(protocol.CodeForbiddenSQL, false,
			"Only read queries are permitted. Rewrite this as a SELECT, SHOW, DESCRIBE or EXPLAIN.",
			"statement begins with %q, which is not a read operation", upperWords[0]).
			WithDetail("leadingKeyword", upperWords[0])
	}

	// And must not contain a mutating verb anywhere. Word-boundary matching
	// means a column named `created_at` or `update_time` is unaffected.
	present := map[string]bool{}
	for _, w := range upperWords {
		present[w] = true
	}
	for _, verb := range forbiddenStatements {
		if present[verb] {
			// CREATE inside EXPLAIN or a CTE name is still refused: the cost
			// of a false positive is an agent rewriting a query, while the
			// cost of a false negative is a write against production.
			return protocol.NewError(protocol.CodeForbiddenSQL, false,
				fmt.Sprintf("This tool is read-only. Remove %s from the statement; if you need the data it would produce, express it as a SELECT.", verb),
				"statement contains the forbidden keyword %s", verb).
				WithDetail("forbiddenKeyword", verb)
		}
	}
	return nil
}

// hasMultipleStatements reports whether a semicolon separates two statements,
// ignoring semicolons inside string literals and a single trailing one.
func hasMultipleStatements(s string) bool {
	inSingle, inDouble := false, false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case ';':
			if inSingle || inDouble {
				continue
			}
			if strings.TrimSpace(s[i+1:]) != "" {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

// Client talks to the SQL Statement Execution API.
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

// QueryResult is the structured shape returned to the caller: named columns and
// rows, not a raw API payload.
type QueryResult struct {
	Columns   []QueryColumn `json:"columns"`
	Rows      [][]any       `json:"rows"`
	RowCount  int           `json:"rowCount"`
	Truncated bool          `json:"truncated"`
}

type QueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type statementRequest struct {
	Statement     string `json:"statement"`
	WarehouseID   string `json:"warehouse_id"`
	Catalog       string `json:"catalog,omitempty"`
	Schema        string `json:"schema,omitempty"`
	WaitTimeout   string `json:"wait_timeout,omitempty"`
	OnWaitTimeout string `json:"on_wait_timeout,omitempty"`
	RowLimit      int    `json:"row_limit,omitempty"`
	Format        string `json:"format,omitempty"`
	Disposition   string `json:"disposition,omitempty"`
}

type statementResponse struct {
	StatementID string `json:"statement_id"`
	Status      struct {
		State string `json:"state"`
		Error *struct {
			ErrorCode string `json:"error_code"`
			Message   string `json:"message"`
		} `json:"error"`
	} `json:"status"`
	Manifest struct {
		Schema struct {
			Columns []struct {
				Name     string `json:"name"`
				TypeText string `json:"type_text"`
			} `json:"columns"`
		} `json:"schema"`
		TotalRowCount int64 `json:"total_row_count"`
		Truncated     bool  `json:"truncated"`
	} `json:"manifest"`
	Result struct {
		DataArray [][]any `json:"data_array"`
		RowCount  int64   `json:"row_count"`
	} `json:"result"`
}

// Execute runs a validated read-only statement against the named environment.
func (d *Client) Execute(ctx context.Context, envName, statement, catalog, schema string, maxRows int) (*QueryResult, *protocol.ToolError) {
	env, ok := d.cfg.Databricks[envName]
	if !ok || !env.Configured() {
		return nil, protocol.ErrNotConfigured(
			fmt.Sprintf("the Databricks %q environment", envName),
			fmt.Sprintf("Set DATABRICKS_%s_HOST, DATABRICKS_%s_TOKEN and DATABRICKS_%s_WAREHOUSE_ID, then restart the server.",
				strings.ToUpper(envName), strings.ToUpper(envName), strings.ToUpper(envName)),
		)
	}
	if maxRows <= 0 || maxRows > d.cfg.DatabricksMaxRows {
		maxRows = d.cfg.DatabricksMaxRows
	}

	body, err := json.Marshal(statementRequest{
		Statement:     statement,
		WarehouseID:   env.WarehouseID,
		Catalog:       catalog,
		Schema:        schema,
		WaitTimeout:   "30s",
		OnWaitTimeout: "CONTINUE",
		RowLimit:      maxRows,
		Format:        "JSON_ARRAY",
		Disposition:   "INLINE",
	})
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}

	resp, tErr := d.post(ctx, env, "/api/2.0/sql/statements", body)
	if tErr != nil {
		return nil, tErr
	}

	// A statement that exceeds the inline wait continues asynchronously; poll
	// until it settles rather than reporting a spurious timeout.
	deadline := time.Now().Add(d.cfg.RequestTimeout * 3)
	for resp.Status.State == "PENDING" || resp.Status.State == "RUNNING" {
		if time.Now().After(deadline) {
			return nil, protocol.NewError(protocol.CodeTimeout, true,
				"The query is still running. Narrow the time range, add a LIMIT, or aggregate in SQL to make it cheaper.",
				"query did not finish within %s", (d.cfg.RequestTimeout*3).String()).
				WithDetail("statementId", resp.StatementID)
		}
		if err := sleepContext(ctx, time.Second); err != nil {
			return nil, protocol.NewError(protocol.CodeTimeout, false, "The call was cancelled.", "cancelled while polling Databricks")
		}
		resp, tErr = d.get(ctx, env, "/api/2.0/sql/statements/"+resp.StatementID)
		if tErr != nil {
			return nil, tErr
		}
	}

	switch resp.Status.State {
	case "SUCCEEDED":
	case "FAILED":
		message := "the query failed"
		if resp.Status.Error != nil {
			message = resp.Status.Error.Message
		}
		return nil, protocol.NewError(protocol.CodeUpstream, false,
			"Databricks rejected the query. Check the table name with SHOW TABLES and the column names with DESCRIBE.",
			"Databricks reported: %s", message)
	case "CANCELED":
		return nil, protocol.NewError(protocol.CodeUpstream, true, "The statement was cancelled server-side; retrying is reasonable.", "query cancelled")
	default:
		return nil, protocol.NewError(protocol.CodeUpstream, true,
			"Retry the query; if the state persists, check the warehouse is running.",
			"unexpected Databricks statement state %q", resp.Status.State)
	}

	result := &QueryResult{
		Rows:      resp.Result.DataArray,
		Truncated: resp.Manifest.Truncated,
	}
	for _, col := range resp.Manifest.Schema.Columns {
		result.Columns = append(result.Columns, QueryColumn{Name: col.Name, Type: col.TypeText})
	}
	result.RowCount = len(result.Rows)
	if int64(result.RowCount) < resp.Manifest.TotalRowCount {
		result.Truncated = true
	}
	return result, nil
}

func (d *Client) post(ctx context.Context, env config.DatabricksEnv, path string, body []byte) (*statementResponse, *protocol.ToolError) {
	return d.do(ctx, env, http.MethodPost, path, body)
}

func (d *Client) get(ctx context.Context, env config.DatabricksEnv, path string) (*statementResponse, *protocol.ToolError) {
	return d.do(ctx, env, http.MethodGet, path, nil)
}

func (d *Client) do(ctx context.Context, env config.DatabricksEnv, method, path string, body []byte) (*statementResponse, *protocol.ToolError) {
	var reader *bytes.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	} else {
		reader = bytes.NewReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(env.Host, "/")+path, reader)
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.Token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := d.http.Do(req)
	if err != nil {
		return nil, protocol.NewError(protocol.CodeNetwork, true,
			"Check the Databricks host is reachable from this machine, then retry.",
			"Databricks request failed: %v", err)
	}
	defer resp.Body.Close()

	raw, err := content.ReadAllLimited(resp.Body, 32<<20)
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, protocol.NewError(protocol.CodeAuth, false,
			"Check the Databricks token for this environment is set and has access to the warehouse.",
			"Databricks rejected the credentials (HTTP %d)", resp.StatusCode)
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, protocol.NewError(protocol.CodeRateLimited, true,
			"The warehouse is saturated. Wait and retry, or reduce query concurrency.",
			"Databricks rate limited the request")
	case resp.StatusCode >= 500:
		return nil, protocol.NewError(protocol.CodeUpstream, true,
			"Databricks is failing server-side; retrying shortly is reasonable.",
			"Databricks server error (HTTP %d)", resp.StatusCode)
	case resp.StatusCode >= 400:
		return nil, protocol.NewError(protocol.CodeUpstream, false,
			"Databricks rejected the request. Verify the warehouse ID, catalog and schema.",
			"Databricks returned HTTP %d: %s", resp.StatusCode, content.TruncateToTokens(string(raw), 100))
	}

	var parsed statementResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, protocol.ErrInternal(fmt.Errorf("decode Databricks response: %w", err))
	}
	return &parsed, nil
}

// sleepContext waits for d unless the context is cancelled first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
