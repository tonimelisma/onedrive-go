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

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// RetryTransport is an http.RoundTripper that wraps an inner transport with
// automatic retry on transient failures (network errors, 429, 5xx) for HTTP
// methods that are safe to replay at the transport boundary. It handles
// exponential backoff, Retry-After headers, optional shared 429 throttle
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

	// ThrottleGate optionally coordinates Retry-After deadlines across
	// multiple transports that belong to the same caller scope.
	ThrottleGate *ThrottleGate

	// throttledUntil stores the local transport-scoped throttle deadline when
	// no shared gate is injected.
	throttleMu     sync.Mutex
	throttledUntil time.Time
}

type logTargetKey struct{}

func WithLogTarget(ctx context.Context, target string) context.Context {
	if target == "" {
		return ctx
	}

	return context.WithValue(ctx, logTargetKey{}, target)
}

func WithRequestLogTarget(req *http.Request, target string) *http.Request {
	if target == "" {
		return req
	}

	return req.WithContext(context.WithValue(req.Context(), logTargetKey{}, target))
}

// RoundTrip executes the HTTP request with automatic retry on transient
// failures. Implements http.RoundTripper. Thread-safe.
func (rt *RetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sleepFn := rt.Sleep
	if sleepFn == nil {
		sleepFn = TimeSleep
	}

	// Shared throttle gate: wait if a previous 429 set a caller-owned deadline.
	if err := rt.waitForThrottle(req.Context(), sleepFn); err != nil {
		return nil, err
	}

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
		if err != nil {
			retryRequest, finalErr := rt.handleNetworkError(req, err, attempt, sleepFn)
			if retryRequest {
				continue
			}

			return nil, finalErr
		}

		done, finalResp, finalErr := rt.handleResponse(req, resp, attempt, sleepFn)
		if done {
			return finalResp, finalErr
		}
	}
}

func (rt *RetryTransport) handleNetworkError(req *http.Request, err error, attempt int, sleepFn SleepFunc) (bool, error) {
	if req.Context().Err() != nil {
		return false, fmt.Errorf("retry: request canceled: %w", req.Context().Err())
	}
	if !retryableRequestMethod(req.Method) {
		return false, err
	}

	logTarget := requestLogTarget(req)
	if attempt < rt.Policy.MaxAttempts {
		backoff := rt.Policy.Delay(attempt)
		if collector := perf.FromContext(req.Context()); collector != nil {
			collector.RecordHTTPRetry(backoff)
		}
		rt.Logger.Debug("retrying after network error",
			slog.String("method", req.Method),
			slog.String("url", logTarget),
			slog.Int("attempt", attempt+1),
			slog.Duration("backoff", backoff),
			slog.String("error", err.Error()),
		)

		if sleepErr := sleepFn(req.Context(), backoff); sleepErr != nil {
			return false, fmt.Errorf("retry: request canceled during backoff: %w", sleepErr)
		}

		return true, nil
	}

	rt.Logger.Warn("request failed after all retries",
		slog.String("method", req.Method),
		slog.String("url", logTarget),
		slog.Int("attempts", attempt+1),
		slog.String("error", err.Error()),
	)

	return false, err
}

