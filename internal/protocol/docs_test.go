package protocol

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every documentation URL must resolve to a file that is actually in the
// repository. A dead reference is worse than none: it sends the agent chasing
// a page that does not exist (Design Log #3).
func TestEveryDocsURLPointsAtAFileThatExists(t *testing.T) {
	repoRoot, err := filepath.Abs("../..")
	require.NoError(t, err)

	for _, url := range []string{
		DocsGitHubQuerySyntax,
		DocsReadOnlySQL,
		DocsRipgrepPatterns,
	} {
		require.True(t, strings.HasPrefix(url, docsBase), "unexpected base for %s", url)

		rel := "docs/errors/" + strings.TrimPrefix(url, docsBase)
		path := filepath.Join(repoRoot, rel)

		info, statErr := os.Stat(path)
		require.NoError(t, statErr, "%s references %s, which does not exist", url, rel)
		require.Greater(t, info.Size(), int64(200), "%s is too short to be useful", rel)
	}
}

// The field is absent from the JSON unless it was set, so errors that gain
// nothing from a reference are not padded with an empty key.
func TestDocsIsOmittedWhenUnset(t *testing.T) {
	plain := ErrInvalidInput("something is wrong")
	raw, err := json.Marshal(plain)
	require.NoError(t, err)
	require.NotContains(t, string(raw), "docs")

	withDocs := ErrInvalidInput("something is wrong").WithDocs(DocsReadOnlySQL)
	raw, err = json.Marshal(withDocs)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"docs"`)
	require.Contains(t, string(raw), DocsReadOnlySQL)
}

// WithDocs composes with WithDetail, since real call sites use both.
func TestWithDocsComposesWithWithDetail(t *testing.T) {
	err := NewError(CodeForbiddenSQL, false, "hint", "message").
		WithDetail("forbiddenKeyword", "DROP").
		WithDocs(DocsReadOnlySQL)

	require.Equal(t, "DROP", err.Details["forbiddenKeyword"])
	require.Equal(t, DocsReadOnlySQL, err.Docs)
	require.Equal(t, CodeForbiddenSQL, err.Code)
}

// The reference is attached at the error site, not derived from the code: one
// code covers several unrelated repairs, so a code-keyed table would hand the
// agent the wrong document.
func TestSameCodeCanCarryDifferentDocs(t *testing.T) {
	query := ErrInvalidInput("each query needs at least one keyword").WithDocs(DocsGitHubQuerySyntax)
	pattern := ErrInvalidInput("ripgrep rejected the search").WithDocs(DocsRipgrepPatterns)

	require.Equal(t, CodeInvalidInput, query.Code)
	require.Equal(t, CodeInvalidInput, pattern.Code)
	require.NotEqual(t, query.Docs, pattern.Docs)
}
