package graph

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.3.4
//
// The callback handler is registered on a mux, so nothing stops it running
// more than once: a browser refresh, a duplicate request, or the user
// reopening the redirect URL all call it again. The result channel holds one
// value, and the waiter has already returned by the time a second call
// arrives, so a blocking send would park that handler goroutine for the life
// of the process and keep the callback server from shutting down.
func TestHandleOAuthCallback_RepeatedCallbackDoesNotBlock(t *testing.T) {
	t.Parallel()

	const state = "state-token"

	resultCh := make(chan callbackResult, 1)

	call := func() {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, callbackPath+"?state="+state+"&code=auth-code", http.NoBody)
		handleOAuthCallback(httptest.NewRecorder(), req, state, resultCh)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		call()
		call()
		call()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "a repeated OAuth callback must not block the handler goroutine")
	}

	result := <-resultCh
	require.NoError(t, result.err)
	assert.Equal(t, "auth-code", result.code, "the first outcome is the one that is reported")
	assert.Empty(t, resultCh, "later callbacks are dropped rather than queued")
}

// Validates: R-6.3.4
//
// The same applies to the failure paths, which report through the same
// one-slot channel.
func TestHandleOAuthCallback_RepeatedStateMismatchDoesNotBlock(t *testing.T) {
	t.Parallel()

	resultCh := make(chan callbackResult, 1)

	call := func() {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, callbackPath+"?state=wrong", http.NoBody)
		handleOAuthCallback(httptest.NewRecorder(), req, "expected", resultCh)
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		call()
		call()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.Fail(t, "a repeated failed callback must not block the handler goroutine")
	}

	result := <-resultCh
	require.Error(t, result.err)
	assert.Contains(t, result.err.Error(), "state mismatch")
}