func (rt *RetryTransport) handleResponse(
	req *http.Request,
	resp *http.Response,
	attempt int,
	sleepFn SleepFunc,
) (bool, *http.Response, error) {
	// --- 2xx success ---
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return true, resp, nil
	}

	// --- Non-retryable status (4xx except 408/429, 507) → return as-is ---
	if !isRetryable(resp.StatusCode) {
		return true, resp, nil
	}
	if !retryableRequestMethod(req.Method) {
		return true, resp, nil
	}

	// Retryable but exhausted — terminal failure (R-6.6.8).
	if attempt >= rt.Policy.MaxAttempts {
		rt.Logger.Warn("request failed after all retries",
			slog.String("method", req.Method),
			slog.String("url", requestLogTarget(req)),
			slog.Int("attempts", attempt+1),
			slog.Int("status", resp.StatusCode),
		)
		return true, resp, nil
	}

	// --- Retryable status → extract backoff, discard body, retry ---
	backoff := rt.retryBackoff(resp, attempt, sleepFn)
	if collector := perf.FromContext(req.Context()); collector != nil {
		collector.RecordHTTPRetry(backoff)
	}

	// Drain response body to allow connection reuse. A drain/close failure
	// is not terminal for the retry path, but it is still operationally
	// relevant because it can reduce connection reuse.
	logTarget := requestLogTarget(req)
	if drainErr := drainAndCloseBody(resp.Body); drainErr != nil {
		rt.Logger.Warn("retry response body cleanup degraded",
			slog.String("method", req.Method),
			slog.String("url", logTarget),
			slog.Int("status", resp.StatusCode),
			slog.String("error", drainErr.Error()),
		)
	}

	rt.Logger.Debug("retrying after HTTP error",
		slog.String("method", req.Method),
		slog.String("url", logTarget),
		slog.Int("status", resp.StatusCode),
		slog.Int("attempt", attempt+1),
		slog.Duration("backoff", backoff),
	)

	if sleepErr := sleepFn(req.Context(), backoff); sleepErr != nil {
		return true, nil, fmt.Errorf("retry: request canceled during backoff: %w", sleepErr)
	}

	return false, nil, nil
}

func retryableRequestMethod(method string) bool {
	switch method {
	case http.MethodGet,
		http.MethodHead,
		http.MethodOptions,
		http.MethodTrace,
		http.MethodPut,
		http.MethodDelete:
		return true
	default:
		return false
	}
}

func drainAndCloseBody(body io.ReadCloser) error {
	_, drainErr := io.Copy(io.Discard, body)
	closeErr := body.Close()

	if drainErr != nil {
		return fmt.Errorf("draining body: %w", drainErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing body: %w", closeErr)
	}

	return nil
}

// waitForThrottle blocks until the active throttle deadline passes. Called at
// the start of every request to enforce 429 Retry-After across this transport
// or an injected caller-owned gate.
func (rt *RetryTransport) waitForThrottle(ctx context.Context, sleepFn SleepFunc) error {
	if rt.ThrottleGate != nil {
		return rt.ThrottleGate.Wait(ctx, sleepFn)
	}

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
		// 429: set the throttle deadline so later matching requests wait.
		if resp.StatusCode == http.StatusTooManyRequests {
			deadline := time.Now().Add(ra)
			if rt.ThrottleGate != nil {
				rt.ThrottleGate.SetDeadline(deadline)
			} else {
				rt.throttleMu.Lock()
				if deadline.After(rt.throttledUntil) {
					rt.throttledUntil = deadline
				}
				rt.throttleMu.Unlock()
			}
		}

		return ra
	}

	return rt.Policy.Delay(attempt)
}

func requestLogTarget(req *http.Request) string {
	target, ok := req.Context().Value(logTargetKey{}).(string)
	if ok && target != "" {
		return target
	}

	return req.URL.String()
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

// SetThrottleDeadline sets the current throttle deadline directly. Used by
// tests to simulate a pre-existing throttle state.
func (rt *RetryTransport) SetThrottleDeadline(deadline time.Time) {
	if rt.ThrottleGate != nil {
		rt.ThrottleGate.SetDeadline(deadline)

		return
	}

	rt.throttleMu.Lock()
	rt.throttledUntil = deadline
	rt.throttleMu.Unlock()
}

// ThrottleDeadline returns the current throttle deadline. Used by tests to
// verify 429 handling sets the deadline correctly.
func (rt *RetryTransport) ThrottleDeadline() time.Time {
	if rt.ThrottleGate != nil {
		return rt.ThrottleGate.Deadline()
	}

	rt.throttleMu.Lock()
	defer rt.throttleMu.Unlock()

	return rt.throttledUntil
}
