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

// Retry and backoff constants.
const (
	maxRetries     = 5
	baseBackoff    = 1 * time.Second
	maxBackoff     = 60 * time.Second
	backoffFactor  = 2.0
	jitterFraction = 0.25
	userAgent      = "onedrive-go/0.1"
)

// TokenSource provides OAuth2 bearer tokens. Defined at the consumer
// (graph package) per Go convention "accept interfaces, return structs".
// The auth increment (1.2) provides the real implementation.
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

// Do executes an HTTP request against the Graph API.
// The path is appended to the client's base URL.
// For non-nil bodies, Content-Type is set to application/json.
// The caller is responsible for closing the response body on success.
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	url := c.baseURL + path

	var attempt int
	for {
		resp, err := c.doOnce(ctx, method, url, body)
		if err != nil {
			// Context cancellation is not retryable.
			if ctx.Err() != nil {
				return nil, fmt.Errorf("graph: request canceled: %w", ctx.Err())
			}

			// Network errors are retryable.
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

		// 2xx — success.
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			c.logger.Debug("request succeeded",
				slog.String("method", method),
				slog.String("path", path),
				slog.Int("status", resp.StatusCode),
			)

			return resp, nil
		}

		// Read and close body for error responses.
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
// For 429 responses with a Retry-After header, that value is used.
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

	// Apply ±25% jitter.
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
