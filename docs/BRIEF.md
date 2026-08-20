# GoResearchMCP - Implementation Brief

*For the implementing agent.*
This document describes what to build, why each piece exists, and what it must
do. It does not prescribe file names, directory structure, or function signatures -
those are your decisions as the implementer. Use your judgment on Go idioms.

---

## What you are building

A single Go binary that speaks the Model Context Protocol (MCP) over stdio.
It gives any MCP-compatible AI client (Claude Code, Cursor, etc.) the ability
to research code: search GitHub, read local files, navigate codebases semantically,
and query Databricks for pipeline telemetry data.

The reference it is inspired by is OctoCode (github.com/bgauryy/octocode),
a TypeScript MCP server. You are not porting it - you are building a clean-room
Go equivalent that fixes its known weaknesses. The result is a single binary,
MIT licensed, fully owned by the team.

---

## Context: why Go, why now

OctoCode is licensed under PolyForm Small Business 1.0.0, which prohibits use
by organizations with more than 100 employees or $1M revenue. Infoblox does not
qualify. Building in Go gives full ownership, no license risk, and several
technical advantages over the TypeScript original (detailed in WEAKNESSES.md).

---

## The 10 tools this binary must expose

These are the capabilities the AI client will call. Design your tool names,
input schemas, and output shapes around these descriptions.

### GitHub tools (always available, require GITHUB_TOKEN)

*Code search*
Search for code across GitHub repositories. Accepts up to 5 queries in one
call and runs them in parallel. Each query can filter by keyword, owner, repo,
file extension, and path. Returns file paths, line anchors, and matched content
fragments. Content fragments must be minified and secret-redacted before
returning. Supports a "concise" mode that returns paths only (no content) for
cheap landscape mapping.

*File content*
Fetch a single file from a GitHub repository. Supports partial reads via line
range. Caches results - the same file should not be fetched twice in a session.
Applies minification: "symbols" mode returns function/type signatures only
(no bodies), "standard" strips comments and blanks, "none" returns raw.
Redacts secrets from content before returning.

*Repository structure*
Browse a repository's directory tree. Supports depth control and pagination -
large trees must be chunked rather than returned all at once. Caches the tree
for the session.

*Repository search*
Find repositories by keywords, topics, language, stars. When both keywords and
topics are provided, split into separate API calls and deduplicate results -
combining them into one query produces poor GitHub search results.

*Pull request search*
Find pull requests by state, author, keywords. Supports a "deep read" mode
that fetches the PR's changed files and patches - useful for "find when this
was introduced" investigations.

### Local filesystem tools (only when ENABLE_LOCAL=true)

Every local tool must validate the requested path against the workspace
boundary before doing anything else. A path that escapes the workspace must
be rejected with a clear error - never silently clamped.

*Local code search*
Wrap ripgrep (rg) for fast pattern search across local files. Supports
file type filtering, regex modes, and a "discovery" mode that returns match
counts per file rather than content (cheap first pass before reading). Never
construct shell strings - always pass arguments as arrays to the subprocess.

*Local file content*
Read a file from the local filesystem. Refuse to read binary files. For files
over 1MB, require a line range or match string - never load a large file
entirely into memory. Apply the same minification and redaction as the GitHub
file content tool.

*Local file finder*
Find files by name pattern, type, extension, or modification time. Cap results
at a configurable limit and signal when more exist.

*Local directory structure*
Walk and display a directory tree. Paginate large directories. For depth=1,
use a single os.ReadDir call - don't walk recursively when you don't need to.

### Semantic navigation (only when ENABLE_LOCAL=true)

*LSP navigation*
Connect to a running language server (gopls, pyright, rust-analyzer, etc.)
via JSON-RPC 2.0 over stdio and perform: go-to-definition, find-references,
hover (type + docs), document symbols (all symbols in a file). Detect which
language server to use from the file extension. If no server is available,
fall back to ripgrep-based regex symbol search and tell the caller which
server to install.

### Data tools (Infoblox-specific, not in OctoCode)

*Databricks query*
Execute a read-only SQL query against Databricks (dev or prod workspace).
Enforce read-only: reject any statement containing INSERT, UPDATE, DELETE, DROP,
TRUNCATE, ALTER, or CREATE before sending it anywhere. Default to dev - require
explicit env: "prod" to query production. Return rows as structured data with
column names. Cap row count at a configurable maximum.

---

## Cross-cutting concerns

These apply to every tool, not just specific ones.

### Security layer

Build a security layer and initialize it before any tool can run. It has three
responsibilities:

*Path validation:* Every local filesystem path passes through a validator that
resolves symlinks and checks the result is within the configured workspace root
or an explicitly allowed path. Symlink resolution is mandatory - a symlink that
points outside the workspace must be rejected after resolution, not before.
Maintain a list of blocked file patterns (.env, *.pem, *.key, .aws/, .kube/,
*.tfstate, and similar) that are refused regardless of path validity.

*Secret redaction:* Every string returned to the caller passes through a
redactor that replaces matched secrets with [REDACTED]. Compile all regex
patterns once at initialization - never compile inside a hot path. Patterns must
cover: AWS keys, GitHub tokens, generic bearer tokens, private key blocks, JWTs,
Anthropic and OpenAI API keys, GCP service account indicators, and high-entropy
strings. Include a redaction count in the response metadata so the caller knows
redaction occurred.

