package driveops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token() (string, error) { return "test-token", nil }

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
)

func newSimpleLaggedPathSession(t *testing.T) (*Session, *[]string) {
	t.Helper()

	gotPaths := make([]string, 0, 2)
	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch r.URL.Path {
		case testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"lagging path route"}}`)
		case testDeleteParentPath:
			writeTestResponsef(t, w, `{"value":[{"id":"retry-id","name":"file.txt"}]}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	return s, &gotPaths
}

func newRecursiveLaggedPathSession(t *testing.T) (*Session, *[]string) {
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
			writeTestResponsef(t, w, `{"value":[{"id":"docs-id","name":"Docs","folder":{"childCount":1}}]}`)
		case testNestedParentByID:
			writeTestResponsef(t, w, `{"value":[{"id":"file-id","name":"file.txt"}]}`)
		default:
			http.Error(w, "unexpected request", http.StatusInternalServerError)
		}
	}))

	return s, &gotPaths
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

	_, err := p.InteractiveSession(t.Context(), &MountSessionConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token path")
}

func TestSessionRuntime_NotLoggedIn(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, graph.ErrNotLoggedIn
	}

	cid, err := driveid.NewCanonicalID("personal:nobody@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("abc123"),
		AccountEmail:        cid.Email(),
	}

	_, err = p.InteractiveSession(t.Context(), mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestSessionRuntime_ZeroDriveID(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:test@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		AccountEmail:        cid.Email(),
		// DriveID intentionally zero.
	}

	_, err = p.InteractiveSession(t.Context(), mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive ID not resolved")
}

// --- Token caching ---

func TestSessionRuntime_TokenCaching(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var callCount int
	ts := &stubTokenSource{}
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
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

	s1, err := p.InteractiveSession(t.Context(), mount)
	require.NoError(t, err)
	require.NotNil(t, s1)

	s2, err := p.InteractiveSession(t.Context(), mount)
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

// --- FlushTokenCache ---

func TestSessionRuntime_FlushTokenCache(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var callCount int
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
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
	_, err = p.InteractiveSession(t.Context(), mount)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Second call reuses cache — no new TokenSourceFn invocation.
	_, err = p.InteractiveSession(t.Context(), mount)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Flush invalidates the cache.
	p.FlushTokenCache()

	// Third call must re-invoke TokenSourceFn.
	_, err = p.InteractiveSession(t.Context(), mount)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "TokenSourceFn should be re-invoked after FlushTokenCache")
}

func TestSessionRuntime_SyncSession_UsesMountSessionConfig(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())

	var tokenPath string
	p.TokenSourceFn = func(_ context.Context, path string, _ *slog.Logger) (graph.TokenSource, error) {
		tokenPath = path
		return &stubTokenSource{}, nil
	}

	ownerCID := driveid.MustCanonicalID("business:owner@example.com")

	session, err := p.SyncSession(t.Context(), &MountSessionConfig{
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

// --- Thread safety ---

func TestSessionRuntime_ThreadSafety(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionRuntime(holder, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
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

			_, errs[idx] = p.InteractiveSession(t.Context(), mount)
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
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("disk read failed")
	}

	cid, err := driveid.NewCanonicalID("personal:err@example.com")
	require.NoError(t, err)

	mount := &MountSessionConfig{
		TokenOwnerCanonical: cid,
		DriveID:             driveid.New("d-err"),
		AccountEmail:        cid.Email(),
	}

	_, err = p.InteractiveSession(t.Context(), mount)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk read failed")
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

func TestSession_ResolveItem_Root(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"root-id","name":"root"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "")
	require.NoError(t, err)
	assert.Equal(t, "root-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/root", gotPath)
}

func TestSession_ResolveItem_Path(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"doc-id","name":"file.txt"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "doc-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/root:/Documents/file.txt:", gotPath)
}

func TestSession_WaitPathVisible_TransientNotFoundThenSuccess(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
			return
		}

		writeTestResponsef(t, w, `{"id":"doc-id","name":"file.txt"}`)
	}))
	s.visibilityWaitSchedule = []time.Duration{0, 0}

	item, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "doc-id", item.ID)
	assert.GreaterOrEqual(t, attempts, 3)
}

func TestSession_WaitPathVisible_FallsBackToParentListingWhenExactPathLags(t *testing.T) {
	t.Parallel()

	s, gotPaths := newSimpleLaggedPathSession(t)

	item, err := s.WaitPathVisible(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "retry-id", item.ID)
	assert.Equal(t, []string{testDeleteTargetPath, testDeleteParentPath}, *gotPaths)
}

