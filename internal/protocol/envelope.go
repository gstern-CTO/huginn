package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
)

// DefaultMaxTokens is the fallback response token budget. Callers normally pass
// the configured value; this is what an unconfigured budget falls back to.
const DefaultMaxTokens = 8000

// MaxHints caps how many suggestions ride along with a response. Past three,
// agents stop reading them.
const MaxHints = 3

// StatusFor maps a result count onto the coarse status an agent branches on.
func StatusFor(count int) Status {
	if count == 0 {
		return StatusEmpty
	}
	return StatusHasResults
}

// Status is the coarse outcome of a tool call. Agents branch on this before
// looking at anything else.
type Status string

const (
	StatusHasResults Status = "hasResults"
	StatusEmpty      Status = "empty"
	StatusError      Status = "error"
)

// ErrorCode is machine-readable so an agent can route to a recovery strategy
// without parsing prose (WEAKNESSES.md #8).
type ErrorCode string

const (
	CodeAuth           ErrorCode = "AUTH_FAILED"
	CodeNotFound       ErrorCode = "NOT_FOUND"
	CodeRateLimited    ErrorCode = "RATE_LIMITED"
	CodeTimeout        ErrorCode = "TIMEOUT"
	CodeNetwork        ErrorCode = "NETWORK_ERROR"
	CodeInvalidInput   ErrorCode = "INVALID_INPUT"
	CodePathDenied     ErrorCode = "PATH_DENIED"
	CodeFileTooLarge   ErrorCode = "FILE_TOO_LARGE"
	CodeBinaryFile     ErrorCode = "BINARY_FILE"
	CodeToolDisabled   ErrorCode = "TOOL_DISABLED"
	CodeNotConfigured  ErrorCode = "NOT_CONFIGURED"
	CodeDependencyMiss ErrorCode = "DEPENDENCY_MISSING"
	CodeForbiddenSQL   ErrorCode = "FORBIDDEN_SQL"
	CodeUpstream       ErrorCode = "UPSTREAM_ERROR"
	CodeInternal       ErrorCode = "INTERNAL_ERROR"
)

// ToolError is a structured, actionable error. Retryable tells the agent
// whether trying again could plausibly succeed; Hint tells it what to do
// instead. Details carries error-specific facts (e.g. actual vs. maximum file
// size) so the agent never has to guess (WEAKNESSES.md #5).
type ToolError struct {
	Code      ErrorCode      `json:"code"`
	Message   string         `json:"message"`
	Retryable bool           `json:"retryable"`
	Hint      string         `json:"hint"`
	Details   map[string]any `json:"details,omitempty"`
}

func (e *ToolError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// WithDetail attaches a piece of structured context to the error.
func (e *ToolError) WithDetail(key string, value any) *ToolError {
	if e.Details == nil {
		e.Details = map[string]any{}
	}
	e.Details[key] = value
	return e
}

func NewError(code ErrorCode, retryable bool, hint, format string, args ...any) *ToolError {
	return &ToolError{
		Code:      code,
		Message:   fmt.Sprintf(format, args...),
		Retryable: retryable,
		Hint:      hint,
	}
}

func ErrInvalidInput(format string, args ...any) *ToolError {
	return NewError(CodeInvalidInput, false,
		"Fix the argument and call the tool again; retrying unchanged will fail identically.",
		format, args...)
}

func ErrNotConfigured(what, how string) *ToolError {
	return NewError(CodeNotConfigured, false, how, "%s is not configured", what)
}

func ErrToolDisabled(tool string) *ToolError {
	return NewError(CodeToolDisabled, false,
		"Start the server with ENABLE_LOCAL=true and WORKSPACE_ROOT set to the directory you want to research.",
		"%s is disabled because local filesystem access is turned off", tool)
}

func ErrInternal(err error) *ToolError {
	return NewError(CodeInternal, false,
		"This is a server-side fault; report it rather than retrying.",
		"internal error: %v", err)
}

// Metadata travels with every response so the agent can reason about
// completeness and cost without inspecting the payload.
type Metadata struct {
	ResultCount     int  `json:"resultCount"`
	HasMore         bool `json:"hasMore"`
	CacheHit        bool `json:"cacheHit"`
	EstimatedTokens int  `json:"estimatedTokens"`
	RedactionCount  int  `json:"redactionCount"`
}

// Envelope is the single response shape every tool returns.
type Envelope struct {
	Status   Status     `json:"status"`
	Data     any        `json:"data,omitempty"`
	Error    *ToolError `json:"error,omitempty"`
	Hints    []string   `json:"hints,omitempty"`
	Metadata Metadata   `json:"metadata"`
}

func Success(data any, count int) *Envelope {
	status := StatusHasResults
	if count == 0 {
		status = StatusEmpty
	}
	return &Envelope{Status: status, Data: data, Metadata: Metadata{ResultCount: count}}
}

func Failure(err *ToolError) *Envelope {
	env := &Envelope{Status: StatusError, Error: err}
	if err != nil && err.Hint != "" {
		env.Hints = []string{err.Hint}
	}
	return env
}

// WithHints appends hints, capping at three: more than that and the agent
// starts ignoring them.
func (e *Envelope) WithHints(hints ...string) *Envelope {
	for _, h := range hints {
		if h == "" || len(e.Hints) >= MaxHints {
			continue
		}
		if !slices.Contains(e.Hints, h) {
			e.Hints = append(e.Hints, h)
		}
	}
	return e
}

// Finalize computes the token estimate over the marshalled payload. It is the
// last thing done to an envelope before it goes out.
func (e *Envelope) Finalize() *Envelope {
	if raw, err := json.Marshal(e.Data); err == nil {
		e.Metadata.EstimatedTokens = EstimateTokens(string(raw))
	}
	return e
}

// EstimateTokens uses the conventional four-characters-per-token
// approximation. It is deliberately cheap: it runs on every response.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// TokenBudget bounds how much content a single response may carry, regardless
// of how much the upstream API returned. Callers add items until the budget
// refuses one, then report hasMore.
type TokenBudget struct {
	limit int
	used  int
}

func NewTokenBudget(limit int) *TokenBudget {
	if limit <= 0 {
		limit = DefaultMaxTokens
	}
	return &TokenBudget{limit: limit}
}

// TryAdd charges the budget for s and reports whether it fit. The first item is
// always admitted: returning an empty result because a single item exceeded the
// budget would be worse than overshooting once.
func (b *TokenBudget) TryAdd(s string) bool {
	cost := EstimateTokens(s)
	if b.used > 0 && b.used+cost > b.limit {
		return false
	}
	b.used += cost
	return true
}

func (b *TokenBudget) Exhausted() bool { return b.used >= b.limit }

func (b *TokenBudget) Remaining() int {
	if b.used >= b.limit {
		return 0
	}
	return b.limit - b.used
}

// Limit is the budget's ceiling, used by callers that truncate a single large
// item to fit rather than dropping it entirely.
func (b *TokenBudget) Limit() int { return b.limit }
