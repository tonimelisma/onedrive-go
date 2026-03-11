package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
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

// Client is a pure HTTP client for the Microsoft Graph API. It handles request
// construction, authentication (including 401 token refresh), and error
// classification. Retry logic lives in retry.RetryTransport, configured on the
// http.Client's Transport — the graph Client itself never retries or sleeps.
// This separation keeps the client stateless and makes the retry decision a
// caller concern (CLI: RetryTransport, sync: raw transport + engine re-queue).
type Client struct {
	baseURL    string
	httpClient *http.Client
	token      TokenSource
	logger     *slog.Logger
	userAgent  string
}

// NewClient creates a Graph API client.
// baseURL is typically "https://graph.microsoft.com/v1.0".
// userAgent is sent in every request; defaults to "onedrive-go/dev" if empty.
// Retry is handled by the httpClient's Transport (wrap with retry.RetryTransport
// for automatic retry, or use http.DefaultTransport for single-attempt dispatch).
func NewClient(
	baseURL string, httpClient *http.Client, token TokenSource,
	logger *slog.Logger, userAgent string,
) *Client {
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
		baseURL:    baseURL,
		httpClient: httpClient,
		token:      token,
		logger:     logger,
		userAgent:  userAgent,
	}
}

// Do executes an authenticated HTTP request against the Graph API.
// The caller is responsible for closing the response body on success.
// On error, returns a *GraphError wrapping a sentinel (use errors.Is to classify).
// Retry is handled by the HTTP transport layer — this method makes exactly one
// logical attempt (plus one 401 token refresh if applicable).
func (c *Client) Do(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	return c.doRequest(ctx, method, path, body, nil)
}

// DoWithHeaders executes an authenticated HTTP request with additional headers.
// It behaves identically to Do but merges extraHeaders into the request.
// Use this for API calls that require special headers (e.g., Prefer for delta queries).
func (c *Client) DoWithHeaders(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	return c.doRequest(ctx, method, path, body, extraHeaders)
}

// doRequest is the shared path for Do and DoWithHeaders. Makes a single
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

	// 2xx → success, caller owns the response body.
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		c.logger.Debug("request succeeded",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", resp.StatusCode),
			slog.String("request_id", resp.Header.Get("request-id")),
		)

		return resp, nil
	}

	// Non-2xx → build GraphError from response.
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
	// This is auth lifecycle management, not transient retry.
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

// buildError reads the error response body, builds a *GraphError with the
// appropriate sentinel, and logs the failure. Used by both Do and pre-auth paths.
func (c *Client) buildError(method, path string, resp *http.Response) *GraphError {
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	resp.Body.Close()

	if readErr != nil {
		errBody = []byte("(failed to read response body)")
	}

	reqID := resp.Header.Get("request-id")
	retryAfter := parseRetryAfter(resp)

	graphErr := &GraphError{
		StatusCode: resp.StatusCode,
		RequestID:  reqID,
		Message:    string(errBody),
		Err:        classifyStatus(resp.StatusCode),
		RetryAfter: retryAfter,
	}

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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("graph: %s canceled: %w", desc, ctx.Err())
		}

		return nil, fmt.Errorf("graph: %s failed: %w", desc, err)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}

	// Non-2xx → build GraphError.
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	resp.Body.Close()

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

	return nil, &GraphError{
		StatusCode: resp.StatusCode,
		RequestID:  reqID,
		Message:    string(errBody),
		Err:        classifyStatus(resp.StatusCode),
		RetryAfter: retryAfter,
	}
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
