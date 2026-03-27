package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
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
		_, err := w.Write([]byte(testDeviceCodeJSON))
		assert.NoError(t, err)
	})

	handler := tokenHandler
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(testTokenJSON))
			assert.NoError(t, err)
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
	ts, err := doLogin(t.Context(), tokenPath, cfg, func(da DeviceAuth) {
		displayed = da
	}, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	// Verify display callback was called with correct values.
	assert.Equal(t, "ABCD-1234", displayed.UserCode)
	assert.Equal(t, "https://microsoft.com/devicelogin", displayed.VerificationURI)

	// Verify token was saved to disk.
	loaded, loadErr := tokenfile.Load(tokenPath)
	require.NoError(t, loadErr)
	require.NotNil(t, loaded)
	assert.Equal(t, "test-access-token", loaded.AccessToken)
	assert.Equal(t, "test-refresh-token", loaded.RefreshToken)

	// Verify the returned TokenSource works.
	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
}

func TestDoLogin_TokenEndpointErrors(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantMessage string
	}{
		{
			name:        "user declined",
			body:        `{"error":"access_denied","error_description":"user declined"}`,
			wantMessage: "access_denied",
		},
		{
			name:        "expired code",
			body:        `{"error":"expired_token","error_description":"device code expired"}`,
			wantMessage: "expired_token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, err := w.Write([]byte(tt.body))
				assert.NoError(t, err)
			})

			tmpDir := t.TempDir()
			tokenPath := filepath.Join(tmpDir, "tokens", "test.json")
			cfg := testOAuthConfig(t, tokenPath, endpoint)

			_, err := doLogin(t.Context(), tokenPath, cfg, noopDisplay, slog.Default())
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantMessage)
		})
	}
}

