package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// do executes an authenticated HTTP request against the Graph API.
// The caller is responsible for closing the response body on success.
// On error, returns a *GraphError wrapping a sentinel (use errors.Is to classify).
// Retry is handled by the HTTP transport layer — this method makes exactly one
// logical attempt (plus one 401 token refresh if applicable).
func (c *Client) do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doRequest(ctx, method, path, body, nil)
}

// doGetWithHeaders executes an authenticated GET request with additional
// headers. Use this for API calls that require special headers such as delta's
// Prefer value.
func (c *Client) doGetWithHeaders(
	ctx context.Context, path string, extraHeaders http.Header,
) (*http.Response, error) {
	return c.doRequest(ctx, http.MethodGet, path, nil, extraHeaders)
}

// doRequest is the shared path for do and doWithHeaders. Makes a single
// authenticated request via doOnce. If the response is non-2xx, reads the
// error body and returns a *GraphError. No retry loop — that's the transport's
// responsibility.
func (c *Client) doRequest(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	url := c.baseURL + path

	resp, err := c.doOnce(ctx, method, url, body, extraHeaders)
	if err != nil {
		return nil, fmt.Errorf("graph: %s %s: %w", method, path, err)
	}

	// 2xx -> success, caller owns the response body.
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		c.logger.Debug("request succeeded",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", resp.StatusCode),
			slog.String("request_id", resp.Header.Get("request-id")),
		)

		return resp, nil
	}

	// Non-2xx -> build GraphError from response.
	return nil, c.buildError(method, path, resp)
}

// doOnce executes a single HTTP request with authentication. Handles 401 token
// refresh as an auth lifecycle concern (not transient retry). The body must be
// seekable (bytes.Reader) if 401 refresh is needed — all callers satisfy this.
func (c *Client) doOnce(
	ctx context.Context, method, url string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	c.logger.Debug("preparing request",
		slog.String("method", method),
		slog.String("url", url),
	)

	tok, err := c.token.Token()
	if err != nil {
		return nil, fmt.Errorf("obtaining token: %w", err)
	}

	req, err := c.buildAuthorizedRequest(ctx, method, url, body, tok, extraHeaders)
	if err != nil {
		return nil, err
	}

	resp, err := c.dispatchRequest(req)
	if err != nil {
		c.logger.Debug("HTTP request failed",
			slog.String("method", method),
			slog.String("url", url),
			slog.String("error", err.Error()),
		)

		return nil, fmt.Errorf("HTTP %s %s: %w", method, url, err)
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
	// This is auth lifecycle management, not transient retry.
	if retryResp, retried, err := c.retryUnauthorized(ctx, method, url, body, extraHeaders, tok, resp); err != nil {
		return nil, err
	} else if retried {
		return retryResp, nil
	}

	return resp, nil
}

func (c *Client) buildAuthorizedRequest(
	ctx context.Context,
	method string,
	url string,
	body io.Reader,
	token string,
	extraHeaders http.Header,
) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
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

	return req, nil
}

func (c *Client) retryUnauthorized(
	ctx context.Context,
	method string,
	url string,
	body io.Reader,
	extraHeaders http.Header,
	token string,
	resp *http.Response,
) (*http.Response, bool, error) {
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, false, nil
	}

	newTok, err := c.token.Token()
	if err != nil {
		return nil, false, fmt.Errorf("refreshing token after 401: %w", err)
	}
	if newTok == token {
		return nil, false, nil
	}

	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, false, fmt.Errorf("closing 401 response body: %w", closeErr)
	}

	c.logger.Info("retrying after 401 with refreshed token",
		slog.String("method", method),
		slog.String("url", url),
	)

	if rewindErr := rewindBody(body); rewindErr != nil {
		return nil, false, rewindErr
	}

	req, err := c.buildAuthorizedRequest(ctx, method, url, body, newTok, extraHeaders)
	if err != nil {
		return nil, false, fmt.Errorf("building retry request: %w", err)
	}

	retryResp, err := c.dispatchRequest(req)
	if err != nil {
		return nil, false, fmt.Errorf("HTTP %s %s retry: %w", method, url, err)
	}

	return retryResp, true, nil
}

// rewindBody seeks an io.Reader back to offset 0 if it implements io.Seeker.
// All callers use bytes.NewReader (which is an io.ReadSeeker), so the body
// is fully available on 401 refresh. Returns nil when body is nil or not seekable.
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
