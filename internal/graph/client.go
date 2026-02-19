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
	url := c.baseURL + path

	var attempt int
	for {
		resp, err := c.doOnce(ctx, method, url, body)
		if err != nil {
			// Fail fast on context cancellation — never retry a deliberate abort.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("graph: request canceled: %w", ctx.Err())
			}

			// Network errors (DNS, TCP, TLS) are always worth retrying.
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
			)

			return resp, nil
		}

		// Must drain and close the body even on errors, otherwise the
		// underlying TCP connection won't be reused by the HTTP client pool.
		errBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()

		if readErr != nil {
			errBody = []byte("(failed to read response body)")
		}

		// Graph API returns request-id in every response — essential for debugging
		// issues with Microsoft support.
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

		sentinel := classifyStatus(resp.StatusCode)
		graphErr := &GraphError{
			StatusCode: resp.StatusCode,
			RequestID:  reqID,
			Message:    string(errBody),
			Err:        sentinel,
		}

		if attempt > 0 {
			c.logger.Error("request failed after retries",
				slog.String("method", method),
				slog.String("path", path),
				slog.Int("status", resp.StatusCode),
				slog.Int("attempts", attempt+1),
			)
		}

		return nil, graphErr
	}
}

// doOnce executes a single HTTP request (no retry).
func (c *Client) doOnce(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
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

	return c.httpClient.Do(req)
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
