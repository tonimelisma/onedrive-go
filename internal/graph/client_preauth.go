package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// unreadableErrorBody is the message of the error of an answer whose body
// could not be read.
const unreadableErrorBody = "(failed to read response body)"

// buildError reads the error response body, builds a *GraphError with the
// appropriate sentinel, and logs the failure. Used by both authenticated and
// pre-auth dispatch paths; path is what the log names the request by, never
// a pre-authenticated URL. A body that cannot be read leaves its own record
// and the placeholder message; the answer is still recorded, by the codes of
// the bytes that did arrive, and returned as a *GraphError.
func (c *Client) buildError(method, path string, resp *http.Response) *GraphError {
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if closeErr := resp.Body.Close(); closeErr != nil {
		c.logger.Debug("failed to close error response body",
			slog.String("method", method),
			slog.String("path", path),
			slog.Any("error", closeErr),
		)
	}

	errBody := body
	if readErr != nil {
		c.logBodyReadFailure(method, path, resp, readErr)
		errBody = []byte(unreadableErrorBody)
	}

	graphErr := graphErrorOfAnswer(resp, errBody)

	// A whole error object can arrive before the read fails. The record then
	// names its codes, sanitized like the error's; the error keeps the
	// placeholder, so it is classified as when nothing arrived.
	arrived := graphErr
	if readErr != nil {
		arrived = graphErrorOfAnswer(resp, body)
	}

	c.logFailedAnswer(method, path, resp, body, arrived)

	return graphErr
}

// graphErrorOfAnswer builds the *GraphError of a failed answer from its
// status, its headers and errBody.
func graphErrorOfAnswer(resp *http.Response, errBody []byte) *GraphError {
	graphErr := buildGraphError(resp.StatusCode, resp.Header.Get("request-id"),
		retry.ResponseRetryAfter(resp, time.Now()), errBody)
	graphErr.AgsDiagnostic = resp.Header.Get(headerAgsDiagnostic)

	return graphErr
}

// logBodyReadFailure records a body that could not be read: a failed
// answer's error body, or the body of an upload fragment's unexpected 2xx
// answer. The record names the answer's status and request ID, so it is
// tied to its answer by more than the request beside it.
func (c *Client) logBodyReadFailure(method, path string, resp *http.Response, readErr error) {
	c.logger.Debug("failed to read response body",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", resp.Header.Get("request-id")),
		slog.Any("error", readErr),
	)
}

// logFailedAnswer writes the one "request failed" record of a failed answer,
// body being the bytes of its body that arrived and graphErr the error built
// from them.
//
// Logged at DEBUG — the HTTP layer lacks context to judge severity.
// Callers decide: e.g. DELETE 404 is success, GET 404 is an error.
// The code chain, the content type and the size and kind of the body that
// arrived are the answer's payload evidence; the body and the message
// themselves are never logged.
func (c *Client) logFailedAnswer(method, path string, resp *http.Response, body []byte, graphErr *GraphError) {
	innerCodes := graphErr.InnerCodes
	if innerCodes == nil {
		innerCodes = []string{}
	}

	c.logger.Debug("request failed",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", graphErr.RequestID),
		slog.String("ags_diagnostic", graphErr.AgsDiagnostic),
		slog.String("code", graphErr.Code),
		slog.Any("inner_codes", innerCodes),
		slog.String("content_type", resp.Header.Get("Content-Type")),
		slog.Int("body_bytes", len(body)),
		slog.String("body_kind", errorBodyKind(body)),
	)
}

