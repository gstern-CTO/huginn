package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/gstern-CTO/huginn/internal/content"
	"github.com/gstern-CTO/huginn/internal/hints"
	"github.com/gstern-CTO/huginn/internal/lsp"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

// lspLocation is the normalised shape returned for every navigation result,
// whether it came from a language server or the fallback.
type lspLocation struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`      // 1-based
	Character int    `json:"character"` // 1-based
	EndLine   int    `json:"endLine,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Name      string `json:"name,omitempty"`
	Kind      string `json:"kind,omitempty"`
}

func toolLSPNavigate() mcp.Tool {
	return mcp.NewTool("lsp_navigate",
		mcp.WithDescription(
			"Navigate code semantically through a language server: go-to-definition, find-references, hover (type and docs), "+
				"and document symbols. The server is detected from the file extension. When no language server is installed the "+
				"call falls back to a ripgrep symbol search and tells you exactly what to install — it never dead-ends.",
		),
		mcp.WithString("operation", mcp.Required(),
			mcp.Description("Which navigation to perform."),
			mcp.Enum("definition", "references", "hover", "documentSymbol"),
		),
		mcp.WithString("path", mcp.Required(), mcp.Description("File to navigate from, absolute or relative to the workspace root.")),
		mcp.WithNumber("line", mcp.Description("1-based line of the symbol. Required for definition, references and hover."), mcp.Min(1)),
		mcp.WithNumber("character", mcp.Description("1-based column of the symbol. Required for definition, references and hover."), mcp.Min(1)),
		mcp.WithString("symbol", mcp.Description("Symbol name. Used to locate the position when line/character are omitted, and by the fallback search.")),
		mcp.WithBoolean("includeDeclaration", mcp.Description("For references, include the declaration itself (default true)."), mcp.DefaultBool(true)),
		mcp.WithNumber("limit", mcp.Description("Maximum locations to return (default 50, max 200)."), mcp.Min(1), mcp.Max(200)),
	)
}

