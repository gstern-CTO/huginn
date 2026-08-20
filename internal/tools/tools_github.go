package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/go-github/v66/github"
	"github.com/mark3labs/mcp-go/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/gstern-CTO/huginn/internal/cache"
	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/content"
	"github.com/gstern-CTO/huginn/internal/ghclient"
	"github.com/gstern-CTO/huginn/internal/hints"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

const maxBulkQueries = 5

// ---------------------------------------------------------------------------
// Code search
// ---------------------------------------------------------------------------

type codeSearchQuery struct {
	Keywords  []string `json:"keywords"`
	Owner     string   `json:"owner,omitempty"`
	Repo      string   `json:"repo,omitempty"`
	Extension string   `json:"extension,omitempty"`
	Path      string   `json:"path,omitempty"`
	Language  string   `json:"language,omitempty"`
	Match     string   `json:"match,omitempty"` // "file" (text) or "path"
}

type codeSearchArgs struct {
	Queries []codeSearchQuery `json:"queries"`
	Concise bool              `json:"concise"`
	Minify  string            `json:"minify"`
	Limit   int               `json:"limit"`
}

type codeMatch struct {
	Repository string   `json:"repository"`
	Path       string   `json:"path"`
	URL        string   `json:"url"`
	LineAnchor *int     `json:"lineAnchor,omitempty"`
	Fragments  []string `json:"fragments,omitempty"`
}

type codeSearchResult struct {
	Query      string              `json:"query"`
	Status     protocol.Status     `json:"status"`
	TotalCount int                 `json:"totalCount"`
	Matches    []codeMatch         `json:"matches,omitempty"`
	Error      *protocol.ToolError `json:"error,omitempty"`
}

func toolGitHubSearchCode() mcp.Tool {
	return mcp.NewTool("github_search_code",
		mcp.WithDescription(
			"Search code across GitHub repositories. Accepts up to 5 queries in one call and runs them in parallel; "+
				"a partial failure returns the successful results alongside a per-query error. "+
				"Content fragments are minified and secret-redacted. Use concise=true for a cheap paths-only landscape map.",
		),
		mcp.WithArray("queries",
			mcp.Required(),
			mcp.Description("Up to 5 search queries, executed in parallel."),
			mcp.MaxItems(maxBulkQueries),
			mcp.Items(map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keywords":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Search terms, ANDed together."},
					"owner":     map[string]any{"type": "string", "description": "Restrict to a user or organisation."},
					"repo":      map[string]any{"type": "string", "description": "Restrict to a repository; requires owner."},
					"extension": map[string]any{"type": "string", "description": "File extension without the dot, e.g. go."},
					"path":      map[string]any{"type": "string", "description": "Restrict to a path prefix."},
					"language":  map[string]any{"type": "string", "description": "GitHub language name, e.g. Go."},
					"match":     map[string]any{"type": "string", "enum": []string{"file", "path"}, "description": "Search file text (default) or file paths."},
				},
				"required": []string{"keywords"},
			}),
		),
		mcp.WithBoolean("concise", mcp.Description("Return paths only, without text fragments. Much cheaper for mapping a landscape."), mcp.DefaultBool(false)),
		mcp.WithString("minify", mcp.Description("Content shaping for fragments: none, standard (default), or symbols."), mcp.Enum("none", "standard", "symbols")),
		mcp.WithNumber("limit", mcp.Description("Maximum matches per query (default 10, max 50)."), mcp.Min(1), mcp.Max(50)),
	)
}