// doPreAuth executes an HTTP request against a pre-authenticated URL. No
// Authorization header is added — the URL itself is pre-authenticated.
// If the httpClient has a RetryTransport, retry happens automatically at
// the transport layer. On non-2xx response, returns *GraphError. A failure
// before the request leaves (ctx already ended, makeReq failed) carries
// errRequestNotSent.
func (c *Client) doPreAuth(
	ctx context.Context, desc string, makeReq func() (*http.Request, error),
) (*http.Response, error) {
	if ctx.Err() != nil {
		// The cause, as below: a stall is retryable, a cancellation is not.
		return nil, fmt.Errorf("graph: %s canceled before it was sent: %w: %w", desc, errRequestNotSent, context.Cause(ctx))
	}

	req, err := makeReq()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errRequestNotSent, err)
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

	logTarget := preAuthLogTarget(desc)
	req = retry.WithRequestLogTarget(req, logTarget)

	resp, err := c.dispatchRequest(req)
	if err != nil {
		if ctx.Err() != nil {
			// The cause, not ctx.Err(): a transfer the stall watchdog
			// abandoned is retryable, while context.Canceled reads as a
			// shutdown and drops the action without retry.
			return nil, fmt.Errorf("graph: %s canceled: %w", desc, context.Cause(ctx))
		}

		return nil, fmt.Errorf("graph: %s failed: %w", desc, err)
	}

	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		return resp, nil
	}

	// Non-2xx -> build GraphError. The log names the request by its target,
	// never by the URL, which carries a credential.
	graphErr := c.buildError(req.Method, logTarget, resp)
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		// The URL's own credential was refused; the account sign-in was never
		// involved, so this must not read as ErrUnauthorized (D-072).
		graphErr.Err = ErrPreAuthURLRejected
	case resp.Header.Get(headerVirusInfected) != "":
		graphErr.Err = ErrMalwareDetected
	}

	return nil, graphErr
}

// preAuthLogTarget is what the logs name the pre-authenticated request desc
// by, never its URL, which carries a credential.
func preAuthLogTarget(desc string) string {
	return "preauth:" + desc
}

func (c *Client) dispatchRequest(req *http.Request) (*http.Response, error) {
	return c.dispatchRequestWith(c.httpClient, req)
}

// dispatchRequestWith sends req through client (c.httpClient, or a copy
// with a different redirect policy). It is the one place every response that
// reaches the client comes back through, so every response body is a
// responseBody here, bounded by the stall window and typed on failure, and a
// failed round trip on a lost HTTP/2 connection carries ErrConnectionLost.
// The answers that do not reach it are never read: the retrying transport
// closes an answer it retries unread, and a redirect's body is closed unread
// before net/http follows it (closingRedirects). A request the retrying
// transport returned unsent because the throttle wait outlasts its deadline
// (*retry.ThrottledError) carries errRequestNotSent and ErrThrottled: it is
// the earlier 429's answer, and nothing was asked of OneDrive.
func (c *Client) dispatchRequestWith(client *http.Client, req *http.Request) (*http.Response, error) {
	// Callers validate the request URL before dispatch: Graph base URLs are
	// checked in NewClient and pre-auth URLs are validated at their boundary.
	resp, err := c.closingRedirects(client).Do(req) //nolint:gosec // Request URLs are validated by the Graph/pre-auth boundary helpers.
	if err != nil {
		// net/http wraps transport failures in *url.Error, whose text carries
		// the full request URL. Pre-authenticated URLs embed their credential,
		// so scrub it before the error can reach logs or the console.
		redacted := markLostConnection(redactURLError(err))

		var throttled *retry.ThrottledError
		if errors.As(redacted, &throttled) {
			return nil, fmt.Errorf("dispatching request: %w: %w: %w", errRequestNotSent, ErrThrottled, redacted)
		}

		return nil, fmt.Errorf("dispatching request: %w", redacted)
	}

	if resp.Body != nil {
		resp.Body = newResponseBody(resp.Body, c.stallWindow(), c.logger)
	}

	return resp, nil
}

// maxRedirects is net/http's own limit on the redirects one request follows,
// kept for a client without a redirect policy of its own.
const maxRedirects = 10

