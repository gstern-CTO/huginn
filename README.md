# Huginn

A single Go binary that speaks the [Model Context Protocol](https://modelcontextprotocol.io)
over stdio, giving any MCP-compatible AI client — Claude Code, Cursor, and others —
the ability to research code: search GitHub, read local files, navigate codebases
semantically through a language server, and query Databricks for pipeline telemetry.

---

## Install

```bash
go install github.com/gstern-CTO/huginn/cmd/huginn@latest
```

Requires Go 1.25.5 or later. With the default `GOTOOLCHAIN=auto`, an older Go
fetches the right toolchain automatically.

## Quick start

```bash
export GITHUB_TOKEN=ghp_...
export ENABLE_LOCAL=true
export WORKSPACE_ROOT=~/code
huginn
```

Two scripts handle setup. Run them in order:

```bash
./pre-run.sh                              # what is installed, what is missing
./huginn.sh --git-token ghp_...           # register with Claude Code
```

`pre-run.sh` checks every dependency Huginn can use — the binary, Go, git, the
Claude Code CLI, ripgrep, all nine language servers, the GitHub token (and
verifies it against the API), the workspace root, Databricks variables, the
cache directory and the metrics port. It never stops at the first problem: it
runs every check and prints one report grouped into required, recommended and
optional, each with an install command. Exit status is non-zero only when
something **required** is missing.

`huginn.sh` verifies the token before writing it anywhere, replaces any previous
registration, and confirms the server reports Connected afterwards. Useful flags:

```bash
./huginn.sh --git-token-file ~/.tok       # keeps the token out of shell history
./huginn.sh --no-token                    # local tools only, 6 of 11
./huginn.sh --workspace ~/code --metrics  # set the root, publish metrics
./huginn.sh --git-token ghp_... --dry-run # show the command, change nothing
```

Prefer `--git-token-file`: a token in the command line is visible in shell
history and to `ps`. Either way Claude Code stores it in `~/.claude.json` in
plain text — that is how MCP environment variables work.

Or register by hand:

```bash
make install
claude mcp add huginn -s user \
  -e ENABLE_LOCAL=true \
  -e WORKSPACE_ROOT=/path/to/your/code \
  -- huginn
```

Supply the token through your shell rather than the command, so it never
lands in a config file. Opening this repository is enough on its own: the
committed `.mcp.json` registers Huginn for the project automatically.

The equivalent configuration by hand:

```json
{
  "mcpServers": {
    "huginn": {
      "command": "huginn",
      "env": {
        "GITHUB_TOKEN": "ghp_...",
        "ENABLE_LOCAL": "true",
        "WORKSPACE_ROOT": "/home/you/code"
      }
    }
  }
}
```

The binary prints nothing on stdout except MCP protocol messages; all logging
goes to stderr.

---

## Tools

GitHub tools need `GITHUB_TOKEN`. Local tools and LSP navigation are registered
only when `ENABLE_LOCAL=true`, so their absence is visible to the agent rather
than being a silent failure at call time.

| Tool | What it does |
| --- | --- |
| `github_search_code` | Up to 5 queries in one call, run in parallel. Filters by keyword, owner, repo, extension, path, language. `concise=true` returns paths only for cheap landscape mapping. |
| `github_file_content` | One file, optionally a line range. Cached in memory and on disk. Minified and secret-redacted. |
| `github_repo_structure` | Directory tree with depth control and pagination. Tree cached per ref. |
| `github_search_repos` | Repositories by keyword, topic, language, stars. Keywords and topics run as separate queries and are deduplicated. |
| `github_search_pull_requests` | PRs by state, author, keywords. `deepRead=true` fetches changed files and patches in parallel. |
| `local_search_code` | ripgrep over the workspace. `discovery=true` returns per-file match counts as a cheap first pass. |
| `local_file_content` | Read a local file. Binary files refused; large files require a line range or `matchString`. |
| `local_find_files` | Find by name pattern, extension, type, modification time. |
| `local_directory_structure` | Walk a directory tree with depth control and pagination. |
| `lsp_navigate` | `definition`, `references`, `hover`, `documentSymbol` through a real language server, with a ripgrep fallback. |
| `databricks_query` | Read-only SQL against Databricks. Defaults to dev; production requires `env="prod"`. |

### Response envelope

Every tool returns the same shape:

```json
{
  "status": "hasResults | empty | error",
  "data": { },
  "error": {
    "code": "PATH_DENIED",
    "message": "…",
    "retryable": false,
    "hint": "what to do instead",
    "details": { "sizeBytes": 4194304, "thresholdBytes": 1048576 }
  },
  "hints": ["Use github_file_content to read acme/widget/main.go."],
  "metadata": {
    "resultCount": 12,
    "hasMore": true,
    "cacheHit": false,
    "estimatedTokens": 1840,
    "redactionCount": 2
  }
}
```

`hints` are generated server-side from the shape of the result. They are what
separates a research tool from a raw API wrapper: the agent is told what to
investigate next instead of having to work it out.

---

## Configuration

Environment variables always win over the JSON config file at
`~/.go-research-mcp/config.json`, which in turn wins over built-in defaults.

| Variable | Default | Meaning |
| --- | --- | --- |
| `GITHUB_TOKEN` | — | Checked after `OCTOCODE_TOKEN` and `GH_TOKEN`; falls back to `gh auth token`. |
| `GITHUB_API_URL` | `https://api.github.com/` | Set for GitHub Enterprise. |
| `ENABLE_LOCAL` | `false` | Enables local filesystem and LSP tools. |
| `WORKSPACE_ROOT` | — | Boundary for every local operation. Required when `ENABLE_LOCAL=true`. |
| `ALLOWED_PATHS` | — | Extra roots outside the workspace, separated by `:` or `,`. |
| `REQUEST_TIMEOUT_SECONDS` | `30` | Per-request upstream timeout. |
| `MAX_RETRIES` | `3` | Retry attempts for rate-limited GitHub requests. |
| `MAX_RESPONSE_TOKENS` | `8000` | Per-response token budget. |
| `GITHUB_CONCURRENCY` | `5` | Cap on in-flight GitHub requests. |
| `LARGE_FILE_BYTES` | `1048576` | Above this, a local read needs a range or match string. |
| `CACHE_DIR` | OS cache dir | Disk cache location. |
| `CACHE_TTL_SECONDS` | `21600` | Cache entry lifetime. |
| `METRICS_ENABLED` | `true` | Serve Prometheus metrics. |
| `METRICS_PORT` | `9090` | Metrics port, bound to localhost. |
| `DATABRICKS_DEV_*`, `DATABRICKS_PROD_*` | — | `_HOST`, `_TOKEN`, `_WAREHOUSE_ID` per environment. |
| `DATABRICKS_MAX_ROWS` | `1000` | Row cap per query. |

There is deliberately no `ENABLE_CLONE`. The brief lists it as a setting but
never describes a tool that would use it, and cloning does not fit a read-only
research server, so the flag was removed rather than left as configuration that
does nothing.

Configuration is validated at startup. Enabling local tools without a valid
`WORKSPACE_ROOT` fails fast; a missing GitHub token only warns, since
local-only mode is a legitimate way to run the server.

See `config.example.json` for the file form.

---

## Security

The security layer is initialised before any tool can run.

**Path validation.** Every local path is expanded, resolved through
`filepath.EvalSymlinks`, and only then checked against the workspace root and
any allowed paths. Resolution before the check is what catches a symlink inside
the workspace pointing at `/etc/shadow`. A path that escapes is rejected, never
clamped back inside — silently reading a different file than the one requested
is worse than an error. Credential-shaped files (`.env`, `*.pem`, `*.key`,
`*.tfstate`, `.aws/`, `.kube/`, `.ssh/`, and similar) are refused regardless.

**Secret redaction.** Everything returned to the caller passes through a
redactor covering AWS keys, GitHub tokens, bearer and basic auth headers,
private key blocks, JWTs, Anthropic/OpenAI/Slack/Stripe/GCP credentials,
URL-embedded passwords, generic `secret = …` assignments, and high-entropy
strings. Patterns are compiled once at initialisation. The entropy threshold sits
above a hex digest's theoretical maximum, so git SHAs and checksums survive while
base64-shaped secrets do not. `metadata.redactionCount` tells the caller
redaction happened.

**Subprocess execution.** Subprocesses are spawned with an explicit argument
array; no code path in this server builds a shell string, so metacharacters in a
search pattern are inert data. Only whitelisted binaries run — `rg`, `find`,
`git`, `gh`, and known language servers — and `git` is further restricted to
read-only subcommands. The child environment carries `PATH` and `HOME` but no
credentials. Output is capped.

**Read-only everywhere.** No tool writes to a repository or a database. SQL is
rejected before it leaves the process if it is anything other than a single
`SELECT`, `SHOW`, `DESCRIBE` or `EXPLAIN`.

---

## Docker

### Install from the published image

Once a release exists, a machine needs only Docker — no Go, no ripgrep, no
repository access:

```bash
./huginn-docker.sh --workspace ~/code --git-token ghp_...
```

That pulls `ghcr.io/gstern-cto/huginn`, registers the container form with
Claude Code, and verifies it connects. Pin a version with `--tag 0.1.0`.

The image is public even though the source is private — on GitHub, package
visibility is independent of repository visibility. The image carries no source:
the multi-stage build discards the stage that holds it, leaving Alpine, ripgrep,
git and one compiled binary. Go does embed source *file paths* for stack traces,
so package structure is visible; the code is not.

One disadvantage over the native install: the token is part of the `docker run`
command rather than an environment variable, so `claude mcp list` prints it in
clear text. `./huginn.sh` does not have this problem.

Releases are cut by pushing a version tag, which builds `linux/amd64` and
`linux/arm64`, publishes `X.Y.Z`, `X.Y` and `latest`, and attaches the static
binaries to the GitHub Release. Nothing publishes from `main`.

### How a containerised MCP server actually works

Huginn speaks MCP over stdio. There is no port to connect to and no daemon to
attach to: the **client starts the server process and owns its stdin and stdout
for the life of a session**. A `docker compose up -d` service would start, read
EOF on stdin, and exit.

So the model is one container per session, started by whatever is driving the
server. `docker-compose.yml` exists to describe the image, mounts, environment
and cache volume in one place, so your MCP client and your local testing spawn
identical containers. Both services sit behind Compose profiles, which means a
bare `docker compose up` deliberately starts nothing rather than appearing to
work.

```bash
make docker-build     # build the image (~59MB)
make docker-smoke     # drive a real handshake through it and list the tools
make run-local        # one interactive session on stdio
```

`make run-local` gives you a live MCP session with the current directory
mounted read-only. Paste JSON-RPC requests one per line:

```json
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"cli","version":"1"}}}
{"jsonrpc":"2.0","id":2,"method":"tools/list"}
```

Point it at another checkout with `HUGINN_WORKSPACE=/path/to/code make run-local`.

### Wiring it into an MCP client

Give the client a command that starts a container per session:

```json
{
  "mcpServers": {
    "huginn": {
      "command": "docker",
      "args": [
        "run", "--rm", "-i",
        "--mount", "type=bind,src=/home/you/code,dst=/workspace,ro",
        "--mount", "type=volume,src=huginn-cache,dst=/var/cache/huginn",
        "--read-only", "--tmpfs", "/tmp",
        "--cap-drop", "ALL",
        "--security-opt", "no-new-privileges",
        "--init",
        "-e", "GITHUB_TOKEN",
        "-e", "ENABLE_LOCAL=true",
        "huginn:local"
      ]
    }
  }
}
```

`-i` is essential and `-t` must be absent: a TTY would corrupt the protocol
stream. `-e GITHUB_TOKEN` without a value passes the variable through from the
client's environment rather than baking it into the config file.

### Why bother containerising a single static binary

- **It ships the dependencies the tools shell out to.** The image carries
  ripgrep 14, so `local_search_code` works on a machine that has never
  installed it. On the host used to develop this, `rg` was not present at all
  and that tool returned `DEPENDENCY_MISSING`; in the container it works.
- **The kernel enforces read-only, not just the path guard.** The workspace is
  a `ro` bind mount, the root filesystem is read-only, all capabilities are
  dropped, and the process runs as uid 65532. The path guard refusing to
  escape the workspace and the kernel refusing to let it are two independent
  layers; a bug in the first no longer means a modified working tree.
- **Reproducible across machines and CI.** One image, one Go toolchain
  version, one ripgrep version, whatever the developer has installed locally.
- **The cache survives.** A named volume is mounted at the cache directory, so
  the disk tier still spans sessions even though every session gets a fresh
  container. Without it, containerising would silently undo weakness #7.
- **A blast radius.** A research tool reads whatever it is pointed at; a
  container bounds that to one mounted directory.

### Language servers in the container

The default image has no language server, so `lsp_navigate` uses its ripgrep
symbol fallback and reports the install command — the designed graceful path,
not a failure. For exact Go navigation:

```bash
make docker-build-go      # larger image: gopls plus the Go toolchain
HUGINN_TARGET=runtime-go make run-local
```

gopls needs the Go toolchain beside it to resolve a module, which is why that
variant keeps the full Go base image instead of Alpine.

### Metrics from a containerised session

Metrics are off by default in session containers. Containers that live for one
session are poor scrape targets, and publishing a fixed port would stop two
sessions running side by side. When you want to watch one long-lived session:

```bash
make run-observed         # metrics on 127.0.0.1:9090/metrics
```

For ephemeral sessions, scraping is the wrong shape; aggregate elsewhere or
accept per-session visibility.

### Configuration

Compose reads a local `.env` (gitignored):

```bash
GITHUB_TOKEN=ghp_...
HUGINN_WORKSPACE=/home/you/code
HUGINN_TARGET=runtime          # or runtime-go
METRICS_PORT=9090
```

Secrets are never baked into the image — they are passed in at run time.

```bash
make docker-cache-clean   # drop the persistent response cache
make docker-clean         # also remove the image
```

---

## Observability

Prometheus metrics are served on `127.0.0.1:9090/metrics`, with `/healthz`
alongside:

- `huginn_tool_calls_total{tool,status}`
- `huginn_tool_latency_seconds{tool}`
- `huginn_cache_hits_total`, `huginn_cache_misses_total`, `huginn_cache_hit_ratio`
- `huginn_github_rate_limit_remaining`, `huginn_github_rate_limit_waits_total`
- `huginn_lsp_servers_active`
- `huginn_secrets_redacted_total`
- `huginn_github_requests_total{outcome}`

A busy metrics port logs a warning; it never takes the MCP server down.

---

## What it fixes

Each item corresponds to a numbered entry in [`docs/WEAKNESSES.md`](docs/WEAKNESSES.md).

1. **Parallel execution is the default.** Bulk queries fan out through
   `errgroup` from the first line, capped at `GITHUB_CONCURRENCY`. A failure in
   one query returns its error alongside the other queries' results.
2. **Rate limiting sleeps exactly as long as it must.** `X-RateLimit-Reset` is
   read and the transport sleeps until that instant plus one second of
   clock-skew margin, rather than applying a fixed exponential backoff.
   `Retry-After` covers secondary limits. A reset more than 15 minutes out is
   reported rather than slept through.
3. **Redaction patterns compile once**, at package initialisation.
4. **A missing language server is never a dead end.** The call falls back to a
   ripgrep symbol search, marks the result `usedFallback`, and names the exact
   install command.
5. **Large-file errors are actionable.** The error carries the actual size, the
   threshold, and a concrete next step; `matchString` then returns a window
   around the first occurrence without loading the file.
6. **Minification uses `go/parser` and `go/ast`** for Go — an exact stdlib
   parser rather than a compiled Rust binary. Other languages use a
   string-literal-aware comment stripper in pure Go.
7. **The cache has a disk tier**, so repository trees and file contents survive
   the restart that happens on every client session.
8. **Errors are structured**, with a machine-readable code, a retry flag, a
   hint, and error-specific details.
9. **Databricks is a first-class tool**, read-only enforced in-process, dev by
   default.
10. **Prometheus metrics** make the server observable rather than a black box.

---

## Layout

Diagrams of the purpose, the data flow and the request lifecycle are in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

```
cmd/huginn/          entry point: config, wiring, stdio serve
internal/protocol/   response envelope, structured errors, token budget
internal/config/     env + file configuration and validation
internal/security/   path guard, secret redaction, safe subprocess execution
internal/cache/      two-tier memory + disk cache
internal/content/    minification, line slicing, binary detection
internal/ghclient/   GitHub client, rate-limit transport, error mapping
internal/lsp/        JSON-RPC 2.0 language server client and pool
internal/databricks/ SQL Statement Execution API, read-only validation
internal/hints/      server-side hint generation
internal/metrics/    Prometheus collectors
internal/tools/      the eleven MCP tools and server wiring
```

---

## Development

```bash
make help          # list every target
make test          # unit tests, offline
make lint          # gofmt + go vet
make build         # ./bin/huginn
make smoke         # MCP handshake against the local binary
make integration   # live GitHub tests; needs GITHUB_TOKEN
make docker-build  # container image
make run-local     # containerised MCP session on stdio
```

Unit tests are hermetic — no network, no dependency on ripgrep or a language
server being installed (those tests skip). Integration tests that make real
GitHub calls sit behind the `integration` build tag.

Coverage focuses on the security boundaries (path traversal, symlink escape,
command injection, secret redaction), configuration precedence, cache hit/miss
and persistence, rate-limit wait calculation, minification, read-only SQL
enforcement, and hint generation.

## Licence

MIT. See [LICENSE](LICENSE).
