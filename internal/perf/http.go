package perf

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RoundTripper records each request it sends on the collector the request's
// context carries, then hands back what Inner answered. Inner is required:
// the transport it names carries the client's timeouts and connection
// settings, so there is no default to send through in its place.
type RoundTripper struct {
	Inner http.RoundTripper
}

func (rt RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.Inner == nil {
		err := errors.New("the performance transport has no inner transport to send the request through")
		// The RoundTripper contract: the body is closed whatever the answer.
		if req.Body != nil {
			if closeErr := req.Body.Close(); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("closing the request body: %w", closeErr))
			}
		}

		return nil, err
	}

	startedAt := time.Now()
	resp, err := rt.Inner.RoundTrip(req)

	statusCode := 0
	if resp != nil {
		statusCode = resp.StatusCode
	}
	if collector := FromContext(req.Context()); collector != nil {
		collector.RecordHTTPRequest(statusCode, time.Since(startedAt), err)
	}
	// The error is returned as the transport produced it. net/http wraps it
	// in a *url.Error that already names the request, so a "round trip:"
	// prefix only put a code-path word into every network failure message.
	// Callers that classify transport errors use errors.Is/As, which see
	// the same chain either way.
	//nolint:wrapcheck // See above: this transport is a transparent decorator.
	return resp, err
}