// closingRedirects returns a copy of client that closes a redirect's body
// unread before it follows the redirect. net/http reads up to 2 KB of that
// body first, to reuse the connection, where no bound of the client reaches:
// a redirect whose body stopped after its headers held the request for as
// long as the server stayed quiet. Over HTTP/2 the close resets only that
// answer's stream. The closed body is replaced by an empty one, which is what
// net/http then reads and closes: a second close of an HTTP/2 body would
// return its buffered bytes to the connection's receive window again.
// client's redirect policy decides as before, and a redirect it keeps
// (http.ErrUseLastResponse) comes back with its body open, through the stall
// window like every answer.
func (c *Client) closingRedirects(client *http.Client) *http.Client {
	policy := client.CheckRedirect
	follow := *client
	follow.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		var err error
		switch {
		case policy != nil:
			err = policy(next, via)
		case len(via) >= maxRedirects:
			err = fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if errors.Is(err, http.ErrUseLastResponse) || next.Response == nil || next.Response.Body == nil {
			return err
		}
		if closeErr := next.Response.Body.Close(); closeErr != nil {
			c.logger.Debug("closing a redirect's response body failed",
				slog.Int("status", next.Response.StatusCode),
				slog.Any("error", closeErr),
			)
		}
		next.Response.Body = http.NoBody

		return err
	}

	return &follow
}

func (c *Client) validatedUploadURL(raw UploadURL) (string, error) {
	validate := c.validators.uploadURL
	if validate == nil {
		validate = validateUploadURL
	}

	parsedURL, err := parseAndValidatePreAuthURL(string(raw), "upload", validate)
	if err != nil {
		return "", fmt.Errorf("graph: validating upload URL: %w", err)
	}

	return parsedURL, nil
}

// validatedDownloadURL is the one gate between a server-supplied
// `@microsoft.graph.downloadUrl` and the request built from it. The returned
// string is what the download routes dispatch; the error never carries the
// URL.
func (c *Client) validatedDownloadURL(raw DownloadURL) (string, error) {
	validate := c.validators.downloadURL
	if validate == nil {
		validate = validateDownloadURL
	}

	parsedURL, err := parseAndValidatePreAuthURL(string(raw), "download", validate)
	if err != nil {
		return "", fmt.Errorf("graph: validating download URL: %w", err)
	}

	return parsedURL, nil
}

func (c *Client) validatedCopyMonitorURL(raw string) (string, error) {
	validate := c.validators.copyMonitor
	if validate == nil {
		validate = validateCopyMonitorURL
	}

	parsedURL, err := parseAndValidatePreAuthURL(raw, "copy monitor", validate)
	if err != nil {
		return "", fmt.Errorf("graph: validating copy monitor URL: %w", err)
	}

	return parsedURL, nil
}

// headerAgsDiagnostic is the Graph gateway's diagnostic header, kept on
// GraphError for support escalation (MS-201).
const headerAgsDiagnostic = "X-Ms-Ags-Diagnostic"

func buildGraphError(statusCode int, requestID string, retryAfter time.Duration, rawBody []byte) *GraphError {
	code, message, innerCodes := parseGraphErrorBody(rawBody)
	if message == "" {
		message = string(rawBody)
	}

	// Everything taken from the body is sanitized here, once, so whatever
	// prints or logs the error (its text, the debug record, quirk retry
	// evidence) only ever sees the sanitized copy.
	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  requestID,
		Code:       sanitizeGraphErrorText(code),
		InnerCodes: sanitizeGraphErrorTexts(innerCodes),
		Message:    sanitizeGraphErrorText(message),
		RawBody:    sanitizeGraphErrorText(string(rawBody)),
		Err:        classifyStatus(statusCode),
		RetryAfter: retryAfter,
	}
	if graphErr.HasCode(graphCodeMalwareDetected) {
		// A 403 here is not a permission answer: the content is blocked.
		graphErr.Err = ErrMalwareDetected
	}

	return graphErr
}
