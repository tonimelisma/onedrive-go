package retry

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RetryTransport is an http.RoundTripper that wraps an inner transport with
// automatic retry on transient failures (network errors, 429, 5xx). It handles
// exponential backoff, Retry-After headers, account-wide 429 throttle
// coordination, and seekable body rewinding between attempts.
//
// CLI callers wrap http.DefaultTransport in a RetryTransport so the graph client
// never needs to know about retries. Sync callers use a raw transport — failed
// requests return immediately for engine-level classification and tracker
// re-queuing (R-6.8.7: workers never block on retry backoff).
type RetryTransport struct {
	// Inner is the underlying transport (typically http.DefaultTransport).
	Inner http.RoundTripper

	// Policy controls the retry loop: MaxAttempts, backoff base/max/multiplier.
	Policy Policy

	// Logger for retry warnings. Must not be nil.
	Logger *slog.Logger

	// Sleep is a context-aware sleep function. Defaults to TimeSleep if nil.
	// Tests override this to avoid real delays.
	Sleep SleepFunc

	// throttleMu guards throttledUntil. Account-wide: when any request gets
	// 429, all subsequent requests through this transport wait until the
	// deadline passes.
	throttleMu     sync.Mutex
	throttledUntil time.Time
}

// RoundTrip executes the HTTP request with automatic retry on transient
// failures. Implements http.RoundTripper. Thread-safe.
func (rt *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sleepFn := rt.Sleep
	if sleepFn == nil {
		sleepFn = TimeSleep
	}

	// Account-wide throttle gate: wait if a previous 429 set a deadline.
	if err := rt.waitForThrottle(req.Context(), sleepFn); err != nil {
		return nil, err
	}

	var lastErr error

	for attempt := 0; ; attempt++ {
		// Rewind seekable bodies so retries send the full payload.
		// Skipped on the first attempt (nothing to rewind).
		if attempt > 0 {
			if err := rewindBody(req); err != nil {
				return nil, err
			}

			// Annotate retried requests so servers can distinguish them.
			req.Header.Set("X-Retry-Count", strconv.Itoa(attempt))
		}

		resp, err := rt.Inner.RoundTrip(req)
		// --- Network error ---
		if err != nil {
			lastErr = err

			if req.Context().Err() != nil {
				return nil, fmt.Errorf("retry: request canceled: %w", req.Context().Err())
			}

			if attempt < rt.Policy.MaxAttempts {
				backoff := rt.Policy.Delay(attempt)
				rt.Logger.Warn("retrying after network error",
					slog.String("method", req.Method),
					slog.String("url", req.URL.String()),
					slog.Int("attempt", attempt+1),
					slog.Duration("backoff", backoff),
					slog.String("error", err.Error()),
				)

				if sleepErr := sleepFn(req.Context(), backoff); sleepErr != nil {
					return nil, fmt.Errorf("retry: request canceled during backoff: %w", sleepErr)
				}

				continue
			}

			return nil, lastErr
		}

		// --- 2xx success ---
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return resp, nil
		}

		// --- Non-retryable status (4xx except 408/429, 507) → return as-is ---
		if !isRetryable(resp.StatusCode) || attempt >= rt.Policy.MaxAttempts {
			return resp, nil
		}

		// --- Retryable status → extract backoff, discard body, retry ---
		backoff := rt.retryBackoff(resp, attempt, sleepFn)

		// Drain response body to allow connection reuse.
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // best-effort drain
		resp.Body.Close()

		rt.Logger.Warn("retrying after HTTP error",
			slog.String("method", req.Method),
			slog.String("url", req.URL.String()),
			slog.Int("status", resp.StatusCode),
			slog.Int("attempt", attempt+1),
			slog.Duration("backoff", backoff),
		)

		if sleepErr := sleepFn(req.Context(), backoff); sleepErr != nil {
			return nil, fmt.Errorf("retry: request canceled during backoff: %w", sleepErr)
		}
	}
}

