package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// --- command structure ---

func TestNewDriveCmd_Structure(t *testing.T) {
	cmd := newDriveCmd()
	assert.Equal(t, "drive", cmd.Name())

	subNames := make([]string, 0, len(cmd.Commands()))
	for _, sub := range cmd.Commands() {
		subNames = append(subNames, sub.Name())
	}

	assert.Contains(t, subNames, "add")
	assert.Contains(t, subNames, "remove")
	assert.Contains(t, subNames, "list")
	assert.Contains(t, subNames, "search")
}

func TestNewDriveRemoveCmd_PurgeFlag(t *testing.T) {
	cmd := newDriveRemoveCmd()

	purgeFlag := cmd.Flags().Lookup("purge")
	require.NotNil(t, purgeFlag, "remove command should have --purge flag")
	assert.Equal(t, "false", purgeFlag.DefValue)
}

func TestNewDriveAddCmd_HasRunE(t *testing.T) {
	cmd := newDriveAddCmd()
	assert.NotNil(t, cmd.RunE)
	assert.Equal(t, "add [canonical-id]", cmd.Use)
}

func TestNewDriveListCmd_HasRunE(t *testing.T) {
	cmd := newDriveListCmd()
	assert.NotNil(t, cmd.RunE)
	assert.Equal(t, "list", cmd.Use)
}

func TestNewDriveSearchCmd_HasRunE(t *testing.T) {
	cmd := newDriveSearchCmd()
	assert.NotNil(t, cmd.RunE)
	assert.Equal(t, "search <term>", cmd.Use)
}

// --- buildConfiguredDriveEntries ---

func TestBuildConfiguredDriveEntries_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	assert.Nil(t, entries)
}

func TestBuildConfiguredDriveEntries_OneDrive_WithSyncDir(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{
		SyncDir: "~/OneDrive",
	}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	assert.Equal(t, "personal:user@example.com", entries[0].CanonicalID)
	assert.Equal(t, "~/OneDrive", entries[0].SyncDir)
	assert.Equal(t, driveStateReady, entries[0].State)
	assert.Equal(t, "configured", entries[0].Source)
}

func TestBuildConfiguredDriveEntries_PausedDrive(t *testing.T) {
	cfg := config.DefaultConfig()
	enabled := false
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{
		SyncDir: "~/OneDrive - Contoso",
		Enabled: &enabled,
	}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	assert.Equal(t, driveStatePaused, entries[0].State)
}

func TestBuildConfiguredDriveEntries_MultipleDrives_Sorted(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:zack@example.com")] = config.Drive{SyncDir: "~/OneDrive-Z"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{SyncDir: "~/OneDrive-A"}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 2)
	// Should be sorted by canonical ID.
	assert.Equal(t, "business:alice@contoso.com", entries[0].CanonicalID)
	assert.Equal(t, "personal:zack@example.com", entries[1].CanonicalID)
}

func TestBuildConfiguredDriveEntries_NoSyncDir_ComputesDefault(t *testing.T) {
	// Set HOME to a temp dir to isolate from real token files.
	setTestDriveHome(t)

	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	// Without token meta, personal defaults to "~/OneDrive".
	assert.Equal(t, "~/OneDrive", entries[0].SyncDir)
}

func TestBuildConfiguredDriveEntries_NoSyncDir_WithTokenMeta(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Create a proper token file with metadata.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json", map[string]string{
		"org_name":     "Contoso",
		"display_name": "Alice Smith",
	})

	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	assert.Equal(t, "~/OneDrive - Contoso", entries[0].SyncDir)
}

// --- collectConfigSyncDirs ---

func TestCollectConfigSyncDirs_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	dirs := collectConfigSyncDirs(cfg, driveid.MustCanonicalID("personal:a@b.com"), testDriveLogger(t))
	assert.Empty(t, dirs)
}

func TestCollectConfigSyncDirs_ExcludesSelf(t *testing.T) {
	cfg := config.DefaultConfig()
	cid := driveid.MustCanonicalID("personal:user@example.com")
	cfg.Drives[cid] = config.Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{SyncDir: "~/Work"}

	dirs := collectConfigSyncDirs(cfg, cid, testDriveLogger(t))
	assert.Equal(t, []string{"~/Work"}, dirs)
}

