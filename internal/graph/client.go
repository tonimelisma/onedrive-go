package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	gosync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// DefaultBaseURL is the production Microsoft Graph API v1.0 endpoint.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

const (
	defaultUserAgent = "onedrive-go/dev"

	// maxErrBodySize caps error response body reads to prevent OOM from
	// malicious or buggy servers returning enormous error responses (B-314).
	maxErrBodySize = 64 * 1024
)

// TokenSource provides OAuth2 bearer tokens.
// Defined at the consumer (graph/) per "accept interfaces, return structs" —
// do not move this interface to the auth provider package.
type TokenSource interface {
	Token() (string, error)
}

// Client is an HTTP client for the Microsoft Graph API.
// It handles request construction, authentication, retry with
// exponential backoff, and error classification.
type Client struct {
	baseURL     string
	httpClient  *http.Client
	token       TokenSource
	logger      *slog.Logger
	userAgent   string
	retryPolicy retry.Policy

	// sleepFunc is called to wait between retries. Defaults to timeSleep.
	// Tests override this to avoid real delays.
	sleepFunc func(ctx context.Context, d time.Duration) error

	// throttleMu guards throttledUntil.
	throttleMu gosync.Mutex
	// throttledUntil is the deadline until which all requests should wait.
	// Set when any request receives a 429 with Retry-After.
	throttledUntil time.Time
}

// NewClient creates a Graph API client.
// baseURL is typically "https://graph.microsoft.com/v1.0".
// userAgent is sent in every request; defaults to "onedrive-go/dev" if empty.
// retryPolicy controls the retry loop — use retry.Transport for standard
// behavior or retry.SyncTransport for single-attempt sync dispatch.
func NewClient(baseURL string, httpClient *http.Client, token TokenSource, logger *slog.Logger, userAgent string, retryPolicy retry.Policy) *Client {
	if token == nil {
		panic("graph.NewClient: token source must not be nil")
	}

	if logger == nil {
		logger = slog.Default()
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	return &Client{
		baseURL:     baseURL,
		httpClient:  httpClient,
		token:       token,
		logger:      logger,
		userAgent:   userAgent,
		retryPolicy: retryPolicy,
		sleepFunc:   timeSleep,
	}
}

// Do executes an authenticated HTTP request against the Graph API with automatic
// retry on transient errors (per architecture.md §7.2).
// The caller is responsible for closing the response body on success.
// On error, returns a *GraphError wrapping a sentinel (use errors.Is to classify).
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doRetry(ctx, method, path, body, nil)
}

// DoWithHeaders executes an authenticated HTTP request with additional headers.
// It behaves identically to Do but merges extraHeaders into every request attempt.
// Use this for API calls that require special headers (e.g., Prefer for delta queries).
func (c *Client) DoWithHeaders(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	return c.doRetry(ctx, method, path, body, extraHeaders)
}

// doRetry is the shared retry loop for Do and DoWithHeaders.
func (c *Client) doRetry(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	// Account-wide throttle gate: wait if a 429 Retry-After set a deadline.
	c.waitForThrottle(ctx)

	url := c.baseURL + path

	var attempt int
	for {
		// Rewind seekable bodies so retries send the full payload.
		if err := rewindBody(body); err != nil {
			return nil, err
		}

		resp, err := c.doOnce(ctx, method, url, body, extraHeaders)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("graph: request canceled: %w", ctx.Err())
			}

			if attempt < c.retryPolicy.MaxAttempts {
				backoff := c.calcBackoff(attempt)
				c.logger.Warn("retrying after network error",
					slog.String("method", method),
					slog.String("path", path),
					slog.Int("attempt", attempt+1),
					slog.Duration("backoff", backoff),
					slog.String("error", err.Error()),
				)

				if sleepErr := c.sleepFunc(ctx, backoff); sleepErr != nil {
					return nil, fmt.Errorf("graph: request canceled: %w", sleepErr)
				}

				attempt++

				continue
			}

			return nil, fmt.Errorf("graph: %s %s failed after %d retries: %w", method, path, c.retryPolicy.MaxAttempts, err)
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			c.logger.Debug("request succeeded",
				slog.String("method", method),
				slog.String("path", path),
				slog.Int("status", resp.StatusCode),
				slog.String("request_id", resp.Header.Get("request-id")),
			)

			return resp, nil
		}

		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		resp.Body.Close()

		if readErr != nil {
			errBody = []byte("(failed to read response body)")
		}

		reqID := resp.Header.Get("request-id")

		if isRetryable(resp.StatusCode) && attempt < c.retryPolicy.MaxAttempts {
			backoff := c.retryBackoff(resp, attempt)
			c.logger.Warn("retrying after HTTP error",
				slog.String("method", method),
				slog.String("path", path),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempt", attempt+1),
				slog.Duration("backoff", backoff),
			)

			if err := c.sleepFunc(ctx, backoff); err != nil {
				return nil, fmt.Errorf("graph: request canceled: %w", err)
			}

			attempt++

			continue
		}

		retryAfter := parseRetryAfter(resp)

		return nil, c.terminalError(method, path, resp.StatusCode, reqID, errBody, attempt, retryAfter)
	}
}

