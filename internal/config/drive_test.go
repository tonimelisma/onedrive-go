package config

import (
	"encoding/json"
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

func TestMatchDrive_NoDrives_CanonicalSelector(t *testing.T) {
	// When no drives are configured but the selector looks like a canonical ID,
	// matchDrive should return the selector as the canonical ID with an empty Drive.
	// This supports zero-config CLI usage (e.g., --drive personal:user@example.com).
	cfg := DefaultConfig()

	id, _, err := MatchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
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

func TestMatchDrive_ExactCanonicalID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := MatchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

func TestMatchDrive_AliasMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive", Alias: "home"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work", Alias: "work"}

	id, _, err := MatchDrive(cfg, "work", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), id)
}

func TestMatchDrive_PartialMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := MatchDrive(cfg, "toni", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

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
	cfg.SkipDotfiles = true
	cfg.LogLevel = "debug"

	drive := &Drive{SyncDir: "~/OneDrive"}
	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))

	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), resolved.CanonicalID)
	assert.True(t, resolved.Enabled)
	assert.True(t, resolved.SkipDotfiles)
	assert.Equal(t, "debug", resolved.LogLevel)
	assert.Equal(t, "/", resolved.RemotePath)
}

func TestBuildResolvedDrive_EnabledDefault(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.True(t, resolved.Enabled, "Enabled should default to true when nil")
}

func TestBuildResolvedDrive_EnabledExplicitFalse(t *testing.T) {
	cfg := DefaultConfig()
	enabled := false
	drive := &Drive{SyncDir: "~/OneDrive", Enabled: &enabled}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.False(t, resolved.Enabled)
}

func TestBuildResolvedDrive_EnabledExplicitTrue(t *testing.T) {
	cfg := DefaultConfig()
	enabled := true
	drive := &Drive{SyncDir: "~/OneDrive", Enabled: &enabled}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.True(t, resolved.Enabled)
}

func TestBuildResolvedDrive_PerDriveOverrides(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SkipDotfiles = false
	cfg.PollInterval = "5m"

	skipDot := true
	drive := &Drive{
		SyncDir:      "~/OneDrive",
		SkipDotfiles: &skipDot,
		SkipDirs:     []string{"vendor"},
		SkipFiles:    []string{"*.log"},
		PollInterval: "10m",
	}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))

	assert.True(t, resolved.SkipDotfiles)
	assert.Equal(t, []string{"vendor"}, resolved.SkipDirs)
	assert.Equal(t, []string{"*.log"}, resolved.SkipFiles)
	assert.Equal(t, "10m", resolved.PollInterval)
}

func TestBuildResolvedDrive_RemotePathDefault(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "/", resolved.RemotePath)
}

func TestBuildResolvedDrive_RemotePathExplicit(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive", RemotePath: "/Documents"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "/Documents", resolved.RemotePath)
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

func TestBuildResolvedDrive_AliasAndDriveID(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{
		SyncDir: "~/OneDrive",
		Alias:   "home",
		DriveID: "abc123",
	}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "home", resolved.Alias)
	assert.Equal(t, driveid.New("abc123"), resolved.DriveID)
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
	assert.Equal(t, "", expandTilde(""))
}

// --- DriveTokenPath ---

func TestDriveTokenPath_Personal(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("personal:toni@outlook.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_personal_toni@outlook.com.json")
}

func TestDriveTokenPath_Business(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("business:alice@contoso.com"))
	assert.NotEmpty(t, path)
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
}

func TestDriveTokenPath_SharePoint_SharesBusinessToken(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"))
	assert.NotEmpty(t, path)
	// SharePoint drives share the business token.
	assert.Contains(t, path, "token_business_alice@contoso.com.json")
	assert.NotContains(t, path, "sharepoint")
}

func TestDriveTokenPath_ZeroID(t *testing.T) {
	path := DriveTokenPath(driveid.CanonicalID{})
	assert.Empty(t, path)
}

func TestDriveTokenPath_PlatformSpecific(t *testing.T) {
	path := DriveTokenPath(driveid.MustCanonicalID("personal:toni@outlook.com"))

	switch runtime.GOOS {
	case platformDarwin:
		assert.Contains(t, path, "Library/Application Support")
	case platformLinux:
		assert.Contains(t, path, ".local/share")
	}
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
	// When StateDir is empty, StatePath falls back to DriveStatePath.
	resolved := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:toni@outlook.com"),
	}

	path := resolved.StatePath()
	assert.Equal(t, DriveStatePath(resolved.CanonicalID), path)
}

func TestStatePath_OverrideDir(t *testing.T) {
	// When StateDir is set, the DB is placed inside that directory.
	tmpDir := t.TempDir()
	resolved := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("personal:toni@outlook.com"),
		StateDir:    tmpDir,
	}

	path := resolved.StatePath()
	assert.Equal(t, filepath.Join(tmpDir, "state_personal_toni@outlook.com.db"), path)
}

func TestStatePath_SharePoint_ColonsReplaced(t *testing.T) {
	tmpDir := t.TempDir()
	resolved := &ResolvedDrive{
		CanonicalID: driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Docs"),
		StateDir:    tmpDir,
	}

	path := resolved.StatePath()
	assert.Equal(t, filepath.Join(tmpDir, "state_sharepoint_alice@contoso.com_marketing_Docs.db"), path)
}

