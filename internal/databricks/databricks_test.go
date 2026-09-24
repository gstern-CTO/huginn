package databricks

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

func TestReadOnlySQLAcceptsReadStatements(t *testing.T) {
	valid := []string{
		`SELECT * FROM telemetry.dns_queries LIMIT 10`,
		`select count(*) from events`,
		`WITH recent AS (SELECT * FROM q WHERE ts > now() - INTERVAL 7 DAYS) SELECT * FROM recent`,
		`SHOW TABLES IN telemetry`,
		`DESCRIBE TABLE telemetry.dns_queries`,
		`EXPLAIN SELECT 1`,
		`SELECT created_at, updated_at, deleted_flag FROM audit`,
		`SELECT * FROM t WHERE note = 'do not drop this'`,
		`SELECT 1; `,
	}
	for _, stmt := range valid {
		require.Nil(t, ValidateReadOnlySQL(stmt), "expected %q to be accepted", stmt)
	}
}

// Read-only enforcement is not delegated to Databricks or to a credential: the
// statement is refused before it leaves this process.
func TestReadOnlySQLRejectsMutations(t *testing.T) {
	mutations := []string{
		`INSERT INTO t VALUES (1)`,
		`UPDATE t SET x = 1`,
		`DELETE FROM t WHERE 1=1`,
		`DROP TABLE t`,
		`TRUNCATE TABLE t`,
		`ALTER TABLE t ADD COLUMN x INT`,
		`CREATE TABLE t (x INT)`,
		`MERGE INTO target USING source ON target.id = source.id`,
		`GRANT SELECT ON t TO user`,
		`COPY INTO t FROM 's3://bucket'`,
		`  create   table  spaced (x int)`,
		"\n\tDROP TABLE t\n",
	}
	for _, stmt := range mutations {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, "expected %q to be refused", stmt)
		require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
		require.False(t, tErr.Retryable, "a refused statement must not invite a retry")
		require.NotEmpty(t, tErr.Hint)
	}
}

// A mutation must not be smuggled in behind a SELECT via a second statement.
func TestReadOnlySQLRejectsStackedStatements(t *testing.T) {
	stacked := []string{
		`SELECT 1; DROP TABLE t`,
		`SELECT 1;DELETE FROM t`,
		`SELECT * FROM t; INSERT INTO t VALUES (1);`,
	}
	for _, stmt := range stacked {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, "expected %q to be refused", stmt)
		require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
	}
}

// A semicolon inside a string literal is data, not a statement separator.
func TestReadOnlySQLAllowsSemicolonInsideLiteral(t *testing.T) {
	require.Nil(t, ValidateReadOnlySQL(`SELECT * FROM t WHERE note = 'a;b'`))
	require.Nil(t, ValidateReadOnlySQL(`SELECT * FROM t WHERE note = "x;y"`))
}

// Comments are stripped before the keyword scan, so a verb cannot hide behind
// a comment marker that the server ignores but the engine does not.
func TestReadOnlySQLStripsCommentsBeforeScanning(t *testing.T) {
	require.Nil(t, ValidateReadOnlySQL("SELECT 1 -- DROP TABLE t\n"))
	require.Nil(t, ValidateReadOnlySQL("SELECT 1 /* INSERT INTO t */"))

	// A leading comment must not disguise a mutation as a read.
	tErr := ValidateReadOnlySQL("-- SELECT 1\nDROP TABLE t")
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
}

// Word-boundary matching means an identifier containing a keyword is fine.
func TestReadOnlySQLDoesNotTripOnKeywordSubstrings(t *testing.T) {
	safe := []string{
		`SELECT created_at FROM t`,
		`SELECT update_time, deleted_at FROM t`,
		`SELECT * FROM created_records`,
		`SELECT insertion_id FROM t`,
		`SELECT altered_state FROM t`,
	}
	for _, stmt := range safe {
		require.Nil(t, ValidateReadOnlySQL(stmt), "expected %q to be accepted", stmt)
	}
}

func TestReadOnlySQLRejectsEmptyAndCommentOnly(t *testing.T) {
	for _, stmt := range []string{"", "   ", "-- just a comment", "/* nothing */"} {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, "expected %q to be refused", stmt)
		require.Equal(t, protocol.CodeInvalidInput, tErr.Code)
	}
}

func TestReadOnlySQLNamesTheOffendingKeyword(t *testing.T) {
	tErr := ValidateReadOnlySQL(`DROP TABLE telemetry.dns_queries`)
	require.NotNil(t, tErr)
	require.Equal(t, "DROP", tErr.Details["leadingKeyword"])
	require.Contains(t, tErr.Message, "DROP")
}

// A comment marker inside a literal, and a quote inside a comment, are the two
// cases that defeat sequential regex stripping.
func TestSanitizeSQLHandlesInterleavedQuotesAndComments(t *testing.T) {
	// The '--' here is data, not a comment: the DROP after it is real.
	tErr := ValidateReadOnlySQL(`SELECT '--' AS marker; DROP TABLE t`)
	require.NotNil(t, tErr, "a mutation after a literal containing -- must still be caught")

	// The apostrophe here is inside a comment and must not open a literal.
	require.Nil(t, ValidateReadOnlySQL("SELECT 1 -- don't do this\n"))

	// A dangling quote is ambiguous and is refused rather than guessed at.
	tErr = ValidateReadOnlySQL(`SELECT 'unterminated`)
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeInvalidInput, tErr.Code)

	tErr = ValidateReadOnlySQL(`SELECT 1 /* unterminated`)
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeInvalidInput, tErr.Code)
}

// Production must never be the default: an agent that omits env gets dev.
func TestDatabricksEnvironmentDefaultsToDev(t *testing.T) {
	cfg := config.Defaults()
	cfg.Databricks = map[string]config.DatabricksEnv{
		"dev": {Host: "https://dev.example.com", Token: "t", WarehouseID: "w"},
	}
	require.True(t, cfg.Databricks["dev"].Configured())
	require.False(t, cfg.Databricks["prod"].Configured(),
		"an unconfigured prod environment must not silently resolve")
}

// A forbidden verb appearing as part of a qualified name is an identifier, not
// a verb. This was latent here before ClickHouse forced it into the open: a
// table called catalog.merge_history or a schema named `update` was refused by
// the rule meant to stop MERGE and UPDATE statements (Design Log #5).
func TestQualifiedNamesAreNotMistakenForVerbs(t *testing.T) {
	for _, stmt := range []string{
		`SELECT * FROM catalog.merge_history`,
		`SELECT * FROM main.update_log`,
		`SELECT * FROM prod.delete_audit WHERE ts > now() - INTERVAL 1 DAY`,
		`SELECT c.set_name FROM main.config AS c`,
		`SELECT * FROM analytics.cache_stats`,
	} {
		require.Nil(t, ValidateReadOnlySQL(stmt), "expected %q to be accepted", stmt)
	}

	// The actual verbs are still refused.
	for _, stmt := range []string{`MERGE INTO t USING s ON t.id = s.id`, `UPDATE t SET x = 1`} {
		require.NotNil(t, ValidateReadOnlySQL(stmt), "expected %q to be refused", stmt)
	}
}
