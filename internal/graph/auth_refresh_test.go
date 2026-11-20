package graph

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/graphtransport"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// Token lifetimes the tests' endpoint issues, in seconds. Microsoft varies
// access-token lifetimes between about 60 and 90 minutes; expiredAtOnce is
// inside oauth2's 10 s early-expiry margin, so the token needs a refresh at
// its next use.
const (
	refreshedLifetimeSeconds = 3600
	longLifetimeSeconds      = 5400
	expiredAtOnceSeconds     = 1
)

// rotatingSignIn is a token endpoint that rotates refresh tokens as the
// identity platform does and refuses any refresh token it has already
// replaced. It counts redemptions per refresh token.
type rotatingSignIn struct {
	srv *httptest.Server

	mu       stdsync.Mutex
	current  string
	issued   int
	redeemed map[string]int
	// expiresIn is the lifetime in seconds of the next token it issues.
	expiresIn int
	// beforeRefusal, when set, runs before a replaced refresh token is
	// refused: the test plays a writer that did not take the lock.
	beforeRefusal func()
}

func newRotatingSignIn(t *testing.T, current string) *rotatingSignIn {
	t.Helper()

	r := &rotatingSignIn{current: current, redeemed: make(map[string]int), expiresIn: refreshedLifetimeSeconds}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		req.Body = http.MaxBytesReader(w, req.Body, signInFormLimit)
		assert.NoError(t, req.ParseForm())
		presented := req.PostForm.Get("refresh_token")

		r.mu.Lock()
		r.redeemed[presented]++
		if presented != r.current {
			refuse := r.beforeRefusal
			r.mu.Unlock()
			if refuse != nil {
				refuse()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			writeTestResponse(t, w, `{"error":"invalid_grant","error_description":"refresh token replaced"}`)

			return
		}
		r.issued++
		r.current = fmt.Sprintf("made-up-rotated-%d", r.issued)
		body := fmt.Sprintf(`{"access_token":"made-up-access-%d","token_type":"Bearer","refresh_token":%q,"expires_in":%d}`,
			r.issued, r.current, r.expiresIn)
		r.mu.Unlock()
		writeSignInJSON(t, w, body)
	}))
	t.Cleanup(r.srv.Close)

	return r
}

// setExpiresIn sets the lifetime in seconds of the tokens it issues next.
func (r *rotatingSignIn) setExpiresIn(seconds int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.expiresIn = seconds
}

// live is the refresh token the service accepts now.
func (r *rotatingSignIn) live() string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.current
}

func (r *rotatingSignIn) counts() (issued int, redeemed map[string]int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	copied := make(map[string]int, len(r.redeemed))
	for token, n := range r.redeemed {
		copied[token] = n
	}

	return r.issued, copied
}

