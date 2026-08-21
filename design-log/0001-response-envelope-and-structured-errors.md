# Design Log #1 — Response envelope and structured errors

**Date:** 2026-08-20
**Status:** Implemented · **retroactive** (written after the code, see README)
**Affects:** `internal/protocol`, every tool in `internal/tools`

## Background

Huginn exposes eleven tools to an AI client. The reference implementation it
replaces, OctoCode, returns errors as strings — recorded as weakness #8 in
`docs/WEAKNESSES.md`.

An agent that receives `"error: not found"` cannot tell a *wait and try again*
failure from a *this will never work* failure. It cannot route to a recovery
strategy without parsing prose, so it either retries what can never succeed or
gives up on what would have worked a second later.

## Problem

Two things had to be true at once:

1. Eleven tools returning eleven different shapes means the agent learns eleven
   contracts. It will get some of them wrong.
2. An error has to carry enough for the agent to act — not just what went wrong,
   but whether retrying helps and what to do instead.

## Questions and Answers

**Q1. One envelope for all tools, or a shape per tool?**
A: One. The agent branches on `status` before it looks at anything else, and
that branch is identical everywhere. Per-tool shapes would buy slightly tighter
typing for each tool at the cost of the agent having to learn all eleven.

**Q2. Is an error a protocol-level failure or a value in the envelope?**
A: Both. The envelope carries the structured error so the agent gets the code,
retry flag and hint; `CallToolResult.IsError` is also set so clients that only
inspect the protocol flag still notice. Choosing one would fail one audience.

**Q3. What is the minimum an error must carry?**
A: A machine-readable `code`, a human `message`, a `retryable` flag, a `hint`
naming the next action, and `details` for error-specific facts. The details
field is what makes "file too large" recoverable — it carries the actual size
and the threshold.

**Q4. Empty result versus error — where is the line?**
A: An empty result means *we looked and there is nothing*. An error means *we
could not look*. These must never be conflated: reporting a failed lookup as
empty tells the agent a symbol has no references, which is a confidently wrong
answer rather than a missing one. (This was later violated and fixed — see
Design Log #2.)

**Q5. Who writes the hints?**
A: The server, from the shape of the result. Hints are the difference between a
research tool and an API wrapper: the agent is told what to investigate next
instead of having to reason it out. Capped at three — past that they get ignored.

**Q6. How is context flooding prevented?**
A: A token budget per response, charged as `len(content)/4`. When exhausted the
response stops, sets `hasMore`, and adds a hint to paginate. The first item is
always admitted — returning nothing because one item was oversized is worse
than overshooting once.

## Design

```go
type Envelope struct {
    Status   Status     `json:"status"`   // hasResults | empty | error
    Data     any        `json:"data,omitempty"`
    Error    *ToolError `json:"error,omitempty"`
    Hints    []string   `json:"hints,omitempty"`
    Metadata Metadata   `json:"metadata"`
}

type ToolError struct {
    Code      ErrorCode      `json:"code"`
    Message   string         `json:"message"`
    Retryable bool           `json:"retryable"`
    Hint      string         `json:"hint"`
    Details   map[string]any `json:"details,omitempty"`
}

type Metadata struct {
    ResultCount     int  `json:"resultCount"`
    HasMore         bool `json:"hasMore"`
    CacheHit        bool `json:"cacheHit"`
    EstimatedTokens int  `json:"estimatedTokens"`
    RedactionCount  int  `json:"redactionCount"`
}
```

`internal/protocol` imports nothing internal, so the envelope contract cannot
develop a cycle with the code that fills it.

Every tool goes through one wrapper, `Server.wrap`, which applies the timeout,
finalises the token estimate, records metrics and sets `IsError`. No individual
tool can forget those steps.

## Implementation Plan

1. `internal/protocol`: envelope, `ToolError`, error codes, token budget
2. `Server.wrap`: the single path all tool results take
3. Per-tool hint generators in `internal/hints`, pure functions over result shape
4. Tests for the budget, the hint generators, and the envelope contract

## Examples

✅ Actionable — the agent can recover:
```json
{ "code": "FILE_TOO_LARGE", "retryable": false,
  "hint": "Pass a line range, or matchString to get a window around the first occurrence.",
  "details": { "sizeBytes": 4194304, "thresholdBytes": 1048576 } }
```

❌ A dead end — nothing to act on:
```json
{ "error": "file too large" }
```

✅ Distinguishing the two failure kinds:
```go
protocol.NewError(protocol.CodeRateLimited, true,  "The limit resets in 42s.", ...)
protocol.NewError(protocol.CodeNotFound,    false, "Verify the owner and repo.", ...)
```

## Trade-offs

**Accepted.** The envelope adds bytes to every response, including trivial ones.
Roughly 30 tokens of metadata against a budget of 8,000.

**Accepted.** `Data` is `any`, so the payload is not statically typed. Typing it
per tool would mean eleven envelope types and defeat Q1.

**Rejected.** Errors as Go `error` values only. They carry no retry flag and no
hint, which is exactly the weakness being fixed.

**Rejected.** Letting each tool set `IsError` itself. One tool forgetting is a
silent failure; centralising it in `wrap` makes it unforgettable.

## Verification Criteria

1. Every tool returns the same five top-level fields, success or failure.
2. Every `ToolError` has a non-empty code, message and hint.
3. A successful response still carries hints — no response is a dead end.
4. The token budget truncates and reports `hasMore` rather than overrunning.

---

## Implementation Results

*Appended after the fact for this retroactive log; results are real.*

Implemented in `internal/protocol/envelope.go` with 15 error codes.
`Server.wrap` in `internal/tools/server.go` is the single exit path.

**Verification.**

| Criterion | Result |
| --- | --- |
| 1. Uniform shape | ✅ `TestEnvelopeContractIsAlwaysHonoured` |
| 2. Errors complete | ✅ same test asserts code, message, hint non-empty |
| 3. Success carries hints | ✅ asserted, capped at `MaxHints` = 3 |
| 4. Budget truncates | ✅ `TestTokenBudgetStopsAddingWhenFull`, `TestTokenBudgetAlwaysAdmitsTheFirstItem` |

Tests: `internal/protocol` 91.7% statement coverage, all passing.

**Deviations from the design.**

1. **`Data` is a `map[string]any` in practice**, not a typed struct per tool.
   The design implied typed payloads; concretely, building maps inline was
   simpler and the JSON is identical. The cost is no compile-time check on
   payload keys.

2. **`IsError` is set in `wrap`, not by tools** — as designed, but worth noting
   it was a deliberate late change after the first two tools each set it
   themselves and one of them forgot.

**Known gap.** Criterion 4 covers the budget but not the interaction between the
budget and per-tool pagination; those are tested separately rather than together.