// waitForThrottle blocks until the account-wide throttle deadline passes.
// Called at the start of every request to enforce 429 Retry-After across
// all concurrent requests through this transport.
func (rt *RetryTransport) waitForThrottle(ctx context.Context, sleepFn SleepFunc) error {
	rt.throttleMu.Lock()
	deadline := rt.throttledUntil
	rt.throttleMu.Unlock()

	if delay := time.Until(deadline); delay > 0 {
		return sleepFn(ctx, delay)
	}

	return nil
}

// retryBackoff returns the backoff duration for a retryable response.
// For 429 (throttled), the Retry-After header takes precedence over computed
// backoff — ignoring it risks extended throttling by the Graph API.
func (rt *RetryTransport) retryBackoff(resp *http.Response, attempt int, _ SleepFunc) time.Duration {
	if ra := parseRetryAfterHeader(resp); ra > 0 {
		// 429: set account-wide throttle deadline so concurrent requests wait.
		if resp.StatusCode == http.StatusTooManyRequests {
			deadline := time.Now().Add(ra)
			rt.throttleMu.Lock()
			if deadline.After(rt.throttledUntil) {
				rt.throttledUntil = deadline
			}
			rt.throttleMu.Unlock()
		}

		return ra
	}

	return rt.Policy.Delay(attempt)
}

// parseRetryAfterHeader extracts the Retry-After header from 429 and 503
// responses and returns it as a time.Duration. Returns 0 when absent,
// unparseable, or the status code is not 429/503.
func parseRetryAfterHeader(resp *http.Response) time.Duration {
	if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
		return 0
	}

	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 0
	}

	seconds, err := strconv.Atoi(ra)
	if err != nil || seconds <= 0 {
		return 0
	}

	return time.Duration(seconds) * time.Second
}

// rewindBody resets the request body for retry. Prefers GetBody (set by
// http.NewRequest for bytes.Reader/Buffer/strings.Reader) which creates a
// fresh reader. Falls back to seeking the body directly if it implements
// io.Seeker (e.g., when the caller wraps an io.SectionReader and sets
// GetBody manually). Non-seekable bodies (nil, http.NoBody) are skipped.
func rewindBody(req *http.Request) error {
	if req.Body == nil || req.Body == http.NoBody {
		return nil
	}

	// Prefer GetBody — creates a fresh ReadCloser, works for all types
	// that http.NewRequest recognizes (bytes.Reader, bytes.Buffer,
	// strings.Reader) and any caller-provided GetBody.
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err != nil {
			return fmt.Errorf("retry: getting fresh request body: %w", err)
		}

		req.Body = body

		return nil
	}

	// Fall back to seeking the existing body (for custom ReadClosers that
	// implement io.Seeker but don't set GetBody).
	if seeker, ok := req.Body.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("retry: rewinding request body: %w", err)
		}
	}

	return nil
}

// isRetryable reports whether the given HTTP status code should be retried.
// Mirrors the Graph API retry set from architecture.md §7.4.
func isRetryable(code int) bool {
	switch code {
	case http.StatusRequestTimeout, // 408
		http.StatusTooManyRequests,     // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	default:
		// 509 Bandwidth Limit Exceeded (SharePoint).
		const statusBandwidthExceeded = 509
		return code == statusBandwidthExceeded
	}
}

// SetThrottleDeadline sets the account-wide throttle deadline directly.
// Used by tests to simulate a pre-existing throttle state.
func (rt *RetryTransport) SetThrottleDeadline(deadline time.Time) {
	rt.throttleMu.Lock()
	rt.throttledUntil = deadline
	rt.throttleMu.Unlock()
}

// ThrottleDeadline returns the current throttle deadline. Used by tests to
// verify 429 handling sets the deadline correctly.
func (rt *RetryTransport) ThrottleDeadline() time.Time {
	rt.throttleMu.Lock()
	defer rt.throttleMu.Unlock()

	return rt.throttledUntil
}
