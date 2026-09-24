package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/gstern-CTO/huginn/internal/clickhouse"
	"github.com/gstern-CTO/huginn/internal/hints"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

func toolClickHouseQuery() mcp.Tool {
	return mcp.NewTool("clickhouse_query",
		mcp.WithDescription(
			"Run a read-only SQL query against ClickHouse and get back named columns and rows. Only SELECT, WITH, SHOW, "+
				"DESCRIBE, EXPLAIN, EXISTS and CHECK are accepted; mutating statements are refused before they are sent, and so "+
				"are table functions such as url() and file() that would read outside the database from inside a SELECT. "+
				"Defaults to the dev service — querying production requires env=\"prod\" explicitly.",
		),
		mcp.WithString("statement", mcp.Required(), mcp.Description("A single read-only SQL statement. Do not append FORMAT; it is added for you.")),
		mcp.WithString("env",
			mcp.Description("Which service to query. Defaults to dev; prod must be requested explicitly."),
			mcp.Enum("dev", "prod"),
			mcp.DefaultString("dev"),
		),
		mcp.WithString("database", mcp.Description("Database to resolve unqualified table names against.")),
		mcp.WithNumber("maxRows", mcp.Description("Maximum rows to return; capped by the server's configured limit."), mcp.Min(1)),
	)
}

func (s *Server) handleClickHouseQuery(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	statement, err := req.RequireString("statement")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("statement is required"))
	}

	// Validation runs first, before environment resolution and before any
	// network call: a refused statement never reaches ClickHouse. The server's
	// own readonly setting is a backstop, not the enforcement — a readonly=2
	// service executes url() happily (Design Log #5, Q3).
	if tErr := clickhouse.ValidateReadOnlySQL(statement); tErr != nil {
		return protocol.Failure(tErr)
	}

	env := strings.ToLower(strings.TrimSpace(req.GetString("env", "dev")))
	if env == "" {
		env = "dev"
	}
	if env != "dev" && env != "prod" {
		return protocol.Failure(protocol.ErrInvalidInput("env must be 'dev' or 'prod', got %q", env))
	}

	database := req.GetString("database", "")
	maxRows := req.GetInt("maxRows", s.cfg.ClickHouseMaxRows)

	if env == "prod" {
		s.logger.Info("clickhouse production query", "database", database)
	}

	result, tErr := s.ch.Execute(ctx, env, statement, database, maxRows)
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	meta := protocol.Metadata{}
	budget := s.budget()

	// Cell values routinely contain tokens and keys, so they go through the
	// redactor like every other string leaving the server.
	kept := make([][]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		cleaned := make([]any, len(row))
		size := 0
		for i, cell := range row {
			if str, ok := cell.(string); ok {
				scrubbed := s.redact(str, &meta)
				cleaned[i] = scrubbed
				size += len(scrubbed)
				continue
			}
			cleaned[i] = cell
			size += 8
		}
		if !budget.TryAdd(strings.Repeat(" ", size)) {
			meta.HasMore = true
			result.Truncated = true
			break
		}
		kept = append(kept, cleaned)
	}
	result.Rows = kept
	result.RowCount = len(kept)

	meta.ResultCount = result.RowCount
	if result.Truncated {
		meta.HasMore = true
	}

	envelope := &protocol.Envelope{
		Status: protocol.StatusFor(result.RowCount),
		Data: map[string]any{
			"env":       env,
			"database":  database,
			"columns":   result.Columns,
			"rows":      result.Rows,
			"rowCount":  result.RowCount,
			"truncated": result.Truncated,
		},
		Metadata: meta,
	}
	envelope.WithHints(hints.ClickHouse(result.RowCount, env, result.Truncated)...)
	if meta.HasMore {
		envelope.WithHints(fmt.Sprintf(
			"The response hit its token budget after %d rows. Aggregate in SQL or add a LIMIT with an offset.", result.RowCount))
	}
	return envelope
}