func (s *Server) handleGitHubSearchCode(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireGitHub(); tErr != nil {
		return protocol.Failure(tErr)
	}
	var args codeSearchArgs
	if err := req.BindArguments(&args); err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("cannot parse arguments: %v", err))
	}
	if len(args.Queries) == 0 {
		return protocol.Failure(protocol.ErrInvalidInput("at least one query is required"))
	}
	if len(args.Queries) > maxBulkQueries {
		return protocol.Failure(protocol.ErrInvalidInput("at most %d queries per call, got %d", maxBulkQueries, len(args.Queries)))
	}
	mode, tErr := content.ParseMinifyMode(args.Minify)
	if tErr != nil {
		return protocol.Failure(tErr)
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 10
	}

	meta := protocol.Metadata{}
	budget := s.budget()

	// Every bulk call is concurrent. Go makes this the natural shape rather
	// than something bolted on afterwards (WEAKNESSES.md #1).
	results := make([]codeSearchResult, len(args.Queries))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(s.githubConcurrency())

	for i, q := range args.Queries {
		i, q := i, q
		group.Go(func() error {
			results[i] = s.runCodeSearch(gctx, q, limit, args.Concise, mode)
			return nil // a failed query is data, not a reason to fail the call
		})
	}
	_ = group.Wait()

	// Apply the token budget across the combined result set, trimming
	// fragments rather than dropping whole queries where possible.
	total, truncated := 0, false
	for i := range results {
		kept := results[i].Matches[:0]
		for _, m := range results[i].Matches {
			for j, frag := range m.Fragments {
				cleaned := s.redact(frag, &meta)
				m.Fragments[j] = cleaned
			}
			if !budget.TryAdd(strings.Join(m.Fragments, "\n") + m.Path) {
				truncated = true
				break
			}
			kept = append(kept, m)
		}
		results[i].Matches = kept
		total += len(kept)
	}

	topFile := ""
	for _, r := range results {
		if len(r.Matches) > 0 {
			topFile = r.Matches[0].Repository + "/" + r.Matches[0].Path
			break
		}
	}

	meta.ResultCount = total
	meta.HasMore = truncated
	env := &protocol.Envelope{
		Status:   protocol.StatusFor(total),
		Data:     map[string]any{"results": results},
		Metadata: meta,
	}
	env.WithHints(hints.CodeSearch(total, topFile, args.Concise)...)
	if truncated {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

func (s *Server) runCodeSearch(ctx context.Context, q codeSearchQuery, limit int, concise bool, mode content.MinifyMode) codeSearchResult {
	queryString, tErr := buildCodeQuery(q)
	if tErr != nil {
		return codeSearchResult{Query: queryString, Status: protocol.StatusError, Error: tErr}
	}

	opts := &github.SearchOptions{
		TextMatch:   !concise,
		ListOptions: github.ListOptions{PerPage: limit},
	}
	res, _, err := s.gh.API.Search.Code(ctx, queryString, opts)
	if err != nil {
		return codeSearchResult{Query: queryString, Status: protocol.StatusError, Error: ghclient.MapError(err, "searching code")}
	}

	out := codeSearchResult{Query: queryString, TotalCount: res.GetTotal()}
	for _, item := range res.CodeResults {
		if len(out.Matches) >= limit {
			break
		}
		m := codeMatch{
			Repository: item.GetRepository().GetFullName(),
			Path:       item.GetPath(),
			URL:        item.GetHTMLURL(),
		}
		if !concise {
			for _, tm := range item.TextMatches {
				fragment := tm.GetFragment()
				if fragment == "" {
					continue
				}
				if shaped := content.Minify(fragment, item.GetPath(), mode); shaped != "" {
					fragment = shaped
				}
				m.Fragments = append(m.Fragments, fragment)
			}
			// Line anchors are derived from cached file text when it is
			// already present: GitHub's search API returns fragments without
			// line numbers, and fetching every file to number them would cost
			// one API call per result.
			if len(m.Fragments) > 0 {
				if line, ok := s.lineAnchorFromCache(item.GetRepository().GetOwner().GetLogin(), item.GetRepository().GetName(), item.GetPath(), m.Fragments[0]); ok {
					m.LineAnchor = &line
				}
			}
		}
		out.Matches = append(out.Matches, m)
	}
	out.Status = protocol.StatusFor(len(out.Matches))
	return out
}

// buildCodeQuery assembles a GitHub search expression from structured fields,
// so the agent never has to know GitHub's qualifier syntax.
func buildCodeQuery(q codeSearchQuery) (string, *protocol.ToolError) {
	var terms []string
	for _, kw := range q.Keywords {
		if kw = strings.TrimSpace(kw); kw != "" {
			terms = append(terms, kw)
		}
	}
	if len(terms) == 0 {
		return "", protocol.ErrInvalidInput("each query needs at least one keyword")
	}
	if q.Repo != "" && q.Owner == "" {
		return "", protocol.ErrInvalidInput("repo filter %q requires an owner", q.Repo)
	}

	parts := []string{strings.Join(terms, " ")}
	if q.Repo != "" {
		parts = append(parts, "repo:"+q.Owner+"/"+q.Repo)
	} else if q.Owner != "" {
		parts = append(parts, "user:"+q.Owner)
	}
	if q.Extension != "" {
		parts = append(parts, "extension:"+strings.TrimPrefix(q.Extension, "."))
	}
	if q.Path != "" {
		parts = append(parts, "path:"+q.Path)
	}
	if q.Language != "" {
		parts = append(parts, "language:"+q.Language)
	}
	switch strings.ToLower(q.Match) {
	case "", "file":
		// GitHub's default is in:file; stating it explicitly changes nothing.
	case "path":
		parts = append(parts, "in:path")
	default:
		return "", protocol.ErrInvalidInput("match must be 'file' or 'path', got %q", q.Match)
	}
	return strings.Join(parts, " "), nil
}

// lineAnchorFromCache locates a fragment's first line within already-cached
// file content. It never triggers a fetch.
func (s *Server) lineAnchorFromCache(owner, repo, path, fragment string) (int, bool) {
	if owner == "" || repo == "" || path == "" {
		return 0, false
	}
	var cached cachedFile
	if !s.cache.GetJSON(fileCacheKey(owner, repo, "", path), &cached) {
		return 0, false
	}
	firstLine := strings.TrimSpace(strings.SplitN(fragment, "\n", 2)[0])
	if firstLine == "" {
		return 0, false
	}
	for i, line := range strings.Split(cached.Content, "\n") {
		if strings.Contains(line, firstLine) {
			return i + 1, true
		}
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// File text
// ---------------------------------------------------------------------------

type cachedFile struct {
	Content   string `json:"content"`
	SHA       string `json:"sha"`
	TotalLine int    `json:"totalLines"`
}

func fileCacheKey(owner, repo, ref, path string) string {
	return cache.CacheKey("ghfile", owner, repo, ref, path)
}

func toolGitHubFileContent() mcp.Tool {
	return mcp.NewTool("github_file_content",
		mcp.WithDescription(
			"Fetch a single file from a GitHub repository, optionally a line range. Results are cached for the session and "+
				"across restarts, so the same file is never fetched twice. Content is minified and secret-redacted.",
		),
		mcp.WithString("owner", mcp.Required(), mcp.Description("Repository owner.")),
		mcp.WithString("repo", mcp.Required(), mcp.Description("Repository name.")),
		mcp.WithString("path", mcp.Required(), mcp.Description("File path within the repository.")),
		mcp.WithString("ref", mcp.Description("Branch, tag or commit SHA. Defaults to the default branch.")),
		mcp.WithNumber("startLine", mcp.Description("First line to return, 1-based."), mcp.Min(1)),
		mcp.WithNumber("endLine", mcp.Description("Last line to return, inclusive."), mcp.Min(1)),
		mcp.WithString("minify",
			mcp.Description("symbols returns declarations without bodies; standard strips comments and blanks; none returns raw."),
			mcp.Enum("none", "standard", "symbols"),
		),
	)
}

func (s *Server) handleGitHubFileContent(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireGitHub(); tErr != nil {
		return protocol.Failure(tErr)
	}
	owner, err := req.RequireString("owner")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("owner is required"))
	}
	repo, err := req.RequireString("repo")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("repo is required"))
	}
	path, err := req.RequireString("path")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("path is required"))
	}
	ref := req.GetString("ref", "")
	startLine := req.GetInt("startLine", 0)
	endLine := req.GetInt("endLine", 0)
	mode, tErr := content.ParseMinifyMode(req.GetString("minify", ""))
	if tErr != nil {
		return protocol.Failure(tErr)
	}

	meta := protocol.Metadata{}
	key := fileCacheKey(owner, repo, ref, path)

	var file cachedFile
	if s.cache.GetJSON(key, &file) {
		meta.CacheHit = true
	} else {
		fetched, tErr := s.fetchGitHubFile(ctx, owner, repo, ref, path)
		if tErr != nil {
			return protocol.Failure(tErr)
		}
		file = *fetched
		s.cache.SetJSON(key, file, 0)
	}

	text, partial, tErr := content.SliceLines(file.Content, startLine, endLine)
	if tErr != nil {
		return protocol.Failure(tErr)
	}
	text = content.Minify(text, path, mode)
	text = s.redact(text, &meta)

	// Enforce the token budget even on a single file: a 20k-line file would
	// otherwise flood the context window regardless of what the caller asked.
	budget := s.budget()
	if !budget.TryAdd(text) {
		text = content.TruncateToTokens(text, budget.Limit())
		meta.HasMore = true
	}

	meta.ResultCount = 1
	data := map[string]any{
		"owner":      owner,
		"repo":       repo,
		"path":       path,
		"ref":        ref,
		"sha":        file.SHA,
		"totalLines": file.TotalLine,
		"content":    text,
		"minify":     string(mode),
	}
	if startLine > 0 || endLine > 0 {
		data["startLine"] = startLine
		data["endLine"] = endLine
	}

	env := &protocol.Envelope{Status: protocol.StatusHasResults, Data: data, Metadata: meta}
	env.WithHints(hints.FileContent(fmt.Sprintf("%s/%s/%s", owner, repo, path), partial || meta.HasMore)...)
	if meta.HasMore {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

func (s *Server) fetchGitHubFile(ctx context.Context, owner, repo, ref, path string) (*cachedFile, *protocol.ToolError) {
	opts := &github.RepositoryContentGetOptions{Ref: ref}
	fileContent, dirContent, _, err := s.gh.API.Repositories.GetContents(ctx, owner, repo, path, opts)
	if err != nil {
		return nil, ghclient.MapError(err, fmt.Sprintf("fetching %s/%s/%s", owner, repo, path))
	}
	if dirContent != nil {
		return nil, protocol.NewError(protocol.CodeInvalidInput, false,
			"Use github_repo_structure to list a directory; this tool reads a single file.",
			"%q is a directory, not a file", path)
	}
	if fileContent == nil {
		return nil, protocol.NewError(protocol.CodeNotFound, false,
			"Verify the path with github_repo_structure.",
			"no text returned for %q", path)
	}

	text, err := fileContent.GetContent()
	if err != nil || (text == "" && fileContent.GetSize() > 0) {
		// GetContents refuses files above 1MB and returns an empty body with
		// encoding "none"; the blob download path handles those.
		rc, _, dlErr := s.gh.API.Repositories.DownloadContents(ctx, owner, repo, path, opts)
		if dlErr != nil {
			return nil, ghclient.MapError(dlErr, fmt.Sprintf("downloading %s/%s/%s", owner, repo, path))
		}
		defer rc.Close()
		raw, readErr := content.ReadAllLimited(rc, int64(s.cfg.MaxSubprocessOutput))
		if readErr != nil {
			return nil, protocol.ErrInternal(readErr)
		}
		text = string(raw)
	}

	return &cachedFile{
		Content:   text,
		SHA:       fileContent.GetSHA(),
		TotalLine: strings.Count(text, "\n") + 1,
	}, nil
}

// ---------------------------------------------------------------------------
// Repository structure
// ---------------------------------------------------------------------------

type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	Size int    `json:"size,omitempty"`
}

