//go:build darwin

package graph

import "net/http"

func doLocalTestRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	//nolint:gosec // Test request targets a local httptest or loopback callback URL under test control.
	return client.Do(req) //nolint:wrapcheck // Test helper preserves the original HTTP client error for assertions.
}
