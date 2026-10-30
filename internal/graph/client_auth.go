package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
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

// doRequest is the shared path for package-internal authenticated requests.
// Makes a single authenticated request via doOnce. If the response is non-2xx,
// reads the error body and returns a *GraphError. No retry loop — that's the
// transport's responsibility.
func (c *Client) doRequest(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header,
) (*http.Response, error) {
	return c.doRequestVia(ctx, method, path, body, extraHeaders, requestRoute{client: c.httpClient})
}

// requestRoute selects how an authenticated request is dispatched. The
// default follows redirects through c.httpClient. keepRedirect sends the
// request (and its 401 refresh retry) through client unchanged and hands a
// 3xx back to the caller instead of treating it as an error, for routes
// whose redirect target must not receive the bearer (/content).
type requestRoute struct {
	client       *http.Client
	keepRedirect bool
}

func (c *Client) doRequestVia(
	ctx context.Context, method, path string, body io.Reader, extraHeaders http.Header, route requestRoute,
) (*http.Response, error) {
	url := c.baseURL + path

	resp, tokenFresh, err := c.doOnce(ctx, method, url, body, extraHeaders, route.client)
	if err != nil {
		return nil, fmt.Errorf("graph: %s %s: %w", method, path, err)
	}

	if route.keepRedirect && resp.StatusCode >= http.StatusMultipleChoices && resp.StatusCode < http.StatusBadRequest {
		return resp, nil
	}

	// 2xx -> success, caller owns the response body.
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		c.logger.Debug("request succeeded",
			slog.String("method", method),
			slog.String("path", path),
			slog.Int("status", resp.StatusCode),
			slog.String("request_id", resp.Header.Get("request-id")),
		)
		if c.authSuccessHook != nil {
			c.authSuccessHook(ctx)
		}

		return resp, nil
	}

	// Non-2xx -> build GraphError from response.
	wwwAuthenticate := resp.Header.Get("WWW-Authenticate")
	graphErr := c.buildError(method, path, resp)
	if tokenFresh && isTransientAuthRejection(graphErr, wwwAuthenticate) {
		// Microsoft issued this token moments ago, so the account is fine;
		// the 401 is the service failing to validate it (D-072).
		graphErr.Err = ErrAuthTransient
	}

	return nil, graphErr
}

// isTransientAuthRejection recognizes a 401 whose body or challenge names a
// server-side validation fault rather than a bad credential: "Unable to
// initialize RPS" (the consumer auth backend), generalException, or an
// invalid_token challenge that does not say the token expired. It is only
// trusted after a forced refresh proved the credential good.
func isTransientAuthRejection(graphErr *GraphError, wwwAuthenticate string) bool {
	if graphErr == nil || graphErr.StatusCode != http.StatusUnauthorized {
		return false
	}

	message := strings.ToLower(graphErr.Message)
	challenge := strings.ToLower(wwwAuthenticate)

	return strings.Contains(message, "unable to initialize rps") ||
		graphErr.HasCode("generalException") ||
		(strings.Contains(challenge, `error="invalid_token"`) && !strings.Contains(challenge, "expired"))
}

// forceRefresher is the optional token-source capability to redeem the
// refresh token before the cached access token expires.
type forceRefresher interface {
	ForceRefresh() (string, error)
}