func toolGitHubRepoStructure() mcp.Tool {
	return mcp.NewTool("github_repo_structure",
		mcp.WithDescription(
			"Browse a repository's directory tree with depth control and pagination. Large trees are chunked rather than "+
				"returned whole. The tree is cached for the session and across restarts.",
		),
		mcp.WithString("owner", mcp.Required(), mcp.Description("Repository owner.")),
		mcp.WithString("repo", mcp.Required(), mcp.Description("Repository name.")),
		mcp.WithString("ref", mcp.Description("Branch, tag or commit SHA. Defaults to the default branch.")),
		mcp.WithString("path", mcp.Description("Subdirectory to descend into. Defaults to the repository root.")),
		mcp.WithNumber("depth", mcp.Description("How many directory levels to include (default 2)."), mcp.Min(1), mcp.Max(20)),
		mcp.WithNumber("page", mcp.Description("1-based page number for large trees."), mcp.Min(1)),
		mcp.WithNumber("pageSize", mcp.Description("Entries per page (default 200, max 1000)."), mcp.Min(1), mcp.Max(1000)),
	)
}

func (s *Server) handleGitHubRepoStructure(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireGitHub(); tErr != nil {
		return protocol.Failure(tErr)
	}
	owner, err := req.RequireString("owner")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("owner is required"))
	}
	repo, err := req.RequireString("repo")
	if err != nil {
		return protocol.Failure(protocol.ErrInvalidInput("repo is required"))
	}
	ref := req.GetString("ref", "")
	base := strings.Trim(req.GetString("path", ""), "/")
	depth := req.GetInt("depth", 2)
	page := req.GetInt("page", 1)
	pageSize := req.GetInt("pageSize", 200)
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 1000 {
		pageSize = 200
	}

	meta := protocol.Metadata{}
	key := cache.CacheKey("ghtree", owner, repo, ref)

	var entries []treeEntry
	if s.cache.GetJSON(key, &entries) {
		meta.CacheHit = true
	} else {
		resolvedRef := ref
		if resolvedRef == "" {
			r, _, rErr := s.gh.API.Repositories.Get(ctx, owner, repo)
			if rErr != nil {
				return protocol.Failure(ghclient.MapError(rErr, fmt.Sprintf("resolving default branch of %s/%s", owner, repo)))
			}
			resolvedRef = r.GetDefaultBranch()
		}
		tree, _, tErr := s.gh.API.Git.GetTree(ctx, owner, repo, resolvedRef, true)
		if tErr != nil {
			return protocol.Failure(ghclient.MapError(tErr, fmt.Sprintf("fetching tree of %s/%s", owner, repo)))
		}
		for _, e := range tree.Entries {
			entries = append(entries, treeEntry{Path: e.GetPath(), Type: e.GetType(), Size: e.GetSize()})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
		s.cache.SetJSON(key, entries, 0)
		if tree.GetTruncated() {
			meta.HasMore = true
		}
	}

	filtered := filterTree(entries, base, depth)
	total := len(filtered)
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	pageEntries := filtered[start:end]

	// The token budget is the second bound, after pagination.
	budget := s.budget()
	kept := make([]treeEntry, 0, len(pageEntries))
	for _, e := range pageEntries {
		if !budget.TryAdd(e.Path) {
			meta.HasMore = true
			break
		}
		kept = append(kept, e)
	}

	meta.ResultCount = len(kept)
	if end < total {
		meta.HasMore = true
	}

	env := &protocol.Envelope{
		Status: protocol.StatusFor(len(kept)),
		Data: map[string]any{
			"owner":        owner,
			"repo":         repo,
			"ref":          ref,
			"path":         base,
			"depth":        depth,
			"page":         page,
			"pageSize":     pageSize,
			"totalEntries": total,
			"entries":      kept,
		},
		Metadata: meta,
	}
	env.WithHints(hints.RepoStructure(meta.HasMore, depth)...)
	return env
}

