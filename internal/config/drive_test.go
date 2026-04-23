package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// --- matchDrive ---

func TestMatchDrive_SingleDrive_AutoSelect(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	id, d, err := MatchDrive(cfg, "", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
	assert.Equal(t, "~/OneDrive", d.SyncDir)
}

func TestMatchDrive_MultipleDrives_NoSelector_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	_, _, err := MatchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple drives")
}

func TestMatchDrive_NoDrives_NoSelector_NoTokens(t *testing.T) {
	// Override HOME so DiscoverTokens finds no real tokens on disk.
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultConfig()

	_, _, err := MatchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts")
}

func TestMatchDrive_NoDrives_CanonicalSelector_Error(t *testing.T) {
	// A canonical ID selector with no drives configured should return an error.
	// Config is mandatory — users must run "login" and "drive add" first.
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultConfig()

	_, _, err := MatchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

func TestMatchDrive_NoDrives_NonCanonicalSelector_Error(t *testing.T) {
	// A non-canonical selector (no ":") with no drives can't match anything.
	// Override HOME to ensure no tokens on disk.
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultConfig()

	_, _, err := MatchDrive(cfg, "home", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

// Validates: R-3.5.1
func TestMatchDrive_ExactCanonicalID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := MatchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

// Validates: R-3.5.1
func TestMatchDrive_DisplayNameMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive", DisplayName: "home"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work", DisplayName: "work"}

	id, _, err := MatchDrive(cfg, "work", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), id)
}

// Validates: R-3.5.1
func TestMatchDrive_DisplayNameMatch_CaseInsensitive(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive", DisplayName: "My Drive"}

	id, _, err := MatchDrive(cfg, "my drive", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)

	// Also match with ALL CAPS.
	id2, _, err := MatchDrive(cfg, "MY DRIVE", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id2)
}

// Validates: R-3.5.1
func TestMatchDrive_PartialMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := MatchDrive(cfg, "toni", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

// Validates: R-3.5.1
func TestMatchDrive_AmbiguousPartialMatch_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:user@example.com")] = Drive{SyncDir: "~/Work"}

	_, _, err := MatchDrive(cfg, "user@example.com", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestMatchDrive_NoMatch_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	_, _, err := MatchDrive(cfg, "nonexistent", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drive matching")
}

// --- buildResolvedDrive ---

func TestBuildResolvedDrive_GlobalDefaults(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogLevel = logLevelDebug

	drive := &Drive{SyncDir: "~/OneDrive"}
	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))

	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
	assert.False(t, resolved.Paused)
	assert.Equal(t, logLevelDebug, resolved.LogLevel)
	assert.Equal(t, cfg.PollInterval, resolved.PollInterval)
}

func TestBuildResolvedDrive_PausedDefault(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.False(t, resolved.Paused, "Paused should default to false when nil")
}

func TestBuildResolvedDrive_PausedExplicitTrue(t *testing.T) {
	cfg := DefaultConfig()
	paused := true
	drive := &Drive{SyncDir: "~/OneDrive", Paused: &paused}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.True(t, resolved.Paused)
}

// Validates: R-2.6.1
func TestBuildResolvedDrive_TimedPauseExpired(t *testing.T) {
	cfg := DefaultConfig()
	paused := true
	past := "2000-01-01T00:00:00Z"
	drive := &Drive{SyncDir: "~/OneDrive", Paused: &paused, PausedUntil: &past}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.False(t, resolved.Paused, "expired timed pause should resolve to Paused=false")
}

func TestBuildResolvedDrive_PausedExplicitFalse(t *testing.T) {
	cfg := DefaultConfig()
	paused := false
	drive := &Drive{SyncDir: "~/OneDrive", Paused: &paused}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.False(t, resolved.Paused)
}

func TestBuildResolvedDrive_NoPerDriveOverridesBeyondDriveFields(t *testing.T) {
	cfg := DefaultConfig()
	cfg.LogLevel = "debug"
	cfg.DryRun = true

	drive := &Drive{
		SyncDir:     "~/OneDrive",
		DisplayName: "home",
	}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))

	assert.Equal(t, "debug", resolved.LogLevel)
	assert.True(t, resolved.DryRun)
	assert.Equal(t, "home", resolved.DisplayName)
}

