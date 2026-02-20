package graph

import (
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"
)

// testTokenJSON is the canonical token response for tests.
const testTokenJSON = `{
	"access_token": "test-access-token",
	"token_type": "Bearer",
	"refresh_token": "test-refresh-token",
	"expires_in": 3600
}`

// testDeviceCodeJSON is the canonical device code response for tests.
// interval=1 to minimize poll delay in tests.
const testDeviceCodeJSON = `{
	"device_code": "test-device-code",
	"user_code": "ABCD-1234",
	"verification_uri": "https://microsoft.com/devicelogin",
	"expires_in": 900,
	"interval": 1
}`

// newMockOAuthServer creates a test server that handles device code + token requests.
// Server cleanup is automatic via t.Cleanup.
// tokenHandler controls the token endpoint behavior. If nil, returns testTokenJSON.
func newMockOAuthServer(t *testing.T, tokenHandler http.HandlerFunc) *oauth2.Endpoint {
	t.Helper()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /devicecode", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(testDeviceCodeJSON))
	})

	handler := tokenHandler
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testTokenJSON))
		}
	}

	mux.HandleFunc("POST /token", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &oauth2.Endpoint{
		DeviceAuthURL: srv.URL + "/devicecode",
		TokenURL:      srv.URL + "/token",
	}
}

// testOAuthConfig builds a test config pointing at a mock server.
func testOAuthConfig(t *testing.T, tokenPath string, endpoint *oauth2.Endpoint) *oauth2.Config {
	t.Helper()

	cfg := oauthConfig(tokenPath, slog.Default())
	cfg.Endpoint = *endpoint

	return cfg
}

// noopDisplay discards the device auth display callback.
func noopDisplay(_ DeviceAuth) {}

func TestDoLogin_Success(t *testing.T) {
	endpoint := newMockOAuthServer(t, nil)
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "test.json")

	cfg := testOAuthConfig(t, tokenPath, endpoint)

	var displayed DeviceAuth
	ts, err := doLogin(context.Background(), tokenPath, cfg, func(da DeviceAuth) {
		displayed = da
	}, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	// Verify display callback was called with correct values.
	assert.Equal(t, "ABCD-1234", displayed.UserCode)
	assert.Equal(t, "https://microsoft.com/devicelogin", displayed.VerificationURI)

	// Verify token was saved to disk.
	loaded, loadErr := loadToken(tokenPath)
	require.NoError(t, loadErr)
	require.NotNil(t, loaded)
	assert.Equal(t, "test-access-token", loaded.AccessToken)
	assert.Equal(t, "test-refresh-token", loaded.RefreshToken)

	// Verify the returned TokenSource works.
	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
}

func TestDoLogin_UserDeclined(t *testing.T) {
	endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"access_denied","error_description":"user declined"}`))
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	_, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "access_denied")
}

func TestDoLogin_ExpiredCode(t *testing.T) {
	endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"expired_token","error_description":"device code expired"}`))
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	_, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expired_token")
}

func TestDoLogin_ContextCancel(t *testing.T) {
	endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := doLogin(ctx, tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestDoLogin_PendingThenSuccess(t *testing.T) {
	var polls atomic.Int32

	endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		n := polls.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// First two polls return pending, third returns token.
		if n <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))

			return
		}

		_, _ = w.Write([]byte(testTokenJSON))
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "pending.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ts, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)

	// Should have polled at least 3 times.
	assert.GreaterOrEqual(t, polls.Load(), int32(3))
}

func TestSaveLoadToken_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "roundtrip.json")

	expiry := time.Now().Add(time.Hour).Truncate(time.Second)
	original := &oauth2.Token{
		AccessToken:  "access-abc",
		TokenType:    "Bearer",
		RefreshToken: "refresh-xyz",
		Expiry:       expiry,
	}

	err := saveToken(path, original)
	require.NoError(t, err)

	loaded, err := loadToken(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, original.AccessToken, loaded.AccessToken)
	assert.Equal(t, original.TokenType, loaded.TokenType)
	assert.Equal(t, original.RefreshToken, loaded.RefreshToken)
	// JSON time encoding may lose sub-second precision; compare truncated.
	assert.True(t, original.Expiry.Equal(loaded.Expiry.Truncate(time.Second)),
		"expiry mismatch: want %v, got %v", original.Expiry, loaded.Expiry)
}

func TestSaveToken_Permissions(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "perms.json")

	tok := &oauth2.Token{AccessToken: "secret"}
	err := saveToken(path, tok)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(tokenFilePerms), info.Mode().Perm())
}