func TestSession_WaitPathVisible_RecoversWhenParentPathListingLags(t *testing.T) {
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

func TestSession_WaitPathVisible_FailsImmediatelyOnNonNotFound(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestSession_WaitPathVisible_ExhaustsNotFoundBudget(t *testing.T) {
	t.Parallel()

	var attempts int

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestSession_WaitPathVisible_ExhaustsNotFoundBudgetReturnsTypedError(t *testing.T) {
	t.Parallel()

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func TestSession_ResolveItem_SlashRoot(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeTestResponsef(t, w, `{"id":"root-id","name":"root"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, "root-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/root", gotPath)
}

func TestMountSession_ResolveItem_MountRootRelativePath(t *testing.T) {
	t.Parallel()

	const mountedRemoteRootItemID = "mount-root-item"

	var gotPaths []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		switch r.URL.Path {
		case "/drives/abcdef0123456789/items/" + mountedRemoteRootItemID + "/children":
			writeTestResponsef(t, w, `{"value":[{"id":"folder-id","name":"Documents","folder":{"childCount":1}}]}`)
		case "/drives/abcdef0123456789/items/folder-id/children":
			writeTestResponsef(t, w, `{"value":[{"id":"file-id","name":"report.docx"}]}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	mountSession := NewMountSession(s, mountedRemoteRootItemID)

	item, err := mountSession.ResolveItem(t.Context(), "Documents/report.docx")
	require.NoError(t, err)
	assert.Equal(t, "file-id", item.ID)
	assert.Equal(t, []string{
		"/drives/abcdef0123456789/items/" + mountedRemoteRootItemID + "/children",
		"/drives/abcdef0123456789/items/folder-id/children",
	}, gotPaths)
}

// Validates: R-6.7.14
func TestSession_ResolveDeleteTarget_FallsBackToParentListingAfterPathNotFound(t *testing.T) {
	t.Parallel()

	s, gotPaths := newSimpleLaggedPathSession(t)

	item, err := s.ResolveDeleteTarget(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "retry-id", item.ID)
	assert.Equal(t, []string{testDeleteTargetPath, testDeleteParentPath}, *gotPaths)
}

func TestSession_ResolveDeleteTarget_RecoversWhenParentPathListingLags(t *testing.T) {
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

func TestSession_ListChildren(t *testing.T) {
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

			s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				writeTestResponsef(t, w, `{"value":[{"id":%q,"name":%q}]}`, tt.responseID, tt.responseName)
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
func TestSession_DeleteResolvedPath_RetriesAfterTransientDeleteNotFound(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotDeleteIDs []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodDelete && r.URL.Path == testDeleteItemPath:
			deleteCalls++
			gotDeleteIDs = append(gotDeleteIDs, "item-to-delete")
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			writeTestResponsef(t, w, `{"id":"retry-id","name":"file.txt"}`)
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
func TestSession_DeleteResolvedPath_ExactPathFalseNegativeStillRetriesViaParentListing(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			writeTestResponsef(t, w, `{"value":[{"id":"retry-id","name":"file.txt"}]}`)
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
func TestSession_DeleteResolvedPath_TreatsMissingPathAsSuccess(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
func TestSession_DeleteResolvedPath_StaleParentListingAfterExactPathMissingTreatsExhaustionAsSuccess(t *testing.T) {
	t.Parallel()

	var deleteCalls int
	var gotMethods []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			writeTestResponsef(t, w, `{"value":[{"id":"retry-id","name":"file.txt"}]}`)
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
func TestSession_PermanentDeleteResolvedPath_UsesPermanentDeleteRoute(t *testing.T) {
	t.Parallel()

	var gotMethods []string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethods = append(gotMethods, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodPost && r.URL.Path == testDeleteItemPath+"/permanentDelete":
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"not ready"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteTargetPath:
			w.WriteHeader(http.StatusNotFound)
			writeTestResponsef(t, w, `{"error":{"code":"itemNotFound","message":"transient false negative"}}`)
		case r.Method == http.MethodGet && r.URL.Path == testDeleteParentPath:
			writeTestResponsef(t, w, `{"value":[{"id":"retry-id","name":"file.txt"}]}`)
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

func TestEnsureFolder_Conflict(t *testing.T) {
	t.Parallel()

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// CreateFolder = POST, ListChildren = GET — both hit /children
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			writeTestResponsef(t, w, `{"error":{"code":"nameAlreadyExists"}}`)

			return
		}
		// GET /children → returns matching folder
		writeTestResponsef(t, w, `{"value":[{"id":"existing-id","name":"docs","folder":{}}]}`)
	}))

	item, err := s.EnsureFolder(t.Context(), "parent-id", "docs")
	require.NoError(t, err)
	assert.Equal(t, "existing-id", item.ID)
}

func TestEnsureFolder_ConflictNotFound(t *testing.T) {
	t.Parallel()

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusConflict)
			writeTestResponsef(t, w, `{"error":{"code":"nameAlreadyExists"}}`)

			return
		}
		// GET /children → no matching folder
		writeTestResponsef(t, w, `{"value":[{"id":"other-id","name":"other","folder":{}}]}`)
	}))

	_, err := s.EnsureFolder(t.Context(), "parent-id", "docs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in parent")
}
