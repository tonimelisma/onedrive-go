// Package e2e contains live-suite helpers plus small pure helpers that can be
// unit tested without invoking the tagged live harness.
package e2e

import (
	"fmt"
	"net/http"
	"strings"
	"time"
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

type authPreflightRetryDecision struct {
	Retry  bool
	Reason string
}

func classifyAuthPreflightAttempt(path string, attempt authPreflightAttempt) authPreflightRetryDecision {
	if attempt.succeeded() {
		return authPreflightRetryDecision{
			Retry:  false,
			Reason: "success",
		}
	}

	switch path {
	case "/me/drives":
		// Strict preflight intentionally keeps polling drive discovery because
		// Graph can lag behind `/me` even when the saved login is valid.
		return authPreflightRetryDecision{
			Retry:  true,
			Reason: "drive_catalog_projection_lag",
		}
	case "/me":
		return classifyTransientAuthPreflightFailure(attempt)
	default:
		return classifyTransientAuthPreflightFailure(attempt)
	}
}

func classifyTransientAuthPreflightFailure(attempt authPreflightAttempt) authPreflightRetryDecision {
	if strings.HasPrefix(attempt.Err, "dispatch request:") {
		return authPreflightRetryDecision{
			Retry:  true,
			Reason: "transient_transport_error",
		}
	}
	if strings.HasPrefix(attempt.Err, "read response body:") {
		return authPreflightRetryDecision{
			Retry:  true,
			Reason: "transient_response_read_error",
		}
	}

	switch attempt.StatusCode {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return authPreflightRetryDecision{
			Retry:  true,
			Reason: "transient_gateway_status",
		}
	}

	return authPreflightRetryDecision{
		Retry:  false,
		Reason: "durable_failure",
	}
}

func formatAuthPreflightFailure(
	driveID string,
	endpoint string,
	elapsed time.Duration,
	attempts []authPreflightAttempt,
) string {
	var builder strings.Builder
	_, _ = fmt.Fprintf(&builder, "auth preflight failed for %s endpoint=%s failed_calls=%d elapsed=%s",
		driveID, endpoint, len(attempts), elapsed.Round(time.Millisecond))

	for i, attempt := range attempts {
		decision := classifyAuthPreflightAttempt(endpoint, attempt)
		_, _ = fmt.Fprintf(&builder, "\n  attempt=%d status=%d code=%q request_id=%q retry=%t reason=%q detail=%q",
			i+1,
			attempt.StatusCode,
			attempt.Code,
			attempt.RequestID,
			decision.Retry,
			decision.Reason,
			attempt.Err,
		)
	}

	return builder.String()
}
