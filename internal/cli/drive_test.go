package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
	"github.com/tonimelisma/onedrive-go/internal/localpath"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

// --- command structure ---

// Validates: R-3.3.2, R-3.3.5, R-3.3.7, R-3.3.9
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

// Validates: R-3.3.4
func TestNewDriveListCmd_HasAllFlag(t *testing.T) {
	cmd := newDriveListCmd()
	flag := cmd.Flags().Lookup("all")
	require.NotNil(t, flag, "--all flag should be registered")
	assert.Equal(t, "false", flag.DefValue)
}

func TestNewDriveSearchCmd_HasRunE(t *testing.T) {
	cmd := newDriveSearchCmd()
	assert.NotNil(t, cmd.RunE)
	assert.Equal(t, "search <term>", cmd.Use)
}

// --- buildConfiguredDriveEntries ---

// Validates: R-3.3.2
func TestBuildConfiguredDriveEntries_Empty(t *testing.T) {
	cfg := config.DefaultConfig()
	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	assert.Nil(t, entries)
}

// Validates: R-3.3.2
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

// Validates: R-3.3.2
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
	// Without account profile, personal defaults to "~/OneDrive".
	assert.Equal(t, "~/OneDrive", entries[0].SyncDir)
}

func TestBuildConfiguredDriveEntries_NoSyncDir_WithAccountProfile(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create a token file and an account profile with org_name.
	bizCID := driveid.MustCanonicalID("business:alice@contoso.com")
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json")
	require.NoError(t, config.SaveAccountProfile(bizCID, &config.AccountProfile{
		OrgName:     "Contoso",
		DisplayName: "Alice Smith",
	}))

	cfg := config.DefaultConfig()
	cfg.Drives[bizCID] = config.Drive{}

	entries := buildConfiguredDriveEntries(cfg, testDriveLogger(t))
	require.Len(t, entries, 1)
	assert.Equal(t, "~/OneDrive - Contoso", entries[0].SyncDir)
}

// Tests for CollectOtherSyncDirs and ResolveAccountNames live in
// internal/config/drive_test.go.

// --- listAvailableDrives ---

func TestListAvailableDrives_Empty(t *testing.T) {
	err := listAvailableDrives(io.Discard)
	assert.NoError(t, err)
}

// --- printDriveListText ---

func TestPrintDriveListText_EmptyBothSections(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, nil))
	assert.Contains(t, buf.String(), "No drives configured")
}

// Validates: R-3.3.2
func TestPrintDriveListText_ConfiguredOnly(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, configured, nil))
	output := buf.String()
	assert.Contains(t, output, "Configured drives:")
	assert.Contains(t, output, "personal:user@example.com")
}

// Validates: R-3.6.1
func TestPrintDriveListText_AvailableOnly(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", State: "", Source: "available", SiteName: "Marketing"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available))
	output := buf.String()
	assert.Contains(t, output, "Available drives")
	assert.Contains(t, output, "business:user@contoso.com")
}

// Validates: R-3.3.2, R-3.6.1
func TestPrintDriveListText_BothSections(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", Source: "available"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, configured, available))
	output := buf.String()
	assert.Contains(t, output, "Configured drives:")
	assert.Contains(t, output, "Available drives")
}

// Validates: R-3.5.1
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

// Validates: R-3.5.1
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
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, configured, nil))
	output := buf.String()
	assert.Contains(t, output, "user@example.com")
	assert.Contains(t, output, "personal:user@example.com")
}

func TestDriveLabel_WithDisplayName(t *testing.T) {
	e := &driveListEntry{
		CanonicalID: "personal:user@example.com",
		DisplayName: "user@example.com",
	}
	assert.Equal(t, "user@example.com (personal:user@example.com)", driveLabel(e))
}

func TestDriveLabel_WithoutDisplayName(t *testing.T) {
	e := &driveListEntry{CanonicalID: "personal:user@example.com"}
	assert.Equal(t, "personal:user@example.com", driveLabel(e))
}

func TestDriveLabel_DisplayNameSameAsCanonicalID(t *testing.T) {
	e := &driveListEntry{
		CanonicalID: "personal:user@example.com",
		DisplayName: "personal:user@example.com",
	}
	assert.Equal(t, "personal:user@example.com", driveLabel(e))
}

