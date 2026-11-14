package driveops

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"testing"
	gosynctest "testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	// signInTestDeadline bounds each request of the tests below: longer than
	// they take, much shorter than the Retry-After the throttled account gets.
	signInTestDeadline = 5 * time.Second
	// signInTestGuard fails a test whose awaited call never returns.
	signInTestGuard = 30 * time.Second
)

// pathTokenSource hands out the base name of its sign-in file as the token,
// so a fake server can tell the sign-ins apart.
type pathTokenSource struct {
	token string
}

func (s pathTokenSource) Token(context.Context) (string, error) { return s.token, nil }

func pathTokenSourceFn(tokenPath string, _ *slog.Logger) (graph.TokenSource, error) {
	return pathTokenSource{token: filepath.Base(tokenPath)}, nil
}

// throttledSignInServer answers /me with a 429 asking for an hour for the
// sign-in whose token contains throttled, and with a profile for every other
// one, counting the requests of each.
type throttledSignInServer struct {
	srv       *httptest.Server
	throttled atomic.Int32
	answered  atomic.Int32
}

func newThrottledSignInServer(t *testing.T, throttled string) *throttledSignInServer {
	t.Helper()

	s := &throttledSignInServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Authorization"), throttled) {
			s.throttled.Add(1)
			w.Header().Set("Retry-After", "3600")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		s.answered.Add(1)
		writeTestResponsef(t, w, `{"id":"user-1","displayName":"Answered"}`)
	}))
	t.Cleanup(s.srv.Close)

	return s
}

func signInTestRuntime(t *testing.T, graphURL string) *SessionRuntime {
	t.Helper()

	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())
	runtime.GraphBaseURL = graphURL
	runtime.TokenSourceFn = pathTokenSourceFn

	return runtime
}

// askMe asks for the profile through the bootstrap client of tokenPath, under
// a deadline shorter than the throttled account's Retry-After.
func askMe(t *testing.T, runtime *SessionRuntime, tokenPath string) (*graph.User, error) {
	t.Helper()

	client, err := runtime.BootstrapClient(tokenPath)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), signInTestDeadline)
	defer cancel()

	user, err := client.Me(ctx)
	if err != nil {
		return nil, fmt.Errorf("ask for the profile: %w", err)
	}

	return user, nil
}

// Validates: R-6.8.27, R-6.8.8
//
// A throttle deadline belongs to the sign-in whose request OneDrive throttled:
// after one account's 429, another account's request through the same runtime
// is sent and answered, not refused as throttled.
func TestSessionRuntime_OneSignInsThrottleDoesNotReachAnother(t *testing.T) {
	server := newThrottledSignInServer(t, "token_a")
	runtime := signInTestRuntime(t, server.srv.URL)
	dir := t.TempDir()

	_, err := askMe(t, runtime, filepath.Join(dir, "token_a.json"))
	require.ErrorIs(t, err, graph.ErrThrottled)

	user, err := askMe(t, runtime, filepath.Join(dir, "token_b.json"))
	require.NoError(t, err, "the other sign-in was never throttled")
	assert.Equal(t, "Answered", user.DisplayName)
	assert.Equal(t, int32(1), server.answered.Load())
}

// Validates: R-6.8.27, R-6.8.8
//
// Every bootstrap client of one sign-in shares one metadata client, so the
// throttle OneDrive set for that sign-in holds for its own next request.
func TestSessionRuntime_BootstrapClientsOfOneSignInShareItsThrottle(t *testing.T) {
	server := newThrottledSignInServer(t, "token_a")
	runtime := signInTestRuntime(t, server.srv.URL)
	tokenPath := filepath.Join(t.TempDir(), "token_a.json")

	_, err := askMe(t, runtime, tokenPath)
	require.ErrorIs(t, err, graph.ErrThrottled)
	require.Equal(t, int32(1), server.throttled.Load())

	_, err = askMe(t, runtime, tokenPath)
	var throttled *retry.ThrottledError
	require.ErrorAs(t, err, &throttled, "the sign-in's own throttle still holds")
	assert.Equal(t, int32(1), server.throttled.Load(), "the refused request was not sent")
	first := runtime.BootstrapMeta(tokenPath)
	again := runtime.BootstrapMeta(tokenPath)
	assert.Same(t, first, again, "one metadata client per sign-in file")
}

// blockingTokenSourceFn is a TokenSourceFn whose load of blockedPath waits
// until release closes; every other path loads at once.
type blockingTokenSourceFn struct {
	blockedPath string
	entered     chan struct{}
	release     chan struct{}
	enterOnce   stdsync.Once
	loads       atomic.Int32
}

func newBlockingTokenSourceFn(t *testing.T, blockedPath string) *blockingTokenSourceFn {
	t.Helper()

	fn := &blockingTokenSourceFn{
		blockedPath: blockedPath,
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	t.Cleanup(fn.unblock)

	return fn
}

func (f *blockingTokenSourceFn) load(tokenPath string, logger *slog.Logger) (graph.TokenSource, error) {
	if tokenPath == f.blockedPath {
		f.loads.Add(1)
		f.enterOnce.Do(func() { close(f.entered) })
		<-f.release
	}

	return pathTokenSourceFn(tokenPath, logger)
}

func (f *blockingTokenSourceFn) unblock() {
	select {
	case <-f.release:
	default:
		close(f.release)
	}
}

// returnsWithin fails the test if call does not return within signInTestGuard.
func returnsWithin(t *testing.T, what string, call func()) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()

	select {
	case <-done:
	case <-time.After(signInTestGuard):
		require.FailNow(t, "blocked", "%s did not return within %s", what, signInTestGuard)
	}
}

