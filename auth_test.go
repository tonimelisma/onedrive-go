package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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

func TestFindTokenFallback_PersonalExists(t *testing.T) {
	// Create a temp directory and a file matching the personal token path.
	personalID := driveid.MustCanonicalID("personal:test-fallback@example.com")
	personalPath := config.DriveTokenPath(personalID)

	if personalPath == "" {
		t.Skip("cannot determine token path on this platform")
	}

	// Create the directory and file.
	dir := filepath.Dir(personalPath)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	require.NoError(t, os.WriteFile(personalPath, []byte("{}"), 0o600))
	t.Cleanup(func() { os.Remove(personalPath) })

	got := findTokenFallback("test-fallback@example.com", slog.Default())
	assert.Equal(t, personalID, got)
}

func TestFindTokenFallback_BusinessExists(t *testing.T) {
	// Create only a business token file — should return business prefix.
	businessID := driveid.MustCanonicalID("business:test-fallback-biz@example.com")
	businessPath := config.DriveTokenPath(businessID)

	if businessPath == "" {
		t.Skip("cannot determine token path on this platform")
	}

	dir := filepath.Dir(businessPath)
	require.NoError(t, os.MkdirAll(dir, 0o700))

	require.NoError(t, os.WriteFile(businessPath, []byte("{}"), 0o600))
	t.Cleanup(func() { os.Remove(businessPath) })

	got := findTokenFallback("test-fallback-biz@example.com", slog.Default())
	assert.Equal(t, businessID, got)
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
	printWhoamiText(&buf, user, drives, nil)
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
	printLoginSuccess(&buf, "personal", "toni@outlook.com", "", "personal:toni@outlook.com", "~/OneDrive")
	printLoginSuccess(&buf, "business", "alice@contoso.com", "Contoso Ltd", "business:alice@contoso.com", "~/OneDrive - Contoso")
	printLoginSuccess(&buf, "business", "bob@example.com", "", "business:bob@example.com", "~/OneDrive - Business")
	printLoginSuccess(&buf, "documentLibrary", "carol@example.com", "", "documentLibrary:carol@example.com", "~/SP")
}

// Validates: R-3.1.4
func TestResolveLogoutAccount_FallbackToProfiles(t *testing.T) {
	// When config is empty but orphaned account profiles exist, --purge should
	// auto-select the single orphan.
	home := t.TempDir()
	t.Setenv("HOME", home)

	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

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

	printLoggedOutAccountsText(&buf, loggedOut)
	output := buf.String()

	assert.Contains(t, output, "Logged out accounts:")
	assert.Contains(t, output, "Alice Smith (alice@outlook.com)")
	assert.Contains(t, output, "2 state databases")
	assert.Contains(t, output, "bob@contoso.com")
	assert.Contains(t, output, "no state databases")
	assert.Contains(t, output, "logout --purge")
}