func TestPrintDriveListText_EmptySyncDir_ShowsNotSet(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "", State: driveStateReady, Source: "configured"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, configured, nil))
	output := buf.String()
	assert.Contains(t, output, "(not set)")
}

// --- printDriveListJSON ---

func TestPrintDriveListJSON_Empty(t *testing.T) {
	var buf bytes.Buffer
	err := printDriveListJSON(&buf, nil, nil)
	assert.NoError(t, err)
}

// Validates: R-3.3.10
func TestPrintDriveListJSON_VerifyOutput(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: "business:user@contoso.com", Source: "available", SiteName: "Marketing"},
	}
	authRequired := []accountAuthRequirement{
		{
			Email:     "user@example.com",
			DriveType: "personal",
			Reason:    authReasonSyncAuthRejected,
			Action:    authAction(authReasonSyncAuthRejected),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printDriveListJSON(&buf, configured, available, authRequired))

	var output driveListJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	require.Len(t, output.Configured, 1)
	require.Len(t, output.Available, 1)
	require.Len(t, output.AccountsRequiringAuth, 1)
	assert.Equal(t, "personal:user@example.com", output.Configured[0].CanonicalID)
	assert.Equal(t, "configured", output.Configured[0].Source)
	assert.Equal(t, "business:user@contoso.com", output.Available[0].CanonicalID)
	assert.Equal(t, "available", output.Available[0].Source)
	assert.Equal(t, authReasonSyncAuthRejected, output.AccountsRequiringAuth[0].Reason)
}

func TestPrintDriveListJSON_NilSlicesRenderAsEmptyArrays(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printDriveListJSON(&buf, nil, nil))

	var output map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
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

// --- HasStateDB indicator ---

// Validates: R-3.3.3
func TestPrintDriveListText_HasStateDB_ShowsMarker(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "personal:user@example.com", State: "available", Source: "available", HasStateDB: true},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available))
	output := buf.String()
	assert.Contains(t, output, "[has sync data]")
}

// Validates: R-3.3.3
func TestPrintDriveListText_NoStateDB_NoMarker(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "personal:user@example.com", State: "available", Source: "available", HasStateDB: false},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available))
	output := buf.String()
	assert.NotContains(t, output, "[has sync data]")
}

