package driveops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token(context.Context) (string, error) { return "test-token", nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

const (
	testDeleteItemPath      = "/drives/abcdef0123456789/items/item-to-delete"
	testDeleteRetryItemPath = "/drives/abcdef0123456789/items/retry-id"
	testDeleteTargetPath    = "/drives/abcdef0123456789/root:/Documents/file.txt:"
	testDeleteParentPath    = "/drives/abcdef0123456789/root:/Documents:/children"
	testNestedTargetPath    = "/drives/abcdef0123456789/root:/Projects/Docs/file.txt:"
	testNestedParentPath    = "/drives/abcdef0123456789/root:/Projects/Docs:/children"
	testNestedExactParent   = "/drives/abcdef0123456789/root:/Projects/Docs:"
	testNestedAncestorPath  = "/drives/abcdef0123456789/root:/Projects:/children"
	testNestedParentByID    = "/drives/abcdef0123456789/items/docs-id/children"

	// testDeleteParentListing is the listing of Documents holding file.txt.
	testDeleteParentListing = `{"value":[{"id":"retry-id","name":"file.txt",` +
		`"parentReference":{"id":"documents-id","path":"/drive/root:/Documents"}}]}`
)

func newSimpleLaggedPathSession(t *testing.T) (*MountSession, *[]string) {
	t.Helper()

	gotPaths := make([]string, 0, 2)
	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch r.URL.Path {
		case testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"lagging path route"}}`)
		case testDeleteParentPath:
			writeTestResponsef(t, w, testDeleteParentListing)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	return NewMountSession(s, ""), &gotPaths
}

func newRecursiveLaggedPathSession(t *testing.T) (*MountSession, *[]string) {
	t.Helper()

	gotPaths := make([]string, 0, 5)
	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch r.URL.Path {
		case testNestedTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"lagging file path"}}`)
		case testNestedParentPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"lagging parent path"}}`)
		case testNestedExactParent:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"lagging parent exact path"}}`)
		case testNestedAncestorPath:
			writeTestResponsef(t, w, `{"value":[{"id":"docs-id","name":"Docs","folder":{"childCount":1},`+
				`"parentReference":{"id":"projects-id","path":"/drive/root:/Projects"}}]}`)
		case testNestedParentByID:
			writeTestResponsef(t, w, `{"value":[{"id":"file-id","name":"file.txt"}]}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	return NewMountSession(s, ""), &gotPaths
}

// --- CleanRemotePath ---

func TestCleanRemotePath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"root slash", "/", ""},
		{"nested with trailing slash", "/foo/bar/", "foo/bar"},
		{"empty string", "", ""},
		{"no slashes", "foo", "foo"},
		{"double slashes", "//double//", "double"},
		{"empty names inside", "/a//b///c/", "a/b/c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, CleanRemotePath(tt.path))
		})
	}
}

// --- NewSessionRuntime ---

func TestNewSessionRuntime(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	require.NotNil(t, p)
	assert.NotNil(t, p.TokenSourceFn, "default TokenSourceFn should be set")
}

// --- SessionRuntime interactive session error paths ---

func TestSessionRuntime_EmptyTokenPath(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	_, err := p.InteractiveSession(&MountSessionConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token path")
}

func TestSessionRuntime_NotLoggedIn(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, graph.ErrNotLoggedIn
	}

	cid, err := driveid.NewCanonicalID("personal:nobody@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("abc123"),
		AccountEmail:        cid.Email(),
	}

	_, err = p.InteractiveSession(mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestSessionRuntime_ZeroDriveID(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:test@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		AccountEmail:        cid.Email(),
		// DriveID intentionally zero.
	}

	_, err = p.InteractiveSession(mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "doesn't know which OneDrive to use for test@example.com")
}

// --- Token caching ---

func TestSessionRuntime_TokenCaching(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var callCount int
	ts := &stubTokenSource{}
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		callCount++
		return ts, nil
	}

	cid, err := driveid.NewCanonicalID("personal:cache@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d1"),
		AccountEmail:        cid.Email(),
	}

	s1, err := p.InteractiveSession(mount)
	require.NoError(t, err)
	require.NotNil(t, s1)

	s2, err := p.InteractiveSession(mount)
	require.NoError(t, err)
	require.NotNil(t, s2)

	// TokenSourceFn should only be called once — second call reuses cache.
	assert.Equal(t, 1, callCount, "TokenSourceFn should be called exactly once for the same token path")
}

func TestSessionRuntime_InteractiveClientsReuseThrottleGatePerMountTarget(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	runtime := NewSessionRuntime(holder, "test/1.0", discardLogger())

	ownerCID := driveid.MustCanonicalID("personal:user@example.com")
	driveA := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		AccountEmail:        ownerCID.Email(),
	})
	driveAAgain := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		AccountEmail:        ownerCID.Email(),
	})
	driveB := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-b"),
		AccountEmail:        ownerCID.Email(),
	})
	rooted := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		RemoteRootItemID:    "remote-root",
		AccountEmail:        ownerCID.Email(),
	})
	rootedAgain := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		RemoteRootItemID:    "remote-root",
		AccountEmail:        ownerCID.Email(),
	})
	rootedWithFallbackEmail := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		RemoteRootItemID:    "remote-root",
	})

	firstRetry, ok := driveA.Meta.Transport.(*retry.RetryTransport)
	require.True(t, ok)
	secondRetry, ok := driveAAgain.Meta.Transport.(*retry.RetryTransport)
	require.True(t, ok)
	thirdRetry, ok := driveB.Meta.Transport.(*retry.RetryTransport)
	require.True(t, ok)
	rootedRetry, ok := rooted.Meta.Transport.(*retry.RetryTransport)
	require.True(t, ok)
	rootedRetryAgain, ok := rootedAgain.Meta.Transport.(*retry.RetryTransport)
	require.True(t, ok)

	assert.Same(t, driveA.Meta, driveAAgain.Meta)
	assert.Same(t, driveA.Transfer, driveAAgain.Transfer)
	assert.Same(t, firstRetry.ThrottleGate, secondRetry.ThrottleGate)
	assert.NotSame(t, firstRetry.ThrottleGate, thirdRetry.ThrottleGate)
	assert.Same(t, rooted.Meta, rootedWithFallbackEmail.Meta)
	assert.Same(t, rootedRetry.ThrottleGate, rootedRetryAgain.ThrottleGate)
	assert.NotSame(t, firstRetry.ThrottleGate, rootedRetry.ThrottleGate)
	assert.Same(t, driveA.Transfer, rooted.Transfer, "interactive transfer client should be shared across targets")
}

// Validates: R-6.6.8
//
// Throttle gates stay per target, but the retry announcements are shared by
// every retrying client of the runtime, so one outage prints one "can't reach
// OneDrive" line however many targets and transfers are in flight.
func TestSessionRuntime_RetryingClientsShareOneRetryNotices(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	runtime := NewSessionRuntime(holder, "test/1.0", discardLogger())

	ownerCID := driveid.MustCanonicalID("personal:user@example.com")
	driveA := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-a"),
		AccountEmail:        ownerCID.Email(),
	})
	driveB := runtime.interactiveClientsForMount(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("drive-b"),
		AccountEmail:        ownerCID.Email(),
	})

	transports := make([]*retry.RetryTransport, 0, 4)
	bootstrap := runtime.BootstrapMeta(config.DriveTokenPath(ownerCID))
	for _, client := range []*http.Client{driveA.Meta, driveB.Meta, driveA.Transfer, bootstrap} {
		rt, ok := client.Transport.(*retry.RetryTransport)
		require.True(t, ok)
		transports = append(transports, rt)
	}

	require.NotNil(t, transports[0].Notices)
	for _, rt := range transports[1:] {
		assert.Same(t, transports[0].Notices, rt.Notices)
	}
	assert.NotSame(t, transports[0].ThrottleGate, transports[1].ThrottleGate,
		"throttle coordination stays per target")
}

// --- FlushTokenCache ---

func TestSessionRuntime_FlushTokenCache(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var callCount int
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		callCount++
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:flush@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d-flush"),
		AccountEmail:        cid.Email(),
	}

	// First call creates the cached entry.
	_, err = p.InteractiveSession(mount)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Second call reuses cache — no new TokenSourceFn invocation.
	_, err = p.InteractiveSession(mount)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Flush invalidates the cache.
	p.FlushTokenCache()

	// Third call must re-invoke TokenSourceFn.
	_, err = p.InteractiveSession(mount)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "TokenSourceFn should be re-invoked after FlushTokenCache")
}

func TestSessionRuntime_SyncSession_UsesMountSessionConfig(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var tokenPath string
	p.TokenSourceFn = func(path string, _ *slog.Logger) (graph.TokenSource, error) {
		tokenPath = path
		return &stubTokenSource{}, nil
	}

	ownerCID := driveid.MustCanonicalID("business:owner@example.com")

	session, err := p.SyncSession(&MountSessionConfig{
		TokenOwnerCanonical: ownerCID,
		DriveID:             driveid.New("remote-drive"),
		RemoteRootItemID:    "root-item",
		AccountEmail:        "owner@example.com",
	})
	require.NoError(t, err)
	require.NotNil(t, session)
	assert.Equal(t, config.DriveTokenPath(ownerCID), tokenPath)
	assert.Equal(t, "owner@example.com", session.AccountEmail())
}

// unsavedTokenSource is a token source whose refreshed sign-in could not be
// written; err is what its SaveError reports.
type unsavedTokenSource struct {
	stubTokenSource
	err error
}

func (s *unsavedTokenSource) SaveError() error { return s.err }

// Validates: R-6.8.26
//
// The runtime lists the save failures of the token sources it created, one
// per sign-in file, sorted by file; sources that save fine, or write no
// file, add nothing, and a flush forgets the sources it let go of.
func TestSessionRuntime_TokenSaveErrors(t *testing.T) {
	p := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())
	pathA, pathB := "/sign-ins/a.json", "/sign-ins/b.json"
	failures := map[string]error{
		pathB: fmt.Errorf("%w to %s: disk full", graph.ErrTokenNotSaved, pathB),
		pathA: fmt.Errorf("%w to %s: read-only", graph.ErrTokenNotSaved, pathA),
	}
	p.TokenSourceFn = func(path string, _ *slog.Logger) (graph.TokenSource, error) {
		if failure, failing := failures[path]; failing {
			return &unsavedTokenSource{err: failure}, nil
		}
		if path == "/sign-ins/saved.json" {
			return &unsavedTokenSource{}, nil
		}

		return &stubTokenSource{}, nil
	}

	assert.Empty(t, p.TokenSaveFailures(), "no source, no failure")

	for _, path := range []string{pathB, "/sign-ins/saved.json", "/sign-ins/plain.json", pathA} {
		_, err := p.BootstrapClient(path)
		require.NoError(t, err)
	}

	assert.Equal(t, []TokenSaveFailure{
		{TokenPath: pathA, Err: failures[pathA]},
		{TokenPath: pathB, Err: failures[pathB]},
	}, p.TokenSaveFailures())

	p.FlushTokenCache()
	assert.Empty(t, p.TokenSaveFailures())
}

// --- Thread safety ---

func TestSessionRuntime_ThreadSafety(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:race@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d-race"),
		AccountEmail:        cid.Email(),
	}

	var wg sync.WaitGroup
	errs := make([]error, 20)

	for i := range 20 {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			_, errs[idx] = p.InteractiveSession(mount)
		}(i)
	}

	wg.Wait()

	for i, e := range errs {
		assert.NoError(t, e, "goroutine %d should not error", i)
	}
}

// --- Session struct ---

func TestSession_Fields(t *testing.T) {
	var s Session
	assert.Nil(t, s.Meta)
	assert.Nil(t, s.Transfer)
	assert.True(t, s.DriveID.IsZero())
	assert.Empty(t, s.AccountEmail())
}

// --- TokenSourceFn error propagation ---

func TestSessionRuntime_TokenSourceError(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("disk read failed")
	}

	cid, err := driveid.NewCanonicalID("personal:err@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d-err"),
		AccountEmail:        cid.Email(),
	}

	_, err = p.InteractiveSession(mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk read failed")
}

// --- BootstrapClient ---

// Validates: R-6.8.27
//
// Account-level calls (identity probes, discovery, search) and drive sessions
// for one sign-in file share the runtime's one token source, however many
// callers ask and from however many goroutines, so a refresh by one is the
// token every other one uses.
func TestSessionRuntime_BootstrapClientReusesOneTokenSourcePerPath(t *testing.T) {
	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())

	var sourcesBuilt atomic.Int32
	runtime.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		sourcesBuilt.Add(1)
		return &stubTokenSource{}, nil
	}

	cid := driveid.MustCanonicalID("personal:bootstrap@example.com")
	tokenPath := config.DriveTokenPath(cid)

	const concurrentCallers = 8
	clients := make([]*graph.Client, concurrentCallers)
	errs := make([]error, concurrentCallers)

	var wg sync.WaitGroup
	for i := range concurrentCallers {
		wg.Go(func() {
			clients[i], errs[i] = runtime.BootstrapClient(tokenPath)
		})
	}
	wg.Wait()

	for i := range concurrentCallers {
		require.NoError(t, errs[i], "caller %d", i)
		assert.NotNil(t, clients[i], "caller %d", i)
	}

	_, err := runtime.InteractiveSession(&MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d-bootstrap"),
		AccountEmail:        cid.Email(),
	})
	require.NoError(t, err)

	assert.Equal(t, int32(1), sourcesBuilt.Load(), "one sign-in file must have one token source in the runtime")
}

// Validates: R-6.8.27
func TestSessionRuntime_BootstrapClientKeepsNotLoggedIn(t *testing.T) {
	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())

	var sourcesTried int
	runtime.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		sourcesTried++
		return nil, graph.ErrNotLoggedIn
	}

	tokenPath := config.DriveTokenPath(driveid.MustCanonicalID("personal:signed-out@example.com"))

	client, err := runtime.BootstrapClient(tokenPath)
	require.ErrorIs(t, err, graph.ErrNotLoggedIn, "callers classify a missing sign-in from the error")
	assert.Nil(t, client)

	_, err = runtime.BootstrapClient(tokenPath)
	require.ErrorIs(t, err, graph.ErrNotLoggedIn)
	assert.Equal(t, 2, sourcesTried, "a failed load is not cached: a later login in the same process is picked up")
}

// Validates: R-6.8.27
func TestSessionRuntime_BootstrapClientNeedsATokenPath(t *testing.T) {
	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())
	runtime.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		assert.Fail(t, "no token source may be built for an empty path")
		return &stubTokenSource{}, nil
	}

	client, err := runtime.BootstrapClient("")
	require.Error(t, err)
	assert.Nil(t, client)
	assert.Contains(t, err.Error(), "token path")
}

// Validates: R-6.8.27
func TestSessionRuntime_BootstrapClientUsesTheRuntimeBaseURL(t *testing.T) {
	var meCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/me", r.URL.Path)
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		meCalls.Add(1)
		writeTestResponsef(t, w, `{"id":"user-1","displayName":"Bootstrap User","mail":"bootstrap@example.com"}`)
	}))
	t.Cleanup(srv.Close)

	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())
	runtime.GraphBaseURL = srv.URL
	runtime.TokenSourceFn = func(_ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	client, err := runtime.BootstrapClient(config.DriveTokenPath(driveid.MustCanonicalID("personal:bootstrap@example.com")))
	require.NoError(t, err)

	user, err := client.Me(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "user-1", user.ID)
	assert.Equal(t, int32(1), meCalls.Load())
}

// Validates: R-6.8.27
//
// The default token source gets the runtime's auth client: a nil one would
// make every token load fail.
func TestSessionRuntime_DefaultTokenSourceHasTheAuthClient(t *testing.T) {
	runtime := NewSessionRuntime(config.NewHolder(config.DefaultConfig(), ""), "test/1.0", discardLogger())
	require.NotNil(t, runtime.authHTTP)
	assert.Positive(t, runtime.authHTTP.Timeout, "sign-in requests must have a deadline")

	tokenPath := filepath.Join(t.TempDir(), "tokens", "valid.json")
	require.NoError(t, tokenfile.Save(tokenPath, &oauth2.Token{
		AccessToken:  "saved-access-token",
		TokenType:    "Bearer",
		RefreshToken: "saved-refresh-token",
		Expiry:       time.Now().Add(time.Hour),
	}))

	ts, err := runtime.TokenSourceFn(tokenPath, discardLogger())
	require.NoError(t, err)

	token, err := ts.Token(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "saved-access-token", token)
}

// --- ResolveItem ---

// newTestSession creates a Session backed by an httptest.Server.
// The handler receives the request path for assertion.
func newTestSession(t *testing.T, handler http.Handler) *Session {
	t.Helper()

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	client := graph.MustNewClient(srv.URL, srv.Client(), &stubTokenSource{}, slog.Default(), "test/1.0")

	return &Session{
		Meta:    client,
		DriveID: driveid.New("abcdef0123456789"),
	}
}

func newTestDriveRootMountSession(t *testing.T, handler http.Handler) *MountSession {
	t.Helper()

	return NewMountSession(newTestSession(t, handler), "")
}

func TestMountSession_ResolveItem_Root(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"root-id","name":"root"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, "root-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/root", gotPath)
}

func TestMountSession_ResolveItem_Path(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"doc-id","name":"file.txt","parentReference":{"id":"docs-id","path":"/drive/root:/Documents"}}`)
	}))

	item, err := s.ResolveItem(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "doc-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/root:/Documents/file.txt:", gotPath)
}

