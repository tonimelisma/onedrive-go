package graph

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// buildError reads the error response body, builds a *GraphError with the
// appropriate sentinel, and logs the failure. Used by both authenticated and
// pre-auth dispatch paths; path is what the log names the request by, never
// a pre-authenticated URL.
func (c *Client) buildError(method, path string, resp *http.Response) *GraphError {
	errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxErrBodySize))
	if closeErr := resp.Body.Close(); closeErr != nil {
		c.logger.Debug("failed to close error response body",
			slog.String("method", method),
			slog.String("path", path),
			slog.Any("error", closeErr),
		)
	}

	if readErr != nil {
		errBody = []byte("(failed to read response body)")
	}

	reqID := resp.Header.Get("request-id")
	retryAfter := retry.ResponseRetryAfter(resp, time.Now())
	graphErr := buildGraphError(resp.StatusCode, reqID, retryAfter, errBody)
	graphErr.AgsDiagnostic = resp.Header.Get(headerAgsDiagnostic)

	// Log at DEBUG — the HTTP layer lacks context to judge severity.
	// Callers decide: e.g. DELETE 404 is success, GET 404 is an error.
	c.logger.Debug("request failed",
		slog.String("method", method),
		slog.String("path", path),
		slog.Int("status", resp.StatusCode),
		slog.String("request_id", reqID),
		slog.String("ags_diagnostic", graphErr.AgsDiagnostic),
	)

	return graphErr
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

	logTarget := "preauth:" + desc
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

func (c *Client) dispatchRequest(req *http.Request) (*http.Response, error) {
	return c.dispatchRequestWith(c.httpClient, req)
}

// dispatchRequestWith sends req through client (c.httpClient, or a copy
// with a different redirect policy).
func (c *Client) dispatchRequestWith(client *http.Client, req *http.Request) (*http.Response, error) {
	// Callers validate the request URL before dispatch: Graph base URLs are
	// checked in NewClient and pre-auth URLs are validated at their boundary.
	resp, err := client.Do(req) //nolint:gosec // Request URLs are validated by the Graph/pre-auth boundary helpers.
	if err != nil {
		// net/http wraps transport failures in *url.Error, whose text carries
		// the full request URL. Pre-authenticated URLs embed their credential,
		// so scrub it before the error can reach logs or the console.
		return nil, fmt.Errorf("dispatching request: %w", redactURLError(err))
	}

	if resp.Body != nil {
		resp.Body = markedResponseBody{ReadCloser: resp.Body}
	}

	return resp, nil
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

	message = sanitizeGraphErrorText(message)
	raw := sanitizeGraphErrorText(string(rawBody))

	graphErr := &GraphError{
		StatusCode: statusCode,
		RequestID:  requestID,
		Code:       code,
		InnerCodes: innerCodes,
		Message:    message,
		RawBody:    raw,
		Err:        classifyStatus(statusCode),
		RetryAfter: retryAfter,
	}
	if graphErr.HasCode(graphCodeMalwareDetected) {
		// A 403 here is not a permission answer: the content is blocked.
		graphErr.Err = ErrMalwareDetected
	}

	return graphErr
}
