# OctoCode Weaknesses and GoResearchMCP Improvements

This document records what is wrong with the OctoCode TypeScript reference
implementation and what the Go version must do differently. Use it as a
validation checklist - each weakness should be provably fixed before calling
the implementation done.

---

## 1. Parallel execution was an afterthought (critical)

OctoCode shipped with sequential MCP tool calls because the Google ADK it
used executed tools one at a time by default. The team had to fork the ADK
and rewrite the core execution loop post-launch to get concurrency.

In Go, concurrency is the natural default. There is no framework to fight.
Bulk queries should be parallel from the first line of code, not retrofitted.
If the Go implementation ever serializes parallel queries, that is a regression
from the original, not an improvement.

---

## 2. Rate limiting sleeps the wrong amount

When GitHub returns a 429, OctoCode uses fixed exponential backoff. The
X-RateLimit-Reset header in the response contains the exact Unix timestamp
when the rate window resets. Ignoring it means either sleeping too long
(wasted time) or waking up before the window resets (immediate second 429).

The Go implementation must read this header and sleep until exactly that
moment. This is one line of code that makes rate-limited sessions
significantly faster.

---

## 3. Secret redaction compiles patterns at call time

OctoCode loads a 32KB regex pattern file and processes it on each content
scan. In Go, compile all regex patterns once at program initialization into
a slice of *regexp.Regexp. A compiled regex runs in microseconds; compiling
it takes milliseconds. At the volume of content a research session processes,
this matters.

---

## 4. LSP failure is a dead end

When a language server is not installed, OctoCode returns a generic error.
The agent has nowhere to go. The Go implementation must fall back to
ripgrep-based regex symbol search when no LSP server is found, and must
include in the error response the exact command to install the missing server.
A dead end should never be the response.

---

## 5. Large file errors don't tell the agent what to do

When a file exceeds the size threshold, OctoCode returns an error. It does
not tell the agent the file's actual size, the threshold that was exceeded,
or what to do instead. The agent is stuck.

The Go implementation must return: the actual file size, the threshold, and
a concrete suggestion (use line range, or search first to find the relevant
section). Every error must be actionable.

---

## 6. Minification requires a compiled Rust binary

OctoCode's token-efficient content shaping runs through a Rust engine
compiled to platform-specific native binaries. Adding support for a new
language or modifying minification behavior requires Rust knowledge and
publishing new binaries for each target platform.

For Go files specifically, the Go standard library includes go/parser and
go/ast - a complete, accurate AST parser that requires zero external
dependencies. Symbol extraction for Go codebases (your primary use case)
should use this rather than a general-purpose Rust engine. Other languages
can use simple comment-stripping heuristics in pure Go. The result is more
accurate for Go and far simpler to maintain.

---

## 7. No disk cache - every session starts cold

OctoCode's cache is in-memory only. When the MCP server restarts (which
happens every Claude Code session), all cached responses are gone. The same
repository tree and the same frequently-read files get fetched again from
GitHub on every session.

A disk cache layer that persists file content and repo trees across restarts
is straightforward to implement in Go and meaningfully reduces API calls and
latency in sessions that revisit the same codebase.

---

## 8. Error types are strings

OctoCode returns errors as strings. An agent that receives a string error
cannot distinguish between "try again" errors (rate limit, network timeout)
and "don't try again" errors (not found, auth failure). It cannot route to
a recovery strategy without parsing the string.

The Go implementation must use a structured error type with a machine-readable
code, a human-readable message, a retry flag, and a hint. The agent deserves
errors it can act on.

---

## 9. No data query capability (this is an addition, not a fix)

OctoCode has no equivalent to the Databricks tool. For DNS telemetry
engineering, the most valuable investigative question is often not "where is
this in the code" but "what does the data show over the past 7 days." The
Databricks tool fills a gap that OctoCode never addressed.

The read-only enforcement on SQL is not optional. Production data access must
require an explicit parameter - the default must be the dev environment.

---

## 10. No way to observe the server

OctoCode writes a stats file but exposes no metrics endpoint. You cannot
wire it into Prometheus or Grafana. For a tool that sits in the critical path
of your engineering workflow, this is a gap.

Prometheus metrics on a sidecar port cost almost nothing to implement in Go
and mean the MCP server is a first-class observable service, not a black box.