// Validates: R-3.3.3
func TestDriveListEntry_HasStateDB_JSON(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "personal:user@example.com",
		State:       "available",
		Source:      "available",
		HasStateDB:  true,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"has_state_db":true`)
}

// Validates: R-3.3.3
func TestDriveListEntry_HasStateDB_OmittedWhenFalse(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "personal:user@example.com",
		State:       "available",
		Source:      "available",
		HasStateDB:  false,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "has_state_db")
}

// --- printDriveSearchText ---

func TestPrintDriveSearchText_Empty(t *testing.T) {
	// Should not panic with no results.
	var buf bytes.Buffer
	require.NoError(t, printDriveSearchText(&buf, nil, "test query"))
}

// Validates: R-3.3.9
func TestPrintDriveSearchText_AuthRequiredWithoutResultsStillExplainsNoMatches(t *testing.T) {
	authRequired := []accountAuthRequirement{
		{
			Email:     "blocked@example.com",
			DriveType: driveid.DriveTypeBusiness,
			Reason:    authReasonInvalidSavedLogin,
			Action:    authAction(authReasonInvalidSavedLogin),
			StateDBs:  1,
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printDriveSearchText(&buf, nil, "marketing", authRequired))

	output := buf.String()
	assert.Contains(t, output, "Authentication required:")
	assert.Contains(t, output, `No SharePoint sites found matching "marketing" in searchable business accounts.`)
}

// Validates: R-3.3.9
func TestPrintDriveSearchText_WithResults(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs", WebURL: "https://contoso.sharepoint.com/sites/marketing"},
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Wiki", SiteName: "Marketing", LibraryName: "Wiki"},
	}
	var buf bytes.Buffer
	assert.NotPanics(t, func() { require.NoError(t, printDriveSearchText(&buf, results, "marketing")) })
}

// Validates: R-3.3.9
func TestPrintDriveSearchText_MultipleSites(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
		{CanonicalID: "sharepoint:user@contoso.com:hr:Docs", SiteName: "HR", LibraryName: "Docs"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveSearchText(&buf, results, "docs"))
	output := buf.String()
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

	var buf bytes.Buffer
	require.NoError(t, printDriveSearchText(&buf, results, "docs"))

	assert.Equal(t, orig0, results[0].SiteName, "input slice should not be mutated")
	assert.Equal(t, orig1, results[1].SiteName, "input slice should not be mutated")
}

// --- printDriveSearchJSON ---

func TestPrintDriveSearchJSON_NoError(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
	}
	var buf bytes.Buffer
	err := printDriveSearchJSON(&buf, results)
	assert.NoError(t, err)
}

// Validates: R-3.3.11
func TestPrintDriveSearchJSON_IncludesAuthRequiredAccounts(t *testing.T) {
	results := []driveSearchResult{
		{CanonicalID: "sharepoint:user@contoso.com:marketing:Docs", SiteName: "Marketing", LibraryName: "Docs"},
	}
	authRequired := []accountAuthRequirement{
		{
			Email:     "user@contoso.com",
			DriveType: "business",
			Reason:    authReasonMissingLogin,
			Action:    authAction(authReasonMissingLogin),
		},
	}

	var buf bytes.Buffer
	require.NoError(t, printDriveSearchJSON(&buf, results, authRequired))

	var decoded driveSearchJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	require.Len(t, decoded.Results, 1)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, authReasonMissingLogin, decoded.AccountsRequiringAuth[0].Reason)
}

func TestPrintDriveSearchJSON_EmptySlice(t *testing.T) {
	var buf bytes.Buffer
	err := printDriveSearchJSON(&buf, []driveSearchResult{})
	assert.NoError(t, err)
}

// --- searchableBusinessTokenIDs ---

func TestSearchableBusinessTokenIDs_NoTokens(t *testing.T) {
	setTestDriveHome(t)
	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	tokens := searchableBusinessTokenIDs(catalog, "")
	assert.Empty(t, tokens)
}

// Validates: R-3.3.9
func TestSearchableBusinessTokenIDs_HasBusinessToken(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create business and personal token files.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json")
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	tokens := searchableBusinessTokenIDs(catalog, "")
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

// Validates: R-3.3.9
func TestSearchableBusinessTokenIDs_FilterSelectsOne(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Two business tokens for different accounts.
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json")
	writeTestTokenFile(t, dataDir, "token_business_bob@fabrikam.com.json")

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	tokens := searchableBusinessTokenIDs(catalog, "alice@contoso.com")
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

// Validates: R-3.3.9
func TestSearchableBusinessTokenIDs_SkipsPersonal(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	catalog := buildAccountCatalog(t.Context(), config.DefaultConfig(), testDriveLogger(t))
	tokens := searchableBusinessTokenIDs(catalog, "")
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

// Validates: R-3.3.7
func TestRemoveDrive_DeletesConfigSection(t *testing.T) {
	// Create a config file with a drive.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	cid := driveid.MustCanonicalID("personal:user@example.com")
	err := removeDrive(io.Discard, cfgPath, cid, "~/OneDrive", testDriveLogger(t))
	require.NoError(t, err)

	// Verify the drive section was deleted.
	data, readErr := localpath.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(data), "personal:user@example.com")
}

// Validates: R-3.3.7
func TestRemoveDrive_DriveNotInConfig(t *testing.T) {
	// removeDrive should return an error when the drive doesn't exist in config.
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"
`), 0o600))

	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	err := removeDrive(io.Discard, cfgPath, cid, "~/Work", testDriveLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "removing drive")
}

// --- purgeSingleDrive ---

// Validates: R-3.3.8
func TestPurgeSingleDrive_DeletesDriveStateButPreservesAccountProfile(t *testing.T) {
	// Isolate HOME so DriveStatePath uses a temp directory.
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create a fake state DB file at the platform default path.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "User Example",
	}))

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

	profile, found, profileErr := config.LookupAccountProfile(cid)
	require.NoError(t, profileErr)
	require.True(t, found, "drive purge must preserve account profile state")
	assert.Equal(t, "User Example", profile.DisplayName)

	// Config section should be gone.
	data, readErr := localpath.ReadFile(cfgPath)
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

	cid := driveid.MustCanonicalID("personal:nobody@example.com")

	err := addNewDrive(io.Discard, cfgPath, cid, testDriveLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token file")
}