func TestMountSession_WaitPathVisible_TransientNotFoundThenSuccess(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
			return
		}

		writeTestResponsef(t, w, `{"id":"doc-id","name":"file.txt","parentReference":{"id":"docs-id","path":"/drive/root:/Documents"}}`)
	}))
	s.visibilityWaitSchedule = []time.Duration{0, 0}

	item, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "doc-id", item.ID)
	assert.GreaterOrEqual(t, attempts, 3)
}

func TestMountSession_WaitPathVisible_FallsBackToParentListingWhenExactPathLags(t *testing.T) {
	t.Parallel()

	s, gotPaths := newSimpleLaggedPathSession(t)

	item, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "retry-id", item.ID)
	assert.Equal(t, []string{testDeleteTargetPath, testDeleteParentPath}, *gotPaths)
}

func TestMountSession_WaitPathVisible_RecoversWhenParentPathListingLags(t *testing.T) {
	t.Parallel()

	s, gotPaths := newRecursiveLaggedPathSession(t)

	item, err := s.WaitPathVisible(t.Context(), "Projects/Docs/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "file-id", item.ID)
	assert.Equal(t, []string{
		testNestedTargetPath,
		testNestedParentPath,
		testNestedExactParent,
		testNestedAncestorPath,
		testNestedParentByID,
	}, *gotPaths)
}

func TestMountSession_WaitPathVisible_FailsImmediatelyOnNonNotFound(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusForbidden)
		writeTestResponsef(t, w, `{"error":{"code":"accessDenied","message":"denied"}}`)
	}))
	s.visibilityWaitSchedule = []time.Duration{0, 0}

	_, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.Error(t, err)
	assert.Equal(t, 1, attempts)
	assert.ErrorContains(t, err, "denied")
}

