package graph

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// refreshedSignInLog is the Info line of a refreshed sign-in written to its file.
const refreshedSignInLog = "refreshed sign-in"

// errTestDiskRefused is the cause a test's sign-in file write fails with.
var errTestDiskRefused = errors.New("test: disk refused the write")

// recordingWrites stands in for tokenfile.Save: it refuses while refuse is
// set, and records every attempt and every access token it wrote, in order.
type recordingWrites struct {
	mu       stdsync.Mutex
	refuse   bool
	attempts int
	written  []string
}

func (w *recordingWrites) write(_ string, tok *oauth2.Token) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.attempts++
	if w.refuse {
		return errTestDiskRefused
	}
	w.written = append(w.written, tok.AccessToken)

	return nil
}

func (w *recordingWrites) setRefuse(refuse bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refuse = refuse
}

func (w *recordingWrites) snapshot() (attempts int, written []string) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.attempts, append([]string(nil), w.written...)
}

// savedSignInPath writes tok to a sign-in file of the test's own and returns
// its path: a source refreshes only a sign-in its file still holds.
func savedSignInPath(t *testing.T, tok *oauth2.Token) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "sign-in.json")
	require.NoError(t, tokenfile.Save(path, tok))

	return path
}

// refreshingSignIn is a token endpoint that answers every refresh with
// refreshedSignInJSON.
func refreshingSignIn(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeSignInJSON(t, w, refreshedSignInJSON)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// expiredSignInSource is the source over a saved sign-in whose access token
// has expired, refreshing through a local endpoint, with its log in logBuf.
func expiredSignInSource(t *testing.T, logBuf *bytes.Buffer) (*tokenBridge, string) {
	t.Helper()

	path := writeExpiredSignIn(t)
	authHTTP := &http.Client{Timeout: testSignInTimeout, Transport: newSignInStandIn(t, refreshingSignIn(t))}
	ts, err := TokenSourceFromPath(path, authHTTP, slog.New(slog.NewTextHandler(logBuf, nil)))
	require.NoError(t, err)

	bridge, ok := ts.(*tokenBridge)
	require.True(t, ok)

	return bridge, path
}

// Validates: R-6.8.26
//
// A refresh writes its token to the sign-in file before the caller has it,
// and says so once at Info; a token that needs no refresh writes nothing.
func TestPersistingTokenSource_SavesARefreshedToken(t *testing.T) {
	var logBuf bytes.Buffer
	ts, path := expiredSignInSource(t, &logBuf)

	got, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, got)
	assert.Equal(t, rotatedRefreshToken, savedRefreshTokenAt(t, path))
	require.NoError(t, TokenSaveError(ts))

	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(logBuf.String(), "msg=\""+refreshedSignInLog+"\""), "one Info line per saved token")
}

// Validates: R-6.8.26
//
// A refreshed sign-in whose write fails does not fail the request: the token
// is in memory. The failure is reported, naming the file and the cause and
// no token; the write is tried again at the next token use, and a write that
// succeeds clears the failure.
func TestPersistingTokenSource_SaveFailureIsReportedAndRetried(t *testing.T) {
	var logBuf bytes.Buffer
	ts, path := expiredSignInSource(t, &logBuf)
	writes := &recordingWrites{refuse: true}
	realWrite := ts.saver.write
	ts.saver.write = func(p string, tok *oauth2.Token) error {
		if err := writes.write(p, tok); err != nil {
			return err
		}

		return realWrite(p, tok)
	}

	got, err := ts.Token(t.Context())
	require.NoError(t, err, "requests keep working while the file cannot be written")
	assert.Equal(t, refreshedAccessToken, got)

	saveErr := TokenSaveError(ts)
	require.ErrorIs(t, saveErr, ErrTokenNotSaved)
	require.ErrorIs(t, saveErr, errTestDiskRefused)
	assert.Contains(t, saveErr.Error(), path)
	assert.Contains(t, saveErr.Error(), "the next run may ask you to sign in again")
	for _, secret := range []string{refreshedAccessToken, rotatedRefreshToken, savedRefreshToken} {
		assert.NotContains(t, saveErr.Error(), secret, "no token material in the error")
		assert.NotContains(t, logBuf.String(), secret, "no token material in the log")
	}
	assert.Equal(t, savedRefreshToken, savedRefreshTokenAt(t, path), "the file keeps what it held")
	assert.NotContains(t, logBuf.String(), "msg=\""+refreshedSignInLog+"\"", "nothing was saved")

	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	attempts, _ := writes.snapshot()
	assert.Equal(t, 2, attempts, "the next token use tries the write again")
	require.ErrorIs(t, TokenSaveError(ts), ErrTokenNotSaved, "still failing")

	writes.setRefuse(false)
	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	require.NoError(t, TokenSaveError(ts), "a write that succeeds clears the failure")
	assert.Equal(t, rotatedRefreshToken, savedRefreshTokenAt(t, path))
	assert.Equal(t, 1, strings.Count(logBuf.String(), "msg=\""+refreshedSignInLog+"\""))

	_, err = ts.Token(t.Context())
	require.NoError(t, err)
	attempts, _ = writes.snapshot()
	assert.Equal(t, 3, attempts, "no write once the file holds the token")
}

