package graph

import (
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
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

// net/http recovers a callback handler's panic per connection and reports it
// through the server's ErrorLog, the login's logger, so it reaches the log
// file; with the standard log package's default it would not. Shutdown
// returns only once the server has stopped serving, so no serving goroutine
// outlives the login.
//
// Validates: R-6.3.10, R-6.3.4
func TestCallbackServer_HandlerPanicIsLoggedAndShutdownWaitsForServe(t *testing.T) {
	t.Parallel()

	capture, logger := synctest.NewLogCapture()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) {
		panic("callback handler exploded")
	})
	server, port, err := startCallbackServer(t.Context(), mux, make(chan callbackResult, 1), logger)
	require.NoError(t, err)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/boom", port), http.NoBody)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		require.NoError(t, response.Body.Close())
	}
	require.Error(t, err, "net/http drops the connection of a handler that panicked")
	require.Eventually(t, func() bool { return capture.Contains("callback handler exploded") },
		5*time.Second, time.Millisecond, "the handler's panic reaches the login's logger")

	server.shutdown(t.Context(), slog.New(slog.DiscardHandler))
	select {
	case <-server.done:
	default:
		require.Fail(t, "shutdown returned before the server stopped serving")
	}
}
