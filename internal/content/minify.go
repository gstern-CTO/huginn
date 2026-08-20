package content

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gstern-CTO/huginn/internal/protocol"
)

// MinifyMode controls how aggressively content is shaped before it is returned.
type MinifyMode string

const (
	// MinifyNone returns content verbatim.
	MinifyNone MinifyMode = "none"
	// MinifyStandard strips comments and blank lines.
	MinifyStandard MinifyMode = "standard"
	// MinifySymbols returns declarations without bodies.
	MinifySymbols MinifyMode = "symbols"
)

func ParseMinifyMode(s string) (MinifyMode, *protocol.ToolError) {
	switch MinifyMode(strings.ToLower(strings.TrimSpace(s))) {
	case "", MinifyStandard:
		return MinifyStandard, nil
	case MinifyNone:
		return MinifyNone, nil
	case MinifySymbols:
		return MinifySymbols, nil
	default:
		return "", protocol.ErrInvalidInput("unknown minify mode %q; expected one of none, standard, symbols", s)
	}
}

// Minify shapes content according to mode.
//
// For Go — the primary language here — symbol extraction runs through go/parser
// and go/ast from the standard library. That is a complete, exact parser with
// no external dependency, replacing OctoCode's platform-specific compiled Rust
// engine (WEAKNESSES.md #6). Other languages fall back to comment-stripping and
// declaration heuristics in pure Go.
func Minify(content, filename string, mode MinifyMode) string {
	switch mode {
	case MinifyNone, "":
		return content
	case MinifySymbols:
		return minifySymbols(content, filename)
	case MinifyStandard:
		return minifyStandard(content, filename)
	default:
		return content
	}
}

// minifySymbols returns the declaration surface of a file: signatures, types,
// constants and variables, with function bodies elided.
func minifySymbols(content, filename string) string {
	if strings.EqualFold(filepath.Ext(filename), ".go") {
		if out, ok := goSymbols(content, filename); ok {
			return out
		}
		// A file that does not parse (a fragment, or a partial read) falls
		// through to the heuristic path rather than returning nothing.
	}
	return heuristicSymbols(content, filename)
}

// goSymbols uses the Go AST. It reports ok=false when the source does not parse,
// so the caller can fall back.
func goSymbols(content, filename string) (string, bool) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, content, parser.SkipObjectResolution)
	if err != nil {
		return "", false
	}

	var sb strings.Builder
	sb.WriteString("package " + file.Name.Name + "\n")

	// Imports first: they tell the reader what the file depends on.
	var imports []string
	for _, imp := range file.Imports {
		if imp.Path == nil {
			continue
		}
		entry := imp.Path.Value
		if imp.Name != nil {
			entry = imp.Name.Name + " " + entry
		}
		imports = append(imports, entry)
	}
	if len(imports) > 0 {
		sb.WriteString("\nimport (\n")
		for _, imp := range imports {
			sb.WriteString("\t" + imp + "\n")
		}
		sb.WriteString(")\n")
	}

	cfg := &printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 4}

	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			line := fset.Position(d.Pos()).Line
			// Print the signature with the body removed. Copying the node
			// keeps the original AST intact for any later pass.
			stripped := *d
			stripped.Body = nil
			stripped.Doc = nil
			var buf bytes.Buffer
			if err := cfg.Fprint(&buf, fset, &stripped); err != nil {
				continue
			}
			sb.WriteString("\n// L" + strconv.Itoa(line) + "\n")
			sb.WriteString(strings.TrimSpace(buf.String()) + "\n")

		case *ast.GenDecl:
			if d.Tok == token.IMPORT {
				continue // already emitted above
			}
			line := fset.Position(d.Pos()).Line
			stripped := *d
			stripped.Doc = nil
			var buf bytes.Buffer
			if err := cfg.Fprint(&buf, fset, &stripped); err != nil {
				continue
			}
			sb.WriteString("\n// L" + strconv.Itoa(line) + "\n")
			sb.WriteString(strings.TrimSpace(buf.String()) + "\n")
		}
	}
	return sb.String(), true
}

// declarationPrefixes are the leading keywords that introduce a declaration in
// the languages handled heuristically.
var declarationPrefixes = []string{
	"func ", "function ", "def ", "class ", "struct ", "interface ", "type ",
	"enum ", "trait ", "impl ", "module ", "package ", "public ", "private ",
	"protected ", "export ", "const ", "let ", "var ", "async ", "fn ",
	"abstract ", "static ", "@interface", "extension ", "protocol ",
}

