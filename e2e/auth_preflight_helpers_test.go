package e2e

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestClassifyAuthPreflightAttempt_MeRetriesTransientGatewayFailures(t *testing.T) {
	t.Parallel()

	decision := classifyAuthPreflightAttempt("/me", authPreflightAttempt{
		StatusCode: http.StatusGatewayTimeout,
		Code:       "GatewayTimeout",
		Err:        `{"error":{"code":"GatewayTimeout","message":"ProfileException"}}`,
	})
	assert.True(t, decision.Retry)
	assert.Equal(t, "transient_gateway_status", decision.Reason)

	decision = classifyAuthPreflightAttempt("/me", authPreflightAttempt{
		Err: "dispatch request: Get \"https://graph.microsoft.com/v1.0/me\": context deadline exceeded",
	})
	assert.True(t, decision.Retry)
	assert.Equal(t, "transient_transport_error", decision.Reason)

	decision = classifyAuthPreflightAttempt("/me", authPreflightAttempt{
		StatusCode: http.StatusGatewayTimeout,
		Err:        "read response body: unexpected EOF",
	})
	assert.True(t, decision.Retry)
	assert.Equal(t, "transient_response_read_error", decision.Reason)
}

func TestClassifyAuthPreflightAttempt_MeRejectsDurableFailures(t *testing.T) {
	t.Parallel()

	decision := classifyAuthPreflightAttempt("/me", authPreflightAttempt{
		StatusCode: http.StatusUnauthorized,
		Code:       "InvalidAuthenticationToken",
		Err:        `{"error":{"code":"InvalidAuthenticationToken"}}`,
	})
	assert.False(t, decision.Retry)
	assert.Equal(t, "durable_failure", decision.Reason)

	decision = classifyAuthPreflightAttempt("/me", authPreflightAttempt{
		StatusCode: http.StatusForbidden,
		Code:       "accessDenied",
		Err:        `{"error":{"code":"accessDenied"}}`,
	})
	assert.False(t, decision.Retry)
	assert.Equal(t, "durable_failure", decision.Reason)
}

func TestClassifyAuthPreflightAttempt_MeDrivesKeepsPolling(t *testing.T) {
	t.Parallel()

	decision := classifyAuthPreflightAttempt("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusForbidden,
		Code:       "accessDenied",
		Err:        `{"error":{"code":"accessDenied"}}`,
	})
	assert.True(t, decision.Retry)
	assert.Equal(t, "drive_catalog_projection_lag", decision.Reason)

	decision = classifyAuthPreflightAttempt("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusGatewayTimeout,
		Code:       "GatewayTimeout",
		Err:        `{"error":{"code":"GatewayTimeout"}}`,
	})
	assert.True(t, decision.Retry)
	assert.Equal(t, "drive_catalog_projection_lag", decision.Reason)

	decision = classifyAuthPreflightAttempt("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusOK,
	})
	assert.False(t, decision.Retry)
	assert.Equal(t, "success", decision.Reason)
}

func TestFormatAuthPreflightFailure_IncludesRetryDecisionReason(t *testing.T) {
	t.Parallel()

	message := formatAuthPreflightFailure("personal:user@example.com", "/me/drives", 3*time.Second, []authPreflightAttempt{
		{
			StatusCode: http.StatusForbidden,
			Code:       "accessDenied",
			RequestID:  "req-1",
			Err:        `{"error":{"code":"accessDenied"}}`,
		},
	})

	assert.Contains(t, message, `retry=true`)
	assert.Contains(t, message, `reason="drive_catalog_projection_lag"`)
	assert.Contains(t, message, `request_id="req-1"`)
}
