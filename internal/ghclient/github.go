package ghclient

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v66/github"
	"golang.org/x/oauth2"

	"github.com/gstern-CTO/huginn/internal/cache"
	"github.com/gstern-CTO/huginn/internal/config"
	"github.com/gstern-CTO/huginn/internal/metrics"
	"github.com/gstern-CTO/huginn/internal/protocol"
)

// maxRateLimitWait bounds how long a single request will sleep waiting for a
// rate limit window. Beyond this the request fails with a retryable error and
// lets the agent decide, rather than blocking a tool call for an hour.
const maxRateLimitWait = 15 * time.Minute

// rateLimitTransport implements GitHub-correct rate limit handling.
//
// OctoCode applies fixed exponential backoff to a 429, which either sleeps
// longer than necessary or wakes before the window resets and immediately earns
// a second 429. GitHub states the exact reset instant in X-RateLimit-Reset (a
// Unix timestamp), so the correct wait is "until that moment"
// (WEAKNESSES.md #2).
type rateLimitTransport struct {
	base       http.RoundTripper
	metrics    *metrics.Metrics
	logger     *slog.Logger
	maxRetries int

	// now and sleep are injectable for tests.
	now   func() time.Time
	sleep func(context.Context, time.Duration) error
}

func newRateLimitTransport(base http.RoundTripper, metrics *metrics.Metrics, logger *slog.Logger, maxRetries int) *rateLimitTransport {
	if base == nil {
		base = http.DefaultTransport
	}
	return &rateLimitTransport{
		base:       base,
		metrics:    metrics,
		logger:     logger,
		maxRetries: maxRetries,
		now:        time.Now,
		sleep:      sleepContext,
	}
}

func (t *rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	attempts := t.maxRetries
	if attempts < 1 {
		attempts = 1
	}

	var lastResp *http.Response
	for attempt := 0; attempt < attempts; attempt++ {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			t.metrics.RecordGitHubRequest("transport_error")
			return nil, err
		}
		t.observeRateLimit(resp)

		wait, limited := t.rateLimitWait(resp)
		if !limited {
			t.metrics.RecordGitHubRequest(outcomeForStatus(resp.StatusCode))
			return resp, nil
		}

		t.metrics.RecordGitHubRequest("rate_limited")
		if attempt == attempts-1 {
			return resp, nil // out of retries: hand the 429 back for structured mapping
		}
		if wait > maxRateLimitWait {
			t.logger.Warn("github rate limit reset is too far out to wait", "wait", wait.String())
			return resp, nil
		}

		// Drain and close before sleeping so the connection can be reused.
		drainAndClose(resp)
		lastResp = nil

		t.metrics.RecordRateLimitWait()
		t.logger.Info("sleeping until github rate limit reset", "wait", wait.Round(time.Second).String())
		if err := t.sleep(req.Context(), wait); err != nil {
			return nil, err
		}
	}
	if lastResp != nil {
		return lastResp, nil
	}
	return nil, errors.New("rate limit retry loop exhausted")
}

// rateLimitWait computes how long to sleep, preferring the exact reset
// timestamp over any heuristic. It returns limited=false when the response is
// not a rate limit rejection.
func (t *rateLimitTransport) rateLimitWait(resp *http.Response) (time.Duration, bool) {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
		return 0, false
	}

	remaining, hasRemaining := headerInt(resp, "X-RateLimit-Remaining")
	// A 403 that is not a rate limit (e.g. genuine permission denial) has
	// remaining requests left; treat it as a normal response.
	if resp.StatusCode == http.StatusForbidden && hasRemaining && remaining > 0 {
		return 0, false
	}
	if resp.StatusCode == http.StatusForbidden && !hasRemaining && resp.Header.Get("Retry-After") == "" {
		return 0, false
	}

	// Primary rate limit: sleep until exactly the reset instant.
	if reset, ok := headerInt(resp, "X-RateLimit-Reset"); ok {
		resetAt := time.Unix(int64(reset), 0)
		if wait := resetAt.Sub(t.now()); wait > 0 {
			// One extra second of margin absorbs clock skew between this
			// host and GitHub; without it a wake-up right on the boundary
			// can earn a second 429.
			return wait + time.Second, true
		}
		return 0, true // window already reset: retry immediately
	}

	// Secondary rate limit / abuse detection uses Retry-After instead.
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil {
			return time.Duration(secs) * time.Second, true
		}
		if when, err := http.ParseTime(ra); err == nil {
			if wait := when.Sub(t.now()); wait > 0 {
				return wait, true
			}
			return 0, true
		}
	}
	return 0, false
}

func (t *rateLimitTransport) observeRateLimit(resp *http.Response) {
	if remaining, ok := headerInt(resp, "X-RateLimit-Remaining"); ok {
		t.metrics.SetRateLimitRemaining(remaining)
	}
}

func headerInt(resp *http.Response, key string) (int, bool) {
	raw := resp.Header.Get(key)
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, false
	}
	return n, true
}

func outcomeForStatus(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "success"
	case code == http.StatusNotFound:
		return "not_found"
	case code == http.StatusUnauthorized:
		return "unauthorized"
	case code >= 500:
		return "server_error"
	default:
		return "other"
	}
}

