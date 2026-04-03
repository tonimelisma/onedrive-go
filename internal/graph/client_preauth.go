package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// buildError reads the error response body, builds a *GraphError with the
// appropriate sentinel, and logs the failure. Used by both authenticated and
// pre-auth dispatch paths.
func (c *Client) buildError(method, path string, resp *http.Response) *GraphError {
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if closeErr := resp.Body.Close(); closeErr != nil {
		c.logger.Warn("failed to close error response body",
			slog.String("method", method),
			slog.String("path", path),
			slog.String("error", closeErr.Error()),
		)
	}

	if readErr != nil {
		errBody = []byte("(failed to read response body)")
	}

	reqID := resp.Header.Get("request-id")
	retryAfter := parseRetryAfter(resp)
	graphErr := buildGraphError(resp.StatusCode, reqID, retryAfter, errBody)

	// Log at DEBUG — the HTTP layer lacks context to judge severity.
	// Callers decide: e.g. DELETE 404 is success, GET 404 is an error.
	c.logger.Debug("request failed",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", reqID),
	)

	return graphErr
}

// doPreAuth executes an HTTP request against a pre-authenticated URL. No
// Authorization header is added — the URL itself is pre-authenticated.
// If the httpClient has a RetryTransport, retry happens automatically at
// the transport layer. On non-2xx response, returns *GraphError.
func (c *Client) doPreAuth(
	ctx context.Context, desc string, makeReq func() (*http.Request, error),
) (*http.Response, error) {
	req, err := makeReq()
	if err != nil {
		return nil, err
	}

	// Set GetBody so RetryTransport can rewind the body between attempts.
	// makeReq creates a fresh reader (e.g., io.SectionReader) each call, but
	// http.NewRequestWithContext wraps it in io.NopCloser which hides io.Seeker.
	// http.NewRequest only auto-sets GetBody for bytes.Reader/Buffer/strings.Reader.
	// Without this, retry of upload chunks with SectionReader bodies would fail.
	if req.GetBody == nil && req.Body != nil && req.Body != http.NoBody {
		req.GetBody = func() (io.ReadCloser, error) {
			freshReq, makeErr := makeReq()
			if makeErr != nil {
				return nil, makeErr
			}

			return freshReq.Body, nil
		}
	}

	req = retry.WithRequestLogTarget(req, "preauth:"+desc)

	resp, err := c.dispatchRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("graph: %s canceled: %w", desc, ctx.Err())
		}

		return nil, fmt.Errorf("graph: %s failed: %w", desc, err)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}

	// Non-2xx -> build GraphError.
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if closeErr := resp.Body.Close(); closeErr != nil {
		c.logger.Warn("failed to close pre-auth error response body",
			slog.String("desc", desc),
			slog.String("error", closeErr.Error()),
		)
	}

	if readErr != nil {
		errBody = []byte("(failed to read response body)")
	}

	reqID := resp.Header.Get("request-id")
	retryAfter := parseRetryAfter(resp)

	c.logger.Debug("pre-auth request failed",
		slog.String("desc", desc),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", reqID),
	)

	return nil, buildGraphError(resp.StatusCode, reqID, retryAfter, errBody)
}

func (c *Client) dispatchRequest(req *http.Request) (*http.Response, error) {
	// Callers validate the request URL before dispatch: Graph base URLs are
	// checked in NewClient and pre-auth URLs are validated at their boundary.
	resp, err := c.httpClient.Do(req) //nolint:gosec // Request URLs are validated by the Graph/pre-auth boundary helpers.
	if err != nil {
		return nil, fmt.Errorf("dispatching request: %w", err)
	}

	return resp, nil
}

func (c *Client) validatedUploadURL(raw UploadURL) (string, error) {
	validate := c.uploadURLValidator
	if validate == nil {
		validate = validateUploadURL
	}

	parsedURL, err := parseAndValidateUploadURL(raw, validate)
	if err != nil {
		return "", fmt.Errorf("graph: validating upload URL: %w", err)
	}

	return parsedURL, nil
}

func (c *Client) validatedCopyMonitorURL(raw string) (string, error) {
	validate := c.copyMonitorValidator
	if validate == nil {
		validate = validateCopyMonitorURL
	}

	parsedURL, err := parseAndValidateUploadURL(UploadURL(raw), validate)
	if err != nil {
		return "", fmt.Errorf("graph: validating copy monitor URL: %w", err)
	}

	return parsedURL, nil
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

func buildGraphError(statusCode int, requestID string, retryAfter time.Duration, rawBody []byte) *GraphError {
	code, message, innerCodes := parseGraphErrorBody(rawBody)
	if message == "" {
		message = string(rawBody)
	}

	message = sanitizeGraphErrorText(message)
	raw := sanitizeGraphErrorText(string(rawBody))

	return &GraphError{
		StatusCode: statusCode,
		RequestID:  requestID,
		Code:       code,
		InnerCodes: innerCodes,
		Message:    message,
		RawBody:    raw,
		Err:        classifyStatus(statusCode),
		RetryAfter: retryAfter,
	}
}
