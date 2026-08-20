# Huginn Architecture

Three views of the same system: what it is for, how data actually moves through
it, and where every decision branches.

---

## 1. Purpose

An engineer's real questions are rarely answerable from one place. "Where is
this implemented" lives in code, "when was it introduced and why" lives in
pull request history, and "what does the data show this week" lives in a
warehouse. Huginn puts all three behind one MCP server so an AI agent can
follow a question wherever it leads, and shapes every answer so that following
it stays cheap and safe.

```mermaid
flowchart TB
    Agent["AI coding agent<br/>Claude Code, Cursor"]

    subgraph Huginn["Huginn: one Go binary, MCP over stdio"]
        direction TB
        Tools["11 research tools<br/>5 GitHub, 4 local, LSP, Databricks"]
        Safe["Security layer<br/>path guard, secret redaction, safe subprocess"]
        Shape["Response shaping<br/>minify, cache, token budget"]
        Guide["Hint engine<br/>what to investigate next"]
        Tools --> Safe
        Safe --> Shape
        Shape --> Guide
    end

    GH[("GitHub<br/>code, repos, pull requests")]
    WS[("Local workspace<br/>read-only")]
    DBX[("Databricks<br/>read-only SQL")]
    Obs["Prometheus metrics<br/>sidecar port"]

    Agent -->|"tools/call over stdio"| Tools
    Tools --> GH
    Tools --> WS
    Tools --> DBX
    Guide -->|"one envelope:<br/>status, data, error, hints, metadata"| Agent
    Huginn -.-> Obs
```

The three things worth noticing:

- **Every answer leaves through the same pipeline.** Nothing reaches the agent
  without passing the redactor, the token budget and the hint engine.
- **One envelope for all eleven tools.** The agent learns one response shape,
  not eleven.
- **stdout is the transport.** Logs go to stderr; metrics go to their own port.

---

## 2. Data flow

A single `github_file_content` call, end to end. This is the path that exercises
caching, rate limiting, minification and redaction together.

```mermaid
sequenceDiagram
    autonumber
    participant Client as AI client
    participant Stdio as MCP stdio transport
    participant Wrap as Server.wrap
    participant Tool as Tool handler
    participant Cache as Two-tier cache
    participant RT as Rate-limit transport
    participant GitHub as GitHub API
    participant Red as Redactor

    Client->>Stdio: tools/call github_file_content
    Stdio->>Wrap: CallToolRequest
    activate Wrap
    Wrap->>Wrap: start timer, apply context timeout
    Wrap->>Tool: handler ctx, req
    activate Tool

    Tool->>Tool: requireGitHub, validate arguments
    Tool->>Cache: Get stable key

    alt memory or disk hit
        Cache-->>Tool: cached bytes
        Note over Cache,Tool: metadata.cacheHit = true<br/>disk tier survives restarts
    else miss
        Cache-->>Tool: miss
        Tool->>RT: fetch repository contents
        RT->>GitHub: HTTP request
        GitHub-->>RT: 429 with X-RateLimit-Reset
        Note over RT,GitHub: sleep until that exact instant<br/>plus one second of clock-skew margin
        RT->>GitHub: retry once
        GitHub-->>RT: 200 file content
        RT-->>Tool: response
        Tool->>Cache: Set in memory and on disk
    end

    Tool->>Tool: minify, go/ast for Go files
    Tool->>Red: Redact content
    Red-->>Tool: cleaned text and redaction count
    Tool->>Tool: apply token budget, set hasMore
    Tool->>Tool: generate hints from the result shape
    Tool-->>Wrap: Envelope
    deactivate Tool

    Wrap->>Wrap: Finalize, estimate tokens
    Wrap->>Wrap: record tool call and latency
    Wrap-->>Stdio: CallToolResult
    deactivate Wrap
    Stdio-->>Client: envelope JSON on stdout
```

Bulk calls differ in one respect: `github_search_code` fans its queries out
through `errgroup` capped at five concurrent requests, and a query that fails
returns its own error alongside the successful queries' results rather than
failing the whole call.

---

## 3. Request lifecycle

Where every request branches, and what the agent gets back at each dead end.
No branch terminates without a structured error carrying a hint.