// Validates: R-3.3.5
func TestAddNewDrive_WithToken(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("personal:user@example.com")

	err := addNewDrive(io.Discard, cfgPath, cid, testDriveLogger(t))
	require.NoError(t, err)

	// Verify config was updated with canonical ID and sync_dir.
	data, readErr := localpath.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.Contains(t, string(data), "personal:user@example.com")
	assert.Contains(t, string(data), "sync_dir")
	assert.Contains(t, string(data), "OneDrive")
}

// --- deriveSharedDisplayName ---

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_Basic(t *testing.T) {
	item := &graph.Item{
		Name:             "Documents",
		SharedOwnerName:  "John Doe",
		SharedOwnerEmail: "john@example.com",
	}
	name, err := deriveSharedDisplayName(item, nil)
	require.NoError(t, err)
	assert.Equal(t, "John's Documents", name)
}

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_FirstNameCollision(t *testing.T) {
	item := &graph.Item{
		Name:             "Documents",
		SharedOwnerName:  "John Doe",
		SharedOwnerEmail: "john@example.com",
	}
	existing := map[string]bool{"John's Documents": true}
	name, err := deriveSharedDisplayName(item, existing)
	require.NoError(t, err)
	assert.Equal(t, "John Doe's Documents", name)
}

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_FullNameCollision(t *testing.T) {
	item := &graph.Item{
		Name:             "Documents",
		SharedOwnerName:  "John Doe",
		SharedOwnerEmail: "john@example.com",
	}
	existing := map[string]bool{
		"John's Documents":     true,
		"John Doe's Documents": true,
	}
	name, err := deriveSharedDisplayName(item, existing)
	require.NoError(t, err)
	assert.Equal(t, "John Doe's Documents (john@example.com)", name)
}

func TestDeriveSharedDisplayName_SingleName(t *testing.T) {
	item := &graph.Item{
		Name:             "Shared Stuff",
		SharedOwnerName:  "Alice",
		SharedOwnerEmail: "alice@example.com",
	}
	name, err := deriveSharedDisplayName(item, nil)
	require.NoError(t, err)
	assert.Equal(t, "Alice's Shared Stuff", name)
}

func TestDeriveSharedDisplayName_EmptyOwnerNameWithEmail(t *testing.T) {
	item := &graph.Item{
		Name:             "Folder",
		SharedOwnerName:  "",
		SharedOwnerEmail: "unknown@example.com",
	}
	name, err := deriveSharedDisplayName(item, nil)
	require.NoError(t, err)
	assert.Equal(t, "Folder (shared by unknown@example.com)", name)
}

func TestDeriveSharedDisplayName_NoIdentity(t *testing.T) {
	item := &graph.Item{
		Name:             "Folder",
		SharedOwnerName:  "",
		SharedOwnerEmail: "",
	}
	_, err := deriveSharedDisplayName(item, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no owner identity")
}

// --- printDriveListText shared drives ---

// Validates: R-3.6.1
func TestPrintDriveListText_SharedDrive(t *testing.T) {
	available := []driveListEntry{
		{
			CanonicalID: "shared:user@example.com:drive1:item1",
			State:       "available",
			Source:      "available",
			OwnerEmail:  "alice@example.com",
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available))
	output := buf.String()
	assert.Contains(t, output, "shared by alice@example.com")
}

func TestPrintDriveListText_SharedAndSharePoint(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "sharepoint:u@c.com:site:lib", State: "available", Source: "available", SiteName: "Marketing"},
		{CanonicalID: "shared:u@c.com:d:i", State: "available", Source: "available", OwnerEmail: "bob@example.com"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available))
	output := buf.String()
	assert.Contains(t, output, "(Marketing)")
	assert.Contains(t, output, "(shared by bob@example.com)")
}

// --- driveListEntry JSON ---

