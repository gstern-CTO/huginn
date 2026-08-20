package protocol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthErrorCarriesScopeHint(t *testing.T) {
	// The brief names this hint verbatim for any auth failure.
	tErr := NewError(CodeAuth, false,
		"Check that GITHUB_TOKEN is set and has repo + read:org scopes.",
		"unauthorised")
	env := Failure(tErr)

	require.Equal(t, StatusError, env.Status)
	require.Contains(t, strings.Join(env.Hints, " "), "repo + read:org")
}

// No response should overwhelm the agent with suggestions.
func TestHintsAreCappedAtThree(t *testing.T) {
	env := &Envelope{}
	env.WithHints("one", "two", "three", "four", "five")
	require.Len(t, env.Hints, MaxHints)
}

func TestHintsAreDeduplicated(t *testing.T) {
	env := &Envelope{}
	env.WithHints("same", "same", "different")
	require.Equal(t, []string{"same", "different"}, env.Hints)
}

func TestEmptyHintsAreIgnored(t *testing.T) {
	env := &Envelope{}
	env.WithHints("", "real", "")
	require.Equal(t, []string{"real"}, env.Hints)
}

// The token budget is what stops a response flooding the context window
// regardless of how much the upstream API returned.
func TestTokenBudgetStopsAddingWhenFull(t *testing.T) {
	budget := NewTokenBudget(10) // ~40 characters

	require.True(t, budget.TryAdd(strings.Repeat("a", 20)), "5 tokens fits")
	require.True(t, budget.TryAdd(strings.Repeat("b", 16)), "4 more tokens fits")
	require.False(t, budget.TryAdd(strings.Repeat("c", 40)), "10 more does not fit")
	require.Equal(t, 1, budget.Remaining())
}

// A single oversized item is admitted rather than returning nothing at all;
// the caller then truncates it to the limit.
func TestTokenBudgetAlwaysAdmitsTheFirstItem(t *testing.T) {
	budget := NewTokenBudget(5)

	require.True(t, budget.TryAdd(strings.Repeat("x", 1000)), "the first item is never refused")
	require.True(t, budget.Exhausted())
	require.Zero(t, budget.Remaining())
	require.False(t, budget.TryAdd("more"))
}

func TestTokenBudgetFallsBackToDefault(t *testing.T) {
	require.Equal(t, DefaultMaxTokens, NewTokenBudget(0).Limit())
	require.Equal(t, DefaultMaxTokens, NewTokenBudget(-5).Limit())
	require.Equal(t, 1234, NewTokenBudget(1234).Limit())
}

func TestEstimateTokens(t *testing.T) {
	require.Zero(t, EstimateTokens(""))
	require.Equal(t, 1, EstimateTokens("abc"))
	require.Equal(t, 1, EstimateTokens("abcd"))
	require.Equal(t, 2, EstimateTokens("abcde"))
	require.Equal(t, 25, EstimateTokens(strings.Repeat("x", 100)))
}

func TestStatusForMapsCounts(t *testing.T) {
	require.Equal(t, StatusEmpty, StatusFor(0))
	require.Equal(t, StatusHasResults, StatusFor(1))
}

func TestSuccessAndFailureShapes(t *testing.T) {
	empty := Success(map[string]any{}, 0)
	require.Equal(t, StatusEmpty, empty.Status)
	require.Zero(t, empty.Metadata.ResultCount)

	full := Success(map[string]any{"a": 1}, 3)
	require.Equal(t, StatusHasResults, full.Status)
	require.Equal(t, 3, full.Metadata.ResultCount)

	// A failure seeds its hints from the error, so no response is ever a
	// dead end.
	failure := Failure(NewError(CodeNotFound, false, "look elsewhere", "missing"))
	require.Equal(t, StatusError, failure.Status)
	require.Equal(t, []string{"look elsewhere"}, failure.Hints)
	require.False(t, failure.Error.Retryable)
}

func TestFinalizeComputesTokenEstimate(t *testing.T) {
	env := Success(map[string]any{"content": strings.Repeat("y", 400)}, 1)
	require.Zero(t, env.Metadata.EstimatedTokens)

	env.Finalize()
	require.Positive(t, env.Metadata.EstimatedTokens)
}

func TestToolErrorDetailsAndMessage(t *testing.T) {
	err := NewError(CodeFileTooLarge, false, "use a range", "%s is too big", "big.log").
		WithDetail("sizeBytes", 4096).
		WithDetail("thresholdBytes", 1024)

	require.Equal(t, "big.log is too big", err.Message)
	require.Equal(t, 4096, err.Details["sizeBytes"])
	require.Contains(t, err.Error(), "FILE_TOO_LARGE")

	var nilErr *ToolError
	require.Equal(t, "<nil>", nilErr.Error())
}
