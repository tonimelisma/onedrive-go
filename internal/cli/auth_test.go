package cli

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	authTestUserID1          = "u1"
	authTestDisplayNameAlice = "Alice"
)

func TestAccountLifecycle(t *testing.T) {
	tests := []struct {
		name  string
		entry accountView
		want  accountLifecycleView
	}{
		{
			name: "configured usable login",
			entry: accountView{
				Email:      "ready@example.com",
				Configured: true,
			},
			want: accountLifecycleView{
				State:               accountLifecycleLoggedInWithConfigured,
				Known:               true,
				HasUsableSavedLogin: true,
				HasConfiguredDrives: true,
				SelectableForLogout: true,
				SelectableForPurge:  true,
			},
		},
		{
			name: "usable login without configured drives",
			entry: accountView{
				Email: "discovered@example.com",
			},
			want: accountLifecycleView{
				State:               accountLifecycleLoggedInWithoutConfigured,
				Known:               true,
				HasUsableSavedLogin: true,
				SelectableForLogout: true,
				SelectableForPurge:  true,
			},
		},
		{
			name: "missing login",
			entry: accountView{
				Email:            "missing@example.com",
				SavedLoginReason: authReasonMissingLogin,
			},
			want: accountLifecycleView{
				State:              accountLifecycleAuthRequiredMissingLogin,
				Known:              true,
				SelectableForPurge: true,
			},
		},
		{
			name: "invalid login",
			entry: accountView{
				Email:            "invalid@example.com",
				SavedLoginReason: authReasonInvalidSavedLogin,
			},
			want: accountLifecycleView{
				State:              accountLifecycleAuthRequiredInvalidLogin,
				Known:              true,
				SelectableForPurge: true,
			},
		},
		{
			name: "persisted auth rejection",
			entry: accountView{
				Email:                 "rejected@example.com",
				AuthRequirementReason: authReasonSyncAuthRejected,
			},
			want: accountLifecycleView{
				State:               accountLifecycleAuthRequiredSyncRejected,
				Known:               true,
				HasUsableSavedLogin: true,
				SelectableForLogout: true,
				SelectableForPurge:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, accountLifecycle(&tt.entry))
		})
	}
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

type fakeLiveDriveCatalogClient struct {
	drives     []graph.Drive
	drivesErr  error
	primary    *graph.Drive
	primaryErr error
}

func (f fakeLiveDriveCatalogClient) Drives(context.Context) ([]graph.Drive, error) {
	return f.drives, f.drivesErr
}

func (f fakeLiveDriveCatalogClient) PrimaryDrive(context.Context) (*graph.Drive, error) {
	return f.primary, f.primaryErr
}

func TestDiscoverLiveDriveCatalog_DegradesToPrimaryDrive(t *testing.T) {
	result := discoverLiveDriveCatalog(
		t.Context(),
		fakeLiveDriveCatalogClient{
			drivesErr: graph.ErrForbidden,
			primary: &graph.Drive{
				ID:        driveid.New("drive-primary"),
				Name:      "OneDrive",
				DriveType: driveid.DriveTypePersonal,
			},
		},
		"user@example.com",
		"Test User",
		driveid.DriveTypePersonal,
		slog.Default(),
	)
	assert.Equal(t, accountAuthHealth{}, result.AuthHealth)
	require.Len(t, result.LiveDrives, 1)
	assert.Equal(t, "OneDrive", result.LiveDrives[0].Name)
	require.NotNil(t, result.Degraded)
	assert.Equal(t, "user@example.com", result.Degraded.Email)
	assert.Equal(t, driveCatalogUnavailableReason, result.Degraded.Reason)
}

func TestDiscoverLiveDriveCatalog_DegradesWithoutPrimaryDrive(t *testing.T) {
	result := discoverLiveDriveCatalog(
		t.Context(),
		fakeLiveDriveCatalogClient{
			drivesErr:  graph.ErrForbidden,
			primaryErr: graph.ErrForbidden,
		},
		"user@example.com",
		"Test User",
		driveid.DriveTypeBusiness,
		slog.Default(),
	)
	assert.Equal(t, accountAuthHealth{}, result.AuthHealth)
	assert.Empty(t, result.LiveDrives)
	require.NotNil(t, result.Degraded)
	assert.Equal(t, driveid.DriveTypeBusiness, result.Degraded.DriveType)
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

	data, err := localpath.ReadFile(dst)
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
	const browserLaunchTimeout = 15 * time.Second

	tempDir := t.TempDir()
	outputPath := filepath.Join(tempDir, "browser-url.txt")
	scriptPath := filepath.Join(tempDir, command)
	script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$CODEX_BROWSER_OUT\"\n"
	require.NoError(t, os.WriteFile(scriptPath, []byte(script), executablePerms))

	t.Setenv("PATH", tempDir)
	t.Setenv("CODEX_BROWSER_OUT", outputPath)

	require.NoError(t, openBrowser(t.Context(), authURL))

	require.Eventually(t, func() bool {
		data, readErr := localpath.ReadFile(outputPath)
		return readErr == nil && string(data) == authURL
	}, browserLaunchTimeout, 25*time.Millisecond)
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
func TestResolveLogoutAccount_PurgeAutoSelectsSingleKnownAccountWithoutSavedLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	seedCatalogAccount(t, driveid.MustCanonicalID("personal:alice@outlook.com"), func(account *config.CatalogAccount) {
		account.UserID = authTestUserID1
		account.DisplayName = authTestDisplayNameAlice
	})

	cfg := config.DefaultConfig()
	logger := slog.Default()

	email, err := resolveLogoutAccount(cfg, true, logger)
	require.NoError(t, err)
	assert.Equal(t, "alice@outlook.com", email)
}