// sourceOn builds a source over the sign-in file at path, as a separate
// process would: its own token in memory, its own lock descriptor.
func sourceOn(t *testing.T, path string, endpoint *httptest.Server) *tokenBridge {
	t.Helper()

	authHTTP := &http.Client{Timeout: testSignInTimeout, Transport: newSignInStandIn(t, endpoint)}
	ts, err := TokenSourceFromPath(path, authHTTP, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	bridge, ok := ts.(*tokenBridge)
	require.True(t, ok)

	return bridge
}

// Validates: R-6.8.30
//
// Two processes on one sign-in file, the access token expired in both. The
// first refreshes and the sign-in service replaces the refresh token. The
// second, still holding the replaced one in memory, used to redeem it, be
// refused and report "sign-in required"; it now reads the file under the
// lock and uses the first's sign-in, with no request of its own.
func TestRefresh_TwoSourcesOneFile_SecondAdoptsFirstsToken(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	first, second := sourceOn(t, path, endpoint.srv), sourceOn(t, path, endpoint.srv)

	firstAccess, err := first.Token(t.Context())
	require.NoError(t, err)
	secondAccess, err := second.Token(t.Context())
	require.NoError(t, err, "the second source is not signed out")

	assert.Equal(t, firstAccess, secondAccess, "the second uses the first's sign-in")
	issued, redeemed := endpoint.counts()
	assert.Equal(t, 1, issued)
	assert.Equal(t, map[string]int{savedRefreshToken: 1}, redeemed, "one redemption, never of a replaced token")
}

// Validates: R-6.8.30
//
// A sign-in another process wrote, whose access token has expired too, is
// refreshed with its refresh token, not with the one this process holds.
func TestRefresh_RotatedTokenOnDiskIsUsedInsteadOfStaleMemory(t *testing.T) {
	endpoint := newRotatingSignIn(t, "made-up-written-by-another")
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	require.NoError(t, tokenfile.Save(path, &oauth2.Token{
		AccessToken: "made-up-other-access", TokenType: "Bearer",
		RefreshToken: "made-up-written-by-another", Expiry: time.Now().Add(-time.Minute),
	}))

	_, err := ts.Token(t.Context())
	require.NoError(t, err)
	_, redeemed := endpoint.counts()
	assert.Equal(t, map[string]int{"made-up-written-by-another": 1}, redeemed)
	assert.Equal(t, "made-up-rotated-1", savedRefreshTokenAt(t, path))
}

// Validates: R-6.8.30
//
// A refresh token the service refuses because a writer that took no lock
// replaced it (or the filesystem cannot lock) is checked against the file
// once more, and the file's refresh token is redeemed, once.
func TestRefresh_InvalidGrantRereadsAndRetriesOnce(t *testing.T) {
	endpoint := newRotatingSignIn(t, "made-up-lockless-write")
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	endpoint.beforeRefusal = func() {
		assert.NoError(t, tokenfile.Save(path, &oauth2.Token{
			AccessToken: "made-up-lockless-access", TokenType: "Bearer",
			RefreshToken: "made-up-lockless-write", Expiry: time.Now().Add(-time.Minute),
		}))
	}

	_, err := ts.Token(t.Context())
	require.NoError(t, err)
	_, redeemed := endpoint.counts()
	assert.Equal(t, 1, redeemed["made-up-lockless-write"], "the file's refresh token is redeemed once")
	// oauth2 itself may send a refused request twice (its auth-style
	// probe); that is one refusal of the refresh.
	oneRefusal := redeemed[savedRefreshToken]
	require.Positive(t, oneRefusal)

	// With the file unchanged, a refusal is final: sign-in required, with
	// no second try of the same token.
	endpoint.beforeRefusal = nil
	stale := sourceOn(t, writeExpiredSignIn(t), endpoint.srv)
	_, err = stale.Token(t.Context())
	require.ErrorIs(t, err, ErrLoginExpired)
	_, redeemed = endpoint.counts()
	assert.Equal(t, 2*oneRefusal, redeemed[savedRefreshToken], "one more refusal, not two")
}

// Validates: R-6.8.30
//
// A sign-in file that is gone was signed out: the refresh is "sign-in
// required", asks nothing of the sign-in service, and nothing recreates the
// file, a retried save included.
func TestRefresh_MissingFileIsSignInRequiredAndFileStaysAbsent(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	ts.saver.mu.Lock()
	ts.saver.pending = &oauth2.Token{AccessToken: "made-up-pending", RefreshToken: "made-up-pending", Expiry: time.Now().Add(time.Hour)}
	ts.saver.err = errTestDiskRefused
	ts.saver.mu.Unlock()
	ts.saver.failing.Store(true)

	require.NoError(t, Logout(t.Context(), path, slog.New(slog.DiscardHandler)))

	_, err := ts.Token(t.Context())
	require.ErrorIs(t, err, ErrNotLoggedIn)
	issued, redeemed := endpoint.counts()
	assert.Zero(t, issued)
	assert.Empty(t, redeemed)
	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, fs.ErrNotExist)
	require.NoError(t, ts.SaveError(), "a signed-out sign-in has nothing left to save")
}

// Validates: R-6.8.30, R-6.8.14
//
// After a 401, a sign-in in the file whose access token is not the rejected
// one is used without a request.
func TestForceRefresh_AdoptsNewerTokenFromDisk(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	rejected := &oauth2.Token{
		AccessToken: "made-up-rejected", TokenType: "Bearer",
		RefreshToken: savedRefreshToken, Expiry: time.Now().Add(time.Hour),
	}
	path := savedSignInPath(t, rejected)
	ts := sourceOn(t, path, endpoint.srv)
	newerAccess, newerRefresh := "made-up-newer-access", "made-up-newer-refresh"
	require.NoError(t, tokenfile.Save(path, &oauth2.Token{
		AccessToken: newerAccess, TokenType: "Bearer",
		RefreshToken: newerRefresh, Expiry: time.Now().Add(time.Hour),
	}))

	got, err := ts.ForceRefresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "made-up-newer-access", got)
	issued, _ := endpoint.counts()
	assert.Zero(t, issued)
}

// Validates: R-6.8.30
//
// While another process holds the sign-in lock, a refresh waits no longer
// than its bound and fails saying another process is refreshing, never
// "sign-in required"; the bound outlasts one sign-in request.
func TestRefresh_LockWaitIsBounded(t *testing.T) {
	assert.Greater(t, signInLockWait, graphtransport.AuthClient().Timeout,
		"a holder stuck in its request releases before a waiter gives up")

	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	release, err := tokenfile.AcquireLock(t.Context(), path)
	require.NoError(t, err)
	defer func() { assert.NoError(t, release()) }()

	_, err = ts.refreshShared(t.Context(), time.Millisecond, ts.tok, false)
	require.ErrorIs(t, err, tokenfile.ErrLockWaitEnded)
	require.NotErrorIs(t, err, ErrNotLoggedIn)
	issued, _ := endpoint.counts()
	assert.Zero(t, issued)
}

