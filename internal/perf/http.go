package perf

import (
	"fmt"
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
	if err != nil {
		return resp, fmt.Errorf("round trip: %w", err)
	}

	return resp, nil
}