func TestMountSession_WaitPathVisible_ExhaustsNotFoundBudget(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"still settling"}}`)
	}))
	s.visibilityWaitSchedule = []time.Duration{0, 0}

	_, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrPathNotVisible)
	assert.GreaterOrEqual(t, attempts, 3)
}

func TestMountSession_WaitPathVisible_ExhaustsNotFoundBudgetReturnsTypedError(t *testing.T) {
	t.Parallel()

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"still settling"}}`)
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	_, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.Error(t, err)

	var visibilityErr *PathNotVisibleError
	require.ErrorAs(t, err, &visibilityErr)
	assert.Equal(t, "Documents/file.txt", visibilityErr.Path)
}

func TestMountSession_ResolveItem_SlashRoot(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"root-id","name":"root"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, "root-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/root", gotPath)
}

// Validates: R-6.7.14
func TestMountSession_ResolveDeleteTarget_FallsBackToParentListingAfterPathNotFound(t *testing.T) {
	t.Parallel()

	s, gotPaths := newSimpleLaggedPathSession(t)

	item, err := s.ResolveDeleteTarget(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "retry-id", item.ID)
	assert.Equal(t, []string{testDeleteTargetPath, testDeleteParentPath}, *gotPaths)
}

// A parent listing that fails after the read by path found nothing is the
// error, and it names the path once and its folder once.
//
// Validates: R-6.7.14
func TestMountSession_ResolveDeleteTarget_AFailedParentListingNamesThePathOnce(t *testing.T) {
	t.Parallel()

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/children") {
			w.WriteHeader(http.StatusForbidden)
			writeTestResponsef(t, w, `{"error":{"code":"accessDenied","message":"denied"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not found"}}`)
	}))

	_, err := s.ResolveDeleteTarget(t.Context(), "Documents/file.txt")
	require.ErrorIs(t, err, graph.ErrForbidden)
	require.EqualError(t, err, `resolve "Documents/file.txt" from the listing of its parent: `+
		`list children for "Documents": graph: HTTP 403 accessDenied: denied`)
}

