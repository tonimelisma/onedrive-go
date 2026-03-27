package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/trustedpath"
)

func TestUniqueAccounts(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):   {},
			driveid.MustCanonicalID("business:alice@example.com"):   {},
			driveid.MustCanonicalID("personal:bob@example.com"):     {},
			driveid.MustCanonicalID("business:charlie@example.com"): {},
		},
	}

	accounts := uniqueAccounts(cfg)

	// Should have 3 unique emails (alice appears twice but only counted once).
	assert.Len(t, accounts, 3)
	assert.Contains(t, accounts, "alice@example.com")
	assert.Contains(t, accounts, "bob@example.com")
	assert.Contains(t, accounts, "charlie@example.com")
}

func TestCanonicalIDForToken(t *testing.T) {
	tests := []struct {
		name     string
		account  string
		driveIDs []driveid.CanonicalID
		want     string
	}{
		{
			"personal drive",
			"alice@example.com",
			[]driveid.CanonicalID{driveid.MustCanonicalID("personal:alice@example.com")},
			"personal:alice@example.com",
		},
		{
			"business preferred over sharepoint",
			"alice@contoso.com",
			[]driveid.CanonicalID{
				driveid.MustCanonicalID("sharepoint:alice@contoso.com:site:lib"),
				driveid.MustCanonicalID("business:alice@contoso.com"),
			},
			"business:alice@contoso.com",
		},
		{
			"all sharepoint falls back to business prefix",
			"alice@contoso.com",
			[]driveid.CanonicalID{driveid.MustCanonicalID("sharepoint:alice@contoso.com:site:lib")},
			"business:alice@contoso.com",
		},
		{
			"empty returns zero",
			"nobody@example.com",
			nil,
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := canonicalIDForToken(tt.account, tt.driveIDs)
			assert.Equal(t, tt.want, got.String())
		})
	}
}

func TestDrivesForAccount(t *testing.T) {
	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:alice@example.com"):                  {},
			driveid.MustCanonicalID("business:alice@example.com"):                  {},
			driveid.MustCanonicalID("sharepoint:alice@example.com:marketing:Docs"): {},
			driveid.MustCanonicalID("personal:bob@example.com"):                    {},
		},
	}

	drives := drivesForAccount(cfg, "alice@example.com")

	assert.Len(t, drives, 3)
	assert.Contains(t, drives, driveid.MustCanonicalID("personal:alice@example.com"))
	assert.Contains(t, drives, driveid.MustCanonicalID("business:alice@example.com"))
	assert.Contains(t, drives, driveid.MustCanonicalID("sharepoint:alice@example.com:marketing:Docs"))
}

func TestFindTokenFallback(t *testing.T) {
	// findTokenFallback probes the filesystem for existing token files.
	// We need to create temp files matching the token path pattern.
	// Since DriveTokenPath uses XDG paths, we test the logic by checking
	// that it returns the correct prefix based on which file exists.

	// With no token files on disk, should default to personal.
	got := findTokenFallback("nobody@example.com", slog.Default())
	assert.Equal(t, driveid.MustCanonicalID("personal:nobody@example.com"), got)
}

func writeTokenFallbackFixture(t *testing.T, tokenID driveid.CanonicalID) {
	t.Helper()

	tokenPath := config.DriveTokenPath(tokenID)
	if tokenPath == "" {
		t.Skip("cannot determine token path on this platform")
	}

	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{}"), 0o600))
	t.Cleanup(func() { removeTestPath(t, tokenPath) })
}

func TestFindTokenFallback_ExistingToken(t *testing.T) {
	tests := []struct {
		name    string
		account string
		tokenID driveid.CanonicalID
	}{
		{
			name:    "PersonalExists",
			account: "test-fallback@example.com",
			tokenID: driveid.MustCanonicalID("personal:test-fallback@example.com"),
		},
		{
			name:    "BusinessExists",
			account: "test-fallback-biz@example.com",
			tokenID: driveid.MustCanonicalID("business:test-fallback-biz@example.com"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			writeTokenFallbackFixture(t, tt.tokenID)

			got := findTokenFallback(tt.account, slog.Default())
			assert.Equal(t, tt.tokenID, got)
		})
	}
}

