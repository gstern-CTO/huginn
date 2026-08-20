package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// Language servers return definitions in three different shapes depending on
// which version of the protocol they implement. All three must be handled, or
// go-to-definition silently returns nothing for some servers.
func TestParseLocationsHandlesAllThreeShapes(t *testing.T) {
	t.Run("single Location", func(t *testing.T) {
		raw := json.RawMessage(`{"uri":"file:///w/main.go","range":{"start":{"line":9,"character":5},"end":{"line":9,"character":12}}}`)
		got := parseLocations(raw)
		require.Len(t, got, 1)
		require.Equal(t, 10, got[0].Line, "LSP lines are zero-based; the API is one-based")
		require.Equal(t, 6, got[0].Character)
	})

	t.Run("array of Locations", func(t *testing.T) {
		raw := json.RawMessage(`[
			{"uri":"file:///w/a.go","range":{"start":{"line":0,"character":0},"end":{"line":0,"character":3}}},
			{"uri":"file:///w/b.go","range":{"start":{"line":4,"character":2},"end":{"line":4,"character":8}}}
		]`)
		got := parseLocations(raw)
		require.Len(t, got, 2)
		require.Equal(t, 1, got[0].Line)
		require.Equal(t, 5, got[1].Line)
	})

	t.Run("array of LocationLinks", func(t *testing.T) {
		raw := json.RawMessage(`[{
			"targetUri":"file:///w/target.go",
			"targetRange":{"start":{"line":20,"character":0},"end":{"line":30,"character":1}},
			"targetSelectionRange":{"start":{"line":20,"character":5},"end":{"line":20,"character":11}}
		}]`)
		got := parseLocations(raw)
		require.Len(t, got, 1)
		require.Equal(t, 21, got[0].Line)
		require.Equal(t, 6, got[0].Character)
	})
}

func TestParseLocationsHandlesNullAndEmpty(t *testing.T) {
	require.Empty(t, parseLocations(json.RawMessage(`null`)))
	require.Empty(t, parseLocations(json.RawMessage(``)))
	require.Empty(t, parseLocations(json.RawMessage(`[]`)))
}

func TestParseHoverHandlesEveryContentShape(t *testing.T) {
	cases := map[string]string{
		`{"contents":{"kind":"markdown","value":"func Foo() error"}}`:  "func Foo() error",
		`{"contents":"plain string hover"}`:                            "plain string hover",
		`{"contents":["first","second"]}`:                              "first\nsecond",
		`{"contents":[{"language":"go","value":"type Bar struct{}"}]}`: "type Bar struct{}",
	}
	for raw, expected := range cases {
		require.Equal(t, expected, parseHover(json.RawMessage(raw)), raw)
	}

	require.Empty(t, parseHover(json.RawMessage(`null`)))
}

// Nested symbols must be flattened with a dotted path so a method reads as
// Type.Method rather than as a bare, ambiguous name.
func TestParseDocumentSymbolsFlattensHierarchy(t *testing.T) {
	raw := json.RawMessage(`[{
		"name":"Handler","kind":23,
		"range":{"start":{"line":4,"character":0},"end":{"line":20,"character":1}},
		"selectionRange":{"start":{"line":4,"character":5},"end":{"line":4,"character":12}},
		"children":[{
			"name":"Serve","kind":6,
			"range":{"start":{"line":8,"character":1},"end":{"line":14,"character":2}},
			"selectionRange":{"start":{"line":8,"character":3},"end":{"line":8,"character":8}}
		}]
	}]`)

	got := parseDocumentSymbols(raw, "/w/main.go")
	require.Len(t, got, 2)
	require.Equal(t, "Handler", got[0].Name)
	require.Equal(t, "struct", got[0].Kind)
	require.Equal(t, "Handler.Serve", got[1].Name, "a nested symbol carries its parent's name")
	require.Equal(t, "method", got[1].Kind)
	require.Equal(t, 9, got[1].Line)
}

// The older flat SymbolInformation shape carries a location instead of ranges.
func TestParseDocumentSymbolsHandlesFlatShape(t *testing.T) {
	raw := json.RawMessage(`[{
		"name":"TopLevel","kind":12,
		"location":{"uri":"file:///w/other.go","range":{"start":{"line":2,"character":0},"end":{"line":6,"character":1}}}
	}]`)

	got := parseDocumentSymbols(raw, "/w/main.go")
	require.Len(t, got, 1)
	require.Equal(t, "TopLevel", got[0].Name)
	require.Equal(t, "function", got[0].Kind)
	require.Equal(t, "/w/other.go", got[0].Path)
	require.Equal(t, 3, got[0].Line)
}

func TestLocateSymbolFindsFirstWordBoundaryOccurrence(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sample.go"
	source := "package main\n\n// mentions Handlers in prose\ntype Handler struct{}\n"
	require.NoError(t, writeFile(path, source))

	line, col, ok := locateSymbol(path, "Handler")
	require.True(t, ok)
	// "Handlers" on line 3 must not match: the boundary excludes it.
	require.Equal(t, 4, line)
	require.Equal(t, 6, col)

	_, _, ok = locateSymbol(path, "NoSuchSymbol")
	require.False(t, ok)
}
