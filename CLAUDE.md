# Huginn Project Rules

Refines the workspace rules in `../CLAUDE.md`, which still apply. This file adds
only what is specific to Huginn.

## Design Log

Project decisions live in [`./design-log/`](./design-log/) and are referenced as
**Design Log #N**. Cross-project decisions live in `../design-log/` and are
referenced as **Workspace Log #N** — see `../CLAUDE.md` for which level a
decision belongs to.

Read `./design-log/README.md` before starting a feature. Design Log #1 and #2
carry constraints that are easy to violate by accident.

## What this project is

A single Go binary that speaks the Model Context Protocol over stdio, giving an
AI client eleven code-research tools: 5 GitHub, 4 local filesystem, LSP
navigation, and read-only Databricks queries. Clean-room Go equivalent of
OctoCode. MIT licensed.

Read `docs/BRIEF.md` for what it must do and `docs/WEAKNESSES.md` for the ten
defects it exists to avoid. `docs/ARCHITECTURE.md` has the diagrams.

## Terminology

Use the codebase's own words in logs and answers:

- **Envelope** — the single response shape every tool returns: `status`, `data`,
  `error`, `hints`, `metadata`. Defined in `internal/protocol`.
- **ToolError** — structured error with `code`, `message`, `retryable`, `hint`,
  `details`. Never a bare string.
- **Hints** — 1–3 server-generated suggestions for what to investigate next.
- **Path guard** — the workspace boundary check in `internal/security`.
- **Redactor** — the secret scrubber; patterns compile once at init.
- **Token budget** — the per-response cap that stops context flooding.
- **Fallback** — LSP degrading to a ripgrep symbol search.

## Constraints that are easy to break

1. **stdout is the MCP transport.** Nothing may print to it but protocol frames.
   All logging goes to stderr. `scripts/mcp-smoke.sh` exists to catch a breach.
2. **Read-only everywhere.** No tool writes to a repository, a filesystem, or a
   database.
3. **Never build a shell string.** Subprocesses take an explicit argv against
   the allowlist in `internal/security/exec.go`.
4. **Every error is actionable.** A new `ToolError` needs a code, a retry flag,
   a hint, and the details the agent needs to recover. An empty result and a
   failure to look are different answers — never conflate them.
5. **Dependencies point one way.** `internal/protocol` imports nothing internal.

## Working here

- Go, statically typed — show type signatures in logs and answers.
- `make lint test` before any commit; `make integration` needs a `GITHUB_TOKEN`.
- Unit tests must stay hermetic: no network, and they skip rather than fail when
  `rg` or a language server is absent.
- Tests that touch security boundaries are not optional — path traversal,
  symlink escape, command injection, and secret redaction each have coverage
  and must keep it.
- Default to non-breaking changes; the tool schemas are a public contract with
  the AI client.