func TestMountSession_ResolveDeleteTarget_RecoversWhenParentPathListingLags(t *testing.T) {
	t.Parallel()

	s, gotPaths := newRecursiveLaggedPathSession(t)

	item, err := s.ResolveDeleteTarget(t.Context(), "Projects/Docs/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "file-id", item.ID)
	assert.Equal(t, []string{
		testNestedTargetPath,
		testNestedParentPath,
		testNestedExactParent,
		testNestedAncestorPath,
		testNestedParentByID,
	}, *gotPaths)
}

func TestMountSession_ListChildren_MountRootUsesMountRootItem(t *testing.T) {
	t.Parallel()

	const mountedRemoteRootItemID = "mount-root-item"

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"value":[{"id":"child1","name":"docs","folder":{"childCount":0}}]}`)
	}))
	mountSession := NewMountSession(s, mountedRemoteRootItemID)

	items, err := mountSession.ListChildren(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "child1", items[0].ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/"+mountedRemoteRootItemID+"/children", gotPath)
}

// --- ListChildren ---

func TestMountSession_ListChildren(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		responseID   string
		responseName string
		wantPath     string
	}{
		{
			name:         "Root",
			path:         "",
			responseID:   "child1",
			responseName: "docs",
			wantPath:     "/drives/abcdef0123456789/items/root/children",
		},
		{
			name:         "Path",
			path:         "Documents",
			responseID:   "child2",
			responseName: "report.docx",
			wantPath:     "/drives/abcdef0123456789/root:/Documents:/children",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var gotPath string

			s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				writeTestResponsef(t, w, `{"value":[{"id":%q,"name":%q,"parentReference":{"path":"/drive/root:/%s"}}]}`,
					tt.responseID, tt.responseName, tt.path)
			}))

			items, err := s.ListChildren(t.Context(), tt.path)
			require.NoError(t, err)
			assert.Len(t, items, 1)
			assert.Equal(t, tt.responseID, items[0].ID)
			assert.Contains(t, gotPath, tt.wantPath)
		})
	}
}

// --- Delegation methods ---

func TestSession_DeleteItem(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	err := s.DeleteItem(t.Context(), "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, http.MethodDelete, gotMethod)
	assert.Equal(t, testDeleteItemPath, gotPath)
}

// Validates: R-6.7.14
func TestMountSession_DeleteResolvedPath_RetriesAfterTransientDeleteNotFound(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotDeleteIDs []string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteItemPath:
			deleteCalls++
			gotDeleteIDs = append(gotDeleteIDs, "item-to-delete")
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			writeTestResponsef(t, w, `{"id":"retry-id","name":"file.txt","parentReference":{"id":"docs-id","path":"/drive/root:/Documents"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteRetryItemPath:
			deleteCalls++
			gotDeleteIDs = append(gotDeleteIDs, "retry-id")
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	err := s.DeleteResolvedPath(t.Context(), "Documents/file.txt", "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, []string{"item-to-delete", "retry-id"}, gotDeleteIDs)
	assert.Equal(t, 2, deleteCalls)
}

// Validates: R-6.7.14
func TestMountSession_DeleteResolvedPath_ExactPathFalseNegativeStillRetriesViaParentListing(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteItemPath:
			deleteCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"transient false negative"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteParentPath:
			writeTestResponsef(t, w, testDeleteParentListing)
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteRetryItemPath:
			deleteCalls++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	err := s.DeleteResolvedPath(t.Context(), "Documents/file.txt", "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, 2, deleteCalls)
	assert.Equal(t, []string{
		"DELETE " + testDeleteItemPath,
		"GET " + testDeleteTargetPath,
		"GET " + testDeleteParentPath,
		"DELETE " + testDeleteRetryItemPath,
	}, gotMethods)
}

// Validates: R-6.7.14
func TestMountSession_DeleteResolvedPath_TreatsMissingPathAsSuccess(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteItemPath:
			deleteCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"gone"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"gone"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteParentPath:
			writeTestResponsef(t, w, `{"value":[]}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	err := s.DeleteResolvedPath(t.Context(), "Documents/file.txt", "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, 1, deleteCalls)
	assert.Equal(t, []string{
		"DELETE " + testDeleteItemPath,
		"GET " + testDeleteTargetPath,
		"GET " + testDeleteParentPath,
	}, gotMethods)
}

// Validates: R-6.7.14
func TestMountSession_DeleteResolvedPath_StaleParentListingAfterExactPathMissingTreatsExhaustionAsSuccess(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteItemPath:
			deleteCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"gone"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"false negative"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteParentPath:
			writeTestResponsef(t, w, testDeleteParentListing)
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteRetryItemPath:
			deleteCalls++
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"already gone"}}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	err := s.DeleteResolvedPath(t.Context(), "Documents/file.txt", "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, 2, deleteCalls)
	assert.Equal(t, []string{
		"DELETE " + testDeleteItemPath,
		"GET " + testDeleteTargetPath,
		"GET " + testDeleteParentPath,
		"DELETE " + testDeleteRetryItemPath,
	}, gotMethods)
}

// Validates: R-6.7.14
func TestMountSession_PermanentDeleteResolvedPath_UsesPermanentDeleteRoute(t *testing.T) {
	t.Parallel()

	var gotMethods []string

	s := newTestDriveRootMountSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testDeleteItemPath+"/permanentDelete":
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"transient false negative"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteParentPath:
			writeTestResponsef(t, w, testDeleteParentListing)
		case r.Method == http.MethodPost && r.URL.Path == testDeleteRetryItemPath+"/permanentDelete":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))
	s.visibilityWaitSchedule = []time.Duration{0}

	err := s.PermanentDeleteResolvedPath(t.Context(), "Documents/file.txt", "item-to-delete")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"POST " + testDeleteItemPath + "/permanentDelete",
		"GET " + testDeleteTargetPath,
		"GET " + testDeleteParentPath,
		"POST " + testDeleteRetryItemPath + "/permanentDelete",
	}, gotMethods)
}

func TestSession_PermanentDeleteItem(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))

	err := s.PermanentDeleteItem(t.Context(), "item-to-perm-delete")
	require.NoError(t, err)
	// Graph API permanentDelete uses POST, not DELETE.
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotPath, "item-to-perm-delete")
}

func TestSession_CreateFolder(t *testing.T) {
	t.Parallel()

	var gotMethod, gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{"id":"new-folder-id","name":"NewFolder"}`)
	}))

	item, err := s.CreateFolder(t.Context(), "parent-id", "NewFolder")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotPath, "parent-id")
	assert.Equal(t, "new-folder-id", item.ID)
	assert.Equal(t, "NewFolder", item.Name)
}

