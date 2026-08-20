package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/gstern-CTO/huginn/internal/databricks"
	"github.com/gstern-CTO/huginn/internal/hints"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

func toolDatabricksQuery() mcp.Tool {
	return mcp.NewTool("databricks_query",
		mcp.WithDescription(
			"Run a read-only SQL query against Databricks and get back named columns and rows. Only SELECT, SHOW, DESCRIBE "+
				"and EXPLAIN are accepted; any statement containing INSERT, UPDATE, DELETE, DROP, TRUNCATE, ALTER or CREATE is "+
				"refused before it is sent. Defaults to the dev workspace — querying production requires env=\"prod\" explicitly.",
		),
		mcp.WithString("statement", mcp.Required(), mcp.Description("A single read-only SQL statement.")),
		mcp.WithString("env",
			mcp.Description("Which workspace to query. Defaults to dev; prod must be requested explicitly."),
			mcp.Enum("dev", "prod"),
			mcp.DefaultString("dev"),
		),
		mcp.WithString("catalog", mcp.Description("Unity Catalog catalog to resolve unqualified names against.")),
		mcp.WithString("schema", mcp.Description("Schema to resolve unqualified names against.")),
		mcp.WithNumber("maxRows", mcp.Description("Maximum rows to return; capped by the server's configured limit."), mcp.Min(1)),
	)
}

func (s *Server) handleDatabricksQuery(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	statement, err := req.RequireString("statement")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("statement is required"))
	}

	// Read-only enforcement runs first, before environment resolution and
	// before any network call: a rejected statement never reaches Databricks.
	if tErr := databricks.ValidateReadOnlySQL(statement); tErr != nil {
		return protocol.Failure(tErr)
	}

	env := strings.ToLower(strings.TrimSpace(req.GetString("env", "dev")))
	if env == "" {
		env = "dev"
	}
	if env != "dev" && env != "prod" {
		return protocol.Failure(protocol.ErrInvalidInput("env must be 'dev' or 'prod', got %q", env))
	}

	catalog := req.GetString("catalog", "")
	schema := req.GetString("schema", "")
	maxRows := req.GetInt("maxRows", s.cfg.DatabricksMaxRows)

	if env == "prod" {
		// Production access is a deliberate act and is logged as one.
		s.logger.Info("databricks production query", "catalog", catalog, "schema", schema)
	}

	result, tErr := s.dbx.Execute(ctx, env, statement, catalog, schema, maxRows)
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	meta := protocol.Metadata{}
	budget := s.budget()

	// Redact cell values: query results routinely contain tokens and keys.
	kept := make([][]any, 0, len(result.Rows))
	for _, row := range result.Rows {
		cleaned := make([]any, len(row))
		rowSize := 0
		for i, cell := range row {
			if str, ok := cell.(string); ok {
				scrubbed := s.redact(str, &meta)
				cleaned[i] = scrubbed
				rowSize += len(scrubbed)
				continue
			}
			cleaned[i] = cell
			rowSize += 8
		}
		if !budget.TryAdd(strings.Repeat(" ", rowSize)) {
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

	env2 := &protocol.Envelope{
		Status: protocol.StatusFor(result.RowCount),
		Data: map[string]any{
			"env":       env,
			"catalog":   catalog,
			"schema":    schema,
			"columns":   result.Columns,
			"rows":      result.Rows,
			"rowCount":  result.RowCount,
			"truncated": result.Truncated,
		},
		Metadata: meta,
	}
	env2.WithHints(hints.Databricks(result.RowCount, env, result.Truncated)...)
	if meta.HasMore {
		env2.WithHints(fmt.Sprintf("The response hit its token budget after %d rows. Aggregate in SQL or add a LIMIT with an offset.", result.RowCount))
	}
	return env2
}
