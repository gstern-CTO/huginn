package content

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

const goSample = `package service

import (
	"context"
	"fmt"
)

// Handler processes requests.
type Handler struct {
	Name string
	port int
}

const DefaultPort = 8080

// Serve starts the handler.
func (h *Handler) Serve(ctx context.Context) error {
	fmt.Println("starting")
	for i := 0; i < 10; i++ {
		_ = i
	}
	return nil
}

func helper(a, b int) (int, error) {
	return a + b, nil
}
`

// Go symbol extraction goes through go/parser and go/ast: a real parser from
// the standard library, not a heuristic and not an external binary.
func TestMinifySymbolsUsesGoAST(t *testing.T) {
	out := Minify(goSample, "service.go", MinifySymbols)

	require.Contains(t, out, "package service")
	require.Contains(t, out, "func (h *Handler) Serve(ctx context.Context) error")
	require.Contains(t, out, "func helper(a, b int) (int, error)")
	require.Contains(t, out, "type Handler struct")
	require.Contains(t, out, "const DefaultPort = 8080")
	require.Contains(t, out, `"context"`)

	// Bodies are elided: that is the entire point of the symbols mode.
	require.NotContains(t, out, `fmt.Println("starting")`)
	require.NotContains(t, out, "for i := 0")
	require.NotContains(t, out, "return a + b")

	require.Less(t, len(out), len(goSample), "symbols mode must shrink the content")
}

func TestMinifySymbolsIncludesLineAnchors(t *testing.T) {
	out := Minify(goSample, "service.go", MinifySymbols)
	require.Contains(t, out, "// L", "each declaration is anchored to its line")
}

// A file that does not parse must still return something useful rather than
// nothing at all.
func TestMinifySymbolsFallsBackOnUnparseableGo(t *testing.T) {
	fragment := "func Broken(a int) error {\n\treturn nil\n" // missing closing brace

	out := Minify(fragment, "broken.go", MinifySymbols)
	require.Contains(t, out, "Broken", "the heuristic path must still find the declaration")
}

func TestMinifySymbolsHeuristicForOtherLanguages(t *testing.T) {
	python := `import os

# a comment
class Service:
    def __init__(self):
        self.value = 1

def helper(a, b):
    return a + b
`
	out := Minify(python, "service.py", MinifySymbols)
	require.Contains(t, out, "class Service")
	require.Contains(t, out, "def helper(a, b)")
	require.NotContains(t, out, "self.value = 1", "indented body lines are not declarations")
	require.NotContains(t, out, "# a comment")
}

func TestMinifyStandardStripsCommentsAndBlanks(t *testing.T) {
	out := Minify(goSample, "service.go", MinifyStandard)

	require.NotContains(t, out, "// Handler processes requests.")
	require.NotContains(t, out, "// Serve starts the handler.")
	require.Contains(t, out, "func helper(a, b int) (int, error) {")
	require.Contains(t, out, "return a + b, nil")
	require.NotContains(t, out, "\n\n", "blank lines are removed")
}

// A comment marker inside a string literal is not a comment. This is why the
// stripper is a scanner rather than a regex.
func TestStripCommentsRespectsStringLiterals(t *testing.T) {
	src := `package main

func main() {
	url := "https://example.com/path" // trailing comment
	re := "a/*b*/c"
	raw := ` + "`" + `keep // this` + "`" + `
}
`
	out := StripComments(src, "main.go")

	require.Contains(t, out, `"https://example.com/path"`, "a URL must survive")
	require.Contains(t, out, `"a/*b*/c"`, "block markers inside a string must survive")
	require.Contains(t, out, "keep // this", "a raw string literal must survive verbatim")
	require.NotContains(t, out, "trailing comment")
}

func TestStripCommentsHandlesBlockComments(t *testing.T) {
	src := "before\n/* multi\nline\ncomment */\nafter\n"
	out := StripComments(src, "x.go")

	require.Contains(t, out, "before")
	require.Contains(t, out, "after")
	require.NotContains(t, out, "multi")
	require.Equal(t, strings.Count(src, "\n"), strings.Count(out, "\n"),
		"line numbering must be preserved so anchors stay valid")
}