// --- SplitParentAndName ---

func TestSplitParentAndName(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantParent string
		wantName   string
	}{
		{"nested path", "foo/bar/baz", "foo/bar", "baz"},
		{"single segment", "baz", "", "baz"},
		{"empty string", "", "", ""},
		{"trailing slash top-level", "/top/", "", "top"},
		{"empty names", "a//b///c", "a/b", "c"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parent, name := SplitParentAndName(tt.path)
			assert.Equal(t, tt.wantParent, parent)
			assert.Equal(t, tt.wantName, name)
		})
	}
}

// --- EnsureFolder ---

func TestEnsureFolder_Created(t *testing.T) {
	t.Parallel()

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		writeTestResponsef(t, w, `{"id":"new-id","name":"docs","folder":{}}`)
	}))

	item, err := s.EnsureFolder(t.Context(), "parent-id", "docs")
	require.NoError(t, err)
	assert.Equal(t, "new-id", item.ID)
	assert.Equal(t, "docs", item.Name)
}

// ensureFolderParent is the folder the EnsureFolder tests create in.
const ensureFolderParent = "parent-id"

// nameLongerThanAPathRoute is the length of a name OneDrive takes (up to 400
// characters) that is longer than the 240 escaped characters a path-addressed
// read takes.
const nameLongerThanAPathRoute = 300