func TestPrintWhoamiText(t *testing.T) {
	user := &graph.User{
		ID:          "user-789",
		DisplayName: "Test User",
		Email:       "test@example.com",
	}

	drives := []graph.Drive{
		{
			ID:         driveid.New("drive-abc"),
			Name:       "OneDrive",
			DriveType:  "personal",
			QuotaUsed:  1073741824, // 1 GB
			QuotaTotal: 5368709120, // 5 GB
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printWhoamiText(&buf, user, drives, nil))
	output := buf.String()

	assert.Contains(t, output, "Test User")
	assert.Contains(t, output, "test@example.com")
	assert.Contains(t, output, "user-789")
	assert.Contains(t, output, "OneDrive")
	assert.Contains(t, output, "personal")
	assert.Contains(t, output, "drive-abc")
}

func TestPrintLoginSuccess_DoesNotPanic(t *testing.T) {
	// Verify the print functions don't panic with various inputs.
	var buf bytes.Buffer
	require.NoError(t, printLoginSuccess(&buf, "personal", "toni@outlook.com", "", "personal:toni@outlook.com", "~/OneDrive"))
	require.NoError(t, printLoginSuccess(&buf, "business", "alice@contoso.com", "Contoso Ltd", "business:alice@contoso.com", "~/OneDrive - Contoso"))
	require.NoError(t, printLoginSuccess(&buf, "business", "bob@example.com", "", "business:bob@example.com", "~/OneDrive - Business"))
	require.NoError(t, printLoginSuccess(&buf, "documentLibrary", "carol@example.com", "", "documentLibrary:carol@example.com", "~/SP"))
}

func TestMoveToken_Success(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	src := filepath.Join(srcDir, "pending.json")
	dst := filepath.Join(dstDir, "nested", "token.json")

	require.NoError(t, os.WriteFile(src, []byte("token"), 0o600))
	require.NoError(t, moveToken(src, dst))

	data, err := trustedpath.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "token", string(data))

	_, statErr := os.Stat(src)
	assert.ErrorIs(t, statErr, fs.ErrNotExist)
}

func TestMoveToken_MissingSourceReturnsError(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "nested", "token.json")

	err := moveToken(filepath.Join(t.TempDir(), "missing.json"), dst)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "moving token to final path")
}

func TestValidateBrowserAuthURL(t *testing.T) {
	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{
			name:   "microsoft login host allowed",
			rawURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=test",
		},
		{
			name:   "loopback allowed for tests",
			rawURL: "http://127.0.0.1:8080/callback",
		},
		{
			name:   "localhost https allowed",
			rawURL: "https://localhost:8443/callback",
		},
		{
			name:    "rejects insecure remote host",
			rawURL:  "http://login.microsoftonline.com/common/oauth2/v2.0/authorize",
			wantErr: "must use https",
		},
		{
			name:    "rejects userinfo",
			rawURL:  "https://user@login.microsoftonline.com/common/oauth2/v2.0/authorize",
			wantErr: "must not contain userinfo",
		},
		{
			name:    "rejects empty host",
			rawURL:  "https:///callback",
			wantErr: "host is empty",
		},
		{
			name:    "rejects loopback with non-http scheme",
			rawURL:  "ftp://127.0.0.1:8080/callback",
			wantErr: "loopback host must use http or https",
		},
		{
			name:    "rejects untrusted host",
			rawURL:  "https://evil.example.com/callback",
			wantErr: "is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateBrowserAuthURL(tt.rawURL)
			if tt.wantErr == "" {
				require.NoError(t, err)
				assert.Equal(t, tt.rawURL, got)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestPendingTokenPath(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	assert.Equal(t, filepath.Join(config.DefaultDataDir(), pendingTokenFile), pendingTokenPath())
}

func TestBrowserHostAllowed(t *testing.T) {
	assert.True(t, browserHostAllowed("login.microsoftonline.com"))
	assert.True(t, browserHostAllowed("tenant.login.microsoftonline.com"))
	assert.True(t, browserHostAllowed("tenant.login.live.com"))
	assert.False(t, browserHostAllowed("example.com"))
}

func TestIsLoopbackBrowserHost(t *testing.T) {
	assert.True(t, isLoopbackBrowserHost("localhost"))
	assert.True(t, isLoopbackBrowserHost("127.0.0.1"))
	assert.True(t, isLoopbackBrowserHost("::1"))
	assert.False(t, isLoopbackBrowserHost("192.168.1.10"))
}

func TestBrowserOpenCommand(t *testing.T) {
	command, err := browserOpenCommand("darwin")
	require.NoError(t, err)
	assert.Equal(t, "open", command)

	command, err = browserOpenCommand("linux")
	require.NoError(t, err)
	assert.Equal(t, "xdg-open", command)

	_, err = browserOpenCommand("windows")
	require.Error(t, err)
}

func TestOpenBrowser_RejectsUntrustedURL(t *testing.T) {
	err := openBrowser(context.Background(), "https://evil.example.com/callback?access_token=secret-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not allowed")
	assert.NotContains(t, err.Error(), "secret-token")
}

func TestOpenBrowser_StartsValidatedCommand(t *testing.T) {
	command, err := browserOpenCommand(runtime.GOOS)
	if err != nil {
		t.Skipf("unsupported platform for browser launch test: %v", err)
	}

	const executablePerms = 0o755
	const authURL = "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=test"

	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "browser-url.txt")
	scriptPath := filepath.Join(tempDir, command)
	script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$CODEX_BROWSER_OUT\"\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), executablePerms))

	t.Setenv("PATH", tempDir)
	t.Setenv("CODEX_BROWSER_OUT", outputPath)

	require.NoError(t, openBrowser(t.Context(), authURL))

	require.Eventually(t, func() bool {
		data, readErr := trustedpath.ReadFile(outputPath)
		return readErr == nil && string(data) == authURL
	}, 5*time.Second, 25*time.Millisecond)
}

func TestOpenBrowser_CommandStartFailure(t *testing.T) {
	_, err := browserOpenCommand(runtime.GOOS)
	if err != nil {
		t.Skipf("unsupported platform for browser launch test: %v", err)
	}

	t.Setenv("PATH", t.TempDir())

	err = openBrowser(t.Context(), "https://login.microsoftonline.com/common/oauth2/v2.0/authorize?client_id=test&nonce=secret-token")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "start browser command")
	assert.NotContains(t, err.Error(), "secret-token")
}

// Validates: R-3.1.4
func TestResolveLogoutAccount_FallbackToProfiles(t *testing.T) {
	// When config is empty but orphaned account profiles exist, --purge should
	// auto-select the single orphan.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create an orphaned account profile (no token).
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "account_personal_alice@outlook.com.json"),
		[]byte(`{"profile":{"user_id":"u1","display_name":"Alice"}}`), 0o600,
	))

	cfg := config.DefaultConfig()
	logger := slog.Default()

	// With purge=true, should auto-select the single orphaned account.
	email, err := resolveLogoutAccount(cfg, "", true, logger)
	require.NoError(t, err)
	assert.Equal(t, "alice@outlook.com", email)
}