func TestSaveToken_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "atomic.json")

	// Write initial token.
	tok1 := &oauth2.Token{AccessToken: "first", RefreshToken: "r1"}
	require.NoError(t, saveToken(path, tok1))

	// Overwrite with second token.
	tok2 := &oauth2.Token{AccessToken: "second", RefreshToken: "r2"}
	require.NoError(t, saveToken(path, tok2))

	// Verify the final file has the second token (not corrupted).
	loaded, err := loadToken(path)
	require.NoError(t, err)
	assert.Equal(t, "second", loaded.AccessToken)
	assert.Equal(t, "r2", loaded.RefreshToken)

	// No temp files should remain.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)

	for _, e := range entries {
		assert.NotContains(t, e.Name(), ".token-")
	}
}

func TestSaveToken_CreatesDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "deep", "nested", "token.json")

	tok := &oauth2.Token{AccessToken: "nested"}
	err := saveToken(path, tok)
	require.NoError(t, err)

	loaded, err := loadToken(path)
	require.NoError(t, err)
	assert.Equal(t, "nested", loaded.AccessToken)
}

func TestLoadToken_NoFile(t *testing.T) {
	tok, err := loadToken(filepath.Join(t.TempDir(), "nonexistent.json"))
	assert.NoError(t, err)
	assert.Nil(t, tok)
}

func TestLoadToken_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "bad.json")

	require.NoError(t, os.WriteFile(path, []byte("not json"), tokenFilePerms))

	_, err := loadToken(path)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding token file")
}

func TestTokenSourceFromPath_NoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")

	_, err := TokenSourceFromPath(context.Background(), path, slog.Default())
	assert.ErrorIs(t, err, ErrNotLoggedIn)
}

func TestTokenSourceFromPath_ValidToken(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "valid.json")

	tok := &oauth2.Token{
		AccessToken:  "saved-access-token",
		RefreshToken: "saved-refresh-token",
		Expiry:       time.Now().Add(time.Hour),
	}

	require.NoError(t, saveToken(path, tok))

	ts, err := TokenSourceFromPath(context.Background(), path, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	// The token source should return the saved access token.
	got, err := ts.Token()
	require.NoError(t, err)
	assert.Equal(t, "saved-access-token", got)
}

func TestTokenSourceFromPath_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "corrupt.json")

	require.NoError(t, os.WriteFile(path, []byte("not valid json"), tokenFilePerms))

	_, err := TokenSourceFromPath(context.Background(), path, slog.Default())
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decoding token file")
}

func TestLogout_RemovesFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "logout.json")

	// Create a token file to delete.
	tok := &oauth2.Token{AccessToken: "doomed"}
	require.NoError(t, saveToken(path, tok))

	// Verify it exists.
	_, err := os.Stat(path)
	require.NoError(t, err)

	// Delete via the public Logout function.
	err = Logout(path, slog.Default())
	require.NoError(t, err)

	// Verify it's gone.
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err))
}

func TestLogout_NoFile(t *testing.T) {
	// Removing a nonexistent file should not be an error (idempotent).
	path := filepath.Join(t.TempDir(), "nonexistent.json")

	err := Logout(path, slog.Default())
	assert.NoError(t, err)
}

func TestTokenBridge(t *testing.T) {
	tok := &oauth2.Token{
		AccessToken: "bridge-token-123",
		Expiry:      time.Now().Add(time.Hour),
	}

	bridge := &tokenBridge{src: oauth2.StaticTokenSource(tok), logger: slog.Default()}

	got, err := bridge.Token()
	require.NoError(t, err)
	assert.Equal(t, "bridge-token-123", got)
}

func TestTokenBridge_Error(t *testing.T) {
	// An expired token with no refresh mechanism should return an error.
	tok := &oauth2.Token{
		AccessToken: "expired",
		Expiry:      time.Now().Add(-time.Hour),
	}

	// StaticTokenSource ignores expiry, so use a Config.TokenSource
	// with a bad endpoint to force a refresh failure.
	cfg := &oauth2.Config{
		ClientID: "test",
		Endpoint: oauth2.Endpoint{TokenURL: "http://invalid.test/token"},
	}

	bridge := &tokenBridge{src: cfg.TokenSource(context.Background(), tok), logger: slog.Default()}

	_, err := bridge.Token()
	require.Error(t, err)
}

func TestOAuthConfig_OnTokenChange(t *testing.T) {
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "callback.json")

	cfg := oauthConfig(tokenPath, slog.Default())

	// Verify the callback is set.
	require.NotNil(t, cfg.OnTokenChange)

	// Simulate what ReuseTokenSource does: call OnTokenChange with a new token.
	newTok := &oauth2.Token{
		AccessToken:  "refreshed-access",
		RefreshToken: "refreshed-refresh",
		Expiry:       time.Now().Add(time.Hour),
	}

	cfg.OnTokenChange(newTok)

	// Verify the token was persisted to disk.
	loaded, err := loadToken(tokenPath)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "refreshed-access", loaded.AccessToken)
	assert.Equal(t, "refreshed-refresh", loaded.RefreshToken)
}