func TestCollectConfigSyncDirs_ComputesDefaultForEmptySyncDir(t *testing.T) {
	setTestDriveHome(t)

	cfg := config.DefaultConfig()
	cid := driveid.MustCanonicalID("personal:user@example.com")
	cfg.Drives[cid] = config.Drive{SyncDir: "~/OneDrive"}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{} // no sync_dir

	dirs := collectConfigSyncDirs(cfg, cid, testDriveLogger(t))
	// Without token meta, business defaults to "~/OneDrive - Business" via BaseSyncDir.
	assert.Contains(t, dirs, "~/OneDrive - Business")
}

// --- readDriveTokenMeta ---

func TestReadDriveTokenMeta_NoToken(t *testing.T) {
	setTestDriveHome(t)
	cid := driveid.MustCanonicalID("personal:noone@example.com")
	orgName, displayName := readDriveTokenMeta(cid, testDriveLogger(t))
	assert.Empty(t, orgName)
	assert.Empty(t, displayName)
}

func TestReadDriveTokenMeta_WithMeta(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", map[string]string{
		"org_name":     "My Org",
		"display_name": "Test User",
	})

	cid := driveid.MustCanonicalID("personal:user@example.com")
	orgName, displayName := readDriveTokenMeta(cid, testDriveLogger(t))
	assert.Equal(t, "My Org", orgName)
	assert.Equal(t, "Test User", displayName)
}

func TestReadDriveTokenMeta_InvalidJSON(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Write invalid JSON.
	tokenPath := filepath.Join(dataDir, "token_personal_user@example.com.json")
	require.NoError(t, os.WriteFile(tokenPath, []byte("not json"), 0o600))

	cid := driveid.MustCanonicalID("personal:user@example.com")
	orgName, displayName := readDriveTokenMeta(cid, testDriveLogger(t))
	assert.Empty(t, orgName)
	assert.Empty(t, displayName)
}

// --- listPausedDrives ---

func TestListPausedDrives_NoPaused(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{SyncDir: "~/OneDrive"}

	err := listPausedDrives(cfg)
	assert.NoError(t, err)
}

func TestListPausedDrives_HasPaused(t *testing.T) {
	cfg := config.DefaultConfig()
	enabled := false
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{
		SyncDir: "~/OneDrive",
		Enabled: &enabled,
	}

	err := listPausedDrives(cfg)
	assert.NoError(t, err)
}

func TestListPausedDrives_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	err := listPausedDrives(cfg)
	assert.NoError(t, err)
}

// --- printDriveListText ---

func TestPrintDriveListText_EmptyBothSections(t *testing.T) {
	// Should not panic with nil slices.
	printDriveListText(nil, nil)
}

func TestPrintDriveListText_ConfiguredOnly(t *testing.T) {
	t.Helper()

	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	assert.NotPanics(t, func() { printDriveListText(configured, nil) })
}

func TestPrintDriveListText_AvailableOnly(t *testing.T) {
	t.Helper()

	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", State: "", Source: "available", SiteName: "Marketing"},
	}
	assert.NotPanics(t, func() { printDriveListText(nil, available) })
}

func TestPrintDriveListText_BothSections(t *testing.T) {
	t.Helper()

	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", Source: "available"},
	}
	assert.NotPanics(t, func() { printDriveListText(configured, available) })
}

func TestPrintDriveListText_EmptySyncDir_ShowsNotSet(t *testing.T) {
	t.Helper()

	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "", State: driveStateNeedsSetup, Source: "configured"},
	}
	assert.NotPanics(t, func() { printDriveListText(configured, nil) })
}

// --- printDriveListJSON ---

func TestPrintDriveListJSON_Empty(t *testing.T) {
	// Redirect stdout to check JSON output.
	// Just verify it doesn't panic with nil.
	err := printDriveListJSON(nil, nil)
	assert.NoError(t, err)
}

// --- resumeExistingDrive ---

func TestResumeExistingDrive_AlreadyEnabled(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:user@example.com")
	d := &config.Drive{SyncDir: "~/OneDrive"} // Enabled is nil (defaults to true)

	err := resumeExistingDrive("", cid, d, testDriveLogger(t))
	assert.NoError(t, err)
}

func TestResumeExistingDrive_ExplicitlyEnabled(t *testing.T) {
	cid := driveid.MustCanonicalID("personal:user@example.com")
	enabled := true
	d := &config.Drive{SyncDir: "~/OneDrive", Enabled: &enabled}

	err := resumeExistingDrive("", cid, d, testDriveLogger(t))
	assert.NoError(t, err)
}

// --- driveListEntry ---

func TestDriveListEntry_JSONRoundTrip(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "personal:user@example.com",
		SyncDir:     "~/OneDrive",
		State:       driveStateReady,
		Source:      "configured",
	}

	data, err := json.Marshal(entry)
	require.NoError(t, err)

	var decoded driveListEntry
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, entry, decoded)
}