func TestDriveListEntry_SharedFieldsJSON(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "shared:user@example.com:driveX:itemY",
		State:       "available",
		Source:      "available",
		OwnerName:   "Alice",
		OwnerEmail:  "alice@example.com",
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"owner_name":"Alice"`)
	assert.Contains(t, string(data), `"owner_email":"alice@example.com"`)
}

func TestDriveListEntry_SharedFieldsOmittedWhenEmpty(t *testing.T) {
	entry := driveListEntry{
		CanonicalID: "personal:user@example.com",
		State:       "ready",
		Source:      "configured",
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.NotContains(t, string(data), "owner_name")
	assert.NotContains(t, string(data), "owner_email")
}

// --- addSharedDrive ---

// Validates: R-3.3.5
func TestAddSharedDrive_AlreadyConfigured(t *testing.T) {
	setTestDriveHome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("shared:user@example.com:driveX:itemY")

	// Write a config that already has this drive.
	require.NoError(t, os.WriteFile(cfgPath, []byte(`
["personal:user@example.com"]
sync_dir = "~/OneDrive"

["shared:user@example.com:driveX:itemY"]
sync_dir = "~/OneDrive-Shared/Test"
`), 0o600))

	err := addSharedDrive(t.Context(), cfgPath, io.Discard, cid, "", testDriveLogger(t), graphhttp.NewProvider(testDriveLogger(t)))
	assert.NoError(t, err)
}

func TestAddSharedDrive_NoToken(t *testing.T) {
	setTestDriveHome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("shared:nobody@example.com:driveX:itemY")

	// Empty config — no primary drive to resolve token from.
	require.NoError(t, os.WriteFile(cfgPath, []byte(""), 0o600))

	err := addSharedDrive(t.Context(), cfgPath, io.Discard, cid, "", testDriveLogger(t), graphhttp.NewProvider(testDriveLogger(t)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token file")
}

// --- collectExistingDisplayNames ---

func TestCollectExistingDisplayNames(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Drives[driveid.MustCanonicalID("personal:user@example.com")] = config.Drive{
		SyncDir:     "~/OneDrive",
		DisplayName: "My Drive",
	}
	cfg.Drives[driveid.MustCanonicalID("business:alice@contoso.com")] = config.Drive{
		SyncDir: "~/Work",
	}

	names := collectExistingDisplayNames(cfg)
	assert.True(t, names["My Drive"])
	assert.True(t, names["alice@contoso.com"]) // DefaultDisplayName fallback
}

// --- extractFirstName ---

func TestExtractFirstName(t *testing.T) {
	assert.Equal(t, "John", extractFirstName("John Doe"))
	assert.Equal(t, "Alice", extractFirstName("Alice"))
	assert.Equal(t, "Mary", extractFirstName("Mary Jane Watson"))
	assert.Empty(t, extractFirstName(""))
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
func writeTestTokenFile(t *testing.T, dir, name string) {
	t.Helper()

	tok := &oauth2.Token{
		AccessToken:  "test-access-token",
		RefreshToken: "test-refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	require.NoError(t, tokenfile.Save(filepath.Join(dir, name), tok))
}

// --- enrichSharedItem tests ---

// staticTokenSource implements graph.TokenSource for testing.
type staticTokenSource struct{}

func (s staticTokenSource) Token() (string, error) { return "test-token", nil }

func newTestGraphClient(t *testing.T, baseURL string) *graph.Client {
	t.Helper()

	if baseURL == "http://should-not-be-called" {
		baseURL = "http://localhost"
	}

	return graph.MustNewClient(baseURL, http.DefaultClient, staticTokenSource{}, slog.Default(), "test")
}

func TestEnrichSharedItem_AlreadyHasEmail(t *testing.T) {
	// No server needed — should return immediately without API call.
	client := newTestGraphClient(t, "http://should-not-be-called")

	item := &graph.Item{
		Name:             "Shared Folder",
		SharedOwnerEmail: "owner@example.com",
		SharedOwnerName:  "Owner",
		RemoteDriveID:    "b!abc123",
		RemoteItemID:     "01DEFGH",
	}

	enrichSharedItem(t.Context(), client, item, slog.Default())

	assert.Equal(t, "owner@example.com", item.SharedOwnerEmail)
	assert.Equal(t, "Owner", item.SharedOwnerName)
}

func TestEnrichSharedItem_MissingEmail_GetItemSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "/drives/")
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "01DEFGH",
			"name": "Shared Folder",
			"createdDateTime": "2024-01-15T10:00:00Z",
			"lastModifiedDateTime": "2024-01-15T10:00:00Z",
			"parentReference": {"driveId": "b!abc123"},
			"folder": {},
			"shared": {
				"owner": {
					"user": {
						"displayName": "Bob Jones",
						"email": "bob@contoso.com"
					}
				}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)

	item := &graph.Item{
		Name:          "Shared Folder",
		RemoteDriveID: "b!abc123",
		RemoteItemID:  "01DEFGH",
	}

	enrichSharedItem(t.Context(), client, item, slog.Default())

	assert.Equal(t, "Bob Jones", item.SharedOwnerName)
	assert.Equal(t, "bob@contoso.com", item.SharedOwnerEmail)
}

func TestEnrichSharedItem_MissingEmail_GetItemFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"not found"}}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)

	item := &graph.Item{
		Name:            "Shared Folder",
		SharedOwnerName: "Partial",
		RemoteDriveID:   "b!abc123",
		RemoteItemID:    "01DEFGH",
	}

	enrichSharedItem(t.Context(), client, item, slog.Default())

	// Original fields preserved on failure.
	assert.Equal(t, "Partial", item.SharedOwnerName)
	assert.Empty(t, item.SharedOwnerEmail)
}

func TestEnrichSharedItem_NoRemoteDriveID(t *testing.T) {
	client := newTestGraphClient(t, "http://should-not-be-called")

	item := &graph.Item{
		Name:         "Shared Folder",
		RemoteItemID: "01DEFGH",
		// RemoteDriveID is empty — should not make API call
	}

	enrichSharedItem(t.Context(), client, item, slog.Default())

	assert.Empty(t, item.SharedOwnerName)
	assert.Empty(t, item.SharedOwnerEmail)
}

func TestEnrichSharedItem_GetItemReturnsNameOnly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		writeTestResponse(t, w, `{
			"id": "01DEFGH",
			"name": "Shared Folder",
			"createdDateTime": "2024-01-15T10:00:00Z",
			"lastModifiedDateTime": "2024-01-15T10:00:00Z",
			"parentReference": {"driveId": "b!abc123"},
			"folder": {},
			"shared": {
				"owner": {
					"user": {
						"displayName": "Bob Jones"
					}
				}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)

	item := &graph.Item{
		Name:          "Shared Folder",
		RemoteDriveID: "b!abc123",
		RemoteItemID:  "01DEFGH",
	}

	enrichSharedItem(t.Context(), client, item, slog.Default())

	assert.Equal(t, "Bob Jones", item.SharedOwnerName)
	assert.Empty(t, item.SharedOwnerEmail) // only name, no email
}

// --- searchSharedItemsWithFallback tests ---

// sharedItemJSON returns a minimal JSON representation of a shared folder.
func sharedItemJSON(name string) string {
	return fmt.Sprintf(`{
		"id": "item-%s",
		"name": %q,
		"createdDateTime": "2024-01-15T10:00:00Z",
		"lastModifiedDateTime": "2024-01-15T10:00:00Z",
		"parentReference": {"driveId": "b!abc123"},
		"folder": {},
		"remoteItem": {
			"id": "remote-1",
			"parentReference": {"driveId": "b!remote123"}
		}
	}`, name, name)
}

// Validates: R-3.6.2
func TestSearchSharedItemsWithFallback_SearchSucceeds(t *testing.T) {
	var sharedWithMeCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "search") {
			writeTestResponsef(t, w, `{"value": [%s]}`, sharedItemJSON("SearchResult"))

			return
		}

		if strings.Contains(r.URL.Path, "sharedWithMe") {
			sharedWithMeCalled = true
		}

		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items := searchSharedItemsWithFallback(t.Context(), client, "test@example.com", slog.Default())

	require.Len(t, items, 1)
	assert.Equal(t, "SearchResult", items[0].Name)
	assert.False(t, sharedWithMeCalled, "SharedWithMe should not be called when search succeeds")
}