func TestBuildResolvedDrive_SharedCanonicalSetsRootItem(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive-Shared"}

	resolved := buildResolvedDrive(
		cfg,
		driveid.MustCanonicalID("shared:user@example.com:b!drive123:item456"),
		drive,
		testLogger(t),
	)

	assert.Equal(t, driveid.New("b!drive123"), resolved.DriveID)
	assert.Equal(t, "item456", resolved.RootItemID)
}

func TestBuildResolvedDrive_SharedCatalogDrivePreservesRootItem(t *testing.T) {
	dataDir := setTestDataDir(t)
	sharedCID := driveid.MustCanonicalID("shared:user@example.com:b!drive123:item456")
	require.NoError(t, UpdateCatalogForDataDir(dataDir, func(catalog *Catalog) error {
		catalog.UpsertDrive(&CatalogDrive{
			CanonicalID:           sharedCID.String(),
			OwnerAccountCanonical: "personal:user@example.com",
			DriveType:             driveid.DriveTypeShared,
			DisplayName:           "Shared Folder",
			RemoteDriveID:         "b!drive123",
		})
		return nil
	}))

	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive-Shared"}

	resolved := buildResolvedDrive(cfg, sharedCID, drive, testLogger(t))

	assert.Equal(t, driveid.New("b!drive123"), resolved.DriveID)
	assert.Equal(t, "item456", resolved.RootItemID)
}

func TestBuildResolvedDrive_TildeExpanded(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))

	home, err := os.UserHomeDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "OneDrive"), resolved.SyncDir)
	assert.False(t, strings.HasPrefix(resolved.SyncDir, "~"))
}

func TestBuildResolvedDrive_AbsolutePathPreserved(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "/absolute/path/OneDrive"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "/absolute/path/OneDrive", resolved.SyncDir)
}

func TestBuildResolvedDrive_DisplayName(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{
		SyncDir:     "~/OneDrive",
		DisplayName: "home",
	}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "home", resolved.DisplayName)
}

func TestBuildResolvedDrive_OwnerField(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{
		SyncDir: "~/OneDrive",
		Owner:   "Alice Smith",
	}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "Alice Smith", resolved.Owner)
}

func TestBuildResolvedDrive_DefaultSyncDir_Personal(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{} // no sync_dir

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:user@example.com"), drive, testLogger(t))
	assert.Equal(t, filepath.Join(home, "OneDrive"), resolved.SyncDir)
}

func TestBuildResolvedDrive_DefaultSyncDir_Business(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{} // no sync_dir

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("business:alice@contoso.com"), drive, testLogger(t))
	// orgName is unavailable at runtime, so defaults to "Business".
	assert.Equal(t, filepath.Join(home, "OneDrive - Business"), resolved.SyncDir)
}

func TestBuildResolvedDrive_DefaultSyncDir_SharePoint(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{} // no sync_dir

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents"), drive, testLogger(t))
	assert.Equal(t, filepath.Join(home, "SharePoint - marketing - Documents"), resolved.SyncDir)
}

func TestBuildResolvedDrive_ExplicitSyncDir_NotOverridden(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/CustomDir"}

	home, err := os.UserHomeDir()
	require.NoError(t, err)

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:user@example.com"), drive, testLogger(t))
	// Explicit sync_dir should NOT be overridden by the default.
	assert.Equal(t, filepath.Join(home, "CustomDir"), resolved.SyncDir)
}

// --- expandTilde ---

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)

	assert.Equal(t, filepath.Join(home, "OneDrive"), expandTilde("~/OneDrive"))
	assert.Equal(t, "/absolute/path", expandTilde("/absolute/path"))
	assert.Equal(t, "relative/path", expandTilde("relative/path"))
	assert.Empty(t, expandTilde(""))
}

// --- DriveStatePath ---