// Validates: R-3.1.3
func TestResolveLogoutAccount_PlainLogoutRequiresUsableSavedLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	seedCatalogAccount(t, driveid.MustCanonicalID("personal:alice@outlook.com"), func(account *config.CatalogAccount) {
		account.UserID = authTestUserID1
	})

	cfg := config.DefaultConfig()
	logger := slog.Default()

	_, err := resolveLogoutAccount(cfg, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts with saved logins are available for plain logout")
	assert.Contains(t, err.Error(), "alice@outlook.com")
}

// Validates: R-3.1.3
func TestResolveLogoutAccount_AutoSelectsSingleKnownAccountWithUsableSavedLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_alice@outlook.com.json")

	cfg := config.DefaultConfig()
	logger := slog.Default()

	email, err := resolveLogoutAccount(cfg, false, logger)
	require.NoError(t, err)
	assert.Equal(t, "alice@outlook.com", email)
}

// Validates: R-3.1.3
func TestResolveLogoutAccount_MultipleUsableSavedLoginsRequireAccount(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_alice@outlook.com.json")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_bob@contoso.com.json")

	cfg := config.DefaultConfig()
	logger := slog.Default()

	_, err := resolveLogoutAccount(cfg, false, logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple accounts with saved logins")
	assert.Contains(t, err.Error(), "alice@outlook.com")
	assert.Contains(t, err.Error(), "bob@contoso.com")
}

// Validates: R-3.1.5
func TestCatalogAuthRequirements_FindsOfflineAuthRequiredAccounts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Catalog account without token -> auth required.
	seedCatalogAccount(t, driveid.MustCanonicalID("personal:alice@outlook.com"), func(account *config.CatalogAccount) {
		account.UserID = authTestUserID1
		account.DisplayName = snapshotTestDisplayNameAliceSmith
	})

	// Catalog account WITH token -> still authenticated.
	seedCatalogAccount(t, driveid.MustCanonicalID("business:bob@contoso.com"), func(account *config.CatalogAccount) {
		account.UserID = "u2"
		account.DisplayName = "Bob Jones"
	})
	writeTestTokenFile(t, dataDir, "token_business_bob@contoso.com.json")

	// Also create a state DB for alice to verify the count.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "state_personal_alice@outlook.com.db"),
		[]byte{}, 0o600,
	))

	cfg := config.DefaultConfig()
	logger := slog.Default()
	stored, err := config.LoadCatalog()
	require.NoError(t, err)

	authRequired := accountViewAuthRequirements(buildAccountViews(t.Context(), cfg, stored, logger), func(accountView) bool {
		return true
	})
	require.Len(t, authRequired, 1)
	assert.Equal(t, "alice@outlook.com", authRequired[0].Email)
	assert.Equal(t, "personal", authRequired[0].DriveType)
	assert.Equal(t, "Alice Smith", authRequired[0].DisplayName)
	assert.Equal(t, 1, authRequired[0].StateDBs)
	assert.Equal(t, authReasonMissingLogin, authRequired[0].Reason)
}

// Validates: R-3.1.4
func TestPurgeOrphanedFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create an orphaned state DB for alice. Catalog-backed metadata is not a
	// filesystem artifact anymore, so purge cleanup only removes retained state.
	aliceState := "state_personal_alice@outlook.com.db"
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, aliceState), []byte(`{}`), 0o600))

	// Also create a file for bob — should NOT be removed.
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "state_personal_bob@outlook.com.db"),
		[]byte{}, 0o600,
	))

	logger := slog.Default()
	err := purgeOrphanedFiles(io.Discard, "alice@outlook.com", logger)
	require.NoError(t, err)

	// Alice's retained state should be gone.
	_, statErr := os.Stat(filepath.Join(dataDir, aliceState))
	assert.True(t, os.IsNotExist(statErr), "expected %s to be deleted", aliceState)

	// Bob's file should remain.
	_, statErr = os.Stat(filepath.Join(dataDir, "state_personal_bob@outlook.com.db"))
	assert.NoError(t, statErr, "bob's state DB should remain")
}

// Validates: R-2.10.47
func TestRunStatusCommand_ClearsPersistedAuthScopeAfterSuccessfulAuthenticatedProof(t *testing.T) {
	setTestDriveHome(t)

	const graphDrivesPath = "/me/drives"
	const primaryDrivePath = "/me/drive"

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedAuthScope(t, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id": "user-123",
				"displayName": "Test User",
				"mail": "user@example.com",
				"userPrincipalName": "user@example.com"
			}`)
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-123","name":"OneDrive","driveType":"personal","quota":{"used":1,"total":2}}]}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-123","name":"OneDrive","driveType":"personal","quota":{"used":1,"total":2}}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runStatusCommand(cc, false))
	assert.False(t, hasPersistedAccountAuthRequirement(t.Context(), cid.Email(), testDriveLogger(t)))
}
