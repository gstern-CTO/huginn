package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/gstern-CTO/huginn/internal/content"
	"github.com/gstern-CTO/huginn/internal/hints"
	"github.com/gstern-CTO/huginn/internal/protocol"
	"github.com/gstern-CTO/huginn/internal/security"
)

// ---------------------------------------------------------------------------
// Local code search (ripgrep)
// ---------------------------------------------------------------------------

type localMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"lineNumber"`
	Line       string `json:"line"`
}

type localFileCount struct {
	Path  string `json:"path"`
	Count int    `json:"count"`
}

func toolLocalSearchCode() mcp.Tool {
	return mcp.NewTool("local_search_code",
		mcp.WithDescription(
			"Search the local workspace with ripgrep. Use discovery=true first for a cheap per-file match-count pass, then "+
				"re-run without it on the densest file. All paths are validated against the workspace boundary.",
		),
		mcp.WithString("pattern", mcp.Required(), mcp.Description("Pattern to search for.")),
		mcp.WithString("path", mcp.Description("Directory or file to search. Defaults to the workspace root.")),
		mcp.WithBoolean("regex", mcp.Description("Treat the pattern as a regular expression (default true). Set false for a literal search."), mcp.DefaultBool(true)),
		mcp.WithBoolean("caseSensitive", mcp.Description("Match case exactly (default false)."), mcp.DefaultBool(false)),
		mcp.WithBoolean("discovery", mcp.Description("Return match counts per file instead of matching lines."), mcp.DefaultBool(false)),
		mcp.WithArray("fileTypes", mcp.Description("Ripgrep type filters, e.g. go, py, ts."), mcp.WithStringItems()),
		mcp.WithArray("globs", mcp.Description("Glob filters, e.g. **/*_test.go or !vendor/**."), mcp.WithStringItems()),
		mcp.WithNumber("maxMatches", mcp.Description("Maximum matching lines to return (default 100, max 500)."), mcp.Min(1), mcp.Max(500)),
	)
}

func (s *Server) handleLocalSearchCode(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireLocal("local_search_code"); tErr != nil {
		return protocol.Failure(tErr)
	}
	pattern, err := req.RequireString("pattern")
	if err != nil || strings.TrimSpace(pattern) == "" {
		return protocol.Failure(protocol.ErrInvalidInput("pattern is required"))
	}
	// The workspace boundary is checked before anything else, including tool
	// availability: a caller that may not touch a path should learn that, not
	// which binaries this host happens to have installed.
	searchPath := req.GetString("path", s.guard.PrimaryRoot())
	resolved, tErr := s.guard.Validate(searchPath)
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	if !s.runner.Available("rg") {
		return protocol.Failure(protocol.NewError(protocol.CodeDependencyMiss, false,
			"Install ripgrep: 'apt install ripgrep', 'brew install ripgrep', or 'cargo install ripgrep'. "+
				"Meanwhile, local_find_files and local_file_content still work.",
			"ripgrep (rg) is not installed"))
	}
	discovery := req.GetBool("discovery", false)
	maxMatches := req.GetInt("maxMatches", 100)
	if maxMatches <= 0 || maxMatches > 500 {
		maxMatches = 100
	}

	// Arguments are assembled as an array and handed straight to the process.
	// No shell is involved anywhere on this path, so metacharacters in the
	// pattern are inert data.
	args := []string{"--color=never", "--no-messages"}
	if !req.GetBool("regex", true) {
		args = append(args, "--fixed-strings")
	}
	if req.GetBool("caseSensitive", false) {
		args = append(args, "--case-sensitive")
	} else {
		args = append(args, "--ignore-case")
	}
	for _, t := range req.GetStringSlice("fileTypes", nil) {
		if t = strings.TrimSpace(t); t != "" {
			args = append(args, "--type", t)
		}
	}
	for _, g := range req.GetStringSlice("globs", nil) {
		if g = strings.TrimSpace(g); g != "" {
			args = append(args, "--glob", g)
		}
	}
	if discovery {
		args = append(args, "--count-matches")
	} else {
		args = append(args, "--json", "--max-count", strconv.Itoa(maxMatches))
	}
	// The `--` terminator keeps a pattern beginning with a dash from being
	// parsed as a flag.
	args = append(args, "--", pattern, resolved)

	res, runErr := s.runner.Run(ctx, "rg", args, resolved)
	if runErr != nil {
		if errors.Is(runErr, context.DeadlineExceeded) {
			return protocol.Failure(protocol.NewError(protocol.CodeTimeout, true,
				"Narrow the search with a path, fileTypes or globs filter.",
				"ripgrep timed out searching %s", searchPath))
		}
		return protocol.Failure(protocol.ErrInternal(runErr))
	}
	// Exit code 1 means "no matches", which is a valid empty result.
	if res.ExitCode > 1 {
		return protocol.Failure(protocol.NewError(protocol.CodeInvalidInput, false,
			"Check the pattern syntax; set regex=false to search for it literally.",
			"ripgrep rejected the search: %s", strings.TrimSpace(firstLine(res.Stderr))))
	}

	meta := protocol.Metadata{}
	budget := s.budget()

	if discovery {
		counts := parseCountMatches(res.Stdout, resolved)
		sort.Slice(counts, func(i, j int) bool { return counts[i].Count > counts[j].Count })
		kept := make([]localFileCount, 0, len(counts))
		total := 0
		for _, c := range counts {
			if !budget.TryAdd(c.Path) {
				meta.HasMore = true
				break
			}
			kept = append(kept, c)
			total += c.Count
		}
		meta.ResultCount = len(kept)
		topFile := ""
		if len(kept) > 0 {
			topFile = kept[0].Path
		}
		env := &protocol.Envelope{
			Status:   protocol.StatusFor(len(kept)),
			Data:     map[string]any{"files": kept, "totalMatches": total, "mode": "discovery"},
			Metadata: meta,
		}
		return env.WithHints(hints.LocalSearch(total, topFile, true)...)
	}

	matches := parseRipgrepJSON(res.Stdout, resolved)
	kept := make([]localMatch, 0, len(matches))
	for _, m := range matches {
		if len(kept) >= maxMatches {
			meta.HasMore = true
			break
		}
		m.Line = s.redact(strings.TrimRight(m.Line, "\r\n"), &meta)
		if !budget.TryAdd(m.Line + m.Path) {
			meta.HasMore = true
			break
		}
		kept = append(kept, m)
	}
	if res.Truncated {
		meta.HasMore = true
	}
	meta.ResultCount = len(kept)

	topFile := ""
	if len(kept) > 0 {
		topFile = kept[0].Path
	}
	env := &protocol.Envelope{
		Status:   protocol.StatusFor(len(kept)),
		Data:     map[string]any{"matches": kept, "mode": "content"},
		Metadata: meta,
	}
	env.WithHints(hints.LocalSearch(len(kept), topFile, false)...)
	if meta.HasMore {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

// ripgrepEvent is the subset of ripgrep's --json stream this server consumes.
type ripgrepEvent struct {
	Type string `json:"type"`
	Data struct {
		Path struct {
			Text string `json:"text"`
		} `json:"path"`
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
	} `json:"data"`
}

func parseRipgrepJSON(stdout, root string) []localMatch {
	var out []localMatch
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev ripgrepEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Type != "match" {
			continue
		}
		out = append(out, localMatch{
			Path:       relativeTo(root, ev.Data.Path.Text),
			LineNumber: ev.Data.LineNumber,
			Line:       ev.Data.Lines.Text,
		})
	}
	return out
}

// parseCountMatches reads ripgrep's `--count-matches` output, which is
// `path:count` per line.
func parseCountMatches(stdout, root string) []localFileCount {
	var out []localFileCount
	for _, line := range strings.Split(stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		idx := strings.LastIndexByte(line, ':')
		if idx < 0 {
			continue
		}
		count := 0
		if _, err := fmt.Sscanf(line[idx+1:], "%d", &count); err != nil {
			continue
		}
		out = append(out, localFileCount{Path: relativeTo(root, line[:idx]), Count: count})
	}
	return out
}

// ---------------------------------------------------------------------------
// Local file text
// ---------------------------------------------------------------------------

func toolLocalFileContent() mcp.Tool {
	return mcp.NewTool("local_file_content",
		mcp.WithDescription(
			"Read a file from the local workspace. Binary files are refused. Files above the size threshold require a line "+
				"range or a match string so a large file is never loaded whole. Content is minified and secret-redacted.",
		),
		mcp.WithString("path", mcp.Required(), mcp.Description("File path, absolute or relative to the workspace root.")),
		mcp.WithNumber("startLine", mcp.Description("First line to return, 1-based."), mcp.Min(1)),
		mcp.WithNumber("endLine", mcp.Description("Last line to return, inclusive."), mcp.Min(1)),
		mcp.WithString("matchString", mcp.Description("Return a window around the first line containing this string. Satisfies the large-file requirement.")),
		mcp.WithNumber("contextLines", mcp.Description("Lines of context around a matchString hit (default 40)."), mcp.Min(0), mcp.Max(500)),
		mcp.WithString("minify", mcp.Description("symbols returns declarations without bodies; standard strips comments and blanks; none returns raw."), mcp.Enum("none", "standard", "symbols")),
	)
}

func (s *Server) handleLocalFileContent(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireLocal("local_file_content"); tErr != nil {
		return protocol.Failure(tErr)
	}
	rawPath, err := req.RequireString("path")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("path is required"))
	}
	resolved, tErr := s.guard.Validate(s.resolveRelative(rawPath))
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	info, statErr := os.Stat(resolved)
	if statErr != nil {
		return protocol.Failure(protocol.NewError(protocol.CodeNotFound, false,
			"Use local_find_files to locate the file by name.",
			"cannot read %q: %v", rawPath, statErr))
	}
	if info.IsDir() {
		return protocol.Failure(protocol.ErrInvalidInput("%q is a directory; use local_directory_structure", rawPath))
	}

	startLine := req.GetInt("startLine", 0)
	endLine := req.GetInt("endLine", 0)
	matchString := req.GetString("matchString", "")
	contextLines := req.GetInt("contextLines", 40)
	mode, tErr := content.ParseMinifyMode(req.GetString("minify", ""))
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	if binary, bErr := content.IsBinaryFile(resolved); bErr != nil {
		return protocol.Failure(protocol.ErrInternal(bErr))
	} else if binary {
		return protocol.Failure(protocol.NewError(protocol.CodeBinaryFile, false,
			"This file is not text. If you need to know what it is, read its size and extension from local_find_files instead.",
			"%q is a binary file", rawPath).
			WithDetail("sizeBytes", info.Size()))
	}

	hasRange := startLine > 0 || endLine > 0 || matchString != ""

	// A too-large file must not be a dead end. The error carries the actual
	// size, the threshold that was exceeded, and what to do instead
	// (WEAKNESSES.md #5).
	if info.Size() > s.cfg.LargeFileBytes && !hasRange {
		return protocol.Failure(protocol.NewError(protocol.CodeFileTooLarge, false,
			fmt.Sprintf("Pass a line range (startLine/endLine), or pass matchString to get a window around the first occurrence. "+
				"To find the right region first, run local_search_code with path=%q.", rawPath),
			"%q is %s, above the %s threshold for an unbounded read",
			rawPath, humanBytes(info.Size()), humanBytes(s.cfg.LargeFileBytes)).
			WithDetail("sizeBytes", info.Size()).
			WithDetail("thresholdBytes", s.cfg.LargeFileBytes).
			WithDetail("suggestedAction", "retry with startLine and endLine, or with matchString"))
	}

	meta := protocol.Metadata{}
	var text string
	var totalLines, matchLine int
	partial := false

	if matchString != "" {
		// Stream the file rather than loading it: this is the path that keeps
		// a multi-hundred-megabyte log readable.
		window, line, total, wErr := readWindowAroundMatch(resolved, matchString, contextLines)
		if wErr != nil {
			return protocol.Failure(wErr)
		}
		text, matchLine, totalLines, partial = window, line, total, true
	} else {
		raw, readErr := content.ReadFileLimited(resolved, s.cfg.LargeFileBytes*8)
		if readErr != nil {
			return protocol.Failure(protocol.ErrInternal(readErr))
		}
		full := string(raw)
		totalLines = strings.Count(full, "\n") + 1
		sliced, isPartial, sErr := content.SliceLines(full, startLine, endLine)
		if sErr != nil {
			return protocol.Failure(sErr)
		}
		text, partial = sliced, isPartial
	}

	text = content.Minify(text, resolved, mode)
	text = s.redact(text, &meta)

	budget := s.budget()
	if !budget.TryAdd(text) {
		text = content.TruncateToTokens(text, budget.Limit())
		meta.HasMore = true
		partial = true
	}
	meta.ResultCount = 1

	data := map[string]any{
		"path":       relativeTo(s.guard.PrimaryRoot(), resolved),
		"sizeBytes":  info.Size(),
		"totalLines": totalLines,
		"content":    text,
		"minify":     string(mode),
		"partial":    partial,
	}
	if matchLine > 0 {
		// The anchor is a separate field rather than a comment prepended to
		// the content: minification would strip a comment, and the caller
		// should not have to parse prose to learn where the window starts.
		data["matchLine"] = matchLine
		data["matchString"] = matchString
	}

	env := &protocol.Envelope{Status: protocol.StatusHasResults, Data: data, Metadata: meta}
	env.WithHints(hints.LocalFile(rawPath, partial)...)
	if meta.HasMore {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

// readWindowAroundMatch scans a file line by line and returns a window centred
// on the first line containing needle. Memory use is bounded by the window, not
// by the file.
func readWindowAroundMatch(path, needle string, contextLines int) (string, int, int, *protocol.ToolError) {
	if contextLines < 0 {
		contextLines = 0
	}
	f, err := os.Open(path)
	if err != nil {
		return "", 0, 0, protocol.ErrInternal(err)
	}
	defer f.Close()

	before := make([]string, 0, contextLines)
	var window []string
	matchLine, lineNo, after := 0, 0, -1

	scanner := content.NewLineScanner(f)
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		if after >= 0 {
			if after < contextLines {
				window = append(window, line)
				after++
			}
			continue
		}
		if strings.Contains(line, needle) {
			matchLine = lineNo
			window = append(window, before...)
			window = append(window, line)
			after = 0
			continue
		}
		if contextLines > 0 {
			if len(before) == contextLines {
				before = before[1:]
			}
			before = append(before, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return "", 0, 0, protocol.ErrInternal(err)
	}
	if matchLine == 0 {
		return "", 0, lineNo, protocol.NewError(protocol.CodeNotFound, false,
			"Use local_search_code with a regex to find what the file actually contains, then read that line range.",
			"no line in the file contains %q", needle).
			WithDetail("totalLines", lineNo)
	}
	return strings.Join(window, "\n") + "\n", matchLine, lineNo, nil
}

// ---------------------------------------------------------------------------
// Local file finder
// ---------------------------------------------------------------------------

type foundFile struct {
	Path       string `json:"path"`
	Type       string `json:"type"`
	SizeBytes  int64  `json:"sizeBytes"`
	ModifiedAt string `json:"modifiedAt"`
}

func toolLocalFindFiles() mcp.Tool {
	return mcp.NewTool("local_find_files",
		mcp.WithDescription(
			"Find files in the workspace by name pattern, type, extension or modification time. Results are capped and the "+
				"response says when more exist.",
		),
		mcp.WithString("path", mcp.Description("Directory to search under. Defaults to the workspace root.")),
		mcp.WithString("namePattern", mcp.Description("Glob matched against the base name, e.g. *_test.go.")),
		mcp.WithArray("extensions", mcp.Description("Extensions to include, with or without the dot."), mcp.WithStringItems()),
		mcp.WithString("type", mcp.Description("Restrict to files or directories."), mcp.Enum("file", "dir", "any")),
		mcp.WithNumber("modifiedWithinHours", mcp.Description("Only entries modified within this many hours."), mcp.Min(0)),
		mcp.WithNumber("limit", mcp.Description("Maximum results (default 200, max 2000)."), mcp.Min(1), mcp.Max(2000)),
	)
}

func (s *Server) handleLocalFindFiles(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireLocal("local_find_files"); tErr != nil {
		return protocol.Failure(tErr)
	}
	root, tErr := s.guard.ValidateDir(s.resolveRelative(req.GetString("path", s.guard.PrimaryRoot())))
	if tErr != nil {
		return protocol.Failure(tErr)
	}
	namePattern := req.GetString("namePattern", "")
	if namePattern != "" {
		if _, err := filepath.Match(namePattern, "probe"); err != nil {
			return protocol.Failure(protocol.ErrInvalidInput("namePattern %q is not a valid glob: %v", namePattern, err))
		}
	}
	extensions := normaliseExtensions(req.GetStringSlice("extensions", nil))
	entryType := req.GetString("type", "file")
	withinHours := req.GetFloat("modifiedWithinHours", 0)
	limit := req.GetInt("limit", s.cfg.FindResultLimit)
	if limit <= 0 || limit > 2000 {
		limit = s.cfg.FindResultLimit
	}
	var cutoff time.Time
	if withinHours > 0 {
		cutoff = time.Now().Add(-time.Duration(withinHours * float64(time.Hour)))
	}

	var found []foundFile
	hasMore := false

	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() && shouldSkipDir(d.Name()) && path != root {
			return filepath.SkipDir
		}
		// Symlinks are not followed: WalkDir does not descend into them, and
		// a symlinked file is reported only if its target stays in bounds.
		if d.Type()&fs.ModeSymlink != 0 {
			if _, vErr := s.guard.Validate(path); vErr != nil {
				return nil
			}
		}
		if path == root {
			return nil
		}
		switch entryType {
		case "file":
			if d.IsDir() {
				return nil
			}
		case "dir":
			if !d.IsDir() {
				return nil
			}
		}
		if _, blocked := security.IsBlockedPath(path); blocked {
			return nil
		}
		if namePattern != "" {
			if ok, _ := filepath.Match(namePattern, d.Name()); !ok {
				return nil
			}
		}
		if len(extensions) > 0 && !d.IsDir() {
			if !slices.Contains(extensions, strings.ToLower(filepath.Ext(d.Name()))) {
				return nil
			}
		}
		info, iErr := d.Info()
		if iErr != nil {
			return nil
		}
		if !cutoff.IsZero() && info.ModTime().Before(cutoff) {
			return nil
		}
		if len(found) >= limit {
			hasMore = true
			return filepath.SkipAll
		}
		kind := "file"
		if d.IsDir() {
			kind = "dir"
		}
		found = append(found, foundFile{
			Path:       relativeTo(root, path),
			Type:       kind,
			SizeBytes:  info.Size(),
			ModifiedAt: formatTime(info.ModTime()),
		})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, context.Canceled) && !errors.Is(walkErr, context.DeadlineExceeded) {
		return protocol.Failure(protocol.ErrInternal(walkErr))
	}

	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })

	meta := protocol.Metadata{ResultCount: len(found), HasMore: hasMore}
	env := &protocol.Envelope{
		Status:   protocol.StatusFor(len(found)),
		Data:     map[string]any{"root": root, "files": found, "limit": limit},
		Metadata: meta,
	}
	return env.WithHints(hints.FindFiles(len(found), hasMore)...)
}

// ---------------------------------------------------------------------------
// Local directory structure
// ---------------------------------------------------------------------------

type dirEntryOut struct {
	Path      string `json:"path"`
	Type      string `json:"type"`
	SizeBytes int64  `json:"sizeBytes,omitempty"`
}

func toolLocalDirectoryStructure() mcp.Tool {
	return mcp.NewTool("local_directory_structure",
		mcp.WithDescription(
			"Walk and display a directory tree from the local workspace, with depth control and pagination for large directories.",
		),
		mcp.WithString("path", mcp.Description("Directory to list. Defaults to the workspace root.")),
		mcp.WithNumber("depth", mcp.Description("How many levels to descend (default 1, which lists just this directory)."), mcp.Min(1), mcp.Max(20)),
		mcp.WithNumber("page", mcp.Description("1-based page number."), mcp.Min(1)),
		mcp.WithNumber("pageSize", mcp.Description("Entries per page (default 200, max 1000)."), mcp.Min(1), mcp.Max(1000)),
	)
}

func (s *Server) handleLocalDirectoryStructure(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireLocal("local_directory_structure"); tErr != nil {
		return protocol.Failure(tErr)
	}
	root, tErr := s.guard.ValidateDir(s.resolveRelative(req.GetString("path", s.guard.PrimaryRoot())))
	if tErr != nil {
		return protocol.Failure(tErr)
	}
	depth := req.GetInt("depth", 1)
	if depth < 1 {
		depth = 1
	}
	page := req.GetInt("page", 1)
	pageSize := req.GetInt("pageSize", 200)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 1000 {
		pageSize = 200
	}

	var entries []dirEntryOut
	var err error
	if depth == 1 {
		// A single level needs exactly one ReadDir. Walking recursively and
		// then discarding everything below the first level would do far more
		// I/O for the same answer.
		entries, err = readDirOneLevel(root)
	} else {
		entries, err = walkDirDepth(ctx, root, depth)
	}
	if err != nil {
		return protocol.Failure(protocol.ErrInternal(err))
	}

	total := len(entries)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageEntries := entries[start:end]

	meta := protocol.Metadata{HasMore: end < total}
	budget := s.budget()
	kept := make([]dirEntryOut, 0, len(pageEntries))
	for _, e := range pageEntries {
		if !budget.TryAdd(e.Path) {
			meta.HasMore = true
			break
		}
		kept = append(kept, e)
	}
	meta.ResultCount = len(kept)

	env := &protocol.Envelope{
		Status: protocol.StatusFor(len(kept)),
		Data: map[string]any{
			"root":         root,
			"depth":        depth,
			"page":         page,
			"pageSize":     pageSize,
			"totalEntries": total,
			"entries":      kept,
		},
		Metadata: meta,
	}
	return env.WithHints(hints.DirectoryStructure(len(kept), meta.HasMore)...)
}

func readDirOneLevel(root string) ([]dirEntryOut, error) {
	dirents, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]dirEntryOut, 0, len(dirents))
	for _, d := range dirents {
		if _, blocked := security.IsBlockedPath(filepath.Join(root, d.Name())); blocked {
			continue
		}
		entry := dirEntryOut{Path: d.Name(), Type: "file"}
		if d.IsDir() {
			entry.Type = "dir"
		} else if info, err := d.Info(); err == nil {
			entry.SizeBytes = info.Size()
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func walkDirDepth(ctx context.Context, root string, maxDepth int) ([]dirEntryOut, error) {
	var out []dirEntryOut
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if path == root {
			return nil
		}
		rel := relativeTo(root, path)
		level := strings.Count(rel, string(filepath.Separator)) + 1
		if d.IsDir() && (shouldSkipDir(d.Name()) || level >= maxDepth) {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			out = append(out, dirEntryOut{Path: rel, Type: "dir"})
			return filepath.SkipDir
		}
		if level > maxDepth {
			return nil
		}
		if _, blocked := security.IsBlockedPath(path); blocked {
			return nil
		}
		entry := dirEntryOut{Path: rel, Type: "file"}
		if d.IsDir() {
			entry.Type = "dir"
		} else if info, iErr := d.Info(); iErr == nil {
			entry.SizeBytes = info.Size()
		}
		out = append(out, entry)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// ---------------------------------------------------------------------------
// Shared local helpers
// ---------------------------------------------------------------------------

// skippedDirs are never descended into: they hold build output and dependency
// trees that swamp results without answering research questions.
var skippedDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, ".venv": true,
	"venv": true, "__pycache__": true, "target": true, "dist": true,
	"build": true, ".idea": true, ".vscode": true, ".terraform": true,
	".mypy_cache": true, ".pytest_cache": true, ".next": true, ".cache": true,
}

func shouldSkipDir(name string) bool { return skippedDirs[name] }

// resolveRelative interprets a relative path against the workspace root, so an
// agent can pass "cmd/server" rather than an absolute path.
func (s *Server) resolveRelative(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return s.guard.PrimaryRoot()
	}
	if filepath.IsAbs(path) || strings.HasPrefix(path, "~") {
		return path
	}
	return filepath.Join(s.guard.PrimaryRoot(), path)
}

func relativeTo(root, path string) string {
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}

func normaliseExtensions(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if !strings.HasPrefix(e, ".") {
			e = "." + e
		}
		out = append(out, e)
	}
	return out
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return s[:idx]
	}
	return s
}
