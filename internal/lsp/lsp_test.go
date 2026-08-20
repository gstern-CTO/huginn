package lsp

import (
	"bufio"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReadLSPMessageParsesFraming(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`
	stream := "Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body

	got, err := readLSPMessage(bufio.NewReader(strings.NewReader(stream)))
	require.NoError(t, err)
	require.JSONEq(t, body, string(got))
}

// Servers commonly send an extra Content-Type header; it must be tolerated.
func TestReadLSPMessageIgnoresOtherHeaders(t *testing.T) {
	body := `{"id":2}`
	stream := "Content-Type: application/vscode-jsonrpc; charset=utf-8\r\n" +
		"Content-Length: " + itoa(len(body)) + "\r\n\r\n" + body

	got, err := readLSPMessage(bufio.NewReader(strings.NewReader(stream)))
	require.NoError(t, err)
	require.Equal(t, body, string(got))
}

// Two messages back to back must be read one at a time, with the reader left
// positioned at the start of the second.
func TestReadLSPMessageReadsConsecutiveMessages(t *testing.T) {
	first, second := `{"id":1}`, `{"id":2}`
	stream := "Content-Length: " + itoa(len(first)) + "\r\n\r\n" + first +
		"Content-Length: " + itoa(len(second)) + "\r\n\r\n" + second

	reader := bufio.NewReader(strings.NewReader(stream))

	got, err := readLSPMessage(reader)
	require.NoError(t, err)
	require.Equal(t, first, string(got))

	got, err = readLSPMessage(reader)
	require.NoError(t, err)
	require.Equal(t, second, string(got))
}

func TestReadLSPMessageRejectsMissingContentLength(t *testing.T) {
	_, err := readLSPMessage(bufio.NewReader(strings.NewReader("X-Other: 1\r\n\r\n{}")))
	require.Error(t, err)
	require.Contains(t, err.Error(), "Content-Length")
}

func TestReadLSPMessageRejectsBadContentLength(t *testing.T) {
	_, err := readLSPMessage(bufio.NewReader(strings.NewReader("Content-Length: abc\r\n\r\n{}")))
	require.Error(t, err)
}

func TestURIRoundTrip(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "workspace", "pkg", "main.go")

	uri := PathToURI(path)
	require.True(t, strings.HasPrefix(uri, "file://"), "got %q", uri)
	require.Equal(t, path, URIToPath(uri))
}

// A path with characters that need escaping must survive the round trip.
func TestURIRoundTripWithSpaces(t *testing.T) {
	path := filepath.Join(string(filepath.Separator), "work space", "my file.go")

	uri := PathToURI(path)
	require.NotContains(t, uri, " ", "the URI must be escaped")
	require.Equal(t, path, URIToPath(uri))
}

func TestURIToPathPassesThroughNonFileURIs(t *testing.T) {
	require.Equal(t, "http://example.com/x", URIToPath("http://example.com/x"))
}

// The extension table is what makes adding a language a data change rather than
// a code change, so its shape is worth asserting.
func TestServerForFileDetection(t *testing.T) {
	cases := map[string]string{
		"main.go":       "gopls",
		"script.py":     "pyright-langserver",
		"lib.rs":        "rust-analyzer",
		"app.ts":        "typescript-language-server",
		"component.tsx": "typescript-language-server",
		"index.js":      "typescript-language-server",
		"main.c":        "clangd",
		"engine.cpp":    "clangd",
	}
	for file, binary := range cases {
		spec, ok := ServerForFile(file)
		require.True(t, ok, "no server for %s", file)
		require.Equal(t, binary, spec.Binary, file)
		require.NotEmpty(t, spec.Install, "%s must carry an install command", file)
	}

	_, ok := ServerForFile("notes.md")
	require.False(t, ok, "markdown has no language server")
}

// Every configured server must name the command that installs it: that is what
// turns a missing dependency into an actionable response.
func TestEveryServerSpecCarriesAnInstallCommand(t *testing.T) {
	for ext, spec := range serversByExtension {
		require.NotEmpty(t, spec.Binary, ext)
		require.NotEmpty(t, spec.Install, ext)
		require.NotEmpty(t, spec.LanguageID, ext)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf []byte
	for n > 0 {
		buf = append([]byte{byte('0' + n%10)}, buf...)
		n /= 10
	}
	return string(buf)
}