// folderConflictChild is one item the folder of a folderConflict holds.
type folderConflictChild struct {
	id, name string
	isFolder bool
}

// answer is the child as Graph answers it, in ensureFolderParent.
func (c folderConflictChild) answer() map[string]any {
	item := map[string]any{"id": c.id, "name": c.name, "parentReference": map[string]any{"id": ensureFolderParent}}
	if c.isFolder {
		item["folder"] = map[string]any{}
	} else {
		item["file"] = map[string]any{}
	}

	return item
}

// folderConflict is a Graph fake of the folder ensureFolderParent, where every
// folder create answers 409. A read of one name answers the child that holds
// it, letter case aside, or 404; always 404 when readMisses, and always
// readStatus with the error code readCode when readStatus is set. A listing
// answers every child. It counts both.
type folderConflict struct {
	children   []folderConflictChild
	readMisses bool
	readStatus int
	readCode   string
	reads      atomic.Int32
	listings   atomic.Int32
}

func (f *folderConflict) session(t *testing.T) *Session {
	t.Helper()

	return newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusConflict)
			writeTestResponsef(t, w, `{"error":{"code":"nameAlreadyExists"}}`)
		case strings.HasSuffix(r.URL.Path, "/children"):
			f.listings.Add(1)
			values := make([]map[string]any, 0, len(f.children))
			for _, child := range f.children {
				values = append(values, child.answer())
			}
			writeFolderConflictJSON(t, w, map[string]any{"value": values})
		default:
			f.reads.Add(1)
			f.serveRead(t, w, r)
		}
	}))
}

