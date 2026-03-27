package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
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

	// maxDeltaPages is the upper bound on pages fetched by DeltaAll/DeltaFolderAll.
	// A buggy API or circular NextLinks could loop forever without this guard.
	defaultMaxDeltaPages = 10000

	// maxRecursionDepth is the upper bound on folder nesting depth for
	// ListChildrenRecursive. Prevents stack overflow on pathological hierarchies
	// or circular references.
	defaultMaxRecursionDepth = 100
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
// caller concern (CLI: RetryTransport, sync: raw transport, single attempt,
// engine records failure for the engine retry sweep).
type Client struct {
	baseURL               string
	httpClient            *http.Client
	token                 TokenSource
	logger                *slog.Logger
	userAgent             string
	deltaPreferHeader     http.Header
	maxDeltaPages         int
	maxRecursionDepth     int
	driveDiscoveryRetries int
	uploadURLValidator    func(*url.URL) error
	copyMonitorValidator  func(*url.URL) error
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

	if err := validateGraphBaseURL(baseURL); err != nil {
		panic(fmt.Sprintf("graph.NewClient: invalid base URL: %v", err))
	}

	return &Client{
		baseURL:               baseURL,
		httpClient:            httpClient,
		token:                 token,
		logger:                logger,
		userAgent:             userAgent,
		deltaPreferHeader:     newDeltaPreferHeader(),
		maxDeltaPages:         defaultMaxDeltaPages,
		maxRecursionDepth:     defaultMaxRecursionDepth,
		driveDiscoveryRetries: retry.DriveDiscoveryPolicy().MaxAttempts,
		uploadURLValidator:    validateUploadURL,
		copyMonitorValidator:  validateCopyMonitorURL,
	}
}

func newDeltaPreferHeader() http.Header {
	return http.Header{
		"Prefer": {"deltashowremoteitemsaliasid"},
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

// buildError reads the error response body, builds a *GraphError with the
// appropriate sentinel, and logs the failure. Used by both Do and pre-auth paths.
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

	// Non-2xx → build GraphError.
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

	return nil, &GraphError{
		StatusCode: resp.StatusCode,
		RequestID:  reqID,
		Message:    string(errBody),
		Err:        classifyStatus(resp.StatusCode),
		RetryAfter: retryAfter,
	}
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