func TestDriveListEntry_JSONOmitsEmpty(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "personal:user@example.com",
		State:       driveStateReady,
		Source:      "available",
	}

	data, err := json.Marshal(entry)
	require.NoError(t, err)

	// sync_dir, site_name, library_name should be omitted.
	assert.NotContains(t, string(data), "sync_dir")
	assert.NotContains(t, string(data), "site_name")
	assert.NotContains(t, string(data), "library_name")
}

// --- printDriveSearchText ---

func TestPrintDriveSearchText_Empty(t *testing.T) {
	// Should not panic with no results.
	printDriveSearchText(nil, "test query")
}

func TestPrintDriveSearchText_WithResults(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs", WebURL: "https://contoso.sharepoint.com/sites/marketing"},
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Wiki", SiteName: "Marketing", LibraryName: "Wiki"},
	}
	assert.NotPanics(t, func() { printDriveSearchText(results, "marketing") })
}

func TestPrintDriveSearchText_MultipleSites(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:hr:Docs", SiteName: "HR", LibraryName: "Docs"},
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
	}
	assert.NotPanics(t, func() { printDriveSearchText(results, "docs") })
}

// --- printDriveSearchJSON ---

func TestPrintDriveSearchJSON_NoError(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
	}
	err := printDriveSearchJSON(results)
	assert.NoError(t, err)
}

func TestPrintDriveSearchJSON_EmptySlice(t *testing.T) {
	err := printDriveSearchJSON([]driveSearchResult{})
	assert.NoError(t, err)
}

// --- findBusinessTokens ---

func TestFindBusinessTokens_NoTokens(t *testing.T) {
	setTestDriveHome(t)
	tokens := findBusinessTokens(testDriveLogger(t))
	assert.Empty(t, tokens)
}

func TestFindBusinessTokens_HasBusinessToken(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Create business and personal token files.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json", nil)
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", nil)

	tokens := findBusinessTokens(testDriveLogger(t))
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

func TestFindBusinessTokens_SkipsPersonal(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", nil)

	tokens := findBusinessTokens(testDriveLogger(t))
	assert.Empty(t, tokens)
}

// --- driveSearchResult ---

func TestDriveSearchResult_JSONRoundTrip(t *testing.T) {
	result := driveSearchResult{
		CanonicalID: "sharepoint:user@contoso.com:marketing:Docs",
		SiteName:    "Marketing",
		LibraryName: "Docs",
		WebURL:      "https://contoso.sharepoint.com",
	}

	data, err := json.Marshal(result)
	require.NoError(t, err)

	var decoded driveSearchResult
	require.NoError(t, json.Unmarshal(data, &decoded))
	assert.Equal(t, result, decoded)
}

// --- pauseDrive ---

func TestPauseDrive_WritesConfig(t *testing.T) {
	// Create a config file with a drive.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	cid := driveid.MustCanonicalID("personal:user@example.com")
	err := pauseDrive(cfgPath, cid, "~/OneDrive")
	assert.NoError(t, err)

	// Verify enabled = false was written.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "enabled")
}

// --- addNewDrive ---

func TestAddNewDrive_NoToken(t *testing.T) {
	setTestDriveHome(t)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(""), 0o600))

	cfg := config.DefaultConfig()
	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	err := addNewDrive(cfgPath, cfg, cid, testDriveLogger(t))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no token found")
}

func TestAddNewDrive_WithToken(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", map[string]string{
		"display_name": "Test User",
	})

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(""), 0o600))

	cfg := config.DefaultConfig()
	cid := driveid.MustCanonicalID("personal:user@example.com")

	err := addNewDrive(cfgPath, cfg, cid, testDriveLogger(t))
	assert.NoError(t, err)

	// Verify config was updated.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "personal:user@example.com")
}

// --- test helpers ---

func testDriveLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// setTestDriveHome overrides HOME to a temp dir so DefaultDataDir() is isolated.
func setTestDriveHome(t *testing.T) {
	t.Helper()

	t.Setenv("HOME", t.TempDir())
}

// writeTestTokenFile creates a token file with the new format (token + meta).
func writeTestTokenFile(t *testing.T, dir, name string, meta map[string]string) {
	t.Helper()

	tokenFile := map[string]any{
		"token": map[string]string{
			"access_token":  "test-access-token",
			"refresh_token": "test-refresh-token",
			"token_type":    "Bearer",
		},
		"meta": meta,
	}

	data, err := json.Marshal(tokenFile)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), data, 0o600))
}
