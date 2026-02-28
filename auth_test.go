package main

import (
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

// --- driveExistsInConfig ---

func TestDriveExistsInConfig_Found(t *testing.T) {
	cfgPath := writeTestAuthConfig(t, `
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`)

	exists, err := driveExistsInConfig(cfgPath, driveid.MustCanonicalID("personal:user@example.com"))
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestDriveExistsInConfig_NotFound(t *testing.T) {
	cfgPath := writeTestAuthConfig(t, `
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`)

	exists, err := driveExistsInConfig(cfgPath, driveid.MustCanonicalID("business:other@contoso.com"))
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestDriveExistsInConfig_NoConfig(t *testing.T) {
	// Non-existent config should not error — LoadOrDefault returns default.
	cfgPath := filepath.Join(t.TempDir(), "nonexistent.toml")

	exists, err := driveExistsInConfig(cfgPath, driveid.MustCanonicalID("personal:user@example.com"))
	require.NoError(t, err)
	assert.False(t, exists)
}

// --- collectExistingSyncDirs ---

func TestCollectExistingSyncDirs_ReturnsConfiguredDirs(t *testing.T) {
	cfgPath := writeTestAuthConfig(t, `
["personal:user@example.com"]
sync_dir = "~/OneDrive"

["business:alice@contoso.com"]
sync_dir = "~/Work"
`)

	dirs := collectExistingSyncDirs(cfgPath, slog.Default())
	assert.Len(t, dirs, 2)
	assert.Contains(t, dirs, "~/OneDrive")
	assert.Contains(t, dirs, "~/Work")
}

func TestCollectExistingSyncDirs_SkipsEmptySyncDir(t *testing.T) {
	cfgPath := writeTestAuthConfig(t, `
["personal:user@example.com"]
sync_dir = "~/OneDrive"

["business:alice@contoso.com"]
`)

	dirs := collectExistingSyncDirs(cfgPath, slog.Default())
	assert.Equal(t, []string{"~/OneDrive"}, dirs)
}

func TestCollectExistingSyncDirs_NoConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "nonexistent.toml")

	dirs := collectExistingSyncDirs(cfgPath, slog.Default())
	assert.Empty(t, dirs)
}

func TestCollectExistingSyncDirs_InvalidConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bad.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte("invalid[toml"), 0o600))

	dirs := collectExistingSyncDirs(cfgPath, slog.Default())
	// Should return nil on error (logged but not fatal).
	assert.Nil(t, dirs)
}

// --- writeLoginConfig ---

func TestWriteLoginConfig_CreatesNewFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	user := &graph.User{DisplayName: "Test User"}

	err := writeLoginConfig(cfgPath, cid, user, "", slog.Default())
	require.NoError(t, err)

	// Verify the config file was created.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "personal:user@example.com")
	assert.Contains(t, string(data), "sync_dir")
}

func TestWriteLoginConfig_AppendsToExisting(t *testing.T) {
	cfgPath := writeTestAuthConfig(t, `
["personal:existing@example.com"]
sync_dir = "~/OneDrive"
`)

	cid := driveid.MustCanonicalID("business:new@contoso.com")
	user := &graph.User{DisplayName: "New User"}

	err := writeLoginConfig(cfgPath, cid, user, "Contoso", slog.Default())
	require.NoError(t, err)

	// Verify both drives exist.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "personal:existing@example.com")
	assert.Contains(t, string(data), "business:new@contoso.com")
}

// --- test helpers ---

func writeTestAuthConfig(t *testing.T, content string) string {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(content), 0o600))

	return cfgPath
}

func TestPrintLoginSuccess_DoesNotPanic(t *testing.T) {
	// Verify the print functions don't panic with various inputs.
	// Output goes to stdout, which is fine in tests.
	printLoginSuccess("personal", "toni@outlook.com", "", "personal:toni@outlook.com", "~/OneDrive")
	printLoginSuccess("business", "alice@contoso.com", "Contoso Ltd", "business:alice@contoso.com", "~/OneDrive - Contoso")
	printLoginSuccess("business", "bob@example.com", "", "business:bob@example.com", "~/OneDrive - Business")
	printLoginSuccess("documentLibrary", "carol@example.com", "", "documentLibrary:carol@example.com", "~/SP")
}
