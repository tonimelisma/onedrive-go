package graph

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// errRequestNotSent marks a request that never left the client: the token
// source failed, the context had already ended, or the request could not be
// built. OneDrive saw nothing, so nothing the request asked for happened. On
// the 401 path the request not sent is the retry, and the attempt before it
// was refused. doPreAuth marks the same failures of a pre-authenticated
// request, and the quirk retry a backoff whose context ended. Callers that
// must know whether a write may have landed (a file create, an upload's last
// fragment) test for it with errors.Is.
var errRequestNotSent = errors.New("request not sent")

// headerContentType and contentTypeJSON are how an authenticated request
// declares a JSON body.
const (
	headerContentType = "Content-Type"
	contentTypeJSON   = "application/json"
)

// do executes an authenticated HTTP request against the Graph API. body is
// the whole JSON request body, nil for none. Every attempt, the transport's
// and the 401 retry included, is built from those bytes, so a request goes
// out again exactly as it first did; a body that cannot be replayed cannot be
// passed. The caller is responsible for closing the response body on success.
// On error, returns a *GraphError wrapping a sentinel (use errors.Is to classify).
// Retry is handled by the HTTP transport layer — this method makes exactly one
// logical attempt (plus one 401 token refresh if applicable).
func (c *Client) do(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
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
	ctx context.Context, method, path string, body []byte, extraHeaders http.Header,
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

// doRequestVia is doRequest dispatched through route.
func (c *Client) doRequestVia(
	ctx context.Context, method, path string, body []byte, extraHeaders http.Header, route requestRoute,
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
// refresh as an auth lifecycle concern (not transient retry); the retry is
// built from the same body bytes as the first attempt. A failure before a
// request leaves carries errRequestNotSent.
// doOnce reports tokenFresh when a 401 forced a token refresh that
// succeeded, so the token on the final attempt is known-good.
func (c *Client) doOnce(
	ctx context.Context, method, url string, body []byte, extraHeaders http.Header, client *http.Client,
) (resp *http.Response, tokenFresh bool, err error) {
	c.logger.Debug("preparing request",
		slog.String("method", method),
		slog.String("url", url),
	)

	tok, err := c.token.Token()
	if err != nil {
		return nil, false, fmt.Errorf("%w: obtaining token: %w", errRequestNotSent, err)
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

// buildAuthorizedRequest builds one attempt of an authenticated request, its
// body a fresh reader over body's bytes, so http.NewRequest also gives it the
// exact Content-Length and the GetBody a retrying transport replays. It fails
// with errRequestNotSent when the request cannot leave: the context has
// already ended (net/http would refuse to send it), or the request cannot be
// built.
func (c *Client) buildAuthorizedRequest(
	ctx context.Context,
	method string,
	url string,
	body []byte,
	token string,
	extraHeaders http.Header,
) (*http.Request, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w: %w", errRequestNotSent, context.Cause(ctx))
	}

	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, fmt.Errorf("%w: creating request: %w", errRequestNotSent, err)
	}

	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", c.userAgent)

	// A request with a body declares it JSON: every authenticated Graph
	// mutation (PATCH, POST) takes JSON. A body-less request that must still
	// declare it passes the header in extraHeaders (RestoreItem); content
	// uploads go through doRawUpload or pre-authenticated URLs instead.
	if len(body) > 0 {
		req.Header.Set(headerContentType, contentTypeJSON)
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

// retryUnauthorized sends the request once more after a 401, with a token
// other than the rejected one. Every failure before that retry leaves carries
// errRequestNotSent: the one request sent was refused.
func (c *Client) retryUnauthorized(
	ctx context.Context,
	client *http.Client,
	method string,
	url string,
	body []byte,
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
			fmt.Errorf("%w: refreshing token after 401: %w", errRequestNotSent, err), closeUnauthorizedBody(resp))
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
				fmt.Errorf("%w: refreshing token after 401: %w", errRequestNotSent, err), closeUnauthorizedBody(resp))
		}
		outcome.tokenFresh = true
		if newTok == token {
			// The bridge rate-limits forced refreshes, so a burst of 401s
			// shares one; this token is that fresh one and was rejected.
			return nil, outcome, nil
		}
	}

	if closeErr := resp.Body.Close(); closeErr != nil {
		return nil, outcome, fmt.Errorf("%w: closing 401 response body: %w", errRequestNotSent, closeErr)
	}

	// Built before the retry is announced: a request that cannot be built
	// fails here, and no second request goes out.
	req, err := c.buildAuthorizedRequest(ctx, method, url, body, newTok, extraHeaders)
	if err != nil {
		return nil, outcome, fmt.Errorf("building retry request: %w", err)
	}

	c.logger.Info("retrying after 401 with refreshed token",
		slog.String("method", method),
		slog.String("url", url),
	)

	retryResp, err := c.dispatchRequestWith(client, req)
	if err != nil {
		return nil, outcome, fmt.Errorf("retrying after 401: %w", err)
	}

	outcome.retried = true

	return retryResp, outcome, nil
}