// filterTree restricts a flat tree listing to a subdirectory and a depth.
func filterTree(entries []treeEntry, base string, depth int) []treeEntry {
	out := make([]treeEntry, 0, len(entries))
	baseDepth := 0
	if base != "" {
		baseDepth = strings.Count(base, "/") + 1
	}
	for _, e := range entries {
		if base != "" && !strings.HasPrefix(e.Path, base+"/") && e.Path != base {
			continue
		}
		relDepth := strings.Count(e.Path, "/") + 1 - baseDepth
		if relDepth > depth {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ---------------------------------------------------------------------------
// Repository search
// ---------------------------------------------------------------------------

type repoResult struct {
	FullName    string   `json:"fullName"`
	Description string   `json:"description,omitempty"`
	Language    string   `json:"language,omitempty"`
	Stars       int      `json:"stars"`
	Topics      []string `json:"topics,omitempty"`
	URL         string   `json:"url"`
	UpdatedAt   string   `json:"updatedAt,omitempty"`
	MatchedVia  []string `json:"matchedVia,omitempty"`
}

func toolGitHubSearchRepos() mcp.Tool {
	return mcp.NewTool("github_search_repos",
		mcp.WithDescription(
			"Find repositories by keywords, topics, language or star count. Keywords and topics are searched as separate "+
				"parallel queries and the results deduplicated, because combining them into one GitHub query returns poor results.",
		),
		mcp.WithArray("keywords", mcp.Description("Free-text terms to match against name, description and README."), mcp.WithStringItems()),
		mcp.WithArray("topics", mcp.Description("GitHub topics to match."), mcp.WithStringItems()),
		mcp.WithString("owner", mcp.Description("Restrict to a user or organisation.")),
		mcp.WithString("language", mcp.Description("Primary language, e.g. Go.")),
		mcp.WithNumber("minStars", mcp.Description("Minimum star count."), mcp.Min(0)),
		mcp.WithNumber("limit", mcp.Description("Maximum repositories to return (default 10, max 50)."), mcp.Min(1), mcp.Max(50)),
		mcp.WithString("sort", mcp.Description("Result ordering."), mcp.Enum("best-match", "stars", "updated")),
	)
}

func (s *Server) handleGitHubSearchRepos(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireGitHub(); tErr != nil {
		return protocol.Failure(tErr)
	}
	keywords := req.GetStringSlice("keywords", nil)
	topics := req.GetStringSlice("topics", nil)
	owner := req.GetString("owner", "")
	language := req.GetString("language", "")
	minStars := req.GetInt("minStars", 0)
	limit := req.GetInt("limit", 10)
	sortBy := req.GetString("sort", "best-match")
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	if len(keywords) == 0 && len(topics) == 0 && owner == "" {
		return protocol.Failure(protocol.ErrInvalidInput("provide keywords, topics, or an owner"))
	}

	// Keywords and topics become separate queries. GitHub scores a combined
	// `keyword topic:x` query poorly; two focused queries plus deduplication
	// returns materially better results.
	type namedQuery struct {
		label string
		query string
	}
	var queries []namedQuery
	qualifiers := repoQualifiers(owner, language, minStars)
	if len(keywords) > 0 {
		queries = append(queries, namedQuery{"keywords", strings.Join(append([]string{strings.Join(keywords, " ")}, qualifiers...), " ")})
	}
	for _, topic := range topics {
		if topic = strings.TrimSpace(topic); topic != "" {
			queries = append(queries, namedQuery{"topic:" + topic, strings.Join(append([]string{"topic:" + topic}, qualifiers...), " ")})
		}
	}
	if len(queries) == 0 {
		queries = append(queries, namedQuery{"owner", strings.Join(qualifiers, " ")})
	}
	if len(queries) > maxBulkQueries {
		queries = queries[:maxBulkQueries]
	}

	type queryOutcome struct {
		label string
		repos []*github.Repository
		err   *protocol.ToolError
	}
	outcomes := make([]queryOutcome, len(queries))
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(s.githubConcurrency())

	opts := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: limit}}
	if sortBy == "stars" || sortBy == "updated" {
		opts.Sort = sortBy
		opts.Order = "desc"
	}

	for i, q := range queries {
		i, q := i, q
		group.Go(func() error {
			res, _, err := s.gh.API.Search.Repositories(gctx, q.query, opts)
			if err != nil {
				outcomes[i] = queryOutcome{label: q.label, err: ghclient.MapError(err, "searching repositories")}
				return nil
			}
			outcomes[i] = queryOutcome{label: q.label, repos: res.Repositories}
			return nil
		})
	}
	_ = group.Wait()

	// Deduplicate across queries, recording which query surfaced each result.
	seen := map[string]int{}
	var merged []repoResult
	var queryErrors []map[string]any
	for _, outcome := range outcomes {
		if outcome.err != nil {
			queryErrors = append(queryErrors, map[string]any{"query": outcome.label, "error": outcome.err})
			continue
		}
		for _, r := range outcome.repos {
			name := r.GetFullName()
			if idx, ok := seen[name]; ok {
				merged[idx].MatchedVia = append(merged[idx].MatchedVia, outcome.label)
				continue
			}
			seen[name] = len(merged)
			merged = append(merged, repoResult{
				FullName:    name,
				Description: r.GetDescription(),
				Language:    r.GetLanguage(),
				Stars:       r.GetStargazersCount(),
				Topics:      r.Topics,
				URL:         r.GetHTMLURL(),
				UpdatedAt:   formatTime(r.GetUpdatedAt().Time),
				MatchedVia:  []string{outcome.label},
			})
		}
	}

	// Repositories matched by more than one query are more likely to be the
	// right answer, so they sort first; stars break the tie.
	sort.SliceStable(merged, func(i, j int) bool {
		if len(merged[i].MatchedVia) != len(merged[j].MatchedVia) {
			return len(merged[i].MatchedVia) > len(merged[j].MatchedVia)
		}
		return merged[i].Stars > merged[j].Stars
	})

	meta := protocol.Metadata{}
	budget := s.budget()
	kept := make([]repoResult, 0, limit)
	for _, r := range merged {
		if len(kept) >= limit {
			meta.HasMore = true
			break
		}
		if !budget.TryAdd(r.FullName + r.Description) {
			meta.HasMore = true
			break
		}
		desc, n := s.redactor.Redact(r.Description)
		r.Description = desc
		meta.RedactionCount += n
		kept = append(kept, r)
	}
	meta.ResultCount = len(kept)

	data := map[string]any{"repositories": kept}
	if len(queryErrors) > 0 {
		data["queryErrors"] = queryErrors
	}
	env := &protocol.Envelope{Status: protocol.StatusFor(len(kept)), Data: data, Metadata: meta}
	env.WithHints(hints.RepoSearch(len(kept))...)
	return env
}

func repoQualifiers(owner, language string, minStars int) []string {
	var q []string
	if owner != "" {
		q = append(q, "user:"+owner)
	}
	if language != "" {
		q = append(q, "language:"+language)
	}
	if minStars > 0 {
		q = append(q, fmt.Sprintf("stars:>=%d", minStars))
	}
	return q
}

// ---------------------------------------------------------------------------
// Pull request search
// ---------------------------------------------------------------------------

type prChangedFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Patch     string `json:"patch,omitempty"`
}

