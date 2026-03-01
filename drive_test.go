package main

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/oauth2"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
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
	assert.Equal(t, "user@example.com", entries[0].DisplayName)
	assert.Equal(t, "~/OneDrive", entries[0].SyncDir)
	assert.Equal(t, driveStateReady, entries[0].State)
	assert.Equal(t, "configured", entries[0].Source)
}

func TestBuildConfiguredDriveEntries_PausedDrive(t *testing.T) {
	cfg := config.DefaultConfig()
	paused := true
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{
		SyncDir: "~/OneDrive - Contoso",
		Paused:  &paused,
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

// collectConfigSyncDirs and readDriveTokenMeta were deleted — their logic now
// lives in config.CollectOtherSyncDirs and config.ReadTokenMetaForSyncDir.
// Tests for these functions live in internal/config/drive_test.go.

// --- listAvailableDrives ---

func TestListAvailableDrives_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	err := listAvailableDrives(cfg)
	assert.NoError(t, err)
}

func TestListAvailableDrives_WithDrives(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{SyncDir: "~/OneDrive"}

	err := listAvailableDrives(cfg)
	assert.NoError(t, err)
}

// --- printDriveListText ---

func TestPrintDriveListText_EmptyBothSections(t *testing.T) {
	output := captureStdout(t, func() { printDriveListText(nil, nil) })
	assert.Contains(t, output, "No drives configured")
}

func TestPrintDriveListText_ConfiguredOnly(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	output := captureStdout(t, func() { printDriveListText(configured, nil) })
	assert.Contains(t, output, "Configured drives:")
	assert.Contains(t, output, "personal:user@example.com")
}

func TestPrintDriveListText_AvailableOnly(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", State: "", Source: "available", SiteName: "Marketing"},
	}
	output := captureStdout(t, func() { printDriveListText(nil, available) })
	assert.Contains(t, output, "Available drives")
	assert.Contains(t, output, "business:user@contoso.com")
}

func TestPrintDriveListText_BothSections(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", Source: "available"},
	}
	output := captureStdout(t, func() { printDriveListText(configured, available) })
	assert.Contains(t, output, "Configured drives:")
	assert.Contains(t, output, "Available drives")
}

func TestBuildConfiguredDriveEntries_ExplicitDisplayName(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{
		SyncDir:     "~/OneDrive",
		DisplayName: "My Personal Drive",
	}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	assert.Equal(t, "My Personal Drive", entries[0].DisplayName)
}

func TestPrintDriveListText_ShowsDisplayName(t *testing.T) {
	configured := []driveListEntry{
		{
			CanonicalID: "personal:user@example.com",
			DisplayName: "user@example.com",
			SyncDir:     "~/OneDrive",
			State:       driveStateReady,
			Source:      "configured",
		},
	}
	output := captureStdout(t, func() { printDriveListText(configured, nil) })
	assert.Contains(t, output, "user@example.com")
	assert.Contains(t, output, "personal:user@example.com")
}

func TestDriveLabel_WithDisplayName(t *testing.T) {
	e := driveListEntry{
		CanonicalID: "personal:user@example.com",
		DisplayName: "user@example.com",
	}
	assert.Equal(t, "user@example.com (personal:user@example.com)", driveLabel(e))
}

func TestDriveLabel_WithoutDisplayName(t *testing.T) {
	e := driveListEntry{CanonicalID: "personal:user@example.com"}
	assert.Equal(t, "personal:user@example.com", driveLabel(e))
}

func TestDriveLabel_DisplayNameSameAsCanonicalID(t *testing.T) {
	e := driveListEntry{
		CanonicalID: "personal:user@example.com",
		DisplayName: "personal:user@example.com",
	}
	assert.Equal(t, "personal:user@example.com", driveLabel(e))
}

func TestPrintDriveListText_EmptySyncDir_ShowsNotSet(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "", State: driveStateNeedsSetup, Source: "configured"},
	}
	output := captureStdout(t, func() { printDriveListText(configured, nil) })
	assert.Contains(t, output, "(not set)")
}

// --- printDriveListJSON ---

func TestPrintDriveListJSON_Empty(t *testing.T) {
	err := printDriveListJSON(nil, nil)
	assert.NoError(t, err)
}

func TestPrintDriveListJSON_VerifyOutput(t *testing.T) {
	// Capture stdout to verify JSON structure.
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = w

	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", Source: "available", SiteName: "Marketing"},
	}

	writeErr := printDriveListJSON(configured, available)
	w.Close()
	os.Stdout = origStdout

	require.NoError(t, writeErr)

	// printDriveListJSON outputs a structured object with "configured" and "available" arrays.
	var output driveListJSONOutput
	require.NoError(t, json.NewDecoder(r).Decode(&output))
	require.Len(t, output.Configured, 1)
	require.Len(t, output.Available, 1)
	assert.Equal(t, "personal:user@example.com", output.Configured[0].CanonicalID)
	assert.Equal(t, "configured", output.Configured[0].Source)
	assert.Equal(t, "business:user@contoso.com", output.Available[0].CanonicalID)
	assert.Equal(t, "available", output.Available[0].Source)
}