func TestDriveStatePath_Personal(t *testing.T) {
	path := DriveStatePath(driveid.MustCanonicalID("personal:toni@outlook.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "state_personal_toni@outlook.com.db")
}

func TestDriveStatePath_Business(t *testing.T) {
	path := DriveStatePath(driveid.MustCanonicalID("business:alice@contoso.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "state_business_alice@contoso.com.db")
}

func TestDriveStatePath_SharePoint(t *testing.T) {
	path := DriveStatePath(driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"))
	assert.NotEmpty(t, path)
	// All colons replaced with underscores.
	assert.Contains(t, path, "state_sharepoint_alice@contoso.com_marketing_Docs.db")
}

func TestDriveStatePath_ZeroID(t *testing.T) {
	// Zero canonical ID should return empty, matching DriveTokenPath behavior.
	path := DriveStatePath(driveid.CanonicalID{})
	assert.Empty(t, path)
}

func TestDriveStatePath_PlatformSpecific(t *testing.T) {
	path := DriveStatePath(driveid.MustCanonicalID("personal:toni@outlook.com"))

	switch runtime.GOOS {
	case platformDarwin:
		assert.Contains(t, path, "Library/Application Support")
	case platformLinux:
		assert.Contains(t, path, ".local/share")
	}
}

// --- StatePath ---

func TestStatePath_DefaultLocation(t *testing.T) {
	// StatePath delegates to DriveStatePath.
	resolved := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:toni@outlook.com"),
	}

	path := resolved.StatePath()
	assert.Equal(t, DriveStatePath(resolved.CanonicalID), path)
}

func TestStatePath_SharePoint_ColonsReplaced(t *testing.T) {
	resolved := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
	}

	path := resolved.StatePath()
	assert.Equal(t, DriveStatePath(resolved.CanonicalID), path)
	assert.Contains(t, path, "state_sharepoint_alice@contoso.com_marketing_Docs.db")
}

// --- Integration: TOML parsing -> resolution ---

// Validates: R-4.1.1, R-3.4.1
func TestLoad_FullConfigWithDrives(t *testing.T) {
	path := writeTestConfig(t, `
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive-Work"
display_name = "work"
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.Equal(t, "debug", cfg.LogLevel)

	require.Len(t, cfg.Drives, 2)
	assert.Equal(t, "home", cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")].DisplayName)
	assert.Equal(t, "work", cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")].DisplayName)
}

func TestResolveDrive_FullIntegration(t *testing.T) {
	path := writeTestConfig(t, `
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive-Work"
display_name = "work"
`)
	resolved, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{Drive: "work"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, "work", resolved.DisplayName)
	assert.Equal(t, "debug", resolved.LogLevel)

	resolved, _, err = ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{Drive: "home"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, "home", resolved.DisplayName)
	assert.Equal(t, "debug", resolved.LogLevel)
}

// --- Env Override Integration ---

func TestResolveDrive_EnvDriveOverride(t *testing.T) {
	t.Setenv(EnvDrive, "work")

	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
display_name = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
display_name = "work"
`)
	overrides := ReadEnvOverrides(testLogger(t))
	resolved, _, err := ResolveDrive(
		EnvOverrides{ConfigPath: path, Drive: overrides.Drive},
		CLIOverrides{},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), resolved.CanonicalID)
}

// --- discoverTokensIn ---

func TestDiscoverTokensIn_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	ids := discoverTokensIn(dir, testLogger(t))
	assert.Empty(t, ids)
}

func TestDiscoverTokensIn_EmptyPath(t *testing.T) {
	ids := discoverTokensIn("", testLogger(t))
	assert.Nil(t, ids)
}

func TestDiscoverTokensIn_NonexistentDir(t *testing.T) {
	ids := discoverTokensIn("/nonexistent/path", testLogger(t))
	assert.Nil(t, ids)
}

func TestDiscoverTokensIn_OneToken(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "token_personal_user@example.com.json")

	ids := discoverTokensIn(dir, testLogger(t))
	require.Len(t, ids, 1)
	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), ids[0])
}

func TestDiscoverTokensIn_TwoTokens_Sorted(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "token_personal_zack@example.com.json")
	writeTokenFile(t, dir, "token_business_alice@contoso.com.json")

	ids := discoverTokensIn(dir, testLogger(t))
	require.Len(t, ids, 2)
	// Should be sorted alphabetically.
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), ids[0])
	assert.Equal(t, driveid.MustCanonicalID("personal:zack@example.com"), ids[1])
}