func TestBuildResolvedDrive_StateDirPropagated(t *testing.T) {
	cfg := DefaultConfig()
	drive := &Drive{SyncDir: "~/OneDrive", StateDir: "/tmp/test-state"}

	resolved := buildResolvedDrive(cfg, driveid.MustCanonicalID("personal:toni@outlook.com"), drive, testLogger(t))
	assert.Equal(t, "/tmp/test-state", resolved.StateDir)
}

// --- Integration: TOML parsing -> resolution ---

func TestLoad_FullConfigWithDrives(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = false
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
alias = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive-Work"
alias = "work"
skip_dotfiles = true
skip_dirs = ["vendor"]
`)
	cfg, err := Load(path, testLogger(t))
	require.NoError(t, err)

	assert.False(t, cfg.SkipDotfiles)
	assert.Equal(t, "debug", cfg.LogLevel)

	require.Len(t, cfg.Drives, 2)
	assert.Equal(t, "home", cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")].Alias)
	assert.Equal(t, "work", cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")].Alias)
}

func TestResolveDrive_FullIntegration(t *testing.T) {
	path := writeTestConfig(t, `
skip_dotfiles = false
log_level = "debug"

["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
alias = "home"

["business:alice@contoso.com"]
sync_dir = "~/OneDrive-Work"
alias = "work"
skip_dotfiles = true
skip_dirs = ["vendor"]
`)
	// Resolve work drive: per-drive overrides should apply.
	resolved, err := ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{Drive: "work"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.True(t, resolved.SkipDotfiles)
	assert.Equal(t, []string{"vendor"}, resolved.SkipDirs)
	assert.Equal(t, "debug", resolved.LogLevel)

	// Resolve home drive: uses global settings.
	resolved, err = ResolveDrive(
		EnvOverrides{ConfigPath: path},
		CLIOverrides{Drive: "home"},
		testLogger(t),
	)
	require.NoError(t, err)
	assert.False(t, resolved.SkipDotfiles)
	assert.Equal(t, "debug", resolved.LogLevel)
}

// --- Env Override Integration ---

func TestResolveDrive_EnvDriveOverride(t *testing.T) {
	t.Setenv(EnvDrive, "work")

	path := writeTestConfig(t, `
["personal:toni@outlook.com"]
sync_dir = "~/OneDrive"
alias = "home"

["business:alice@contoso.com"]
sync_dir = "~/Work"
alias = "work"
`)
	overrides := ReadEnvOverrides(testLogger(t))
	resolved, err := ResolveDrive(
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
	require.NoError(t, os.Mkdir(filepath.Join(dir, "token_personal_dir@example.com.json"), 0o755))

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
	// Without token meta, business defaults to "~/OneDrive - Business" via BaseSyncDir.
	assert.Contains(t, dirs, "~/OneDrive - Business")
}

func TestCollectOtherSyncDirs_WithTokenMeta(t *testing.T) {
	dataDir := setTestDataDir(t)

	// Create a token file with org_name metadata.
	writeTokenFileWithMeta(t, dataDir, "token_business_alice@contoso.com.json", map[string]string{
		"org_name": "Contoso Ltd",
	})

	cfg := DefaultConfig()
	selfID := driveid.MustCanonicalID("personal:self@example.com")
	cfg.Drives[selfID] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{} // no sync_dir

	dirs := CollectOtherSyncDirs(cfg, selfID, testLogger(t))
	// With org_name from token meta, business resolves to "~/OneDrive - Contoso Ltd".
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

// --- readTokenMetaForSyncDir ---

func TestReadTokenMetaForSyncDir_NoToken(t *testing.T) {
	setTestDataDir(t)

	cid := driveid.MustCanonicalID("personal:nobody@example.com")
	orgName, displayName := ReadTokenMetaForSyncDir(cid, testLogger(t))
	assert.Empty(t, orgName)
	assert.Empty(t, displayName)
}

func TestReadTokenMetaForSyncDir_WithMeta(t *testing.T) {
	dataDir := setTestDataDir(t)

	writeTokenFileWithMeta(t, dataDir, "token_personal_user@example.com.json", map[string]string{
		"org_name":     "TestOrg",
		"display_name": "Test User",
	})

	cid := driveid.MustCanonicalID("personal:user@example.com")
	orgName, displayName := ReadTokenMetaForSyncDir(cid, testLogger(t))
	assert.Equal(t, "TestOrg", orgName)
	assert.Equal(t, "Test User", displayName)
}

func TestReadTokenMetaForSyncDir_ZeroID(t *testing.T) {
	orgName, displayName := ReadTokenMetaForSyncDir(driveid.CanonicalID{}, testLogger(t))
	assert.Empty(t, orgName)
	assert.Empty(t, displayName)
}

// --- test helpers ---

// writeTokenFile creates an empty file with the given name in dir.
func writeTokenFile(t *testing.T, dir, name string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600)
	require.NoError(t, err)
}

// writeTokenFileWithMeta creates a properly formatted token file with metadata.
func writeTokenFileWithMeta(t *testing.T, dir, name string, meta map[string]string) {
	t.Helper()

	tf := map[string]any{
		"token": map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
			"expiry":        "2099-01-01T00:00:00Z",
		},
		"meta": meta,
	}

	data, err := json.Marshal(tf)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	return dataDir
}
