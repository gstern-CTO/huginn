// Package sqlguard enforces read-only SQL for every warehouse-backed tool.
//
// The enforcement happens here, in process, before a statement leaves the
// binary. It is deliberately not delegated to a read-only credential or to the
// server's own settings: Design Log #5 records a ClickHouse server configured
// with readonly=2 refusing writes while still executing url(), which is a
// working exfiltration path out of an otherwise legitimate SELECT.
//
// Dialects differ in their verbs, so each supplies its own tables. What they
// must never differ in is the scanner below, which is the part that took two
// attempts to get right.
package sqlguard

import (
	"regexp"
	"strings"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

var (
	wordRe = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)
	// calleeRe finds an identifier applied as a function: the shape a table
	// function takes. Run against sanitised SQL only, so a name appearing
	// inside a string literal cannot match.
	calleeRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*\(`)
)

// Dialect describes one engine's notion of a read-only statement.
type Dialect struct {
	// Name appears in error messages, e.g. "ClickHouse".
	Name string
	// AllowedLeading are the verbs a statement may begin with.
	AllowedLeading map[string]bool
	// Forbidden are verbs refused anywhere in the statement.
	Forbidden []string
	// ForbiddenCallees are functions refused anywhere, lowercase. Empty for
	// engines with no equivalent hazard. ClickHouse needs this because its
	// table functions reach the network and the filesystem from inside a
	// SELECT, which no verb check can catch.
	ForbiddenCallees []string
	// DocsURL is attached to every refusal so the agent can read the rules.
	DocsURL string
	// AllowedHint is the one-line remedy offered when a statement is refused.
	AllowedHint string
}

// QueryResult is the shape every SQL-backed tool returns, so an agent that has
// learned one warehouse tool has learned them all.
type QueryResult struct {
	Columns   []QueryColumn `json:"columns"`
	Rows      [][]any       `json:"rows"`
	RowCount  int           `json:"rowCount"`
	Truncated bool          `json:"truncated"`
}

// QueryColumn is one column's name and engine-reported type.
type QueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// Validate refuses anything that is not a single read-only statement.
//
// The order matters: the statement is sanitised first, so every later check
// scans text in which comments are gone and literal contents are blanked. That
// is what stops `WHERE note = 'do not drop this'` being read as a DROP.
func (d Dialect) Validate(statement string) *protocol.ToolError {
	trimmed := strings.TrimSpace(statement)
	if trimmed == "" {
		return protocol.ErrInvalidInput("statement must not be empty")
	}

	stripped, ok := Sanitize(trimmed)
	if !ok {
		return protocol.ErrInvalidInput("statement has an unterminated string literal or block comment")
	}
	stripped = strings.TrimSpace(stripped)
	if stripped == "" || strings.Trim(stripped, "'\"`  \t\n") == "" {
		return protocol.ErrInvalidInput("statement contains only comments")
	}

	// Multiple statements are refused outright: validating each one would
	// still risk disagreeing with the engine about where the boundaries are.
	if HasMultipleStatements(stripped) {
		return d.refuse("Send exactly one read-only statement per call.",
			"multiple SQL statements in one call are not allowed")
	}

	upper := strings.ToUpper(stripped)
	spans := wordRe.FindAllStringIndex(upper, -1)
	if len(spans) == 0 {
		return protocol.ErrInvalidInput("statement contains no SQL keywords")
	}

	leading := upper[spans[0][0]:spans[0][1]]
	if !d.AllowedLeading[leading] {
		return d.refuse(d.AllowedHint,
			"statement begins with %q, which is not a read operation", leading).
			WithDetail("leadingKeyword", leading)
	}

	// Word-boundary matching, so a column named created_at or update_time is
	// unaffected. A word adjacent to a dot is part of a qualified name —
	// `system.query_log`, `catalog.merge_history` — and is an identifier, not a
	// verb. Without this, introspecting ClickHouse via system.* is refused by
	// the very rule meant to stop SYSTEM SHUTDOWN, and the hints recommend a
	// query the validator rejects.
	present := make(map[string]bool, len(spans))
	for _, sp := range spans {
		start, end := sp[0], sp[1]
		if start > 0 && upper[start-1] == '.' {
			continue // qualified: db.WORD
		}
		if end < len(upper) && upper[end] == '.' {
			continue // qualifier: WORD.table
		}
		present[upper[start:end]] = true
	}
	for _, verb := range d.Forbidden {
		if present[verb] {
			return d.refuse(
				"This tool is read-only. Remove "+verb+" from the statement; if you need the data it would produce, express it as a read.",
				"statement contains the forbidden keyword %s", verb).
				WithDetail("forbiddenKeyword", verb)
		}
	}

	// A read verb is not sufficient on its own: see the package comment.
	if callee, found := d.forbiddenCallee(stripped); found {
		return d.refuse(
			"The function "+callee+"() can reach outside the database, so it is refused even inside a SELECT. Query a table instead.",
			"statement calls %s(), which is not permitted", callee).
			WithDetail("forbiddenFunction", callee)
	}

	return nil
}

// forbiddenCallee reports the first denylisted function applied in the
// sanitised statement.
func (d Dialect) forbiddenCallee(sanitised string) (string, bool) {
	if len(d.ForbiddenCallees) == 0 {
		return "", false
	}
	denied := make(map[string]bool, len(d.ForbiddenCallees))
	for _, name := range d.ForbiddenCallees {
		denied[strings.ToLower(name)] = true
	}
	for _, m := range calleeRe.FindAllStringSubmatch(sanitised, -1) {
		if name := strings.ToLower(m[1]); denied[name] {
			return name, true
		}
	}
	return "", false
}

func (d Dialect) refuse(hint, format string, args ...any) *protocol.ToolError {
	err := protocol.NewError(protocol.CodeForbiddenSQL, false, hint, format, args...)
	if d.DocsURL != "" {
		err.WithDocs(d.DocsURL)
	}
	return err
}

// Sanitize removes comments and blanks the contents of string and identifier
// literals, leaving only the parts of a statement that carry SQL syntax.
//
// It has to be one pass rather than two: a comment marker can appear inside a
// literal ('--') and a quote can appear inside a comment (-- don't), so
// stripping either one first corrupts the other. ok is false for an
// unterminated literal or comment, which is refused rather than guessed at.
func Sanitize(s string) (string, bool) {
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
		// keyword or function name inside it is correctly treated as data.
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

// HasMultipleStatements reports whether a semicolon separates two statements,
// ignoring semicolons inside string literals and a single trailing one.
func HasMultipleStatements(s string) bool {
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