// Validates: R-6.8.30
//
// The lock wait of a refresh is signInLockWait, or what is left of the
// starting caller's deadline when that is shorter.
func TestRefresh_LockWaitEndsWithTheCallersDeadline(t *testing.T) {
	ts := sourceOn(t, writeExpiredSignIn(t), newRotatingSignIn(t, savedRefreshToken).srv)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	ts.now = func() time.Time { return now }

	assert.Equal(t, signInLockWait, ts.lockWaitFor(t.Context()))
	short, cancel := context.WithDeadline(t.Context(), now.Add(5*time.Second))
	defer cancel()
	assert.Equal(t, 5*time.Second, ts.lockWaitFor(short))
	long, cancelLong := context.WithDeadline(t.Context(), now.Add(time.Hour))
	defer cancelLong()
	assert.Equal(t, signInLockWait, ts.lockWaitFor(long))
}

// failingUntilCleared is a sign-in file write that refuses while refusing is
// set and otherwise writes the file.
type failingUntilCleared struct {
	mu       stdsync.Mutex
	refusing bool
}

func (f *failingUntilCleared) write(path string, tok *oauth2.Token) error {
	f.mu.Lock()
	refusing := f.refusing
	f.mu.Unlock()
	if refusing {
		return errTestDiskRefused
	}

	return tokenfile.Save(path, tok) //nolint:wrapcheck // stands in for tokenfile.Save itself
}

func (f *failingUntilCleared) set(refusing bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refusing = refusing
}

// Validates: R-6.8.26, R-6.8.30
//
// A forced refresh whose token expires before the one the file holds (a
// shorter lifetime, soon after a refresh) is still the successor and is
// written: the file keeps the refresh token the service accepts.
func TestForceRefresh_ShorterLifetimeIsStillSaved(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)

	endpoint.setExpiresIn(longLifetimeSeconds)
	_, err := ts.Token(t.Context())
	require.NoError(t, err)
	require.Equal(t, endpoint.live(), savedRefreshTokenAt(t, path))

	endpoint.setExpiresIn(refreshedLifetimeSeconds)
	_, err = ts.ForceRefresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, endpoint.live(), savedRefreshTokenAt(t, path), "the file holds the live refresh token")
}

// Validates: R-6.8.26, R-6.8.30
//
// While every save fails, the next refresh redeems the refresh token in
// memory, not the replaced one the file still holds, so the process stays
// signed in.
func TestRefresh_PersistentSaveFailureStaysSignedIn(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	writes := &failingUntilCleared{refusing: true}
	ts.saver.write = writes.write

	endpoint.setExpiresIn(expiredAtOnceSeconds)
	_, err := ts.Token(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, TokenSaveError(ts), ErrTokenNotSaved)

	_, err = ts.Token(t.Context())
	require.NoError(t, err, "still signed in with the token in memory")
	require.ErrorIs(t, TokenSaveError(ts), ErrTokenNotSaved)

	writes.set(false)
	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	require.NoError(t, TokenSaveError(ts))
	assert.Equal(t, endpoint.live(), savedRefreshTokenAt(t, path), "the chain is written once the disk takes it")
}

// Validates: R-6.8.26, R-6.8.30
//
// A pending save is not retried over a sign-in another process wrote to the
// file meanwhile (a login, or its refresh): that one is newer, and the
// unsaved chain is dropped.
func TestPersistingTokenSource_RetryDoesNotOverwriteANewerFile(t *testing.T) {
	endpoint := newRotatingSignIn(t, savedRefreshToken)
	path := writeExpiredSignIn(t)
	ts := sourceOn(t, path, endpoint.srv)
	writes := &failingUntilCleared{refusing: true}
	ts.saver.write = writes.write

	_, err := ts.Token(t.Context())
	require.NoError(t, err)
	require.ErrorIs(t, TokenSaveError(ts), ErrTokenNotSaved)

	otherRefresh := "made-up-written-by-a-login"
	loginAccess := "made-up-login-access"
	require.NoError(t, tokenfile.Save(path, &oauth2.Token{
		AccessToken: loginAccess, TokenType: "Bearer",
		RefreshToken: otherRefresh, Expiry: time.Now().Add(time.Hour),
	}))
	writes.set(false)
	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, otherRefresh, savedRefreshTokenAt(t, path), "the newer sign-in is kept")
	require.NoError(t, TokenSaveError(ts), "the superseded chain is dropped")
}