func TestDoLogin_ContextCancel(t *testing.T) {
	endpoint := newMockOAuthServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"error":"authorization_pending"}`))
		assert.NoError(t, err)
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
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
			_, err := w.Write([]byte(`{"error":"authorization_pending"}`))
			assert.NoError(t, err)

			return
		}

		_, err := w.Write([]byte(testTokenJSON))
		assert.NoError(t, err)
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "pending.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ts, err := doLogin(t.Context(), tokenPath, cfg, noopDisplay, slog.Default())
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

	err := tokenfile.Save(path, original)
	require.NoError(t, err)

	loaded, err := tokenfile.Load(path)
	require.NoError(t, err)
	require.NotNil(t, loaded)

	assert.Equal(t, original.AccessToken, loaded.AccessToken)
	assert.Equal(t, original.TokenType, loaded.TokenType)
	assert.Equal(t, original.RefreshToken, loaded.RefreshToken)
	// JSON time encoding may lose sub-second precision; compare truncated.
	assert.True(t, original.Expiry.Equal(loaded.Expiry.Truncate(time.Second)),
		"expiry mismatch: want %v, got %v", original.Expiry, loaded.Expiry)
}

func TestLoadToken_BareTokenRejected(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "old-format.json")

	// Bare oauth2.Token without the {"token": ...} wrapper — rejected.
	oldTokenData := map[string]string{
		"access_" + "token":  "old-token",
		"refresh_" + "token": "old-refresh",
		"token_" + "type":    "Bearer",
	}

	oldToken, err := json.MarshalIndent(oldTokenData, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, oldToken, tokenfile.FilePerms))

	_, err = tokenfile.Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decoding")
}

func TestSaveToken_Permissions(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "perms.json")

	tok := &oauth2.Token{AccessToken: "secret"}
	err := tokenfile.Save(path, tok)
	require.NoError(t, err)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(tokenfile.FilePerms), info.Mode().Perm())
}

func TestSaveToken_AtomicWrite(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "atomic.json")

	// Write initial token.
	tok1 := &oauth2.Token{AccessToken: "first", RefreshToken: "r1"}
	require.NoError(t, tokenfile.Save(path, tok1))

	// Overwrite with second token.
	tok2 := &oauth2.Token{AccessToken: "second", RefreshToken: "r2"}
	require.NoError(t, tokenfile.Save(path, tok2))

	// Verify the final file has the second token (not corrupted).
	loaded, err := tokenfile.Load(path)
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
	err := tokenfile.Save(path, tok)
	require.NoError(t, err)

	loaded, err := tokenfile.Load(path)
	require.NoError(t, err)
	assert.Equal(t, "nested", loaded.AccessToken)
}

func TestLoadToken_NoFile(t *testing.T) {
	tok, err := tokenfile.Load(filepath.Join(t.TempDir(), "nonexistent.json"))
	require.ErrorIs(t, err, tokenfile.ErrNotFound)
	assert.Nil(t, tok)
}

func TestLoadToken_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "bad.json")

	require.NoError(t, os.WriteFile(path, []byte("not json"), tokenfile.FilePerms))

	_, err := tokenfile.Load(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenfile: decoding")
}

func TestTokenSourceFromPath_NoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nonexistent.json")

	_, err := TokenSourceFromPath(t.Context(), path, slog.Default())
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

	require.NoError(t, tokenfile.Save(path, tok))

	ts, err := TokenSourceFromPath(t.Context(), path, slog.Default())
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

	require.NoError(t, os.WriteFile(path, []byte("not valid json"), tokenfile.FilePerms))

	_, err := TokenSourceFromPath(t.Context(), path, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenfile: decoding")
}

func TestLogout_RemovesFile(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "tokens", "logout.json")

	// Create a token file to delete.
	tok := &oauth2.Token{AccessToken: "doomed"}
	require.NoError(t, tokenfile.Save(path, tok))

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
	tokenSrv := httptest.NewServer(http.NotFoundHandler())
	tokenURL := tokenSrv.URL + "/token"
	tokenSrv.Close()

	cfg := &oauth2.Config{
		ClientID: "test",
		Endpoint: oauth2.Endpoint{TokenURL: tokenURL},
	}

	bridge := &tokenBridge{src: cfg.TokenSource(t.Context(), tok), logger: slog.Default()}

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
	loaded, err := tokenfile.Load(tokenPath)
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, "refreshed-access", loaded.AccessToken)
	assert.Equal(t, "refreshed-refresh", loaded.RefreshToken)
}

func TestOAuthConfig_Defaults(t *testing.T) {
	cfg := oauthConfig("/tmp/test.json", slog.Default())

	assert.Equal(t, defaultClientID, cfg.ClientID)
	assert.Equal(t, defaultScopes(), cfg.Scopes)
	assert.NotEmpty(t, cfg.Endpoint.DeviceAuthURL)
	assert.NotEmpty(t, cfg.Endpoint.TokenURL)
}

func TestTokenPath_UsesDriveName(t *testing.T) {
	// Verify that oauthConfig passes through the token path correctly
	// by checking that OnTokenChange writes to the expected location.
	tmpDir := t.TempDir()

	paths := []string{"personal", "work", "shared"}
	for _, name := range paths {
		path := filepath.Join(tmpDir, "tokens", name+".json")
		cfg := oauthConfig(path, slog.Default())

		tok := &oauth2.Token{AccessToken: "tok-" + name}
		cfg.OnTokenChange(tok)

		loaded, err := tokenfile.Load(path)
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
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenfile.FilePerms))

	tokenPath := filepath.Join(blocker, "tokens", "test.json")
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	_, err := doLogin(t.Context(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saving token")
}

func TestDoLogin_DeviceAuthError(t *testing.T) {
	// DeviceAuth fails when the endpoint is unreachable.
	tokenPath := filepath.Join(t.TempDir(), "tokens", "test.json")
	deviceSrv := httptest.NewServer(http.NotFoundHandler())
	deviceAuthURL := deviceSrv.URL + "/devicecode"
	tokenURL := deviceSrv.URL + "/token"
	deviceSrv.Close()

	cfg := &oauth2.Config{
		ClientID: defaultClientID,
		Scopes:   defaultScopes(),
		Endpoint: oauth2.Endpoint{
			DeviceAuthURL: deviceAuthURL,
			TokenURL:      tokenURL,
		},
	}

	_, err := doLogin(t.Context(), tokenPath, cfg, noopDisplay, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "device auth request failed")
}

func TestSaveToken_CreateTempError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	tmpDir := t.TempDir()
	tokDir := filepath.Join(tmpDir, "tokens")
	require.NoError(t, os.MkdirAll(tokDir, tokenfile.DirPerms))

	// Make directory read-only so CreateTemp fails.
	setTestDirPermissions(t, tokDir, 0o555)
	t.Cleanup(func() {
		setTestDirPermissions(t, tokDir, tokenfile.DirPerms)
	})

	path := filepath.Join(tokDir, "token.json")
	tok := &oauth2.Token{AccessToken: "fail"}

	err := tokenfile.Save(path, tok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating temp file")
}

func TestLoadToken_ReadError(t *testing.T) {
	// Reading a directory as a file produces a non-ENOENT error.
	dir := t.TempDir()

	_, err := tokenfile.Load(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenfile: reading")
}

func TestSaveToken_MkdirAllError(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file where the directory should be, blocking MkdirAll.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenfile.FilePerms))

	path := filepath.Join(blocker, "sub", "token.json")
	tok := &oauth2.Token{AccessToken: "fail"}

	err := tokenfile.Save(path, tok)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tokenfile: creating directory")
}

func TestOAuthConfig_OnTokenChangeError(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a file where the tokens directory should be, so saveToken fails.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenfile.FilePerms))

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

	require.NoError(t, tokenfile.Save(path, tok))

	data, err := trustedpath.ReadFile(path)
	require.NoError(t, err)

	// Verify it's valid JSON with the new wrapper format.
	var parsed map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &parsed))

	// Token is now nested under "token" key.
	tokenMap, ok := parsed["token"].(map[string]interface{})
	require.True(t, ok, "expected 'token' key in JSON")
	assert.Equal(t, "at", tokenMap["access_token"])
	assert.Equal(t, "Bearer", tokenMap["token_type"])
	assert.Equal(t, "rt", tokenMap["refresh_token"])
}

func TestLogin_Success(t *testing.T) {
	// Test the public Login function with a mock server.
	endpoint := newMockOAuthServer(t, nil)
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "login-public.json")

	// We need to override the endpoint used by Login. Since Login calls
	// oauthConfig internally, we test via doLogin (which is what Login delegates to).
	cfg := testOAuthConfig(t, tokenPath, endpoint)

	ts, err := doLogin(t.Context(), tokenPath, cfg, noopDisplay, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
}

// --- Authorization Code + PKCE Flow Tests ---

// newMockAuthCodeServer creates a test server that handles authorization + token
// endpoints for the auth code flow. The authorize endpoint redirects to the
// callback URL with the code and state. tokenHandler controls the token endpoint.
func newMockAuthCodeServer(t *testing.T, tokenHandler http.HandlerFunc) *oauth2.Endpoint {
	t.Helper()

	mux := http.NewServeMux()

	// Authorization endpoint: redirects to the callback URL with code + state.
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirectURI := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		// Return the code via redirect to the callback server.
		callback := redirectURI + "?code=test-auth-code&state=" + url.QueryEscape(state)
		http.Redirect(w, r, callback, http.StatusFound)
	})

	handler := tokenHandler
	if handler == nil {
		handler = func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, err := w.Write([]byte(testTokenJSON))
			assert.NoError(t, err)
		}
	}

	mux.HandleFunc("POST /token", handler)

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &oauth2.Endpoint{
		AuthURL:  srv.URL + "/authorize",
		TokenURL: srv.URL + "/token",
	}
}

// testAuthCodeConfig builds a test config for auth code flow.
func testAuthCodeConfig(t *testing.T, tokenPath string, endpoint *oauth2.Endpoint) *oauth2.Config {
	t.Helper()

	cfg := oauthConfig(tokenPath, slog.Default())
	cfg.Endpoint = *endpoint

	return cfg
}

// simulateBrowserCallback acts as the browser: fetches the auth URL which
// redirects to the localhost callback server, delivering the code.
func simulateBrowserCallback(t *testing.T) func(context.Context, string) error {
	t.Helper()

	// Use an HTTP client that doesn't follow redirects automatically — we need
	// to follow the redirect ourselves to hit the localhost callback server.
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return func(ctx context.Context, authURL string) error {
		// Step 1: Hit the authorize endpoint.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, http.NoBody)
		require.NoError(t, err, "failed to create authorize request")

		resp, err := client.Do(req) //nolint:gosec // Test request targets the local httptest auth server.
		require.NoError(t, err, "failed to hit authorize endpoint")
		require.NoError(t, resp.Body.Close())

		// Step 2: Follow the redirect to the localhost callback.
		location := resp.Header.Get("Location")
		require.NotEmpty(t, location, "authorize endpoint must redirect")

		callbackReq, err := http.NewRequestWithContext(ctx, http.MethodGet, location, http.NoBody)
		require.NoError(t, err, "failed to create callback request")

		callbackResp, err := http.DefaultClient.Do(callbackReq) //nolint:gosec // Test request targets the local callback server.
		require.NoError(t, err, "failed to hit callback")
		require.NoError(t, callbackResp.Body.Close())

		return nil
	}
}

func TestDoAuthCodeLogin_Success(t *testing.T) {
	endpoint := newMockAuthCodeServer(t, nil)
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "authcode.json")

	cfg := testAuthCodeConfig(t, tokenPath, endpoint)
	openURL := simulateBrowserCallback(t)

	ts, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	// Verify token was saved to disk.
	loaded, loadErr := tokenfile.Load(tokenPath)
	require.NoError(t, loadErr)
	require.NotNil(t, loaded)
	assert.Equal(t, "test-access-token", loaded.AccessToken)
	assert.Equal(t, "test-refresh-token", loaded.RefreshToken)

	// Verify the returned TokenSource works.
	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
}

func TestDoAuthCodeLogin_InvalidState(t *testing.T) {
	// Set up a server that returns a mismatched state value.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirectURI := r.URL.Query().Get("redirect_uri")
		// Return a WRONG state value to simulate CSRF.
		callback := redirectURI + "?code=test-auth-code&state=wrong-state-value"
		http.Redirect(w, r, callback, http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testTokenJSON))
		assert.NoError(t, err)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoint := &oauth2.Endpoint{
		AuthURL:  srv.URL + "/authorize",
		TokenURL: srv.URL + "/token",
	}

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "csrf.json")
	cfg := testAuthCodeConfig(t, tokenPath, endpoint)
	openURL := simulateBrowserCallback(t)

	_, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "state mismatch")
}

func TestDoAuthCodeLogin_ContextCancel(t *testing.T) {
	// Set up a server that never redirects — simulates user not completing auth.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, _ *http.Request) {
		// Don't redirect — just return 200. The callback never fires.
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testTokenJSON))
		assert.NoError(t, err)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoint := &oauth2.Endpoint{
		AuthURL:  srv.URL + "/authorize",
		TokenURL: srv.URL + "/token",
	}

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "cancel.json")
	cfg := testAuthCodeConfig(t, tokenPath, endpoint)

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()

	openURL := func(ctx context.Context, authURL string) error {
		// Just hit the authorize endpoint but don't follow redirects.
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, http.NoBody)
		if err != nil {
			return fmt.Errorf("build authorize request: %w", err)
		}

		resp, err := http.DefaultClient.Do(req) //nolint:gosec // Test request targets the local httptest auth server.
		if err == nil {
			require.NoError(t, resp.Body.Close())
		}

		return nil
	}

	_, err := doAuthCodeLogin(ctx, tokenPath, cfg, openURL, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "browser auth canceled")
}

func TestDoAuthCodeLogin_MissingCode(t *testing.T) {
	// Server redirects without a code parameter.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /authorize", func(w http.ResponseWriter, r *http.Request) {
		redirectURI := r.URL.Query().Get("redirect_uri")
		state := r.URL.Query().Get("state")
		// Redirect without a code — just state.
		callback := redirectURI + "?state=" + url.QueryEscape(state)
		http.Redirect(w, r, callback, http.StatusFound)
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, err := w.Write([]byte(testTokenJSON))
		assert.NoError(t, err)
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	endpoint := &oauth2.Endpoint{
		AuthURL:  srv.URL + "/authorize",
		TokenURL: srv.URL + "/token",
	}

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "nocode.json")
	cfg := testAuthCodeConfig(t, tokenPath, endpoint)
	openURL := simulateBrowserCallback(t)

	_, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing authorization code")
}

func TestDoAuthCodeLogin_ExchangeError(t *testing.T) {
	// Token endpoint returns an error on exchange.
	endpoint := newMockAuthCodeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, err := w.Write([]byte(`{"error":"invalid_grant","error_description":"code expired"}`))
		assert.NoError(t, err)
	})

	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "exchange-fail.json")
	cfg := testAuthCodeConfig(t, tokenPath, endpoint)
	openURL := simulateBrowserCallback(t)

	_, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token exchange failed")
}

func TestDoAuthCodeLogin_SaveError(t *testing.T) {
	// Token exchange succeeds but saving fails.
	endpoint := newMockAuthCodeServer(t, nil)
	tmpDir := t.TempDir()

	// Create a file where the tokens directory should be, so MkdirAll fails.
	blocker := filepath.Join(tmpDir, "blocked")
	require.NoError(t, os.WriteFile(blocker, []byte("x"), tokenfile.FilePerms))

	tokenPath := filepath.Join(blocker, "tokens", "test.json")
	cfg := testAuthCodeConfig(t, tokenPath, endpoint)
	openURL := simulateBrowserCallback(t)

	_, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "saving token")
}

func TestDoAuthCodeLogin_OpenURLFails(t *testing.T) {
	// openURL returns an error — should fall back to printing the URL
	// and still complete the flow if the callback eventually fires.
	endpoint := newMockAuthCodeServer(t, nil)
	tmpDir := t.TempDir()
	tokenPath := filepath.Join(tmpDir, "tokens", "fallback.json")

	cfg := testAuthCodeConfig(t, tokenPath, endpoint)

	// openURL fails, but we still need to simulate the browser hitting the
	// callback. Parse the auth URL and do the redirect manually.
	browserSim := simulateBrowserCallback(t)
	browserErrCh := make(chan error, 1)
	openURL := func(ctx context.Context, authURL string) error {
		// Simulate browser callback in background despite the "error".
		go func() {
			browserErrCh <- browserSim(ctx, authURL)
		}()
		return fmt.Errorf("browser open failed")
	}

	ts, err := doAuthCodeLogin(t.Context(), tokenPath, cfg, openURL, slog.Default())
	require.NoError(t, err)
	require.NotNil(t, ts)

	tok, tokenErr := ts.Token()
	require.NoError(t, tokenErr)
	assert.Equal(t, "test-access-token", tok)
	require.NoError(t, <-browserErrCh)
}

func TestGenerateState(t *testing.T) {
	state1, err := generateState()
	require.NoError(t, err)
	assert.Len(t, state1, stateTokenBytes*2) // hex encoding doubles the length

	state2, err := generateState()
	require.NoError(t, err)
	assert.NotEqual(t, state1, state2, "consecutive states should differ")
}