// Validates: R-6.8.26, R-6.8.30
//
// Which sign-in is newer is decided by lineage, not expiry: a token
// redeemed from the refresh token the file holds is written even when it
// expires before the file's (access-token lifetimes vary), and a token whose
// parent the file no longer holds is never written over the file.
func TestPersistingTokenSource_NeverWritesAnOlderToken(t *testing.T) {
	base := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	signIn := func(name string, expiresIn time.Duration) *oauth2.Token {
		return &oauth2.Token{AccessToken: "made-up-access-" + name, RefreshToken: "made-up-refresh-" + name, Expiry: base.Add(expiresIn)}
	}

	onDisk := signIn("on-disk", 90*time.Minute)
	saver := newTokenSaver(filepath.Join(t.TempDir(), "sign-in.json"), onDisk, slog.New(slog.DiscardHandler))
	writes := &recordingWrites{}
	saver.write = writes.write

	successor := signIn("successor", 60*time.Minute)
	saver.save(successor, onDisk.RefreshToken)
	saver.save(signIn("stale-branch", 4*time.Hour), onDisk.RefreshToken)
	_, written := writes.snapshot()
	assert.Equal(t, []string{"made-up-access-successor"}, written,
		"the successor is written although it expires first; a token from a replaced parent never is")

	writes.setRefuse(true)
	next := signIn("next", time.Hour)
	saver.save(next, successor.RefreshToken)
	require.ErrorIs(t, saver.saveError(), ErrTokenNotSaved)
	writes.setRefuse(false)
	saver.save(signIn("after-next", time.Hour), next.RefreshToken)
	require.NoError(t, saver.saveError())
	_, written = writes.snapshot()
	assert.Equal(t, []string{"made-up-access-successor", "made-up-access-after-next"}, written,
		"a chain that began on the file's sign-in is written while the file still holds that start")
}

// Validates: R-6.8.26, R-6.8.14
//
// The refresh a 401 forces writes its token to the sign-in file like any
// other refresh.
func TestTokenBridge_ForceRefreshIsSaved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sign-in.json")
	cached := &oauth2.Token{
		AccessToken: "cached-access-token", TokenType: "Bearer",
		RefreshToken: savedRefreshToken, Expiry: time.Now().Add(time.Minute),
	}
	require.NoError(t, tokenfile.Save(path, cached))

	cfg := &oauth2.Config{ClientID: "test", Endpoint: oauth2.Endpoint{TokenURL: refreshingSignIn(t).URL + "/token"}}
	bridge := newTokenBridge(cfg, testAuthClient(), path, cached, slog.New(slog.DiscardHandler))

	got, err := bridge.ForceRefresh(t.Context())
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, got)
	assert.Equal(t, rotatedRefreshToken, savedRefreshTokenAt(t, path))
	require.NoError(t, bridge.SaveError())
}

// Validates: R-6.8.26, R-6.8.27
//
// A refresh started by a caller with a deadline writes its token too.
func TestTokenBridge_TokenUnderACallersDeadlineIsSaved(t *testing.T) {
	var logBuf bytes.Buffer
	ts, path := expiredSignInSource(t, &logBuf)

	ctx, cancel := context.WithTimeout(t.Context(), heldSignInGuard)
	defer cancel()

	got, err := ts.Token(ctx)
	require.NoError(t, err)
	assert.Equal(t, refreshedAccessToken, got)
	assert.Equal(t, rotatedRefreshToken, savedRefreshTokenAt(t, path))
}

// Validates: R-6.8.26
//
// A token source that writes no file reports no save failure.
func TestTokenSaveError_SourceWithoutAFileReportsNone(t *testing.T) {
	require.NoError(t, TokenSaveError(&forcingToken{}))
}