// Validates: R-3.6.2
func TestSearchSharedItemsWithFallback_SearchFails_SharedWithMeSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "search") {
			w.WriteHeader(http.StatusForbidden)
			writeTestResponse(t, w, `{"error":{"code":"generalException","message":"search failed"}}`)

			return
		}

		if strings.Contains(r.URL.Path, "sharedWithMe") {
			writeTestResponsef(t, w, `{"value": [%s]}`, sharedItemJSON("FallbackResult"))

			return
		}

		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items := searchSharedItemsWithFallback(t.Context(), client, "test@example.com", slog.Default())

	require.Len(t, items, 1)
	assert.Equal(t, "FallbackResult", items[0].Name)
}

func TestSearchSharedItemsWithFallback_BothFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"generalException","message":"failed"}}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items := searchSharedItemsWithFallback(t.Context(), client, "test@example.com", slog.Default())

	assert.Nil(t, items)
}

func TestSearchSharedItemsWithFallback_SearchReturnsEmpty(t *testing.T) {
	var sharedWithMeCalled bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(r.URL.Path, "search") {
			writeTestResponse(t, w, `{"value": []}`)

			return
		}

		if strings.Contains(r.URL.Path, "sharedWithMe") {
			sharedWithMeCalled = true
		}

		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items := searchSharedItemsWithFallback(t.Context(), client, "test@example.com", slog.Default())

	assert.Empty(t, items)
	assert.False(t, sharedWithMeCalled, "empty is not an error — no fallback needed")
}