type prResult struct {
	Number       int                 `json:"number"`
	Title        string              `json:"title"`
	State        string              `json:"state"`
	Author       string              `json:"author,omitempty"`
	Repository   string              `json:"repository,omitempty"`
	URL          string              `json:"url"`
	CreatedAt    string              `json:"createdAt,omitempty"`
	MergedAt     string              `json:"mergedAt,omitempty"`
	Body         string              `json:"body,omitempty"`
	ChangedFiles []prChangedFile     `json:"changedFiles,omitempty"`
	DeepReadErr  *protocol.ToolError `json:"deepReadError,omitempty"`
}

func toolGitHubSearchPullRequests() mcp.Tool {
	return mcp.NewTool("github_search_pull_requests",
		mcp.WithDescription(
			"Find pull requests by state, author or keywords. With deepRead=true the changed files and patches are fetched "+
				"in parallel, which is how you answer 'when was this introduced and why'.",
		),
		mcp.WithString("owner", mcp.Description("Restrict to a user or organisation.")),
		mcp.WithString("repo", mcp.Description("Restrict to a repository; requires owner.")),
		mcp.WithArray("keywords", mcp.Description("Free-text terms to match in title and body."), mcp.WithStringItems()),
		mcp.WithString("state", mcp.Description("Pull request state."), mcp.Enum("open", "closed", "merged", "all")),
		mcp.WithString("author", mcp.Description("GitHub login of the PR author.")),
		mcp.WithNumber("prNumber", mcp.Description("Fetch one specific PR by number; requires owner and repo."), mcp.Min(1)),
		mcp.WithBoolean("deepRead", mcp.Description("Also fetch changed files and patches for each result."), mcp.DefaultBool(false)),
		mcp.WithNumber("limit", mcp.Description("Maximum pull requests to return (default 5, max 25)."), mcp.Min(1), mcp.Max(25)),
	)
}

