package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token() (string, error) { return "test-token", nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
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

// --- NewSessionProvider ---

func TestNewSessionProvider(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	require.NotNil(t, p)
	assert.NotNil(t, p.TokenSourceFn, "default TokenSourceFn should be set")
}

// --- SessionProvider.Session error paths ---

func TestSessionProvider_EmptyTokenPath(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	resolved := &config.ResolvedDrive{
		CanonicalID: driveid.CanonicalID{}, // zero value → empty token path
	}

	_, err := p.Session(t.Context(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token path")
}

func TestSessionProvider_NotLoggedIn(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, graph.ErrNotLoggedIn
	}

	cid, err := driveid.NewCanonicalID("personal:nobody@example.com")
	require.NoError(t, err)

	resolved := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("abc123"),
	}

	_, err = p.Session(t.Context(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestSessionProvider_ZeroDriveID(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:test@example.com")
	require.NoError(t, err)

	resolved := &config.ResolvedDrive{
		CanonicalID: cid,
		// DriveID intentionally zero
	}

	_, err = p.Session(t.Context(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive ID not resolved")
}

// --- Token caching ---

func TestSessionProvider_TokenCaching(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	var callCount int
	ts := &stubTokenSource{}
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		callCount++
		return ts, nil
	}

	cid, err := driveid.NewCanonicalID("personal:cache@example.com")
	require.NoError(t, err)

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("d1"),
	}

	s1, err := p.Session(t.Context(), rd)
	require.NoError(t, err)
	require.NotNil(t, s1)

	s2, err := p.Session(t.Context(), rd)
	require.NoError(t, err)
	require.NotNil(t, s2)

	// TokenSourceFn should only be called once — second call reuses cache.
	assert.Equal(t, 1, callCount, "TokenSourceFn should be called exactly once for the same token path")
}

// --- FlushTokenCache ---

func TestSessionProvider_FlushTokenCache(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	var callCount int
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		callCount++
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:flush@example.com")
	require.NoError(t, err)

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("d-flush"),
	}

	// First call creates the cached entry.
	_, err = p.Session(t.Context(), rd)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Second call reuses cache — no new TokenSourceFn invocation.
	_, err = p.Session(t.Context(), rd)
	require.NoError(t, err)
	assert.Equal(t, 1, callCount)

	// Flush invalidates the cache.
	p.FlushTokenCache()

	// Third call must re-invoke TokenSourceFn.
	_, err = p.Session(t.Context(), rd)
	require.NoError(t, err)
	assert.Equal(t, 2, callCount, "TokenSourceFn should be re-invoked after FlushTokenCache")
}

// --- Thread safety ---

func TestSessionProvider_ThreadSafety(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:race@example.com")
	require.NoError(t, err)

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("d-race"),
	}

	var wg sync.WaitGroup
	errs := make([]error, 20)

	for i := range 20 {
		wg.Add(1)

		go func(idx int) {
			defer wg.Done()

			_, errs[idx] = p.Session(t.Context(), rd)
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
	assert.Nil(t, s.Resolved)
}

// --- TokenSourceFn error propagation ---

func TestSessionProvider_TokenSourceError(t *testing.T) {
	holder := config.NewHolder(config.DefaultConfig(), "")
	p := NewSessionProvider(holder, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("disk read failed")
	}

	cid, err := driveid.NewCanonicalID("personal:err@example.com")
	require.NoError(t, err)

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("d-err"),
	}

	_, err = p.Session(t.Context(), rd)
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

	client := graph.NewClient(srv.URL, srv.Client(), &stubTokenSource{}, slog.Default(), "test/1.0")

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
		fmt.Fprintf(w, `{"id":"root-id","name":"root"}`)
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
		fmt.Fprintf(w, `{"id":"doc-id","name":"file.txt"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "Documents/file.txt")
	require.NoError(t, err)
	assert.Equal(t, "doc-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/root:/Documents/file.txt:", gotPath)
}

func TestSession_ResolveItem_SlashRoot(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintf(w, `{"id":"root-id","name":"root"}`)
	}))

	item, err := s.ResolveItem(t.Context(), "/")
	require.NoError(t, err)
	assert.Equal(t, "root-id", item.ID)
	assert.Equal(t, "/drives/abcdef0123456789/items/root", gotPath)
}

// --- ListChildren ---

func TestSession_ListChildren_Root(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintf(w, `{"value":[{"id":"child1","name":"docs"}]}`)
	}))

	items, err := s.ListChildren(t.Context(), "")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "child1", items[0].ID)
	assert.Contains(t, gotPath, "/drives/abcdef0123456789/items/root/children")
}

func TestSession_ListChildren_Path(t *testing.T) {
	t.Parallel()

	var gotPath string

	s := newTestSession(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		fmt.Fprintf(w, `{"value":[{"id":"child2","name":"report.docx"}]}`)
	}))

	items, err := s.ListChildren(t.Context(), "Documents")
	require.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Equal(t, "child2", items[0].ID)
	assert.Contains(t, gotPath, "/drives/abcdef0123456789/root:/Documents:/children")
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
	assert.Equal(t, "/drives/abcdef0123456789/items/item-to-delete", gotPath)
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
		fmt.Fprintf(w, `{"id":"new-folder-id","name":"NewFolder"}`)
	}))

	item, err := s.CreateFolder(t.Context(), "parent-id", "NewFolder")
	require.NoError(t, err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Contains(t, gotPath, "parent-id")
	assert.Equal(t, "new-folder-id", item.ID)
	assert.Equal(t, "NewFolder", item.Name)
}