func TestOAuthConfig_Defaults(t *testing.T) {
	cfg := oauthConfig("/tmp/test.json", slog.Default())

	assert.Equal(t, defaultClientID, cfg.ClientID)
	assert.Equal(t, defaultScopes, cfg.Scopes)
	assert.NotEmpty(t, cfg.Endpoint.DeviceAuthURL)
	assert.NotEmpty(t, cfg.Endpoint.TokenURL)
}

func TestTokenPath_UsesProfileName(t *testing.T) {
	// Verify that oauthConfig passes through the token path correctly
	// by checking that OnTokenChange writes to the expected location.
	tmpDir := t.TempDir()

	paths := []string{"personal", "work", "shared"}
	for _, name := range paths {
		path := filepath.Join(tmpDir, "tokens", name+".json")
		cfg := oauthConfig(path, slog.Default())

		tok := &oauth2.Token{AccessToken: "tok-" + name}
		cfg.OnTokenChange(tok)

		loaded, err := loadToken(path)
		require.NoError(t, err, "token path %s", name)
		assert.Equal(t, "tok-"+name, loaded.AccessToken, "token path %s", name)
	}
}

func TestDoLogin_SaveError(t *testing.T) {
	// When saveToken fails (e.g., read-only directory), doLogin should return an error.
	endpoint := newMockOAuthServer(t, nil)
	tmpDir := t.TempDir()

	// Create a file where the tokens directory should be, so MkdirAll fails.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenFilePerms))

	tokenPath := filepath.Join(blocker, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	_, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saving token")
}

func TestDoLogin_DeviceAuthError(t *testing.T) {
	// DeviceAuth fails when the endpoint is unreachable.
	tokenPath := filepath.Join(t.TempDir(), "tokens", "test.json")

	cfg := &oauth2.Config{
		ClientID: defaultClientID,
		Scopes:   defaultScopes,
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: "http://127.0.0.1:1/devicecode",
			TokenURL:      "http://127.0.0.1:1/token",
		},
	}

	_, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device auth request failed")
}

func TestSaveToken_CreateTempError(t *testing.T) {
	tmpDir := t.TempDir()
	tokDir := filepath.Join(tmpDir, "tokens")
	require.NoError(t, os.MkdirAll(tokDir, dirPerms))

	// Make directory read-only so CreateTemp fails.
	require.NoError(t, os.Chmod(tokDir, 0o555))
	t.Cleanup(func() { os.Chmod(tokDir, dirPerms) })

	path := filepath.Join(tokDir, "token.json")
	tok := &oauth2.Token{AccessToken: "fail"}

	err := saveToken(path, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "creating temp file")
}

func TestLoadToken_ReadError(t *testing.T) {
	// Reading a directory as a file produces a non-ENOENT error.
	dir := t.TempDir()

	_, err := loadToken(dir)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "reading token file")
}

func TestSaveToken_MkdirAllError(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file where the directory should be, blocking MkdirAll.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenFilePerms))

	path := filepath.Join(blocker, "sub", "token.json")
	tok := &oauth2.Token{AccessToken: "fail"}

	err := saveToken(path, tok)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "creating token directory")
}

func TestOAuthConfig_OnTokenChangeError(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file where the tokens directory should be, so saveToken fails.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenFilePerms))

	tokenPath := filepath.Join(blocker, "sub", "callback.json")

	cfg := oauthConfig(tokenPath, slog.Default())

	// OnTokenChange should log a warning but not panic.
	newTok := &oauth2.Token{AccessToken: "will-fail"}
	cfg.OnTokenChange(newTok)

	// Verify nothing was written (since it failed). The token file's parent
	// can't be created (blocked by a regular file), so the path can't exist.
	_, statErr := os.Stat(tokenPath)
	assert.Error(t, statErr)
}

func TestSaveToken_JSONFormat(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "format.json")

	tok := &oauth2.Token{
		AccessToken:  "at",
		TokenType:    "Bearer",
		RefreshToken: "rt",
	}

	require.NoError(t, saveToken(path, tok))

	data, err := os.ReadFile(path)
	require.NoError(t, err)

	// Verify it's valid JSON and has expected fields.
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, "at", parsed["access_token"])
	assert.Equal(t, "Bearer", parsed["token_type"])
	assert.Equal(t, "rt", parsed["refresh_token"])
}

func TestLogin_Success(t *testing.T) {
	// Test the public Login function with a mock server.
	endpoint := newMockOAuthServer(t, nil)
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "login-public.json")

	// We need to override the endpoint used by Login. Since Login calls
	// oauthConfig internally, we test via doLogin (which is what Login delegates to).
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ts, err := doLogin(context.Background(), tokenPath, cfg, noopDisplay, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
}