func (s *Server) handleGitHubSearchPullRequests(ctx context.Context, req mcp.CallToolRequest) *protocol.Envelope {
	if tErr := s.requireGitHub(); tErr != nil {
		return protocol.Failure(tErr)
	}
	owner := req.GetString("owner", "")
	repo := req.GetString("repo", "")
	keywords := req.GetStringSlice("keywords", nil)
	state := req.GetString("state", "all")
	author := req.GetString("author", "")
	prNumber := req.GetInt("prNumber", 0)
	deepRead := req.GetBool("deepRead", false)
	limit := req.GetInt("limit", 5)
	if limit <= 0 || limit > 25 {
		limit = 5
	}
	if repo != "" && owner == "" {
		return protocol.Failure(protocol.ErrInvalidInput("repo filter requires an owner"))
	}

	meta := protocol.Metadata{}
	budget := s.budget()
	var results []prResult

	if prNumber > 0 {
		if owner == "" || repo == "" {
			return protocol.Failure(protocol.ErrInvalidInput("prNumber requires both owner and repo"))
		}
		pr, _, err := s.gh.API.PullRequests.Get(ctx, owner, repo, prNumber)
		if err != nil {
			return protocol.Failure(ghclient.MapError(err, fmt.Sprintf("fetching %s/%s#%d", owner, repo, prNumber)))
		}
		results = append(results, prResult{
			Number:     pr.GetNumber(),
			Title:      pr.GetTitle(),
			State:      pr.GetState(),
			Author:     pr.GetUser().GetLogin(),
			Repository: owner + "/" + repo,
			URL:        pr.GetHTMLURL(),
			CreatedAt:  formatTime(pr.GetCreatedAt().Time),
			MergedAt:   formatTime(pr.GetMergedAt().Time),
			Body:       pr.GetBody(),
		})
	} else {
		query, tErr := buildPRQuery(owner, repo, keywords, state, author)
		if tErr != nil {
			return protocol.Failure(tErr)
		}
		res, _, err := s.gh.API.Search.Issues(ctx, query, &github.SearchOptions{
			ListOptions: github.ListOptions{PerPage: limit},
		})
		if err != nil {
			return protocol.Failure(ghclient.MapError(err, "searching pull requests"))
		}
		for _, issue := range res.Issues {
			if len(results) >= limit {
				break
			}
			results = append(results, prResult{
				Number:     issue.GetNumber(),
				Title:      issue.GetTitle(),
				State:      issue.GetState(),
				Author:     issue.GetUser().GetLogin(),
				Repository: repoFromIssueURL(issue.GetHTMLURL()),
				URL:        issue.GetHTMLURL(),
				CreatedAt:  formatTime(issue.GetCreatedAt().Time),
				Body:       issue.GetBody(),
			})
		}
	}

	if deepRead && len(results) > 0 {
		s.deepReadPRs(ctx, results, owner, repo)
	}

	kept := make([]prResult, 0, len(results))
	for _, r := range results {
		r.Body = s.redact(content.TruncateToTokens(r.Body, 400), &meta)
		for i := range r.ChangedFiles {
			r.ChangedFiles[i].Patch = s.redact(r.ChangedFiles[i].Patch, &meta)
		}
		if !budget.TryAdd(r.Title + r.Body + patchSize(r.ChangedFiles)) {
			meta.HasMore = true
			break
		}
		kept = append(kept, r)
	}
	meta.ResultCount = len(kept)

	env := &protocol.Envelope{
		Status:   protocol.StatusFor(len(kept)),
		Data:     map[string]any{"pullRequests": kept},
		Metadata: meta,
	}
	env.WithHints(hints.PRSearch(len(kept), deepRead)...)
	if meta.HasMore {
		env.WithHints(hints.BudgetExhausted())
	}
	return env
}

