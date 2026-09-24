package sqlguard

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// These two moved here with the code they exercise (Design Log #5). Every
// behavioural test still lives beside its dialect.

func TestSanitizeSQLBlanksLiteralContent(t *testing.T) {
	out, ok := Sanitize(`SELECT * FROM t WHERE x = 'DROP TABLE y'`)
	require.True(t, ok)
	require.NotContains(t, out, "DROP")
	require.Contains(t, out, "SELECT")

	// A doubled quote is an escaped quote, not the end of the literal.
	out, ok = Sanitize(`SELECT 'it''s fine' FROM t`)
	require.True(t, ok)
	require.NotContains(t, out, "fine")
	require.Contains(t, out, "FROM t")
}

func TestHasMultipleStatements(t *testing.T) {
	require.False(t, HasMultipleStatements("SELECT 1"))
	require.False(t, HasMultipleStatements("SELECT 1;"))
	require.False(t, HasMultipleStatements("SELECT 1;   "))
	require.True(t, HasMultipleStatements("SELECT 1; SELECT 2"))
	require.False(t, HasMultipleStatements("SELECT ';'"))
}