*Safe subprocess execution:* Any tool that spawns a subprocess must use an
explicit argument array - never a shell string. Maintain a whitelist of allowed
binaries (rg, find, git, gh, gopls, pyright-langserver, rust-analyzer, and
similar language servers). Reject anything not on the whitelist before it runs.
Cap subprocess output at a configurable size limit.

### Caching

Cache GitHub responses to avoid redundant API calls. Use two tiers: in-memory
for the current session (fast, volatile) and disk for persistence across server
restarts. File content and repository trees are worth caching - search results
are not (queries differ too often). Cache keys must be stable: same inputs
always produce the same key.

### Rate limiting

Respect GitHub's rate limits. Authenticated accounts get 5000 requests/hour,
unauthenticated get 60. When a 429 or rate limit response arrives, read the
X-RateLimit-Reset header (a Unix timestamp) and sleep until exactly that
moment, then retry once. Do not use fixed exponential backoff for rate limit
responses - it either sleeps too long or not long enough.

### Parallel execution

All bulk tool calls (multiple queries in one invocation) must run concurrently.
Go's concurrency primitives make this natural - use errgroup or similar.
Cap concurrent GitHub requests at 5 to respect rate limits. Partial failures
in a bulk call must not fail the whole call - return successful results alongside
per-query error fields.

### Standardized responses

Every tool returns the same envelope:
- A status field: "hasResults", "empty", or "error"
- A data field with the actual results
- An error field (on failure) that is a structured object, not a raw string.
  The error object includes a machine-readable code, a human message, whether
  retrying might help, and a hint for what the agent should do next.
- A hints array: 1-3 contextual suggestions for what to investigate next,
  generated server-side based on the result. Hints are the most important UX
  feature - they guide the agent without requiring it to reason about next steps.
- A metadata field: result count, whether more pages exist, cache hit flag,
  estimated token count, redaction count.

### Token budgeting

Every tool response has a token budget (configurable, default ~8000 tokens,
estimated as content length / 4). When the budget is exhausted, stop adding
results, set hasMore=true, and include a hint to paginate or narrow the query.
This prevents context window flooding regardless of how many results the
underlying API returned.

### Hints - implement these specifically

Hints are what differentiate a useful research tool from a raw API wrapper.
Generate them server-side based on result type. Examples:

After a code search with results:
â†’ "Use file content to read [top result file]"
â†’ "Use LSP references to trace where these symbols are called from"

After a code search with no results:
â†’ "Try broader keywords or remove the extension filter"
â†’ "Try match=path to search filenames instead of content"

After fetching file content:
â†’ "Use LSP definition to trace any symbol in this file"
â†’ "Use PR search to find when this file was last changed and why"

After LSP references returning more than 20 results:
â†’ "Too many references - narrow with a more specific symbol or filter by directory"

After a Databricks query with no rows:
â†’ "Try a longer time range or check the table name with SHOW TABLES"

After any auth error:
â†’ "Check that GITHUB_TOKEN is set and has repo + read:org scopes"

---

## Configuration

The binary is configured through environment variables with optional fallback
to a JSON config file at ~/.go-research-mcp/config.json. Environment variables
always win over the file.

Key settings to support:
- GitHub token (check OCTOCODE_TOKEN, then GH_TOKEN, then GITHUB_TOKEN in that order;
  also try gh auth token as a final fallback)
- GitHub API URL (for GitHub Enterprise)
- Whether local tools are enabled (default: false for safety)
- Whether cloning is enabled (default: false)
- Workspace root path (local tools are constrained to this)
- Additional allowed paths beyond workspace root
- Request timeout and max retries
- Max response token budget
- Databricks host and token per environment

Validate config at startup. If local tools are enabled but workspace root
doesn't exist, fail fast with a clear error. If no GitHub token is found,
log a warning but don't fail - local-only mode is valid.

---

## Observability

Expose Prometheus metrics on a separate HTTP port (default 9090, configurable).
Metrics to expose: tool call count by tool name and status, tool call latency
histogram by tool name, cache hit ratio, GitHub rate limit remaining, active
LSP server count. This lets you wire the MCP server into your existing
Grafana stack and observe it the same way you observe ti-proxy-grpc.

---

## Go specifics

Use Go 1.22 or later. Keep the dependency list minimal - prefer stdlib.
The approved external dependencies are: an MCP server SDK (mark3labs/mcp-go
is the most mature), the official GitHub API client (google/go-github),
oauth2 for GitHub token auth, an in-memory cache library, errgroup for
parallel execution, and testify for test assertions. Do not add dependencies
without a clear reason.

This is a single binary in a single Go module. Do not create internal packages
unless there is a clear reusability reason. Flat is fine. Keep the structure
as simple as the problem allows.

---

## What the binary does NOT do

- It does not serve HTTP for AI clients - only stdio (MCP protocol)
- It does not write to any repository or database (read-only everywhere)
- It does not manage credentials - it reads them from the environment
- It does not implement OAuth flows - token is passed in as env var
- It does not have a web UI or dashboard

---

## Definition of done

A developer on a fresh machine should be able to:

export GITHUB_TOKEN=ghp_...
export ENABLE_LOCAL=true
export WORKSPACE_ROOT=~/code
go install github.com/yourorg/go-research-mcp@latest

And then add it to Claude Code's MCP config and immediately have all tools
available. The binary should produce no output on startup - only the MCP
protocol messages over stdio. Errors go to stderr.

Tests must cover: all security boundary cases (path traversal, symlink escape,
command injection, secret redaction), config loading priority, cache hit/miss
behavior, and the hint generation logic. Integration tests that make real GitHub
API calls are acceptable but must be gated behind a build tag.