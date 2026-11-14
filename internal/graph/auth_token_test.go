package graph

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	stdsync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

const (
	// refreshedSignInJSON is the token endpoint's answer to a refresh: a new
	// access token and a rotated refresh token.
	refreshedSignInJSON = `{
		"access_token": "refreshed-access-token",
		"token_type": "Bearer",
		"refresh_token": "rotated-refresh-token",
		"expires_in": 3600
	}`
	refreshedAccessToken = "refreshed-access-token"
	rotatedRefreshToken  = "rotated-refresh-token"

	// callerDeadline is the deadline of a caller that will not wait for a
	// refresh the token endpoint holds.
	callerDeadline = 50 * time.Millisecond

	// heldSignInGuard fails a test whose awaited event never happens.
	heldSignInGuard = 30 * time.Second

	// concurrentTokenCallers is how many callers ask for a token at once.
	concurrentTokenCallers = 8
)

// errTestRequestAbandoned is a cancellation cause a test gives a caller's
// context, as the transfer stall watchdog gives a stalled transfer's.
var errTestRequestAbandoned = errors.New("test: request abandoned")

// heldSignIn is a token endpoint that holds every refresh until the test
// releases it and then answers it with refreshedSignInJSON. A held refresh
// whose client gives up first closes ended.
type heldSignIn struct {
	srv      *httptest.Server
	arrived  chan struct{} // closed when the first refresh arrives
	ended    chan struct{} // closed when the client gives up on the first refresh
	release  chan struct{}
	released stdsync.Once
	requests atomic.Int32
}

func newHeldSignIn(t *testing.T) *heldSignIn {
	t.Helper()

	held := &heldSignIn{
		arrived: make(chan struct{}),
		ended:   make(chan struct{}),
		release: make(chan struct{}),
	}
	held.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Reading the form first lets the server notice the client hanging
		// up: r.Context() ends with the connection only once the body is read.
		r.Body = http.MaxBytesReader(w, r.Body, signInFormLimit)
		assert.NoError(t, r.ParseForm())

		first := held.requests.Add(1) == 1
		if first {
			close(held.arrived)
		}

		select {
		case <-held.release:
			writeSignInJSON(t, w, refreshedSignInJSON)
		case <-r.Context().Done():
			if first {
				close(held.ended)
			}
		}
	}))
	// Cleanups run last-in first-out: the held requests are released before
	// Close waits for them.
	t.Cleanup(held.srv.Close)
	t.Cleanup(held.answer)

	return held
}

// answer lets every held refresh, and every later one, answer.
func (h *heldSignIn) answer() {
	h.released.Do(func() { close(h.release) })
}

// tokenSourceVia builds the source for a saved sign-in whose access token has
// expired, refreshing through held with a sign-in client that gives up after
// signInTimeout. It returns the source and the sign-in file.
func tokenSourceVia(t *testing.T, held *heldSignIn, signInTimeout time.Duration) (TokenSource, string) {
	t.Helper()

	path := writeExpiredSignIn(t)
	authHTTP := &http.Client{Timeout: signInTimeout, Transport: newSignInStandIn(t, held.srv)}
	ts, err := TokenSourceFromPath(path, authHTTP, slog.New(slog.DiscardHandler))
	require.NoError(t, err)

	return ts, path
}

// waitFor fails the test if ch does not close within heldSignInGuard.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-ch:
	case <-time.After(heldSignInGuard):
		require.FailNow(t, "timed out waiting", what)
	}
}

// isClosed reports whether ch has been closed.
func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// savedRefreshTokenAt is the refresh token the sign-in file at path holds.
func savedRefreshTokenAt(t *testing.T, path string) string {
	t.Helper()

	saved, err := tokenfile.Load(path)
	require.NoError(t, err)

	return saved.RefreshToken
}

// Validates: R-6.8.27
//
// A caller waits for a refresh no longer than its own context allows: with
// the token endpoint holding the refresh, Token returns at the caller's
// deadline, not at the sign-in client's 60 s, and the refresh it leaves
// behind is still open for whoever asks next.
func TestTokenSource_ARefreshEndsWithTheCallersContext(t *testing.T) {
	held := newHeldSignIn(t)
	ts, _ := tokenSourceVia(t, held, testSignInTimeout)

	ctx, cancel := context.WithTimeout(t.Context(), callerDeadline)
	defer cancel()

	started := time.Now()
	_, err := ts.Token(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(started), testSignInTimeout/2, "the caller's deadline ended the wait, not the sign-in client's")
	assert.False(t, isClosed(held.ended), "the caller leaving did not cancel the refresh")
	assert.Equal(t, int32(1), held.requests.Load())

	held.answer()
	got, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, got)
}

// Validates: R-6.8.27
//
// One refresh at a time, and a caller that stops waiting does not take it
// from the others: the next caller gets the token the same refresh produced,
// the endpoint is asked once, and the rotated refresh token is saved.
func TestTokenSource_ARefreshOneCallerAbandonedStillServesTheNext(t *testing.T) {
	held := newHeldSignIn(t)
	ts, path := tokenSourceVia(t, held, testSignInTimeout)

	abandoned, abandon := context.WithCancelCause(t.Context())
	firstErr := make(chan error, 1)
	go func() {
		_, err := ts.Token(abandoned)
		firstErr <- err
	}()

	waitFor(t, held.arrived, "the first caller's refresh to reach the token endpoint")
	abandon(errTestRequestAbandoned)
	err := <-firstErr
	require.ErrorIs(t, err, errTestRequestAbandoned, "the caller's cancellation cause is kept")
	assert.Equal(t, savedRefreshToken, savedRefreshTokenAt(t, path), "nothing is saved before the refresh finishes")

	held.answer()
	got, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, got)
	assert.Equal(t, int32(1), held.requests.Load(), "the next caller shares the refresh the first one started")
	assert.Equal(t, rotatedRefreshToken, savedRefreshTokenAt(t, path))

	again, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, again)
	assert.Equal(t, int32(1), held.requests.Load(), "a valid token needs no refresh")
}