func TestDiscoverTokensIn_IgnoresNonTokenFiles(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "token_personal_user@example.com.json")
	writeTokenFile(t, dir, "state_personal_user@example.com.db")
	writeTokenFile(t, dir, "config.toml")
	writeTokenFile(t, dir, "random.json")

	ids := discoverTokensIn(dir, testLogger(t))
	require.Len(t, ids, 1)
	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), ids[0])
}

func TestDiscoverTokensIn_SkipsMalformed(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "token_personal_user@example.com.json") // valid
	writeTokenFile(t, dir, "token_.json")                          // empty inner
	writeTokenFile(t, dir, "token_nounderscore.json")              // no type/email separator

	ids := discoverTokensIn(dir, testLogger(t))
	require.Len(t, ids, 1)
	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), ids[0])
}

func TestDiscoverTokensIn_IgnoresDirectories(t *testing.T) {
	dir := t.TempDir()
	writeTokenFile(t, dir, "token_personal_user@example.com.json")

	// Create a subdirectory that matches the token naming pattern.
	require.NoError(t, os.Mkdir(filepath.Join(dir, "token_personal_dir@example.com.json"), 0o700))

	ids := discoverTokensIn(dir, testLogger(t))
	require.Len(t, ids, 1)
	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), ids[0])
}

// --- MatchDrive smart error messages ---

func TestMatchDrive_NoDrives_NoSelector_TokensExist_SuggestsDriveAdd(t *testing.T) {
	dataDir := setTestDataDir(t)
	writeTokenFile(t, dataDir, "token_personal_user@example.com.json")

	cfg := DefaultConfig()

	_, _, err := MatchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "drive add")
}

func TestMatchDrive_NoDrives_NoSelector_NoTokens_SuggestsLogin(t *testing.T) {
	// Override HOME so DiscoverTokens finds no real tokens on disk.
	setTestDataDir(t) // creates empty data dir

	cfg := DefaultConfig()

	_, _, err := MatchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "login")
}

// --- collectOtherSyncDirs ---

func TestCollectOtherSyncDirs_Empty(t *testing.T) {
	cfg := DefaultConfig()
	dirs := CollectOtherSyncDirs(cfg, driveid.MustCanonicalID("personal:a@b.com"), testLogger(t))
	assert.Empty(t, dirs)
}

func TestCollectOtherSyncDirs_ExcludesSelf(t *testing.T) {
	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:other@contoso.com")] = Drive{SyncDir: "~/Work"}

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	assert.Equal(t, []string{"~/Work"}, dirs)
}

func TestCollectOtherSyncDirs_ComputesBaseForEmptySyncDir(t *testing.T) {
	setTestDataDir(t) // isolated HOME

	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{} // no sync_dir

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	// Without a catalog account entry, business defaults to "~/OneDrive - Business" via BaseSyncDir.
	assert.Contains(t, dirs, "~/OneDrive - Business")
}

func TestCollectOtherSyncDirs_WithCatalogAccount(t *testing.T) {
	dataDir := setTestDataDir(t)

	bizCID := driveid.MustCanonicalID("business:alice@contoso.com")
	seedCatalogAccount(t, bizCID, func(account *CatalogAccount) {
		account.OrgName = "Contoso Ltd"
		account.DisplayName = catalogAccountTestAlice
	})

	// Also create a token file so discovery sees the account.
	writeTokenFile(t, dataDir, "token_business_alice@contoso.com.json")

	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[bizCID] = Drive{} // no sync_dir

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	// With org_name from the catalog account, business resolves to "~/OneDrive - Contoso Ltd".
	assert.Contains(t, dirs, "~/OneDrive - Contoso Ltd")
}

func TestCollectOtherSyncDirs_SkipsEmptyBaseName(t *testing.T) {
	setTestDataDir(t)

	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	assert.Empty(t, dirs)
}

// --- cleanup-correctness tests (no token metadata fallback) ---

