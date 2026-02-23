package config

import (
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

	id, d, err := matchDrive(cfg, "", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
	assert.Equal(t, "~/OneDrive", d.SyncDir)
}

func TestMatchDrive_MultipleDrives_NoSelector_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	_, _, err := matchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple drives")
}

func TestMatchDrive_NoDrives_NoSelector_NoTokens(t *testing.T) {
	// Override HOME so DiscoverTokens finds no real tokens on disk.
	t.Setenv("HOME", t.TempDir())

	cfg := DefaultConfig()

	_, _, err := matchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no accounts")
}

func TestMatchDrive_NoDrives_CanonicalSelector(t *testing.T) {
	// When no drives are configured but the selector looks like a canonical ID,
	// matchDrive should return the selector as the canonical ID with an empty Drive.
	// This supports zero-config CLI usage (e.g., --drive personal:user@example.com).
	cfg := DefaultConfig()

	id, _, err := matchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

func TestMatchDrive_NoDrives_NonCanonicalSelector_Error(t *testing.T) {
	// A non-canonical selector (no ":") with no drives can't match anything —
	// token discovery only applies when no selector is given.
	cfg := DefaultConfig()

	_, _, err := matchDrive(cfg, "home", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drives configured")
}

func TestMatchDrive_ExactCanonicalID(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := matchDrive(cfg, "personal:toni@outlook.com", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

func TestMatchDrive_AliasMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive", Alias: "home"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work", Alias: "work"}

	id, _, err := matchDrive(cfg, "work", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("business:alice@contoso.com"), id)
}

func TestMatchDrive_PartialMatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = Drive{SyncDir: "~/Work"}

	id, _, err := matchDrive(cfg, "toni", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:toni@outlook.com"), id)
}

func TestMatchDrive_AmbiguousPartialMatch_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:user@example.com")] = Drive{SyncDir: "~/Work"}

	_, _, err := matchDrive(cfg, "user@example.com", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous")
}

func TestMatchDrive_NoMatch_Error(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:toni@outlook.com")] = Drive{SyncDir: "~/OneDrive"}

	_, _, err := matchDrive(cfg, "nonexistent", testLogger(t))
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

// --- matchDrive with token discovery ---

func TestMatchDrive_NoDrives_NoSelector_OneToken(t *testing.T) {
	dataDir := setTestDataDir(t)
	writeTokenFile(t, dataDir, "token_personal_user@example.com.json")

	cfg := DefaultConfig()

	id, _, err := matchDrive(cfg, "", testLogger(t))
	require.NoError(t, err)
	assert.Equal(t, driveid.MustCanonicalID("personal:user@example.com"), id)
}

func TestMatchDrive_NoDrives_NoSelector_MultipleTokens(t *testing.T) {
	dataDir := setTestDataDir(t)
	writeTokenFile(t, dataDir, "token_personal_user@example.com.json")
	writeTokenFile(t, dataDir, "token_business_alice@contoso.com.json")

	cfg := DefaultConfig()

	_, _, err := matchDrive(cfg, "", testLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multiple accounts found")
	assert.Contains(t, err.Error(), "personal:user@example.com")
	assert.Contains(t, err.Error(), "business:alice@contoso.com")
}

// --- test helpers ---

// writeTokenFile creates an empty file with the given name in dir.
func writeTokenFile(t *testing.T, dir, name string) {
	t.Helper()

	err := os.WriteFile(filepath.Join(dir, name), []byte("{}"), 0o600)
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
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	return dataDir
}