// waitForThrottle blocks until the account-wide throttle deadline passes.
// Called at the start of every request to enforce 429 Retry-After across all workers.
func (c *Client) waitForThrottle(ctx context.Context) {
	c.throttleMu.Lock()
	deadline := c.throttledUntil
	c.throttleMu.Unlock()

	if delay := time.Until(deadline); delay > 0 {
		_ = c.sleepFunc(ctx, delay) //nolint:errcheck // context cancellation handled by caller
	}
}

// doOnce executes a single HTTP request (no retry).
func (c *Client) doOnce(
	ctx context.Context, method, url string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	c.logger.Debug("preparing request",
		slog.String("method", method),
		slog.String("url", url),
	)

	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	tok, err := c.token.Token()
	if err != nil {
		return nil, fmt.Errorf("obtaining token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("User-Agent", c.userAgent)

	// Implicit default: when a body is present, Content-Type is set to
	// application/json. All Graph API mutation endpoints (PATCH, POST) expect
	// JSON. Upload endpoints override this via extraHeaders or use pre-auth
	// URLs that bypass doOnce entirely (B-159).
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Merge caller-supplied headers (e.g., Prefer for delta queries).
	for key, vals := range extraHeaders {
		for _, v := range vals {
			req.Header.Add(key, v)
		}
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		c.logger.Debug("HTTP request failed",
			slog.String("method", method),
			slog.String("url", url),
			slog.String("error", err.Error()),
		)

		return nil, err
	}

	c.logger.Debug("HTTP response received",
		slog.String("method", method),
		slog.String("url", url),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", resp.Header.Get("request-id")),
	)

	// 401 token refresh: if the server rejects the token, try once more with
	// a fresh token. This handles the common case where the token expires
	// between our Token() call and the server's validation. The oauth2
	// ReuseTokenSource underneath auto-refreshes when the cached token has
	// expired (10-second safety margin ensures detection after HTTP RTT).
	// This is auth lifecycle management, not transient retry — it applies
	// even with SyncTransport (MaxAttempts: 0).
	if resp.StatusCode == http.StatusUnauthorized {
		newTok, tokErr := c.token.Token()
		if tokErr == nil && newTok != tok {
			resp.Body.Close()

			c.logger.Info("retrying after 401 with refreshed token",
				slog.String("method", method),
				slog.String("url", url),
			)

			if err := rewindBody(body); err != nil {
				return nil, err
			}

			req2, reqErr := http.NewRequestWithContext(ctx, method, url, body)
			if reqErr != nil {
				return nil, fmt.Errorf("creating retry request: %w", reqErr)
			}

			req2.Header.Set("Authorization", "Bearer "+newTok)
			req2.Header.Set("User-Agent", c.userAgent)

			if body != nil {
				req2.Header.Set("Content-Type", "application/json")
			}

			for key, vals := range extraHeaders {
				for _, v := range vals {
					req2.Header.Add(key, v)
				}
			}

			return c.httpClient.Do(req2)
		}
	}

	return resp, nil
}

// terminalError builds a GraphError and logs the final failure.
// Extracted from doRetry to keep the retry loop under funlen limits.
func (c *Client) terminalError(
	method, path string, statusCode int, reqID string, body []byte, attempt int, retryAfter time.Duration,
) *GraphError {
	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  reqID,
		Message:    string(body),
		Err:        classifyStatus(statusCode),
		RetryAfter: retryAfter,
	}

	if attempt > 0 {
		c.logger.Error("request failed after retries",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", statusCode),
			slog.String("request_id", reqID),
			slog.Int("attempts", attempt+1),
		)
	} else {
		// Non-retried failures are logged at DEBUG — the HTTP layer lacks context
		// to judge severity. Callers decide: e.g. DELETE 404 is success, GET 404
		// is an error the caller will report.
		c.logger.Debug("request failed",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", statusCode),
			slog.String("request_id", reqID),
		)
	}

	return graphErr
}

// doPreAuthRetry executes HTTP requests against pre-authenticated URLs with
// retry on transient failures (network errors, 429, 5xx). The makeReq function
// is called on each attempt to create a fresh request, enabling body re-reads.
// No Authorization header is added — the URL itself is pre-authenticated.
//
// On success (2xx), returns the response for the caller to interpret.
// On non-retryable error or retry exhaustion, returns *GraphError (matching doRetry).
func (c *Client) doPreAuthRetry(
	ctx context.Context, desc string, makeReq func() (*http.Request, error),
) (*http.Response, error) {
	var attempt int

	for {
		req, err := makeReq()
		if err != nil {
			return nil, err
		}

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("graph: %s canceled: %w", desc, ctx.Err())
			}

			if attempt < c.retryPolicy.MaxAttempts {
				backoff := c.calcBackoff(attempt)
				c.logger.Warn("retrying pre-auth request after network error",
					slog.String("desc", desc),
					slog.Int("attempt", attempt+1),
					slog.Duration("backoff", backoff),
					slog.String("error", err.Error()),
				)

				if sleepErr := c.sleepFunc(ctx, backoff); sleepErr != nil {
					return nil, fmt.Errorf("graph: %s canceled: %w", desc, sleepErr)
				}

				attempt++

				continue
			}

			return nil, fmt.Errorf("graph: %s failed after %d retries: %w", desc, c.retryPolicy.MaxAttempts, err)
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return resp, nil
		}

		errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
		resp.Body.Close()

		if readErr != nil {
			errBody = []byte("(failed to read response body)")
		}

		reqID := resp.Header.Get("request-id")

		if isRetryable(resp.StatusCode) && attempt < c.retryPolicy.MaxAttempts {
			backoff := c.retryBackoff(resp, attempt)
			c.logger.Warn("retrying pre-auth request after HTTP error",
				slog.String("desc", desc),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempt", attempt+1),
				slog.Duration("backoff", backoff),
			)

			if sleepErr := c.sleepFunc(ctx, backoff); sleepErr != nil {
				return nil, fmt.Errorf("graph: %s canceled: %w", desc, sleepErr)
			}

			attempt++

			continue
		}

		retryAfter := parseRetryAfter(resp)

		return nil, c.preAuthTerminalError(desc, resp.StatusCode, reqID, errBody, attempt, retryAfter)
	}
}

