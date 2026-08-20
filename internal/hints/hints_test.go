package hints

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Hints are the feature that turns a raw API wrapper into a research tool, so
// their generation is treated as behaviour worth testing rather than cosmetics.

func TestCodeSearchHintsDependOnResults(t *testing.T) {
	empty := CodeSearch(0, "", false)
	require.NotEmpty(t, empty)
	joined := strings.Join(empty, " ")
	require.Contains(t, joined, "broader keywords")
	require.Contains(t, joined, "match=path", "an empty search must suggest searching filenames")

	withResults := CodeSearch(3, "acme/widget/main.go", false)
	joined = strings.Join(withResults, " ")
	require.Contains(t, joined, "acme/widget/main.go", "the hint must name the concrete top result")
	require.Contains(t, joined, "github_file_content")
	require.NotContains(t, joined, "broader keywords")
}

func TestCodeSearchHintsAdaptToConciseMode(t *testing.T) {
	concise := strings.Join(CodeSearch(5, "a/b.go", true), " ")
	require.Contains(t, concise, "concise")

	full := strings.Join(CodeSearch(5, "a/b.go", false), " ")
	require.Contains(t, full, "lsp_navigate")
}

func TestCodeSearchHintsWarnOnLargeResultSets(t *testing.T) {
	joined := strings.Join(CodeSearch(50, "a/b.go", false), " ")
	require.Contains(t, joined, "narrow")
}

// The brief calls out this specific case: more than twenty references means the
// agent should narrow rather than read them all.
func TestLSPReferencesHintWarnsAboveTwenty(t *testing.T) {
	joined := strings.Join(LSP("references", 25, false), " ")
	require.Contains(t, joined, "Too many references")

	joined = strings.Join(LSP("references", 5, false), " ")
	require.NotContains(t, joined, "Too many references")
}

// A fallback result must say so: textual matches carry false positives that
// language-server results do not.
func TestLSPHintsFlagFallbackResults(t *testing.T) {
	joined := strings.Join(LSP("definition", 3, true), " ")
	require.Contains(t, joined, "ripgrep")
	require.Contains(t, joined, "false positives")

	joined = strings.Join(LSP("definition", 3, false), " ")
	require.NotContains(t, joined, "ripgrep")
}

func TestLSPHintsForEmptyResults(t *testing.T) {
	joined := strings.Join(LSP("definition", 0, false), " ")
	require.Contains(t, joined, "1-based")
	require.Contains(t, joined, "local_search_code")
}

func TestDatabricksHintsForEmptyResult(t *testing.T) {
	joined := strings.Join(Databricks(0, "dev", false), " ")
	require.Contains(t, joined, "longer time range")
	require.Contains(t, joined, "SHOW TABLES")
}

// A dev result must remind the agent that production is a deliberate opt-in.
func TestDatabricksHintsMentionEnvironment(t *testing.T) {
	joined := strings.Join(Databricks(10, "dev", false), " ")
	require.Contains(t, joined, "env=prod")

	joined = strings.Join(Databricks(10, "prod", false), " ")
	require.NotContains(t, joined, "env=prod")
}

func TestDatabricksHintsOnTruncation(t *testing.T) {
	joined := strings.Join(Databricks(1000, "dev", true), " ")
	require.Contains(t, joined, "row cap")
}

func TestFileContentHintsMentionPartialReads(t *testing.T) {
	joined := strings.Join(FileContent("acme/widget/main.go", true), " ")
	require.Contains(t, joined, "partial read")

	joined = strings.Join(FileContent("acme/widget/main.go", false), " ")
	require.NotContains(t, joined, "partial read")
	require.Contains(t, joined, "lsp_navigate")
}

func TestEveryHintGeneratorProducesSomething(t *testing.T) {
	// A response with no guidance is a dead end, whatever the result shape.
	generators := map[string][]string{
		"codeSearch/empty":    CodeSearch(0, "", false),
		"codeSearch/results":  CodeSearch(1, "a/b.go", false),
		"fileContent":         FileContent("a/b.go", false),
		"repoStructure":       RepoStructure(false, 1),
		"repoSearch/empty":    RepoSearch(0),
		"repoSearch/results":  RepoSearch(3),
		"prSearch/empty":      PRSearch(0, false),
		"prSearch/results":    PRSearch(3, true),
		"localSearch/empty":   LocalSearch(0, "", false),
		"localSearch/results": LocalSearch(3, "a.go", true),
		"localFile":           LocalFile("a.go", true),
		"findFiles/empty":     FindFiles(0, false),
		"findFiles/results":   FindFiles(5, true),
		"dirStructure":        DirectoryStructure(5, true),
		"dirStructure/empty":  DirectoryStructure(0, false),
		"lsp/hover":           LSP("hover", 1, false),
		"lsp/symbols":         LSP("documentSymbol", 12, false),
		"databricks/rows":     Databricks(5, "prod", false),
	}
	for name, hints := range generators {
		require.NotEmpty(t, hints, "%s produced no hints", name)
		for _, h := range hints {
			require.NotEmpty(t, strings.TrimSpace(h), "%s produced a blank hint", name)
		}
	}
}
