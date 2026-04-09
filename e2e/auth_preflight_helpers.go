// Package e2e contains live-suite helpers plus small pure helpers that can be
// unit tested without invoking the tagged live harness.
package e2e

import (
	"net/http"
	"strings"
)

type authPreflightAttempt struct {
	StatusCode int
	Code       string
	RequestID  string
	Err        string
}

func (attempt authPreflightAttempt) succeeded() bool {
	return attempt.StatusCode == http.StatusOK && attempt.Err == ""
}

func shouldRetryAuthPreflight(path string, attempt authPreflightAttempt) bool {
	if attempt.succeeded() {
		return false
	}

	switch path {
	case "/me/drives":
		// Strict preflight intentionally keeps polling drive discovery because
		// Graph can lag behind `/me` even when the saved login is valid.
		return true
	case "/me":
		return isTransientAuthPreflightFailure(attempt)
	default:
		return isTransientAuthPreflightFailure(attempt)
	}
}

func isTransientAuthPreflightFailure(attempt authPreflightAttempt) bool {
	switch attempt.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}

	if attempt.StatusCode != 0 {
		return false
	}

	return strings.HasPrefix(attempt.Err, "dispatch request:") ||
		strings.HasPrefix(attempt.Err, "read response body:")
}
