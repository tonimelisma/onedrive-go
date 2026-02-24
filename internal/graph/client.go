package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// DefaultBaseURL is the production Microsoft Graph API v1.0 endpoint.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

// Per architecture.md §7.2: base 1s, factor 2x, max 60s, ±25% jitter, max 5 retries.
const (
	maxRetries     = 5
	baseBackoff    = 1 * time.Second
	maxBackoff     = 60 * time.Second
	backoffFactor  = 2.0
	jitterFraction = 0.25
	userAgent      = "onedrive-go/0.1"
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
	baseURL    string
	httpClient *http.Client
	token      TokenSource
	logger     *slog.Logger

	// sleepFunc is called to wait between retries. Defaults to timeSleep.
	// Tests override this to avoid real delays.
	sleepFunc func(ctx context.Context, d time.Duration) error
}

// NewClient creates a Graph API client.
// baseURL is typically "https://graph.microsoft.com/v1.0".
func NewClient(baseURL string, httpClient *http.Client, token TokenSource, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{
		baseURL:    baseURL,
		httpClient: httpClient,
		token:      token,
		logger:     logger,
		sleepFunc:  timeSleep,
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

			if attempt < maxRetries {
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

			return nil, fmt.Errorf("graph: %s %s failed after %d retries: %w", method, path, maxRetries, err)
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

		errBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			errBody = []byte("(failed to read response body)")
		}

		reqID := resp.Header.Get("request-id")

		if isRetryable(resp.StatusCode) && attempt < maxRetries {
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

		return nil, c.terminalError(method, path, resp.StatusCode, reqID, errBody, attempt)
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
	req.Header.Set("User-Agent", userAgent)

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

	return resp, nil
}

// terminalError builds a GraphError and logs the final failure.
// Extracted from doRetry to keep the retry loop under funlen limits.
func (c *Client) terminalError(
	method, path string, statusCode int, reqID string, body []byte, attempt int,
) *GraphError {
	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  reqID,
		Message:    string(body),
		Err:        classifyStatus(statusCode),
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
		c.logger.Warn("request failed",
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

			if attempt < maxRetries {
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

			return nil, fmt.Errorf("graph: %s failed after %d retries: %w", desc, maxRetries, err)
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			return resp, nil
		}

		errBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			errBody = []byte("(failed to read response body)")
		}

		reqID := resp.Header.Get("request-id")

		if isRetryable(resp.StatusCode) && attempt < maxRetries {
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

		return nil, c.preAuthTerminalError(desc, resp.StatusCode, reqID, errBody, attempt)
	}
}

// preAuthTerminalError builds a GraphError and logs the final failure for pre-auth URLs.
// Mirrors terminalError but uses desc instead of method+path.
func (c *Client) preAuthTerminalError(
	desc string, statusCode int, reqID string, body []byte, attempt int,
) *GraphError {
	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  reqID,
		Message:    string(body),
		Err:        classifyStatus(statusCode),
	}

	if attempt > 0 {
		c.logger.Error("pre-auth request failed after retries",
			slog.String("desc", desc),
			slog.Int("status", statusCode),
			slog.String("request_id", reqID),
			slog.Int("attempts", attempt+1),
		)
	} else {
		c.logger.Warn("pre-auth request failed",
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
				return time.Duration(seconds) * time.Second
			}
		}
	}

	return c.calcBackoff(attempt)
}

// calcBackoff computes exponential backoff with ±25% jitter.
func (c *Client) calcBackoff(attempt int) time.Duration {
	backoff := float64(baseBackoff) * math.Pow(backoffFactor, float64(attempt))
	if backoff > float64(maxBackoff) {
		backoff = float64(maxBackoff)
	}

	// Jitter prevents thundering herd when multiple workers hit rate limits simultaneously.
	jitter := backoff * jitterFraction * (rand.Float64()*2 - 1) //nolint:gosec // jitter does not need crypto rand
	backoff += jitter

	return time.Duration(backoff)
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
