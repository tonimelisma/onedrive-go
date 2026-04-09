package e2e

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestShouldRetryAuthPreflight_MeRetriesTransientGatewayFailures(t *testing.T) {
	t.Parallel()

	assert.True(t, shouldRetryAuthPreflight("/me", authPreflightAttempt{
		StatusCode: http.StatusGatewayTimeout,
		Code:       "GatewayTimeout",
		Err:        `{"error":{"code":"GatewayTimeout","message":"ProfileException"}}`,
	}))
	assert.True(t, shouldRetryAuthPreflight("/me", authPreflightAttempt{
		Err: "dispatch request: Get \"https://graph.microsoft.com/v1.0/me\": context deadline exceeded",
	}))
}

func TestShouldRetryAuthPreflight_MeRejectsDurableFailures(t *testing.T) {
	t.Parallel()

	assert.False(t, shouldRetryAuthPreflight("/me", authPreflightAttempt{
		StatusCode: http.StatusUnauthorized,
		Code:       "InvalidAuthenticationToken",
		Err:        `{"error":{"code":"InvalidAuthenticationToken"}}`,
	}))
	assert.False(t, shouldRetryAuthPreflight("/me", authPreflightAttempt{
		StatusCode: http.StatusForbidden,
		Code:       "accessDenied",
		Err:        `{"error":{"code":"accessDenied"}}`,
	}))
}

func TestShouldRetryAuthPreflight_MeDrivesKeepsPolling(t *testing.T) {
	t.Parallel()

	assert.True(t, shouldRetryAuthPreflight("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusForbidden,
		Code:       "accessDenied",
		Err:        `{"error":{"code":"accessDenied"}}`,
	}))
	assert.True(t, shouldRetryAuthPreflight("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusGatewayTimeout,
		Code:       "GatewayTimeout",
		Err:        `{"error":{"code":"GatewayTimeout"}}`,
	}))
	assert.False(t, shouldRetryAuthPreflight("/me/drives", authPreflightAttempt{
		StatusCode: http.StatusOK,
	}))
}