// deepReadPRs fetches changed files for each result concurrently. A failure on
// one PR is recorded on that PR and does not affect the others.
func (s *Server) deepReadPRs(ctx context.Context, results []prResult, owner, repo string) {
	group, gctx := errgroup.WithContext(ctx)
	group.SetLimit(s.githubConcurrency())

	for i := range results {
		i := i
		prOwner, prRepo := owner, repo
		if results[i].Repository != "" {
			if parts := strings.SplitN(results[i].Repository, "/", 2); len(parts) == 2 {
				prOwner, prRepo = parts[0], parts[1]
			}
		}
		if prOwner == "" || prRepo == "" {
			results[i].DeepReadErr = protocol.ErrInvalidInput("cannot determine the repository for PR #%d; pass owner and repo", results[i].Number)
			continue
		}
		group.Go(func() error {
			files, _, err := s.gh.API.PullRequests.ListFiles(gctx, prOwner, prRepo, results[i].Number,
				&github.ListOptions{PerPage: 50})
			if err != nil {
				results[i].DeepReadErr = ghclient.MapError(err, fmt.Sprintf("reading files of %s/%s#%d", prOwner, prRepo, results[i].Number))
				return nil
			}
			for _, f := range files {
				results[i].ChangedFiles = append(results[i].ChangedFiles, prChangedFile{
					Filename:  f.GetFilename(),
					Status:    f.GetStatus(),
					Additions: f.GetAdditions(),
					Deletions: f.GetDeletions(),
					Patch:     content.TruncateToTokens(f.GetPatch(), 500),
				})
			}
			return nil
		})
	}
	_ = group.Wait()
}