func (s *Server) handleLSPNavigate(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireLocal("lsp_navigate"); tErr != nil {
		return protocol.Failure(tErr)
	}
	operation, err := req.RequireString("operation")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("operation is required"))
	}
	rawPath, err := req.RequireString("path")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("path is required"))
	}
	resolved, tErr := s.guard.Validate(s.resolveRelative(rawPath))
	if tErr != nil {
		return protocol.Failure(tErr)
	}
	symbol := req.GetString("symbol", "")
	line := req.GetInt("line", 0)
	character := req.GetInt("character", 0)
	limit := req.GetInt("limit", 50)
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	needsPosition := operation == "definition" || operation == "references" || operation == "hover"
	if needsPosition && (line <= 0 || character <= 0) {
		if symbol == "" {
			return protocol.Failure(protocol.ErrInvalidInput(
				"%s needs either a 1-based line and character, or a symbol name to locate", operation))
		}
		// Locate the symbol's first occurrence so the agent can navigate by
		// name without having to know coordinates.
		foundLine, foundCol, ok := locateSymbol(resolved, symbol)
		if !ok {
			return protocol.Failure(protocol.NewError(protocol.CodeNotFound, false,
				"Use local_search_code to find where the symbol is declared, then pass that line and character.",
				"symbol %q does not occur in %s", symbol, rawPath))
		}
		line, character = foundLine, foundCol
	}

	srv, known := lsp.ServerForFile(resolved)
	if !known {
		return s.lspFallback(ctx, operation, resolved, symbol, limit,
			fmt.Sprintf("no language server is configured for %s files", filepath.Ext(resolved)), "")
	}
	if !s.runner.Available(srv.Binary) {
		return s.lspFallback(ctx, operation, resolved, symbol, limit,
			fmt.Sprintf("%s is not installed", srv.Binary), srv.Install)
	}

	client, err := s.lsp.ClientFor(ctx, srv, resolved)
	if err != nil {
		return s.lspFallback(ctx, operation, resolved, symbol, limit,
			fmt.Sprintf("%s could not be started: %v", srv.Binary, err), srv.Install)
	}
	if err := client.EnsureOpen(resolved); err != nil {
		return protocol.Failure(protocol.ErrInternal(err))
	}

	meta := protocol.Metadata{}
	var locations []lspLocation
	var hoverText string

	// LSP positions are zero-based; this server's API is one-based throughout,
	// so the conversion happens here and nowhere else.
	position := map[string]any{"line": line - 1, "character": character - 1}
	docParam := map[string]any{"uri": lsp.PathToURI(resolved)}

	switch operation {
	case "definition":
		raw, callErr := client.Call(ctx, "textDocument/definition", map[string]any{
			"textDocument": docParam, "position": position,
		})
		if callErr != nil {
			return s.lspFallback(ctx, operation, resolved, symbol, limit,
				fmt.Sprintf("%s failed: %v", srv.Binary, callErr), srv.Install)
		}
		locations = parseLocations(raw)

	case "references":
		raw, callErr := client.Call(ctx, "textDocument/references", map[string]any{
			"textDocument": docParam,
			"position":     position,
			"context":      map[string]any{"includeDeclaration": req.GetBool("includeDeclaration", true)},
		})
		if callErr != nil {
			return s.lspFallback(ctx, operation, resolved, symbol, limit,
				fmt.Sprintf("%s failed: %v", srv.Binary, callErr), srv.Install)
		}
		locations = parseLocations(raw)

	case "hover":
		raw, callErr := client.Call(ctx, "textDocument/hover", map[string]any{
			"textDocument": docParam, "position": position,
		})
		if callErr != nil {
			return s.lspFallback(ctx, operation, resolved, symbol, limit,
				fmt.Sprintf("%s failed: %v", srv.Binary, callErr), srv.Install)
		}
		hoverText = parseHover(raw)

	case "documentSymbol":
		raw, callErr := client.Call(ctx, "textDocument/documentSymbol", map[string]any{
			"textDocument": docParam,
		})
		if callErr != nil {
			return s.lspFallback(ctx, operation, resolved, symbol, limit,
				fmt.Sprintf("%s failed: %v", srv.Binary, callErr), srv.Install)
		}
		locations = parseDocumentSymbols(raw, resolved)

	default:
		return protocol.Failure(protocol.ErrInvalidInput("unknown operation %q", operation))
	}

	budget := s.budget()
	kept := make([]lspLocation, 0, len(locations))
	for _, loc := range locations {
		if len(kept) >= limit {
			meta.HasMore = true
			break
		}
		loc.Path = relativeTo(s.guard.PrimaryRoot(), loc.Path)
		loc.Preview = s.redact(previewLine(filepath.Join(s.guard.PrimaryRoot(), loc.Path), loc.Line), &meta)
		if !budget.TryAdd(loc.Path + loc.Preview) {
			meta.HasMore = true
			break
		}
		kept = append(kept, loc)
	}

	data := map[string]any{
		"operation":    operation,
		"path":         relativeTo(s.guard.PrimaryRoot(), resolved),
		"server":       srv.Binary,
		"usedFallback": false,
		"locations":    kept,
	}
	count := len(kept)
	if operation == "hover" {
		hoverText = s.redact(hoverText, &meta)
		data["hover"] = hoverText
		delete(data, "locations")
		if hoverText != "" {
			count = 1
		} else {
			count = 0
		}
	}
	meta.ResultCount = count

	env := &protocol.Envelope{Status: protocol.StatusFor(count), Data: data, Metadata: meta}
	env.WithHints(hints.LSP(operation, count, false)...)
	if meta.HasMore {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

// lspFallback answers with a ripgrep symbol search when a language server is
// unavailable, and always names the command that would install it.
//
// A missing language server must never be a dead end: OctoCode returns a
// generic error and leaves the agent with nowhere to go (WEAKNESSES.md #4).
func (s *Server) lspFallback(ctx context.Context, operation, path, symbol string, limit int, reason, installCmd string) *protocol.Envelope {
	if symbol == "" {
		symbol = symbolAtUnknownPosition(path)
	}
	if symbol == "" {
		tErr := protocol.NewError(protocol.CodeDependencyMiss, false,
			installHint(installCmd)+" Alternatively pass a symbol name so the fallback text search can run.",
			"%s, and no symbol name was supplied for the fallback search", reason)
		if installCmd != "" {
			tErr.WithDetail("installCommand", installCmd)
		}
		return protocol.Failure(tErr)
	}

	meta := protocol.Metadata{}
	locations, searchErr := s.ripgrepSymbolSearch(ctx, symbol, limit)

	data := map[string]any{
		"operation":      operation,
		"path":           relativeTo(s.guard.PrimaryRoot(), path),
		"usedFallback":   true,
		"fallbackReason": reason,
		"symbol":         symbol,
		"locations":      locations,
	}
	if installCmd != "" {
		data["installCommand"] = installCmd
	}
	if searchErr != nil {
		data["fallbackError"] = searchErr
	}

	budget := s.budget()
	kept := make([]lspLocation, 0, len(locations))
	for _, loc := range locations {
		loc.Preview = s.redact(loc.Preview, &meta)
		if !budget.TryAdd(loc.Path + loc.Preview) {
			meta.HasMore = true
			break
		}
		kept = append(kept, loc)
	}
	data["locations"] = kept
	meta.ResultCount = len(kept)

	env := &protocol.Envelope{Status: protocol.StatusFor(len(kept)), Data: data, Metadata: meta}
	env.WithHints(hints.LSP(operation, len(kept), true)...)
	if installCmd != "" {
		env.WithHints(fmt.Sprintf("For exact results install the language server: %s", installCmd))
	}
	return env
}

func installHint(installCmd string) string {
	if installCmd == "" {
		return "No language server is configured for this file type."
	}
	return "Install the language server with: " + installCmd + "."
}

// ripgrepSymbolSearch is the textual approximation of a symbol lookup. It
// searches for the identifier on a word boundary and reports every hit.
func (s *Server) ripgrepSymbolSearch(ctx context.Context, symbol string, limit int) ([]lspLocation, *protocol.ToolError) {
	if !s.runner.Available("rg") {
		return nil, protocol.NewError(protocol.CodeDependencyMiss, false,
			"Install ripgrep so the fallback symbol search can run: 'apt install ripgrep' or 'brew install ripgrep'.",
			"neither a language server nor ripgrep is available")
	}
	args := []string{
		"--json", "--color=never", "--no-messages",
		"--max-count", strconv.Itoa(limit),
		"--", `\b` + regexp.QuoteMeta(symbol) + `\b`, s.guard.PrimaryRoot(),
	}
	res, err := s.runner.Run(ctx, "rg", args, s.guard.PrimaryRoot())
	if err != nil {
		return nil, protocol.ErrInternal(err)
	}
	matches := parseRipgrepJSON(res.Stdout, s.guard.PrimaryRoot())

	out := make([]lspLocation, 0, len(matches))
	for _, m := range matches {
		if len(out) >= limit {
			break
		}
		out = append(out, lspLocation{
			Path:      m.Path,
			Line:      m.LineNumber,
			Character: strings.Index(m.Line, symbol) + 1,
			Preview:   strings.TrimSpace(m.Line),
			Name:      symbol,
		})
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// LSP payload parsing
// ---------------------------------------------------------------------------

type lspRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

type lspRawLocation struct {
	URI            string   `json:"uri"`
	TargetURI      string   `json:"targetUri"`
	Range          lspRange `json:"range"`
	TargetRange    lspRange `json:"targetRange"`
	TargetSelRange lspRange `json:"targetSelectionRange"`
}

// parseLocations handles all three shapes a server may return: a single
// Location, an array of Locations, or an array of LocationLinks.
func parseLocations(raw json.RawMessage) []lspLocation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var many []lspRawLocation
	if err := json.Unmarshal(raw, &many); err != nil {
		var single lspRawLocation
		if err := json.Unmarshal(raw, &single); err != nil {
			return nil
		}
		many = []lspRawLocation{single}
	}

	out := make([]lspLocation, 0, len(many))
	for _, loc := range many {
		uri, rng := loc.URI, loc.Range
		if uri == "" && loc.TargetURI != "" {
			uri = loc.TargetURI
			rng = loc.TargetSelRange
			if rng.End.Line == 0 && rng.Start.Line == 0 {
				rng = loc.TargetRange
			}
		}
		if uri == "" {
			continue
		}
		out = append(out, lspLocation{
			Path:      lsp.URIToPath(uri),
			Line:      rng.Start.Line + 1,
			Character: rng.Start.Character + 1,
			EndLine:   rng.End.Line + 1,
		})
	}
	return out
}

// symbolKinds maps the LSP SymbolKind enum to readable names.
var symbolKinds = map[int]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
	6: "method", 7: "property", 8: "field", 9: "constructor", 10: "enum",
	11: "interface", 12: "function", 13: "variable", 14: "constant",
	15: "string", 16: "number", 17: "boolean", 18: "array", 19: "object",
	20: "key", 21: "null", 22: "enumMember", 23: "struct", 24: "event",
	25: "operator", 26: "typeParameter",
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children"`
	// SymbolInformation (the flat, older shape) carries location instead.
	Location struct {
		URI   string   `json:"uri"`
		Range lspRange `json:"range"`
	} `json:"location"`
}

func parseDocumentSymbols(raw json.RawMessage, path string) []lspLocation {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var symbols []lspDocumentSymbol
	if err := json.Unmarshal(raw, &symbols); err != nil {
		return nil
	}

	var out []lspLocation
	var walk func(syms []lspDocumentSymbol, prefix string)
	walk = func(syms []lspDocumentSymbol, prefix string) {
		for _, sym := range syms {
			rng := sym.SelectionRange
			target := path
			if sym.Location.URI != "" {
				target = lsp.URIToPath(sym.Location.URI)
				rng = sym.Location.Range
			}
			name := sym.Name
			if prefix != "" {
				name = prefix + "." + name
			}
			out = append(out, lspLocation{
				Path:      target,
				Line:      rng.Start.Line + 1,
				Character: rng.Start.Character + 1,
				EndLine:   sym.Range.End.Line + 1,
				Name:      name,
				Kind:      symbolKinds[sym.Kind],
			})
			if len(sym.Children) > 0 {
				walk(sym.Children, name)
			}
		}
	}
	walk(symbols, "")

	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

// parseHover flattens the several shapes the hover result may take.
func parseHover(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var hover struct {
		Contents json.RawMessage `json:"contents"`
	}
	if err := json.Unmarshal(raw, &hover); err != nil {
		return ""
	}

	// MarkupContent: {kind, value}
	var markup struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(hover.Contents, &markup); err == nil && markup.Value != "" {
		return markup.Value
	}
	// Plain string
	var text string
	if err := json.Unmarshal(hover.Contents, &text); err == nil {
		return text
	}
	// Array of strings or MarkedStrings
	var parts []json.RawMessage
	if err := json.Unmarshal(hover.Contents, &parts); err == nil {
		var sb strings.Builder
		for _, part := range parts {
			var str string
			if err := json.Unmarshal(part, &str); err == nil {
				sb.WriteString(str + "\n")
				continue
			}
			var marked struct {
				Language string `json:"language"`
				Value    string `json:"value"`
			}
			if err := json.Unmarshal(part, &marked); err == nil {
				sb.WriteString(marked.Value + "\n")
			}
		}
		return strings.TrimSpace(sb.String())
	}
	return ""
}

// ---------------------------------------------------------------------------
// Position helpers
// ---------------------------------------------------------------------------

// locateSymbol finds the first word-boundary occurrence of a symbol in a file
// and returns its 1-based line and column.
func locateSymbol(path, symbol string) (int, int, bool) {
	raw, err := content.ReadFileLimited(path, 32<<20)
	if err != nil {
		return 0, 0, false
	}
	re, err := regexp.Compile(`\b` + regexp.QuoteMeta(symbol) + `\b`)
	if err != nil {
		return 0, 0, false
	}
	for i, line := range strings.Split(string(raw), "\n") {
		if loc := re.FindStringIndex(line); loc != nil {
			return i + 1, loc[0] + 1, true
		}
	}
	return 0, 0, false
}

// symbolAtUnknownPosition is a last resort for the fallback path: it uses the
// file's base name, which is frequently the type or module name being sought.
func symbolAtUnknownPosition(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// previewLine reads a single line for display alongside a location.
func previewLine(path string, line int) string {
	if line <= 0 {
		return ""
	}
	raw, err := content.ReadFileLimited(path, 8<<20)
	if err != nil {
		return ""
	}
	lines := strings.Split(string(raw), "\n")
	if line > len(lines) {
		return ""
	}
	return strings.TrimSpace(lines[line-1])
}