// Validates: R-6.8.27
//
// Loading one sign-in file holds up nothing else: while its read is under way,
// a bootstrap client and a drive session for another sign-in are built.
func TestSessionRuntime_ATokenLoadDoesNotHoldTheRuntime(t *testing.T) {
	dir := t.TempDir()
	blockedPath := filepath.Join(dir, "token_a.json")
	fn := newBlockingTokenSourceFn(t, blockedPath)
	runtime := signInTestRuntime(t, "http://localhost")
	runtime.TokenSourceFn = fn.load

	blockedDone := make(chan error, 1)
	go func() {
		_, err := runtime.BootstrapClient(blockedPath)
		blockedDone <- err
	}()
	select {
	case <-fn.entered:
	case <-time.After(signInTestGuard):
		require.FailNow(t, "the blocked load never started")
	}

	returnsWithin(t, "a bootstrap client for another sign-in", func() {
		_, err := runtime.BootstrapClient(filepath.Join(dir, "token_b.json"))
		assert.NoError(t, err)
	})
	returnsWithin(t, "a drive session for another sign-in", func() {
		cid := driveid.MustCanonicalID("personal:other@example.com")
		_, err := runtime.SyncSession(&MountSessionConfig{TokenOwnerCanonical: cid, DriveID: driveid.New("d-other")})
		assert.NoError(t, err)
	})

	fn.unblock()
	require.NoError(t, <-blockedDone)
}

// Validates: R-6.8.27
//
// Callers that need one sign-in file at the same time share one load and get
// the same token source.
func TestSessionRuntime_ConcurrentCallersOfOneSignInLoadItOnce(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token_a.json")
	fn := newBlockingTokenSourceFn(t, tokenPath)
	runtime := signInTestRuntime(t, "http://localhost")
	runtime.TokenSourceFn = fn.load

	const callers = 2
	sources := make([]graph.TokenSource, callers)
	errs := make([]error, callers)

	var wg stdsync.WaitGroup
	for i := range callers {
		wg.Go(func() {
			sources[i], errs[i] = runtime.getOrCreateTokenSource(tokenPath)
		})
	}

	select {
	case <-fn.entered:
	case <-time.After(signInTestGuard):
		require.FailNow(t, "the load never started")
	}
	fn.unblock()
	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i], "caller %d", i)
	}
	assert.Equal(t, sources[0], sources[1], "both callers get the one source")
	assert.Equal(t, int32(1), fn.loads.Load(), "the file is read once")
}

// tokenLoadPanic is the value the panicking loads below panic with.
const tokenLoadPanic = "token load panicked"

// Validates: R-6.8.27
//
// A load that panics is a failed load like any other: the panic reaches the
// caller that ran it, and the next caller for that sign-in file reads the file
// again instead of waiting for an answer that never comes.
func TestSessionRuntime_ATokenLoadThatPanicsHoldsNoLaterCaller(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token_a.json")
	runtime := signInTestRuntime(t, "http://localhost")

	var loads atomic.Int32
	runtime.TokenSourceFn = func(tokenPath string, logger *slog.Logger) (graph.TokenSource, error) {
		if loads.Add(1) == 1 {
			panic(tokenLoadPanic)
		}

		return pathTokenSourceFn(tokenPath, logger)
	}

	require.PanicsWithValue(t, tokenLoadPanic, func() {
		ts, err := runtime.getOrCreateTokenSource(tokenPath)
		assert.Failf(t, "the panicking load returned", "%v, %v", ts, err)
	})

	var (
		ts  graph.TokenSource
		err error
	)
	returnsWithin(t, "the next caller for the sign-in", func() {
		ts, err = runtime.getOrCreateTokenSource(tokenPath)
	})
	require.NoError(t, err)
	assert.NotNil(t, ts)
	assert.Equal(t, int32(2), loads.Load(), "the file is read again")
}

// Validates: R-6.8.27
//
// A caller that is waiting for a load when its loader panics is released with
// an error; it does not wait for an answer that never comes.
func TestSessionRuntime_ATokenLoadThatPanicsReleasesItsWaiters(t *testing.T) {
	gosynctest.Test(t, func(t *testing.T) {
		runtime := signInTestRuntime(t, "http://localhost")
		release := make(chan struct{})
		runtime.TokenSourceFn = func(string, *slog.Logger) (graph.TokenSource, error) {
			<-release
			panic(tokenLoadPanic)
		}

		const tokenPath = "token_a.json"
		loaderPanic := make(chan any, 1)
		go func() {
			defer func() { loaderPanic <- recover() }()

			ts, err := runtime.getOrCreateTokenSource(tokenPath)
			assert.Failf(t, "the panicking load returned", "%v, %v", ts, err)
		}()
		gosynctest.Wait() // the loader is inside its load

		waiterErr := make(chan error, 1)
		go func() {
			_, err := runtime.getOrCreateTokenSource(tokenPath)
			waiterErr <- err
		}()
		gosynctest.Wait() // the waiter is waiting for that load

		close(release)
		gosynctest.Wait()

		assert.Equal(t, tokenLoadPanic, <-loaderPanic)
		select {
		case err := <-waiterErr:
			require.ErrorIs(t, err, errTokenSourceLoadAbandoned)
		default:
			require.FailNow(t, "the waiter is still waiting for the load")
		}
	})
}
