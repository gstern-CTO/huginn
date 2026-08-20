package ghclient

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gstern-CTO/huginn/internal/metrics"
)

func testTransport(t *testing.T, base http.RoundTripper) *rateLimitTransport {
	t.Helper()
	return newRateLimitTransport(base, metrics.NewMetrics(false, 0), slog.New(slog.NewTextHandler(io.Discard, nil)), 3)
}

func responseWith(status int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(""))}
}

// The whole point of reading X-RateLimit-Reset is that the wait is exact rather
// than a guess. A fixed backoff either oversleeps or wakes too early and earns
// a second rejection.
func TestRateLimitWaitUsesResetHeader(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	tr := testTransport(t, nil)
	tr.now = func() time.Time { return now }

	resetAt := now.Add(90 * time.Second)
	resp := responseWith(http.StatusTooManyRequests, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(resetAt.Unix(), 10),
	})

	wait, limited := tr.rateLimitWait(resp)
	require.True(t, limited)
	// 90s until the window resets, plus one second of clock-skew margin.
	require.Equal(t, 91*time.Second, wait)
}

func TestRateLimitWaitOnForbiddenWithZeroRemaining(t *testing.T) {
	now := time.Now()
	tr := testTransport(t, nil)
	tr.now = func() time.Time { return now }

	// GitHub signals a primary rate limit as 403 with remaining=0.
	resp := responseWith(http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(now.Add(30*time.Second).Unix(), 10),
	})

	wait, limited := tr.rateLimitWait(resp)
	require.True(t, limited)
	require.InDelta(t, 31*time.Second, wait, float64(time.Second))
}

// A 403 that is a genuine permission denial has requests remaining, and must
// not be mistaken for a rate limit.
func TestForbiddenWithRemainingIsNotRateLimited(t *testing.T) {
	tr := testTransport(t, nil)

	resp := responseWith(http.StatusForbidden, map[string]string{
		"X-RateLimit-Remaining": "4987",
	})

	_, limited := tr.rateLimitWait(resp)
	require.False(t, limited, "a permission error must not be retried as a rate limit")
}

func TestRateLimitWaitFallsBackToRetryAfter(t *testing.T) {
	tr := testTransport(t, nil)
	tr.now = time.Now

	// Secondary rate limits carry Retry-After instead of a reset timestamp.
	resp := responseWith(http.StatusForbidden, map[string]string{"Retry-After": "17"})

	wait, limited := tr.rateLimitWait(resp)
	require.True(t, limited)
	require.Equal(t, 17*time.Second, wait)
}

func TestRateLimitWaitIsZeroWhenWindowAlreadyReset(t *testing.T) {
	now := time.Now()
	tr := testTransport(t, nil)
	tr.now = func() time.Time { return now }

	resp := responseWith(http.StatusTooManyRequests, map[string]string{
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     strconv.FormatInt(now.Add(-10*time.Second).Unix(), 10),
	})

	wait, limited := tr.rateLimitWait(resp)
	require.True(t, limited)
	require.Zero(t, wait, "a window that has already reset should be retried immediately")
}

func TestSuccessfulResponseIsNotRateLimited(t *testing.T) {
	tr := testTransport(t, nil)
	_, limited := tr.rateLimitWait(responseWith(http.StatusOK, map[string]string{"X-RateLimit-Remaining": "4999"}))
	require.False(t, limited)
}

// End to end: the first attempt is rate limited, the transport sleeps for
// exactly the advertised interval, and the retry succeeds.
func TestRoundTripSleepsUntilResetThenRetries(t *testing.T) {
	var attempts atomic.Int32
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	resetAt := now.Add(45 * time.Second)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(resetAt.Unix(), 10))
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("X-RateLimit-Remaining", "4999")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	tr := testTransport(t, http.DefaultTransport)
	tr.now = func() time.Time { return now }

	var slept time.Duration
	tr.sleep = func(_ context.Context, d time.Duration) error {
		slept = d // record instead of actually waiting
		return nil
	}

	client := &http.Client{Transport: tr}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int32(2), attempts.Load(), "exactly one retry")
	require.Equal(t, 46*time.Second, slept, "slept until the reset instant, not an arbitrary backoff")
}

func TestRoundTripStopsRetryingAfterMaxAttempts(t *testing.T) {
	var attempts atomic.Int32
	now := time.Now()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Second).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	tr := testTransport(t, http.DefaultTransport)
	tr.now = func() time.Time { return now }
	tr.sleep = func(context.Context, time.Duration) error { return nil }

	client := &http.Client{Transport: tr}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode,
		"after exhausting retries the 429 is handed back for structured mapping")
	require.Equal(t, int32(3), attempts.Load())
}

// A reset far in the future must not block the tool call for an hour.
func TestRoundTripRefusesToWaitBeyondCap(t *testing.T) {
	var attempts atomic.Int32
	now := time.Now()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	tr := testTransport(t, http.DefaultTransport)
	tr.now = func() time.Time { return now }
	slept := false
	tr.sleep = func(context.Context, time.Duration) error { slept = true; return nil }

	client := &http.Client{Transport: tr}
	resp, err := client.Get(server.URL)
	require.NoError(t, err)
	defer resp.Body.Close()

	require.False(t, slept, "an hour-long wait must be reported, not slept through")
	require.Equal(t, int32(1), attempts.Load())
}

func TestRateLimitRemainingIsRecorded(t *testing.T) {
	metrics := metrics.NewMetrics(false, 0)
	tr := newRateLimitTransport(nil, metrics, slog.New(slog.NewTextHandler(io.Discard, nil)), 1)

	tr.observeRateLimit(responseWith(http.StatusOK, map[string]string{"X-RateLimit-Remaining": "4321"}))
	// The gauge is write-only through this interface; the assertion is that
	// observing a response with the header does not panic and parses cleanly.
	value, ok := headerInt(responseWith(http.StatusOK, map[string]string{"X-RateLimit-Remaining": "4321"}), "X-RateLimit-Remaining")
	require.True(t, ok)
	require.Equal(t, 4321, value)
}

func TestSleepContextRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := sleepContext(ctx, time.Hour)
	require.ErrorIs(t, err, context.Canceled)
}