// preAuthTerminalError builds a GraphError and logs the final failure for pre-auth URLs.
// Mirrors terminalError but uses desc instead of method+path.
func (c *Client) preAuthTerminalError(
	desc string, statusCode int, reqID string, body []byte, attempt int, retryAfter time.Duration,
) *GraphError {
	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  reqID,
		Message:    string(body),
		Err:        classifyStatus(statusCode),
		RetryAfter: retryAfter,
	}

	if attempt > 0 {
		c.logger.Error("pre-auth request failed after retries",
			slog.String("desc", desc),
			slog.Int("status", statusCode),
			slog.String("request_id", reqID),
			slog.Int("attempts", attempt+1),
		)
	} else {
		c.logger.Debug("pre-auth request failed",
			slog.String("desc", desc),
			slog.Int("status", statusCode),
			slog.String("request_id", reqID),
		)
	}

	return graphErr
}

// retryBackoff returns the backoff duration for a retryable response.
// For 429 (throttled), the Graph API's Retry-After header takes precedence
// over calculated backoff — ignoring it risks extended throttling.
func (c *Client) retryBackoff(resp *http.Response, attempt int) time.Duration {
	if resp.StatusCode == http.StatusTooManyRequests {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if seconds, err := strconv.Atoi(ra); err == nil && seconds > 0 {
				deadline := time.Now().Add(time.Duration(seconds) * time.Second)

				c.throttleMu.Lock()
				if deadline.After(c.throttledUntil) {
					c.throttledUntil = deadline
				}
				c.throttleMu.Unlock()

				return time.Duration(seconds) * time.Second
			}
		}
	}

	return c.calcBackoff(attempt)
}

// calcBackoff computes exponential backoff using the client's retry policy.
func (c *Client) calcBackoff(attempt int) time.Duration {
	return c.retryPolicy.Delay(attempt)
}

// parseRetryAfter extracts the Retry-After header from 429 and 503 responses
// and returns it as a time.Duration. Returns 0 when the header is absent,
// unparseable, or the status code is not 429/503.
func parseRetryAfter(resp *http.Response) time.Duration {
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

// rewindBody seeks an io.Reader back to offset 0 if it implements io.Seeker.
// All callers use bytes.NewReader (which is an io.ReadSeeker), so the body
// is fully available on retry. Returns nil when body is nil or not seekable.
func rewindBody(body io.Reader) error {
	if body == nil {
		return nil
	}

	if seeker, ok := body.(io.Seeker); ok {
		if _, err := seeker.Seek(0, io.SeekStart); err != nil {
			return fmt.Errorf("graph: rewinding request body for retry: %w", err)
		}
	}

	return nil
}

// timeSleep waits for the given duration or until the context is canceled.
// It is the default sleepFunc for Client.
func timeSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