// serveRead answers a read of one name, items/{parent}:/{name}:.
func (f *folderConflict) serveRead(t *testing.T, w http.ResponseWriter, r *http.Request) {
	t.Helper()

	_, route, _ := strings.Cut(r.URL.Path, ":/")
	name := strings.TrimSuffix(route, ":")
	if f.readStatus != 0 {
		w.WriteHeader(f.readStatus)
		writeTestResponsef(t, w, `{"error":{"code":%q}}`, f.readCode)
		return
	}
	for _, child := range f.children {
		if !f.readMisses && strings.EqualFold(child.name, name) {
			writeFolderConflictJSON(t, w, child.answer())
			return
		}
	}
	w.WriteHeader(http.StatusNotFound)
	writeTestResponsef(t, w, `{"error":{"code":"itemNotFound"}}`)
}

func writeFolderConflictJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()

	data, err := json.Marshal(v)
	require.NoError(t, err)
	_, err = w.Write(data)
	require.NoError(t, err)
}

// A folder that exists is found by one read of its name: its parent is never
// listed, so a second upload of a tree costs no listing per folder.
//
// Validates: R-1.3.1
func TestEnsureFolder_Conflict(t *testing.T) {
	t.Parallel()

	fake := &folderConflict{children: []folderConflictChild{{id: "existing-id", name: "docs", isFolder: true}}}

	item, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
	require.NoError(t, err)
	assert.Equal(t, "existing-id", item.ID)
	assert.Equal(t, int32(1), fake.reads.Load())
	assert.Zero(t, fake.listings.Load(), "a folder that exists costs no listing of its parent")
}

// A folder OneDrive already holds under a name that differs only in letter
// case is the folder the 409 meant, found and used: OneDrive treats the two
// names as one, and the read answers its stored spelling.
//
// Validates: R-1.3.1, R-1.5.3
func TestEnsureFolder_ConflictFindsTheFolderInAnotherCase(t *testing.T) {
	t.Parallel()

	fake := &folderConflict{children: []folderConflictChild{{id: "existing-id", name: "Docs", isFolder: true}}}

	item, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
	require.NoError(t, err)
	assert.Equal(t, "existing-id", item.ID)
	assert.Equal(t, "Docs", item.Name)
	assert.Zero(t, fake.listings.Load())
}