func buildPRQuery(owner, repo string, keywords []string, state, author string) (string, *protocol.ToolError) {
	parts := []string{"is:pr"}
	for _, kw := range keywords {
		if kw = strings.TrimSpace(kw); kw != "" {
			parts = append(parts, kw)
		}
	}
	if repo != "" {
		parts = append(parts, "repo:"+owner+"/"+repo)
	} else if owner != "" {
		parts = append(parts, "user:"+owner)
	}
	switch strings.ToLower(state) {
	case "", "all":
	case "open", "closed":
		parts = append(parts, "state:"+strings.ToLower(state))
	case "merged":
		parts = append(parts, "is:merged")
	default:
		return "", protocol.ErrInvalidInput("state must be open, closed, merged or all, got %q", state)
	}
	if author != "" {
		parts = append(parts, "author:"+author)
	}
	if len(parts) == 1 {
		return "", protocol.ErrInvalidInput("provide at least one of keywords, owner, author or state")
	}
	return strings.Join(parts, " "), nil
}

func repoFromIssueURL(htmlURL string) string {
	// https://github.com/<owner>/<repo>/pull/<n>
	parts := strings.Split(strings.TrimPrefix(htmlURL, "https://"), "/")
	if len(parts) >= 3 {
		return parts[1] + "/" + parts[2]
	}
	return ""
}

func patchSize(files []prChangedFile) string {
	var sb strings.Builder
	for _, f := range files {
		sb.WriteString(f.Patch)
	}
	return sb.String()
}

// githubConcurrency caps in-flight GitHub requests so a bulk call cannot burn
// through the rate limit in one burst.
func (s *Server) githubConcurrency() int {
	n := s.cfg.GitHubConcurrency
	if n <= 0 {
		n = config.DefaultGitHubConcurrency
	}
	return n
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