func TestPrintDriveListJSON_NilSlicesRenderAsEmptyArrays(t *testing.T) {
	origStdout := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)

	os.Stdout = w

	writeErr := printDriveListJSON(nil, nil)
	w.Close()
	os.Stdout = origStdout

	require.NoError(t, writeErr)

	var output map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(r).Decode(&output))
	assert.Equal(t, "[]", string(output["configured"]))
	assert.Equal(t, "[]", string(output["available"]))
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
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
		{CanonicalID: "sharepoint:user@contoso.com:hr:Docs", SiteName: "HR", LibraryName: "Docs"},
	}
	output := captureStdout(t, func() { printDriveSearchText(results, "docs") })
	// Verify alphabetical sort: HR should appear before Marketing.
	hrIdx := strings.Index(output, "HR")
	mktIdx := strings.Index(output, "Marketing")
	require.NotEqual(t, -1, hrIdx, "HR should appear in output")
	require.NotEqual(t, -1, mktIdx, "Marketing should appear in output")
	assert.Less(t, hrIdx, mktIdx, "HR should appear before Marketing (alphabetical)")
}

func TestPrintDriveSearchText_DoesNotMutateInput(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing"},
		{CanonicalID: "sharepoint:user@contoso.com:hr:Docs", SiteName: "HR"},
	}
	// Copy original order.
	orig0 := results[0].SiteName
	orig1 := results[1].SiteName

	captureStdout(t, func() { printDriveSearchText(results, "docs") })

	assert.Equal(t, orig0, results[0].SiteName, "input slice should not be mutated")
	assert.Equal(t, orig1, results[1].SiteName, "input slice should not be mutated")
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
	tokens := findBusinessTokens("", testDriveLogger(t))
	assert.Empty(t, tokens)
}

func TestFindBusinessTokens_HasBusinessToken(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Create business and personal token files.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json", nil)
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", nil)

	tokens := findBusinessTokens("", testDriveLogger(t))
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

func TestFindBusinessTokens_FilterSelectsOne(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	// Two business tokens for different accounts.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json", nil)
	writeTestTokenFile(t, dataDir, "token_business_bob@fabrikam.com.json", nil)

	tokens := findBusinessTokens("alice@contoso.com", testDriveLogger(t))
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

func TestFindBusinessTokens_SkipsPersonal(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json", nil)

	tokens := findBusinessTokens("", testDriveLogger(t))
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

// --- removeDrive ---

func TestRemoveDrive_DeletesConfigSection(t *testing.T) {
	// Create a config file with a drive.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	cid := driveid.MustCanonicalID("personal:user@example.com")
	err := removeDrive(cfgPath, cid, "~/OneDrive", testDriveLogger(t))
	assert.NoError(t, err)

	// Verify the drive section was deleted.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(data), "personal:user@example.com")
}

func TestRemoveDrive_DriveNotInConfig(t *testing.T) {
	// removeDrive should return an error when the drive doesn't exist in config.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	err := removeDrive(cfgPath, cid, "~/Work", testDriveLogger(t))
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "removing drive")
}

// --- purgeSingleDrive ---

func TestPurgeSingleDrive_DeletesStateDB(t *testing.T) {
	// Isolate HOME so DriveStatePath uses a temp directory.
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o755))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create a fake state DB file at the platform default path.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	// Create a config file with this drive.
	cfgDir := t.TempDir()
	cfgPath := filepath.Join(cfgDir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	// Purge should delete both state DB and config section.
	err := purgeSingleDrive(cfgPath, cid, testDriveLogger(t))
	require.NoError(t, err)

	// State DB file should be gone.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr), "state DB should be deleted")

	// Config section should be gone.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(data), "personal:user@example.com")
}

// --- removeAccountDriveConfigs ---

func TestRemoveAccountDriveConfigs_RemovesMultiple(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	// Create config with 2 drives for the same account.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["business:alice@contoso.com"]
sync_dir = "~/Work"

["sharepoint:alice@contoso.com:marketing:Documents"]
sync_dir = "~/Marketing"
`), 0o600))

	affected := []driveid.CanonicalID{
		driveid.MustCanonicalID("business:alice@contoso.com"),
		driveid.MustCanonicalID("sharepoint:alice@contoso.com:marketing:Documents"),
	}

	err := removeAccountDriveConfigs(cfgPath, affected, testDriveLogger(t))
	require.NoError(t, err)

	// Reload and verify 0 drives remain.
	cfg, loadErr := config.Load(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	assert.Empty(t, cfg.Drives)
}

func TestRemoveAccountDriveConfigs_ContinuesOnError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	// Config has one drive but we pass a non-existent CID too.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	affected := []driveid.CanonicalID{
		driveid.MustCanonicalID("business:nobody@example.com"), // doesn't exist
		driveid.MustCanonicalID("personal:user@example.com"),   // exists
	}

	// Continues past the missing one, returns error for it.
	err := removeAccountDriveConfigs(cfgPath, affected, testDriveLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "business:nobody@example.com")

	// The existing drive should still have been removed.
	cfg, loadErr := config.Load(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	assert.Empty(t, cfg.Drives)
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
	assert.Contains(t, err.Error(), "no token file")
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

	// Verify config was updated with canonical ID and sync_dir.
	data, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "personal:user@example.com")
	assert.Contains(t, string(data), "sync_dir")
	assert.Contains(t, string(data), "OneDrive")
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

// writeTestTokenFile creates a token file using the canonical tokenfile.Save,
// ensuring test files match the real on-disk format exactly.
func writeTestTokenFile(t *testing.T, dir, name string, meta map[string]string) {
	t.Helper()

	tok := &oauth2.Token{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, tokenfile.Save(filepath.Join(dir, name), tok, meta))
}