// doOnce executes a single HTTP request with authentication. Handles 401 token
// refresh as an auth lifecycle concern (not transient retry). The body must be
// seekable (bytes.Reader) if 401 refresh is needed — all callers satisfy this.
// doOnce reports tokenFresh when a 401 forced a token refresh that
// succeeded, so the token on the final attempt is known-good.
func (c *Client) doOnce(
	ctx context.Context, method, url string, body io.Reader, extraHeaders http.Header, client *http.Client,
) (resp *http.Response, tokenFresh bool, err error) {
	c.logger.Debug("preparing request",
		slog.String("method", method),
		slog.String("url", url),
	)

	tok, err := c.token.Token()
	if err != nil {
		return nil, false, fmt.Errorf("obtaining token: %w", err)
	}

	req, err := c.buildAuthorizedRequest(ctx, method, url, body, tok, extraHeaders)
	if err != nil {
		return nil, false, err
	}

	resp, err = c.dispatchRequestWith(client, req)
	if err != nil {
		c.logger.Debug("HTTP request failed",
			slog.String("method", method),
			slog.String("url", url),
			slog.Any("error", err),
		)

		// doRequest adds the method and path; *url.Error already names the
		// URL. Wrapping here again printed the request three times.
		return nil, false, err
	}

	c.logger.Debug("HTTP response received",
		slog.String("method", method),
		slog.String("url", url),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", resp.Header.Get("request-id")),
	)

	// 401 token refresh: if the server rejects the token, try once more with
	// a fresh token. ReuseTokenSource refreshes only an expired token, so
	// when it hands back the same rejected token the refresh is forced. This
	// is auth lifecycle management, not transient retry.
	retryResp, outcome, err := c.retryUnauthorized(ctx, client, method, url, body, extraHeaders, tok, resp)
	if err != nil {
		return nil, false, err
	}
	if outcome.retried {
		return retryResp, outcome.tokenFresh, nil
	}

	return resp, outcome.tokenFresh, nil
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

// closeUnauthorizedBody closes a 401 response that will not be returned.
func closeUnauthorizedBody(resp *http.Response) error {
	if resp == nil || resp.Body == nil {
		return nil
	}
	if err := resp.Body.Close(); err != nil {
		return fmt.Errorf("closing 401 response body: %w", err)
	}

	return nil
}

// unauthorizedRetry is what retryUnauthorized did with a 401.
type unauthorizedRetry struct {
	retried    bool // a second request was sent with a different token
	tokenFresh bool // a forced refresh succeeded, so the account sign-in is good
}

func (c *Client) retryUnauthorized(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	body io.Reader,
	extraHeaders http.Header,
	token string,
	resp *http.Response,
) (*http.Response, unauthorizedRetry, error) {
	if resp.StatusCode != http.StatusUnauthorized {
		return nil, unauthorizedRetry{}, nil
	}

	newTok, err := c.token.Token()
	if err != nil {
		// The 401 response is not handed back on an error, so it is closed
		// here; leaving it open leaked the connection.
		return nil, unauthorizedRetry{}, errors.Join(
			fmt.Errorf("refreshing token after 401: %w", err), closeUnauthorizedBody(resp))
	}

	// A token different from the rejected one was just issued (by this
	// call's refresh or a concurrent one), so it is known-good either way.
	outcome := unauthorizedRetry{tokenFresh: newTok != token}
	if newTok == token {
		refresher, canForce := c.token.(forceRefresher)
		if !canForce {
			return nil, outcome, nil
		}
		newTok, err = refresher.ForceRefresh()
		if err != nil {
			// invalid_grant arrives here as ErrLoginExpired+ErrUnauthorized:
			// the sign-in really is gone, which is the only fatal 401.
			return nil, outcome, errors.Join(
				fmt.Errorf("refreshing token after 401: %w", err), closeUnauthorizedBody(resp))
		}
		outcome.tokenFresh = true
		if newTok == token {
			// The bridge rate-limits forced refreshes, so a burst of 401s
			// shares one; this token is that fresh one and was rejected.
			return nil, outcome, nil
		}
	}

	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, outcome, fmt.Errorf("closing 401 response body: %w", closeErr)
	}

	c.logger.Info("retrying after 401 with refreshed token",
		slog.String("method", method),
		slog.String("url", url),
	)

	if rewindErr := rewindBody(body); rewindErr != nil {
		return nil, outcome, rewindErr
	}

	req, err := c.buildAuthorizedRequest(ctx, method, url, body, newTok, extraHeaders)
	if err != nil {
		return nil, outcome, fmt.Errorf("building retry request: %w", err)
	}

	retryResp, err := c.dispatchRequestWith(client, req)
	if err != nil {
		return nil, outcome, fmt.Errorf("retrying after 401: %w", err)
	}

	outcome.retried = true

	return retryResp, outcome, nil
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