// Validates: R-3.1.4
func TestResolveLogoutAccount_NoPurgeShowsOrphans(t *testing.T) {
	// Without --purge, should show an error listing orphaned accounts.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "account_personal_alice@outlook.com.json"),
		[]byte(`{"profile":{"user_id":"u1"}}`), 0o600,
	))

	cfg := config.DefaultConfig()
	logger := slog.Default()

	_, err := resolveLogoutAccount(cfg, "", false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "orphaned data remains for")
	assert.Contains(t, err.Error(), "alice@outlook.com")
}

// Validates: R-3.1.5
func TestFindLoggedOutAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Account profile without token → logged out.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "account_personal_alice@outlook.com.json"),
		[]byte(`{"profile":{"user_id":"u1","display_name":"Alice Smith"}}`), 0o600,
	))

	// Account profile WITH token → still authenticated.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "account_business_bob@contoso.com.json"),
		[]byte(`{"profile":{"user_id":"u2","display_name":"Bob Jones"}}`), 0o600,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "token_business_bob@contoso.com.json"),
		[]byte(`{}`), 0o600,
	))

	// Also create a state DB for alice to verify the count.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "state_personal_alice@outlook.com.db"),
		[]byte{}, 0o600,
	))

	cfg := config.DefaultConfig()
	logger := slog.Default()

	loggedOut := findLoggedOutAccounts(cfg, "", logger)
	require.Len(t, loggedOut, 1)
	assert.Equal(t, "alice@outlook.com", loggedOut[0].Email)
	assert.Equal(t, "personal", loggedOut[0].DriveType)
	assert.Equal(t, "Alice Smith", loggedOut[0].DisplayName)
	assert.Equal(t, 1, loggedOut[0].StateDBs)
}