// heuristicSymbols extracts declaration-looking lines for languages without a
// stdlib parser. It is intentionally simple: precision here is a convenience,
// not a correctness boundary.
func heuristicSymbols(content, filename string) string {
	stripped := StripComments(content, filename)
	var out []string
	for i, line := range strings.Split(stripped, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		// Ignore deeply indented lines: they are almost always statements
		// inside a body rather than declarations.
		if leadingWidth(line) > 4 {
			continue
		}
		lower := strings.ToLower(trimmed)
		for _, prefix := range declarationPrefixes {
			if strings.HasPrefix(lower, prefix) {
				// Drop an opening brace so the output reads as a signature.
				sig := strings.TrimSuffix(strings.TrimSpace(trimmed), "{")
				out = append(out, "L"+strconv.Itoa(i+1)+": "+strings.TrimSpace(sig))
				break
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// minifyStandard removes comments and blank lines while preserving code.
func minifyStandard(content, filename string) string {
	stripped := StripComments(content, filename)
	lines := strings.Split(stripped, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.TrimSpace(trimmed) == "" {
			continue
		}
		out = append(out, trimmed)
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, "\n") + "\n"
}

// commentSyntax describes how a language delimits comments.
type commentSyntax struct {
	line       []string
	blockOpen  string
	blockClose string
	// quotes are the string delimiters that must be respected so that a `//`
	// inside a URL literal is not mistaken for a comment.
	quotes []byte
	// rawQuote is a delimiter with no escape processing (Go backticks).
	rawQuote byte
}

var cLike = commentSyntax{line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: []byte{'"', '\''}}

func syntaxFor(filename string) commentSyntax {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".go":
		return commentSyntax{line: []string{"//"}, blockOpen: "/*", blockClose: "*/", quotes: []byte{'"', '\''}, rawQuote: '`'}
	case ".py", ".rb", ".sh", ".bash", ".zsh", ".yaml", ".yml", ".toml", ".tf", ".pl", ".r":
		return commentSyntax{line: []string{"#"}, quotes: []byte{'"', '\''}}
	case ".sql":
		return commentSyntax{line: []string{"--"}, blockOpen: "/*", blockClose: "*/", quotes: []byte{'"', '\''}}
	case ".lua":
		return commentSyntax{line: []string{"--"}, blockOpen: "--[[", blockClose: "]]", quotes: []byte{'"', '\''}}
	case ".hs":
		return commentSyntax{line: []string{"--"}, blockOpen: "{-", blockClose: "-}", quotes: []byte{'"'}}
	case ".lisp", ".clj", ".el":
		return commentSyntax{line: []string{";"}, quotes: []byte{'"'}}
	case ".ini", ".cfg", ".conf":
		return commentSyntax{line: []string{"#", ";"}, quotes: []byte{'"', '\''}}
	case ".json":
		return commentSyntax{} // JSON has no comments
	case ".md", ".txt", ".rst":
		return commentSyntax{} // prose: nothing to strip
	default:
		return cLike
	}
}

// stripComments removes comments while respecting string literals. It is a
// character-level scanner rather than a regex, because a regex cannot tell a
// comment marker from the same characters inside a string.
func StripComments(content, filename string) string {
	syn := syntaxFor(filename)
	if len(syn.line) == 0 && syn.blockOpen == "" {
		return content
	}

	var out strings.Builder
	out.Grow(len(content))

	var (
		inBlock  bool
		inQuote  bool
		quoteCh  byte
		inRaw    bool
		escaping bool
	)

	for i := 0; i < len(content); i++ {
		c := content[i]

		if inRaw {
			out.WriteByte(c)
			if c == syn.rawQuote {
				inRaw = false
			}
			continue
		}

		if inQuote {
			out.WriteByte(c)
			switch {
			case escaping:
				escaping = false
			case c == '\\':
				escaping = true
			case c == quoteCh:
				inQuote = false
			}
			continue
		}

		if inBlock {
			if syn.blockClose != "" && strings.HasPrefix(content[i:], syn.blockClose) {
				i += len(syn.blockClose) - 1
				inBlock = false
			} else if c == '\n' {
				out.WriteByte('\n') // keep line numbering stable
			}
			continue
		}

		// Not inside a comment or string: check for the start of one.
		if syn.rawQuote != 0 && c == syn.rawQuote {
			inRaw = true
			out.WriteByte(c)
			continue
		}
		if containsByte(syn.quotes, c) {
			inQuote = true
			quoteCh = c
			out.WriteByte(c)
			continue
		}
		if syn.blockOpen != "" && strings.HasPrefix(content[i:], syn.blockOpen) {
			inBlock = true
			i += len(syn.blockOpen) - 1
			continue
		}
		if marker, ok := matchLineComment(content[i:], syn.line); ok {
			_ = marker
			// Skip to the end of the line, leaving the newline in place.
			for i < len(content) && content[i] != '\n' {
				i++
			}
			if i < len(content) {
				out.WriteByte('\n')
			}
			continue
		}
		out.WriteByte(c)
	}

	return out.String()
}

func matchLineComment(s string, markers []string) (string, bool) {
	for _, m := range markers {
		if m != "" && strings.HasPrefix(s, m) {
			return m, true
		}
	}
	return "", false
}

func containsByte(set []byte, c byte) bool {
	for _, s := range set {
		if s == c {
			return true
		}
	}
	return false
}

func leadingWidth(line string) int {
	n := 0
	for _, r := range line {
		switch r {
		case ' ':
			n++
		case '\t':
			n += 4
		default:
			return n
		}
	}
	return n
}