// --- annotateStateDB ---

// Validates: R-3.3.3
func TestAnnotateStateDB_DetectsExistingDB(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create a state DB file on disk.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	entries := []driveListEntry{
		{CanonicalID: cid.String(), parsedCID: cid, State: "available", Source: "available"},
	}

	annotateStateDB(entries)
	assert.True(t, entries[0].HasStateDB, "should detect existing state DB")
}

// Validates: R-3.3.3
func TestAnnotateStateDB_NoDBFile(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	entries := []driveListEntry{
		{CanonicalID: cid.String(), parsedCID: cid, State: "available", Source: "available"},
	}

	annotateStateDB(entries)
	assert.False(t, entries[0].HasStateDB, "should not detect state DB when file doesn't exist")
}

// Validates: R-3.3.3
func TestAnnotateStateDB_MixedEntries(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cidWithDB := driveid.MustCanonicalID("personal:user@example.com")
	cidWithoutDB := driveid.MustCanonicalID("business:alice@contoso.com")

	// Create state DB only for the first drive.
	statePath := config.DriveStatePath(cidWithDB)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	entries := []driveListEntry{
		{CanonicalID: cidWithDB.String(), parsedCID: cidWithDB, State: "available", Source: "available"},
		{CanonicalID: cidWithoutDB.String(), parsedCID: cidWithoutDB, State: "available", Source: "available"},
	}

	annotateStateDB(entries)
	assert.True(t, entries[0].HasStateDB, "first entry should have state DB")
	assert.False(t, entries[1].HasStateDB, "second entry should not have state DB")
}

// --- purgeOrphanedDriveState ---

// Validates: R-3.3.8
func TestPurgeOrphanedDriveState_DeletesStateDB(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create a fake state DB file.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))

	err := purgeOrphanedDriveState(io.Discard, cid, testDriveLogger(t))
	require.NoError(t, err)

	// State DB should be deleted.
	_, statErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(statErr), "state DB should be deleted")
}

// Validates: R-3.3.8
func TestPurgeOrphanedDriveState_NoStateDB(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// No state DB on disk — should succeed with "no orphaned state" message.
	err := purgeOrphanedDriveState(io.Discard, cid, testDriveLogger(t))
	assert.NoError(t, err)
}

