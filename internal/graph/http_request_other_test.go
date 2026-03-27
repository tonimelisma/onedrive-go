//go:build !darwin

package graph

import "net/http"

func doLocalTestRequest(client *http.Client, req *http.Request) (*http.Response, error) {
	return client.Do(req) //nolint:wrapcheck // Test helper preserves the original HTTP client error for assertions.
}