// Validates: R-6.8.27
//
// A refresh nobody waits for any more still ends, by the sign-in client's own
// timeout, and a refresh that did not finish writes no token file.
func TestTokenSource_AnAbandonedRefreshWritesNoTokenFile(t *testing.T) {
	held := newHeldSignIn(t)
	ts, path := tokenSourceVia(t, held, stalledSignInTimeout)
	before, err := tokenfile.Load(path)
	require.NoError(t, err)

	abandoned, abandon := context.WithCancel(t.Context())
	firstErr := make(chan error, 1)
	go func() {
		_, tokenErr := ts.Token(abandoned)
		firstErr <- tokenErr
	}()

	waitFor(t, held.arrived, "the refresh to reach the token endpoint")
	abandon()
	require.ErrorIs(t, <-firstErr, context.Canceled)

	waitFor(t, held.ended, "the sign-in client to give up on the refresh nobody waits for")

	// The next caller either finds the timed-out refresh still closing or
	// starts its own, which the endpoint holds until the client gives up as
	// well; either way it gets an error, and when it returns no refresh is
	// left running.
	_, err = ts.Token(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded, "the sign-in client's timeout, not a token")

	after, err := tokenfile.Load(path)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a refresh that did not finish writes nothing")
}

// Validates: R-6.8.27
//
// Callers that need a token at the same time share one refresh.
func TestTokenSource_ConcurrentCallersShareOneRefresh(t *testing.T) {
	held := newHeldSignIn(t)
	ts, _ := tokenSourceVia(t, held, testSignInTimeout)

	tokens := make([]string, concurrentTokenCallers)
	errs := make([]error, concurrentTokenCallers)

	var wg stdsync.WaitGroup
	for i := range concurrentTokenCallers {
		wg.Go(func() {
			tokens[i], errs[i] = ts.Token(t.Context())
		})
	}

	waitFor(t, held.arrived, "the refresh to reach the token endpoint")
	held.answer()
	wg.Wait()

	for i := range concurrentTokenCallers {
		require.NoError(t, errs[i], "caller %d", i)
		assert.Equal(t, refreshedAccessToken, tokens[i], "caller %d", i)
	}
	assert.Equal(t, int32(1), held.requests.Load(), "one refresh for every caller")
}

// panickingSignIn is a sign-in transport with a bug.
type panickingSignIn struct{}

func (panickingSignIn) RoundTrip(*http.Request) (*http.Response, error) {
	panic("sign-in transport bug")
}

// Validates: R-6.8.27, R-6.3.10
//
// The refresh runs on a goroutine of the token source's own; a panic there is
// recovered through panicguard, logged once with its stack, and fails the
// callers waiting for it instead of the process. It holds no later caller:
// the next one refreshes again (and meets the same bug).
func TestTokenSource_ARefreshThatPanicsFailsItsCallers(t *testing.T) {
	var logBuf bytes.Buffer
	path := writeExpiredSignIn(t)
	ts, err := TokenSourceFromPath(path, &http.Client{Timeout: testSignInTimeout, Transport: panickingSignIn{}},
		slog.New(slog.NewTextHandler(&logBuf, nil)))
	require.NoError(t, err)

	_, err = ts.Token(t.Context())
	require.ErrorIs(t, err, panicguard.ErrPanic)
	assert.Contains(t, logBuf.String(), "Internal error while refreshing the sign-in")
	assert.Equal(t, savedRefreshToken, savedRefreshTokenAt(t, path))

	_, err = ts.Token(t.Context())
	require.ErrorIs(t, err, panicguard.ErrPanic, "a later caller is not held by the refresh that panicked")
}

// Validates: R-6.8.27
//
// A request whose token refresh its context cut short never left the client:
// it carries errRequestNotSent, keeps the context's cause, and OneDrive saw
// nothing.
func TestDo_ATokenRefreshCutByTheContextIsNotSent(t *testing.T) {
	var graphRequests atomic.Int32
	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		graphRequests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(graphSrv.Close)

	held := newHeldSignIn(t)
	ts, _ := tokenSourceVia(t, held, testSignInTimeout)
	client := MustNewClient(graphSrv.URL, http.DefaultClient, ts, slog.New(slog.DiscardHandler), "test-agent")

	ctx, abandon := context.WithCancelCause(t.Context())
	doErr := make(chan error, 1)
	go func() {
		resp, err := client.do(ctx, http.MethodGet, "/me", nil)
		if resp != nil {
			assert.NoError(t, resp.Body.Close())
		}
		doErr <- err
	}()

	waitFor(t, held.arrived, "the request's token refresh to reach the token endpoint")
	abandon(errTestRequestAbandoned)
	err := <-doErr

	require.ErrorIs(t, err, errRequestNotSent)
	require.ErrorIs(t, err, errTestRequestAbandoned, "the cause decides how the caller classifies it")
	assert.Zero(t, graphRequests.Load())
}
