package driveops

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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
	return slog.Default()
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
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	require.NotNil(t, p)
	assert.NotNil(t, p.TokenSourceFn, "default TokenSourceFn should be set")
}

// --- SessionProvider.Session error paths ---

func TestSessionProvider_EmptyTokenPath(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

	resolved := &config.ResolvedDrive{
		CanonicalID: driveid.CanonicalID{}, // zero value → empty token path
	}

	_, err := p.Session(context.Background(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token path")
}

func TestSessionProvider_NotLoggedIn(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, graph.ErrNotLoggedIn
	}

	cid, err := driveid.NewCanonicalID("personal:nobody@example.com")
	require.NoError(t, err)

	resolved := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("abc123"),
	}

	_, err = p.Session(context.Background(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestSessionProvider_ZeroDriveID(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	cid, err := driveid.NewCanonicalID("personal:test@example.com")
	require.NoError(t, err)

	resolved := &config.ResolvedDrive{
		CanonicalID: cid,
		// DriveID intentionally zero
	}

	_, err = p.Session(context.Background(), resolved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive ID not resolved")
}

// --- Token caching ---

func TestSessionProvider_TokenCaching(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())

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

	s1, err := p.Session(context.Background(), rd)
	require.NoError(t, err)
	require.NotNil(t, s1)

	s2, err := p.Session(context.Background(), rd)
	require.NoError(t, err)
	require.NotNil(t, s2)

	// TokenSourceFn should only be called once — second call reuses cache.
	assert.Equal(t, 1, callCount, "TokenSourceFn should be called exactly once for the same token path")
}

// --- Thread safety ---

func TestSessionProvider_ThreadSafety(t *testing.T) {
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
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

			_, errs[idx] = p.Session(context.Background(), rd)
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
	cfg := config.DefaultConfig()
	p := NewSessionProvider(cfg, &http.Client{}, &http.Client{}, "test/1.0", discardLogger())
	p.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("disk read failed")
	}

	cid, err := driveid.NewCanonicalID("personal:err@example.com")
	require.NoError(t, err)

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DriveID:     driveid.New("d-err"),
	}

	_, err = p.Session(context.Background(), rd)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "disk read failed")
}