// The folder is listed, once, only when the read cannot settle the name: a
// name the path route cannot carry, a read answered 400 (measured on a
// Personal drive for a folder named $value that its parent's listing holds),
// or a read that finds nothing equal although the create said the name
// exists.
//
// Validates: R-1.3.1
func TestEnsureFolder_ConflictListsOnceWhenTheReadCannotFindTheName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label, name string
		readMisses  bool
		readStatus  int
		readCode    string
		wantReads   int32
	}{
		{label: "a name longer than a path route carries", name: strings.Repeat("a", nameLongerThanAPathRoute)},
		{label: "a name holding a colon", name: "a:b"},
		{label: "a read that finds nothing equal", name: "docs", readMisses: true, wantReads: 1},
		{
			label: "a read answered 400", name: "$value",
			readStatus: http.StatusBadRequest, readCode: "invalidRequest", wantReads: 1,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &folderConflict{
				children:   []folderConflictChild{{id: "existing-id", name: tc.name, isFolder: true}},
				readMisses: tc.readMisses,
				readStatus: tc.readStatus,
				readCode:   tc.readCode,
			}

			item, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, tc.name)
			require.NoError(t, err)
			assert.Equal(t, "existing-id", item.ID)
			assert.Equal(t, tc.wantReads, fake.reads.Load())
			assert.Equal(t, int32(1), fake.listings.Load())
		})
	}
}

// A read that fails, other than by finding nothing or by a 400, is the error:
// a listing would not answer what the read could not.
//
// Validates: R-1.3.1
func TestEnsureFolder_AReadThatFailsIsTheError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		status int
		code   string
		want   error
	}{
		{status: http.StatusForbidden, code: "accessDenied", want: graph.ErrForbidden},
		{status: http.StatusInternalServerError, code: "generalException", want: graph.ErrServerError},
	} {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()

			fake := &folderConflict{
				children:   []folderConflictChild{{id: "existing-id", name: "docs", isFolder: true}},
				readStatus: tc.status,
				readCode:   tc.code,
			}

			_, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
			require.ErrorIs(t, err, tc.want)
			assert.Zero(t, fake.listings.Load())
		})
	}
}

// A 409 for a name neither the read nor the listing finds is an error, the
// same after a read answered 400 as after one that found nothing.
//
// Validates: R-1.3.1
func TestEnsureFolder_ConflictNotFound(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label      string
		readStatus int
		readCode   string
	}{
		{label: "a read that finds nothing"},
		{label: "a read answered 400", readStatus: http.StatusBadRequest, readCode: "invalidRequest"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &folderConflict{
				children:   []folderConflictChild{{id: "other-id", name: "other", isFolder: true}},
				readStatus: tc.readStatus,
				readCode:   tc.readCode,
			}

			_, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
			require.EqualError(t, err, `folder "docs" reported as existing but not found in parent`)
			assert.Equal(t, int32(1), fake.reads.Load())
			assert.Equal(t, int32(1), fake.listings.Load())
		})
	}
}

// A read or a listing that holds the name without an item ID is an error
// that says so: nothing can be made in an item no request can name, and "not
// found" would hide what OneDrive answered.
//
// Validates: R-1.3.1
func TestEnsureFolder_AFolderWithoutAnIDIsAnError(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		label        string
		readMisses   bool
		want         string
		wantListings int32
	}{
		{label: "read", want: `resolving existing folder "docs": the read of "docs" in its folder answered without an item ID`},
		{
			label: "listed", readMisses: true, wantListings: 1,
			want: `resolving existing folder "docs": the listing of the folder holds "docs" without an item ID`,
		},
	} {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()

			fake := &folderConflict{
				children:   []folderConflictChild{{name: "docs", isFolder: true}},
				readMisses: tc.readMisses,
			}

			_, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
			require.EqualError(t, err, tc.want)
			assert.Equal(t, tc.wantListings, fake.listings.Load())
		})
	}
}

// A file that holds the folder's name is said to be a file: the folder cannot
// be made, and nothing is uploaded into the file.
//
// Validates: R-1.3.1
func TestEnsureFolder_AFileHoldingTheNameIsSaidSo(t *testing.T) {
	t.Parallel()

	fake := &folderConflict{children: []folderConflictChild{{id: "file-id", name: "docs"}}}

	_, err := fake.session(t).EnsureFolder(t.Context(), ensureFolderParent, "docs")
	require.EqualError(t, err, `"docs" is a file on OneDrive`)
	assert.Zero(t, fake.listings.Load())
}