func drainAndClose(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	_ = resp.Body.Close()
}

// sleepContext sleeps for d unless the context is cancelled first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Client wraps go-github with this server's caching and error mapping.
type Client struct {
	API     *github.Client
	cache   *cache.Cache
	metrics *metrics.Metrics
	cfg     *config.Config
}

// New builds an authenticated client. A GitHub Enterprise base URL
// is honoured when configured.
func New(cfg *config.Config, cache *cache.Cache, metrics *metrics.Metrics, logger *slog.Logger) (*Client, error) {
	if !cfg.HasGitHub() {
		return nil, errors.New("no GitHub token configured")
	}

	oauthClient := oauth2.NewClient(
		context.Background(),
		oauth2.StaticTokenSource(&oauth2.Token{AccessToken: cfg.GitHubToken}),
	)
	// Wrap the oauth transport so the rate limit logic sees the real response
	// headers and the retry re-sends an already-authenticated request.
	httpClient := &http.Client{
		Transport: newRateLimitTransport(oauthClient.Transport, metrics, logger, cfg.MaxRetries),
		Timeout:   cfg.RequestTimeout,
	}

	api := github.NewClient(httpClient)
	if cfg.GitHubAPIURL != "" && cfg.GitHubAPIURL != config.DefaultGitHubAPIURL {
		parsed, err := url.Parse(cfg.GitHubAPIURL)
		if err != nil {
			return nil, fmt.Errorf("invalid GitHub API URL %q: %w", cfg.GitHubAPIURL, err)
		}
		api.BaseURL = parsed
	}

	return &Client{API: api, cache: cache, metrics: metrics, cfg: cfg}, nil
}

// mapGitHubError converts a transport or API error into a structured, actionable
// ToolError. The retry flag is the point: an agent must be able to tell a
// "wait and try again" failure from a "this will never work" failure without
// parsing prose (WEAKNESSES.md #8).
func MapError(err error, what string) *protocol.ToolError {
	if err == nil {
		return nil
	}

	var rateErr *github.RateLimitError
	if errors.As(err, &rateErr) {
		wait := time.Until(rateErr.Rate.Reset.Time)
		return protocol.NewError(protocol.CodeRateLimited, true,
			fmt.Sprintf("The GitHub rate limit resets in %s. Wait for it, or reduce the number of queries per call.", wait.Round(time.Second)),
			"GitHub rate limit exceeded while %s", what).
			WithDetail("resetAt", rateErr.Rate.Reset.Time.UTC().Format(time.RFC3339)).
			WithDetail("limit", rateErr.Rate.Limit)
	}

	var abuseErr *github.AbuseRateLimitError
	if errors.As(err, &abuseErr) {
		hint := "GitHub applied a secondary rate limit. Slow down and retry."
		if abuseErr.RetryAfter != nil {
			hint = fmt.Sprintf("GitHub applied a secondary rate limit; retry after %s.", abuseErr.RetryAfter.Round(time.Second))
		}
		return protocol.NewError(protocol.CodeRateLimited, true, hint, "GitHub secondary rate limit while %s", what)
	}

	var errResp *github.ErrorResponse
	if errors.As(err, &errResp) {
		switch errResp.Response.StatusCode {
		case http.StatusUnauthorized:
			return protocol.NewError(protocol.CodeAuth, false,
				"Check that GITHUB_TOKEN is set and has repo + read:org scopes.",
				"GitHub rejected the credentials while %s", what)
		case http.StatusForbidden:
			return protocol.NewError(protocol.CodeAuth, false,
				"Check that GITHUB_TOKEN is set and has repo + read:org scopes; for an organisation repository the token may also need SSO authorisation.",
				"GitHub denied access while %s: %s", what, errResp.Message)
		case http.StatusNotFound:
			return protocol.NewError(protocol.CodeNotFound, false,
				"Verify the owner, repository and path. Use repository search to confirm the repository exists and is visible to this token.",
				"not found while %s", what)
		case http.StatusUnprocessableEntity:
			return protocol.NewError(protocol.CodeInvalidInput, false,
				"GitHub rejected the query syntax. Simplify the query: remove qualifiers and retry with plain keywords.",
				"GitHub could not process the request while %s: %s", what, errResp.Message)
		}
		if errResp.Response.StatusCode >= 500 {
			return protocol.NewError(protocol.CodeUpstream, true,
				"GitHub is failing server-side. Retrying shortly is reasonable.",
				"GitHub server error (%d) while %s", errResp.Response.StatusCode, what)
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return protocol.NewError(protocol.CodeTimeout, true,
			"The request exceeded the configured timeout. Narrow the query or raise REQUEST_TIMEOUT_SECONDS.",
			"timed out while %s", what)
	}
	if errors.Is(err, context.Canceled) {
		return protocol.NewError(protocol.CodeTimeout, false, "The call was cancelled.", "cancelled while %s", what)
	}

	return protocol.NewError(protocol.CodeNetwork, true,
		"A network-level failure occurred; retrying is reasonable.",
		"network error while %s: %v", what, err)
}
