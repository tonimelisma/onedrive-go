package perf

import (
	"net/http"
	"time"
)

type RoundTripper struct {
	Inner http.RoundTripper
}

func (rt RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	inner := rt.Inner
	if inner == nil {
		inner = http.DefaultTransport
	}

	startedAt := time.Now()
	resp, err := inner.RoundTrip(req)

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