func TestSortedDriveSearchResults_ReturnsSortedClone(t *testing.T) {
	t.Parallel()

	input := []driveSearchResult{
		{SiteName: "Zulu", LibraryName: "B"},
		{SiteName: "Alpha", LibraryName: "B"},
		{SiteName: "Alpha", LibraryName: "A"},
	}

	sorted := sortedDriveSearchResults(input)

	assert.Equal(t, []driveSearchResult{
		{SiteName: "Alpha", LibraryName: "A"},
		{SiteName: "Alpha", LibraryName: "B"},
		{SiteName: "Zulu", LibraryName: "B"},
	}, sorted)
	assert.Equal(t, []driveSearchResult{
		{SiteName: "Zulu", LibraryName: "B"},
		{SiteName: "Alpha", LibraryName: "B"},
		{SiteName: "Alpha", LibraryName: "A"},
	}, input)
}

// Validates: R-3.3.10
func TestBuildConfiguredAuthRequirements_UsesOnlyAccountsNeedingAuth(t *testing.T) {
	setTestDriveHome(t)

	cfg := &config.Config{
		Drives: map[driveid.CanonicalID]config.Drive{
			driveid.MustCanonicalID("personal:ready@example.com"):   {},
			driveid.MustCanonicalID("business:blocked@example.com"): {},
		},
	}

	requirements := buildConfiguredAuthRequirements(cfg, map[string]accountAuthHealth{
		"ready@example.com": {
			State: authStateReady,
		},
		"blocked@example.com": {
			State:  authStateAuthenticationNeeded,
			Reason: authReasonSyncAuthRejected,
			Action: authAction(authReasonSyncAuthRejected),
		},
	}, testDriveLogger(t))

	require.Len(t, requirements, 1)
	assert.Equal(t, "blocked@example.com", requirements[0].Email)
	assert.Equal(t, driveid.DriveTypeBusiness, requirements[0].DriveType)
	assert.Equal(t, authReasonSyncAuthRejected, requirements[0].Reason)
}

// Validates: R-3.3.10
func TestAnnotateConfiguredDriveAuth_AndPrintSections(t *testing.T) {
	t.Parallel()

	blockedCID := driveid.MustCanonicalID("business:blocked@example.com")
	readyCID := driveid.MustCanonicalID("personal:ready@example.com")
	configured := []driveListEntry{
		{CanonicalID: blockedCID.String(), parsedCID: blockedCID, State: "ready", Source: "configured"},
	}
	available := []driveListEntry{
		{CanonicalID: readyCID.String(), parsedCID: readyCID, State: "available", Source: "available"},
	}

	annotateConfiguredDriveAuth(configured, map[string]accountAuthHealth{
		"blocked@example.com": {
			State:  authStateAuthenticationNeeded,
			Reason: authReasonMissingLogin,
		},
	})

	assert.Equal(t, authStateAuthenticationNeeded, configured[0].AuthState)
	assert.Equal(t, authReasonMissingLogin, configured[0].AuthReason)
	assert.Equal(t, "required", driveAuthLabel(&configured[0]))
	assert.Equal(t, authStateReady, driveAuthLabel(nil))
	assert.Nil(t, optionalAuthRequirements(nil))

	var buf bytes.Buffer
	require.NoError(t, printDriveListSections(&buf, configured, available, []accountAuthRequirement{{
		Email:  "blocked@example.com",
		Reason: authReasonMissingLogin,
		Action: authAction(authReasonMissingLogin),
	}}))
	assert.Contains(t, buf.String(), "Configured drives:")
	assert.Contains(t, buf.String(), "Available drives (not configured):")
	assert.Contains(t, buf.String(), "Authentication required:")
}

// Validates: R-2.10.47
func TestDriveService_RunList_ClearsPersistedAuthScopeAfterSuccessfulDiscovery(t *testing.T) {
	setTestDriveHome(t)

	const graphDrivesPath = "/me/drives"
	const primaryDrivePath = "/me/drive"

	cid := driveid.MustCanonicalID("personal:user@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedAuthScope(t, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-123","name":"OneDrive","driveType":"personal"}]}`)
		case r.URL.Path == primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-123","name":"OneDrive","driveType":"personal"}`)
		case strings.HasPrefix(r.URL.Path, "/me/drive/search("):
			writeTestResponse(t, w, `{"value":[]}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	svc := newDriveService(&CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	})

	require.NoError(t, svc.runList(t.Context(), false))
	assert.False(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))
}