// Validates: R-3.1.4
func TestPurgeOrphanedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create orphaned files for alice.
	files := []string{
		"state_personal_alice@outlook.com.db",
		"drive_personal_alice@outlook.com.json",
		"account_personal_alice@outlook.com.json",
	}
	for _, f := range files {
		require.NoError(t, os.WriteFile(filepath.Join(dataDir, f), []byte(`{}`), 0o600))
	}

	// Also create a file for bob — should NOT be removed.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "state_personal_bob@outlook.com.db"),
		[]byte{}, 0o600,
	))

	logger := slog.Default()
	err := purgeOrphanedFiles("alice@outlook.com", logger)
	require.NoError(t, err)

	// Alice's files should be gone.
	for _, f := range files {
		_, statErr := os.Stat(filepath.Join(dataDir, f))
		assert.True(t, os.IsNotExist(statErr), "expected %s to be deleted", f)
	}

	// Bob's file should remain.
	_, statErr := os.Stat(filepath.Join(dataDir, "state_personal_bob@outlook.com.db"))
	assert.NoError(t, statErr, "bob's state DB should remain")
}

// Validates: R-3.1.6
func TestPrintWhoamiJSON(t *testing.T) {
	t.Parallel()

	user := &graph.User{
		ID:          "user-123",
		DisplayName: "Alice Smith",
		Email:       "alice@example.com",
	}

	drives := []graph.Drive{
		{
			ID:         driveid.New("drive-abc"),
			Name:       "OneDrive",
			DriveType:  "personal",
			QuotaUsed:  1073741824,
			QuotaTotal: 5368709120,
		},
	}

	loggedOut := []loggedOutAccount{
		{
			Email:       "bob@example.com",
			DriveType:   "business",
			DisplayName: "Bob Jones",
			StateDBs:    1,
		},
	}

	var buf bytes.Buffer
	err := printWhoamiJSON(&buf, user, drives, loggedOut)
	require.NoError(t, err)

	var decoded whoamiOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))

	require.NotNil(t, decoded.User)
	assert.Equal(t, "user-123", decoded.User.ID)
	assert.Equal(t, "Alice Smith", decoded.User.DisplayName)
	assert.Equal(t, "alice@example.com", decoded.User.Email)

	require.Len(t, decoded.Drives, 1)
	assert.Contains(t, decoded.Drives[0].ID, "drive-abc")
	assert.Equal(t, "OneDrive", decoded.Drives[0].Name)
	assert.Equal(t, "personal", decoded.Drives[0].DriveType)
	assert.Equal(t, int64(1073741824), decoded.Drives[0].QuotaUsed)

	require.Len(t, decoded.LoggedOutAccounts, 1)
	assert.Equal(t, "bob@example.com", decoded.LoggedOutAccounts[0].Email)
}

// Validates: R-3.1.6
func TestPrintWhoamiJSON_LoggedOutOnly(t *testing.T) {
	t.Parallel()

	loggedOut := []loggedOutAccount{
		{
			Email:     "carol@outlook.com",
			DriveType: "personal",
			StateDBs:  2,
		},
	}

	var buf bytes.Buffer
	err := printWhoamiJSON(&buf, nil, nil, loggedOut)
	require.NoError(t, err)

	var decoded whoamiOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))

	assert.Nil(t, decoded.User)
	assert.Empty(t, decoded.Drives)
	require.Len(t, decoded.LoggedOutAccounts, 1)
	assert.Equal(t, "carol@outlook.com", decoded.LoggedOutAccounts[0].Email)
}

// Validates: R-3.1.6
func TestPrintWhoamiJSON_Empty(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	err := printWhoamiJSON(&buf, nil, nil, nil)
	require.NoError(t, err)

	var decoded whoamiOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))

	assert.Nil(t, decoded.User)
	assert.Empty(t, decoded.Drives)
	assert.Empty(t, decoded.LoggedOutAccounts)
}

// Validates: R-3.1.5
func TestPrintLoggedOutAccountsText(t *testing.T) {
	var buf bytes.Buffer

	loggedOut := []loggedOutAccount{
		{
			Email:       "alice@outlook.com",
			DriveType:   "personal",
			DisplayName: "Alice Smith",
			StateDBs:    2,
		},
		{
			Email:     "bob@contoso.com",
			DriveType: "business",
			StateDBs:  0,
		},
	}

	require.NoError(t, printLoggedOutAccountsText(&buf, loggedOut))
	output := buf.String()

	assert.Contains(t, output, "Logged out accounts:")
	assert.Contains(t, output, "Alice Smith (alice@outlook.com)")
	assert.Contains(t, output, "2 state databases")
	assert.Contains(t, output, "bob@contoso.com")
	assert.Contains(t, output, "no state databases")
	assert.Contains(t, output, "logout --purge")
}