```mermaid
flowchart TD
    Start(["tools/call arrives"]) --> Which{"Which tool<br/>family?"}

    Which -->|"local or LSP"| L1{"ENABLE_LOCAL<br/>set?"}
    L1 -->|no| ErrDisabled["TOOL_DISABLED<br/>hint names ENABLE_LOCAL"]
    L1 -->|yes| L2["Expand path,<br/>resolve symlinks"]
    L2 --> L3{"Still inside the workspace<br/>after resolution?"}
    L3 -->|no| ErrPath["PATH_DENIED<br/>reports the resolved path,<br/>never clamps it back"]
    L3 -->|yes| L4{"Blocked pattern?<br/>.env, *.pem, .ssh/"}
    L4 -->|yes| ErrPath
    L4 -->|no| L5{"Read a file, or<br/>navigate a symbol?"}

    L5 -->|"read"| L6{"Larger than the<br/>size threshold?"}
    L6 -->|"yes, no range given"| ErrBig["FILE_TOO_LARGE<br/>actual size, threshold,<br/>and what to do instead"]
    L6 -->|no| Shape

    L5 -->|"navigate"| P1{"Language server<br/>installed?"}
    P1 -->|yes| P2["Mask comments and strings,<br/>locate the identifier,<br/>query the pooled server"]
    P2 --> Shape
    P1 -->|no| P3{"ripgrep<br/>available?"}
    P3 -->|yes| P4["Symbol fallback,<br/>usedFallback true,<br/>install command included"]
    P4 --> Shape
    P3 -->|no| ErrDep["DEPENDENCY_MISSING<br/>never reported as an<br/>empty result"]

    Which -->|"GitHub"| G1{"Token<br/>configured?"}
    G1 -->|no| ErrCfg["NOT_CONFIGURED<br/>hint names the required scopes"]
    G1 -->|yes| G2["Fan out queries in parallel,<br/>capped at 5 concurrent"]
    G2 --> G3{"Cache hit?"}
    G3 -->|yes| Shape
    G3 -->|no| G4{"Rate limited?"}
    G4 -->|yes| G5["Sleep until X-RateLimit-Reset,<br/>bounded by max retries"]
    G5 --> G4
    G4 -->|no| G6["Store in memory<br/>and on disk"]
    G6 --> Shape

    Which -->|"Databricks"| D1{"Single read-only<br/>statement?"}
    D1 -->|no| ErrSQL["FORBIDDEN_SQL<br/>names the offending keyword,<br/>refused before any network call"]
    D1 -->|yes| D2{"env=prod passed<br/>explicitly?"}
    D2 -->|no| D3["Query the dev workspace"]
    D2 -->|yes| D4["Query the prod workspace"]
    D3 --> Shape
    D4 --> Shape

    Shape["Minify, redact secrets,<br/>apply the token budget"] --> Hints["Generate 1 to 3 hints<br/>from the result shape"]
    Hints --> Env(["Envelope returned to the agent"])

    ErrDisabled --> Env
    ErrPath --> Env
    ErrBig --> Env
    ErrDep --> Env
    ErrCfg --> Env
    ErrSQL --> Env
```

### Why the dead ends matter

Every error box above is a place where the reference implementation returned a
bare string and left the agent stuck. Each one here carries a machine-readable
code, a retry flag, a hint, and error-specific details — the size and threshold
for a file that was too large, the resolved path for one that was denied, the
keyword for SQL that was refused.

The `DEPENDENCY_MISSING` box is the sharpest example. When neither a language
server nor ripgrep can answer, the response must be an error. Reporting it as
an empty result would tell the agent the symbol has no references, which is a
confidently wrong answer rather than a missing one.

---

## Package layout

The graph below is the real one, extracted with `go list`, not a sketch.

```mermaid
flowchart TD
    Cmd["cmd/huginn<br/>entry point"]

    subgraph Composition["Composition"]
        Tools["internal/tools<br/>11 tools, server wiring"]
    end

    subgraph Capabilities["Capabilities"]
        GHC["internal/ghclient"]
        LSPP["internal/lsp"]
        DBXP["internal/databricks"]
        Content["internal/content"]
        Hints["internal/hints<br/>no internal imports"]
    end

    subgraph Foundation["Foundation"]
        Config["internal/config"]
        Cache["internal/cache"]
        Security["internal/security"]
        Metrics["internal/metrics"]
        Protocol["internal/protocol<br/>imports nothing internal"]
    end

    Cmd --> Tools
    Cmd --> Config

    Tools --> GHC
    Tools --> LSPP
    Tools --> DBXP
    Tools --> Content
    Tools --> Hints
    Tools --> Cache
    Tools --> Config
    Tools --> Security
    Tools --> Metrics
    Tools --> Protocol

    GHC --> Cache
    GHC --> Config
    GHC --> Metrics
    GHC --> Protocol

    DBXP --> Config
    DBXP --> Content
    DBXP --> Protocol

    LSPP --> Security
    LSPP --> Metrics

    Content --> Protocol
    Config --> Security
    Config --> Protocol
    Security --> Protocol
    Cache --> Metrics
    Metrics --> Protocol
```

Dependencies point one way. `protocol` imports nothing internal and everything
that produces a response depends on it, so the envelope contract cannot
develop a cycle with the code that fills it. `hints` imports nothing at all —
it is pure functions over result shapes, which is why it is the most heavily
tested package in the tree.