func TestStripCommentsPerLanguage(t *testing.T) {
	cases := []struct {
		file    string
		src     string
		gone    string
		survive string
	}{
		{"a.py", "x = 1  # comment\ny = 2\n", "comment", "x = 1"},
		{"a.sql", "SELECT 1 -- comment\nFROM t\n", "comment", "SELECT 1"},
		{"a.sh", "echo hi # comment\n", "comment", "echo hi"},
		{"a.lua", "local x = 1 -- comment\n", "comment", "local x = 1"},
		{"a.json", `{"a": 1}`, "", `{"a": 1}`},
	}
	for _, tc := range cases {
		out := StripComments(tc.src, tc.file)
		if tc.gone != "" {
			require.NotContains(t, out, tc.gone, tc.file)
		}
		require.Contains(t, out, tc.survive, tc.file)
	}
}

func TestMinifyNoneIsVerbatim(t *testing.T) {
	require.Equal(t, goSample, Minify(goSample, "service.go", MinifyNone))
}

func TestParseMinifyMode(t *testing.T) {
	for input, expected := range map[string]MinifyMode{
		"":         MinifyStandard,
		"standard": MinifyStandard,
		"none":     MinifyNone,
		"symbols":  MinifySymbols,
		"SYMBOLS":  MinifySymbols,
	} {
		mode, err := ParseMinifyMode(input)
		require.Nil(t, err, input)
		require.Equal(t, expected, mode, input)
	}

	_, err := ParseMinifyMode("aggressive")
	require.NotNil(t, err)
	require.Equal(t, protocol.CodeInvalidInput, err.Code)
	require.NotEmpty(t, err.Hint)
}

// MaskNonCode must not change the length of the text: callers rely on the
// masked copy having identical offsets, lines and columns to the original.
func TestMaskNonCodePreservesLengthAndPositions(t *testing.T) {
	src := "package demo\n\n// Widget is documented here.\nfunc use() string { return \"Widget\" }\n\ntype Widget struct{}\n"

	masked := MaskNonCode(src, "demo.go")

	require.Equal(t, len(src), len(masked), "masking must preserve length")
	require.Equal(t, strings.Count(src, "\n"), strings.Count(masked, "\n"), "newlines must survive")

	lines := strings.Split(masked, "\n")
	require.NotContains(t, lines[2], "Widget", "the doc comment must be masked")
	require.NotContains(t, lines[3], "Widget", "the string literal must be masked")
	require.Contains(t, lines[3], "func use() string", "surrounding code must survive")
	require.Contains(t, lines[5], "type Widget struct{}", "the declaration must survive")

	// The column of the surviving declaration is unchanged.
	require.Equal(t, strings.Index(src, "type Widget"), strings.Index(masked, "type Widget"))
}

func TestMaskNonCodeHandlesRawStringsAndBlockComments(t *testing.T) {
	src := "package demo\n\n/* Target in a block */\nvar s = `Target in a raw string`\nvar Target = 1\n"

	masked := MaskNonCode(src, "demo.go")
	require.Equal(t, len(src), len(masked))

	lines := strings.Split(masked, "\n")
	require.NotContains(t, lines[2], "Target")
	require.NotContains(t, lines[3], "Target")
	require.Contains(t, lines[4], "var Target = 1")
}

// An unterminated literal must not swallow the rest of the file.
func TestMaskNonCodeStopsAtEndOfLineForUnterminatedString(t *testing.T) {
	src := "x = \"oops\nTarget = 1\n"

	masked := MaskNonCode(src, "demo.py")
	require.Equal(t, len(src), len(masked))
	require.Contains(t, masked, "Target = 1")
}

func TestMaskNonCodeLeavesCommentlessFormatsAlone(t *testing.T) {
	src := `{"key": "value"}`
	// JSON has no comments, but its string contents are still masked.
	masked := MaskNonCode(src, "data.json")
	require.Equal(t, len(src), len(masked))
	require.Contains(t, masked, `{"`)
}