func TestBuildResolvedDrive_DriveIDFromCatalogDriveOnly(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:meta@example.com")

	seedCatalogDrive(t, cid, func(drive *CatalogDrive) {
		drive.RemoteDriveID = "abcdef0123456789"
	})

	// Also need a token file for discovery.
	writeTokenFile(t, dataDir, "token_personal_meta@example.com.json")

	cfg := DefaultConfig()
	d := Drive{SyncDir: "~/sync"}
	resolved := buildResolvedDrive(cfg, cid, &d, testLogger(t))

	assert.Equal(t, "abcdef0123456789", resolved.DriveID.String())
}

func TestBuildResolvedDrive_NoCatalogDrive_DriveIDStaysZero(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("personal:nometa@example.com")

	cfg := DefaultConfig()
	d := Drive{SyncDir: "~/sync"}
	resolved := buildResolvedDrive(cfg, cid, &d, testLogger(t))

	assert.True(t, resolved.DriveID.IsZero(), "DriveID should be zero when no metadata exists")
}

func TestResolveAccountNames_NoFallback(t *testing.T) {
	setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:noprofile@example.com")

	orgName, displayName := ResolveAccountNames(cid, testLogger(t))
	assert.Empty(t, orgName, "org_name should be empty without a catalog account entry")
	assert.Empty(t, displayName, "display_name should be empty without a catalog account entry")
}

func TestCollectOtherSyncDirs_NoFallback(t *testing.T) {
	dataDir := setTestDataDir(t)
	cid := driveid.MustCanonicalID("business:noprofile@example.com")

	// Create a token so the drive is discoverable.
	writeTokenFile(t, dataDir, "token_business_noprofile@example.com.json")

	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[cid] = Drive{} // no sync_dir, no catalog account entry

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	// Without a catalog account entry, business defaults to "~/OneDrive - Business" via BaseSyncDir.
	assert.Contains(t, dirs, "~/OneDrive - Business")
}

// Validates: R-3.1.4
func TestDiscoverStateDBsForEmailIn_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	paths := discoverStateDBsForEmailIn(dir, "alice@outlook.com", slog.Default())
	assert.Nil(t, paths)
}

// Validates: R-3.1.4
func TestDiscoverStateDBsForEmailIn_MatchesEmail(t *testing.T) {
	dir := t.TempDir()

	// Personal and SharePoint state DBs for alice — both should match.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_personal_alice@outlook.com.db"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_sharepoint_alice@outlook.com_marketing_Docs.db"), []byte{}, 0o600))
	// Different account — should NOT match.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_personal_bob@outlook.com.db"), []byte{}, 0o600))

	paths := discoverStateDBsForEmailIn(dir, "alice@outlook.com", slog.Default())
	require.Len(t, paths, 2)
	assert.Contains(t, paths, filepath.Join(dir, "state_personal_alice@outlook.com.db"))
	assert.Contains(t, paths, filepath.Join(dir, "state_sharepoint_alice@outlook.com_marketing_Docs.db"))
}

// Validates: R-3.1.4
func TestDiscoverStateDBsForEmailIn_NoSubstringCollision(t *testing.T) {
	dir := t.TempDir()

	// "a@b.com" should NOT match "ba@b.com".
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_personal_ba@b.com.db"), []byte{}, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state_personal_a@b.com.db"), []byte{}, 0o600))

	paths := discoverStateDBsForEmailIn(dir, "a@b.com", slog.Default())
	require.Len(t, paths, 1)
	assert.Contains(t, paths, filepath.Join(dir, "state_personal_a@b.com.db"))
}

// --- test helpers ---

// writeTokenFile creates a minimal valid token file with the given name in dir.
func writeTokenFile(t *testing.T, dir, name string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte(`{"token":{"access_token":"test","token_type":"Bearer"}}`), 0o600)
	require.NoError(t, err)
}

// setTestDataDir overrides HOME so DefaultDataDir() returns a temp directory,
// creates that directory, and returns its path. This isolates token discovery
// tests from real tokens on the developer's machine.
func setTestDataDir(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)

	// DefaultDataDir() derives from HOME — create the expected directory structure.
	dataDir := DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	return dataDir
}
