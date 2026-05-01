package cli

import (
	"bytes"
	"context"
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
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/sharedref"

	"github.com/tonimelisma/onedrive-go/internal/tokenfile"
)

const (
	testDriveSearchAllPath      = "/me/drive/root/search(q='*')"
	testGraphMePath             = "/me"
	testSharedWithMePath        = "/me/drive/sharedWithMe"
	driveTestUserExample        = "User Example"
	testSharedFolderGetItemPath = "/drives/b!drive1234567890/items/source-item-folder"
	testSharedFileGetItemPath   = "/drives/b!drive1234567891/items/source-item-file"
	graphDrivesPath             = "/me/drives"
	primaryDrivePath            = "/me/drive"
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
	assert.Contains(t, subNames, "reset-sync-state")
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
	// Without a catalog account entry, personal defaults to "~/OneDrive".
	assert.Equal(t, "~/OneDrive", entries[0].SyncDir)
}

func TestBuildConfiguredDriveEntries_NoSyncDir_WithCatalogAccount(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	// Create a token file and a catalog account with org_name.
	bizCID := driveid.MustCanonicalID("business:alice@contoso.com")
	writeTestTokenFile(t, dataDir, "token_business_alice@contoso.com.json")
	seedCatalogAccount(t, bizCID, func(account *config.CatalogAccount) {
		account.OrgName = workflowTestOrgContoso
		account.DisplayName = snapshotTestDisplayNameAliceSmith
	})

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
	var buf bytes.Buffer
	require.NoError(t, listAvailableDrives(&buf))
	assert.Equal(t,
		"Run 'onedrive-go drive add <canonical-id>' to add a drive.\nRun 'onedrive-go drive list' to see available drives.\n",
		buf.String(),
	)
}

// --- printDriveListText ---

func TestPrintDriveListText_EmptyBothSections(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, nil, nil, nil))
	assert.Contains(t, buf.String(), "No drives configured")
}

// Validates: R-3.3.2
func TestPrintDriveListText_ConfiguredOnly(t *testing.T) {
	configured := []driveListEntry{
		{CanonicalID: "personal:user@example.com", SyncDir: "~/OneDrive", State: driveStateReady, Source: "configured"},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, configured, nil, nil, nil))
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
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
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
	require.NoError(t, printDriveListText(&buf, configured, available, nil, nil))
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

func TestPrintDriveListText_IncludesDegradedSection(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, nil, nil, []accountDegradedNotice{
		driveCatalogDegradedNotice("user@example.com", "Test User", driveid.DriveTypePersonal),
	}))

	output := buf.String()
	assert.Contains(t, output, "Accounts with degraded live discovery:")
	assert.Contains(t, output, "Test User (user@example.com)")
	assert.Contains(t, output, degradedReasonText(driveCatalogUnavailableReason))
}

type fakeAccessibleDriveClient struct {
	drives     []graph.Drive
	drivesErr  error
	primary    *graph.Drive
	primaryErr error
}

func (f fakeAccessibleDriveClient) Drives(context.Context) ([]graph.Drive, error) {
	return f.drives, f.drivesErr
}

func (f fakeAccessibleDriveClient) PrimaryDrive(context.Context) (*graph.Drive, error) {
	return f.primary, f.primaryErr
}

func TestDiscoverAccessibleDrives_DegradesToPrimaryDrive(t *testing.T) {
	cfg := config.DefaultConfig()
	entries, authRequired, degraded := discoverAccessibleDrives(
		t.Context(),
		fakeAccessibleDriveClient{
			drivesErr: graph.ErrForbidden,
			primary: &graph.Drive{
				ID:        driveid.New("drive-primary"),
				Name:      "OneDrive",
				DriveType: driveid.DriveTypePersonal,
			},
		},
		cfg,
		nil,
		driveid.MustCanonicalID("personal:user@example.com"),
		testDriveLogger(t),
	)
	require.Empty(t, authRequired)
	require.Len(t, entries, 1)
	assert.Equal(t, "personal:user@example.com", entries[0].CanonicalID)
	require.Len(t, degraded, 1)
	assert.Equal(t, driveCatalogUnavailableReason, degraded[0].Reason)
}

func TestDiscoverAccessibleDrives_UnauthorizedReturnsAuthRequired(t *testing.T) {
	entries, authRequired, degraded := discoverAccessibleDrives(
		t.Context(),
		fakeAccessibleDriveClient{
			drivesErr: graph.ErrUnauthorized,
		},
		config.DefaultConfig(),
		nil,
		driveid.MustCanonicalID("business:user@contoso.com"),
		testDriveLogger(t),
	)
	assert.Empty(t, entries)
	assert.Empty(t, degraded)
	require.Len(t, authRequired, 1)
	assert.Equal(t, "user@contoso.com", authRequired[0].Email)
	assert.Equal(t, authReasonSyncAuthRejected, authRequired[0].Reason)
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
	require.NoError(t, printDriveListText(&buf, configured, nil, nil, nil))
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
	require.NoError(t, printDriveListText(&buf, configured, nil, nil, nil))
	output := buf.String()
	assert.Contains(t, output, "(not set)")
}

// --- printDriveListJSON ---

func TestPrintDriveListJSON_Empty(t *testing.T) {
	var buf bytes.Buffer
	require.NoError(t, printDriveListJSON(&buf, nil, nil, nil, nil))

	var output driveListJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	assert.Empty(t, output.Configured)
	assert.Empty(t, output.Available)
	assert.Nil(t, output.AccountsRequiringAuth)
	assert.Nil(t, output.AccountsDegraded)
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
	require.NoError(t, printDriveListJSON(&buf, configured, available, authRequired, nil))

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
	require.NoError(t, printDriveListJSON(&buf, nil, nil, nil, nil))

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
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
	output := buf.String()
	assert.Contains(t, output, "[has sync data]")
}

// Validates: R-3.3.3
func TestPrintDriveListText_NoStateDB_NoMarker(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "personal:user@example.com", State: "available", Source: "available", HasStateDB: false},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
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
	require.NoError(t, printDriveSearchJSON(&buf, results))

	var output driveSearchJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	require.Len(t, output.Results, 1)
	assert.Equal(t, results, output.Results)
	assert.Nil(t, output.AccountsRequiringAuth)
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
	require.NoError(t, printDriveSearchJSON(&buf, []driveSearchResult{}))

	var output driveSearchJSONOutput
	require.NoError(t, json.Unmarshal(buf.Bytes(), &output))
	assert.Empty(t, output.Results)
	assert.Nil(t, output.AccountsRequiringAuth)
}

// --- searchableBusinessTokenIDs ---

func TestSearchableBusinessTokenIDs_NoTokens(t *testing.T) {
	setTestDriveHome(t)
	views := loadDefaultAccountViews(t)
	tokens := searchableBusinessTokenIDs(views, "")
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

	views := loadDefaultAccountViews(t)
	tokens := searchableBusinessTokenIDs(views, "")
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

	views := loadDefaultAccountViews(t)
	tokens := searchableBusinessTokenIDs(views, "alice@contoso.com")
	require.Len(t, tokens, 1)
	assert.Equal(t, "business:alice@contoso.com", tokens[0].String())
}

// Validates: R-3.3.9
func TestSearchableBusinessTokenIDs_SkipsPersonal(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	views := loadDefaultAccountViews(t)
	tokens := searchableBusinessTokenIDs(views, "")
	assert.Empty(t, tokens)
}

func loadDefaultAccountViews(t *testing.T) []accountView {
	t.Helper()

	stored, err := config.LoadCatalog()
	require.NoError(t, err)
	return buildAccountViews(t.Context(), config.DefaultConfig(), stored, testDriveLogger(t))
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
func TestPurgeSingleDrive_DeletesDriveStateButPreservesCatalogAccount(t *testing.T) {
	// Isolate HOME so DriveStatePath uses a temp directory.
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))

	cid := driveid.MustCanonicalID("personal:user@example.com")

	// Create a fake state DB file at the platform default path.
	statePath := config.DriveStatePath(cid)
	require.NotEmpty(t, statePath)
	require.NoError(t, os.WriteFile(statePath, []byte("fake-db"), 0o600))
	seedCatalogAccount(t, cid, func(account *config.CatalogAccount) {
		account.DisplayName = driveTestUserExample
	})

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

	profile, found := loadCatalogAccount(t, cid)
	require.True(t, found, "drive purge must preserve catalog account state")
	assert.Equal(t, driveTestUserExample, profile.DisplayName)

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

	home, homeErr := os.UserHomeDir()
	require.NoError(t, homeErr)
	info, statErr := os.Stat(filepath.Join(home, "OneDrive"))
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
}

func TestAddNewDrive_SyncDirCreateFailureRollsBackConfig(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	home, homeErr := os.UserHomeDir()
	require.NoError(t, homeErr)
	require.NoError(t, os.WriteFile(filepath.Join(home, "OneDrive"), []byte("not a directory"), 0o600))

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	err := addNewDrive(io.Discard, cfgPath, cid, testDriveLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating sync directory")

	cfg, loadErr := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	assert.NotContains(t, cfg.Drives, cid)
}

// Validates: R-3.3.5
func TestAddNewDrive_SyncDirCreateFailureRollsBackBackfilledSyncDir(t *testing.T) {
	setTestDriveHome(t)
	dataDir := config.DefaultDataDir()
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	writeTestTokenFile(t, dataDir, "token_personal_user@example.com.json")

	home, homeErr := os.UserHomeDir()
	require.NoError(t, homeErr)
	require.NoError(t, os.WriteFile(filepath.Join(home, "OneDrive"), []byte("not a directory"), 0o600))

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`["personal:user@example.com"]
`), 0o600))

	err := addNewDrive(io.Discard, cfgPath, cid, testDriveLogger(t))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "creating sync directory")

	cfg, loadErr := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	drive, found := cfg.Drives[cid]
	require.True(t, found)
	assert.Empty(t, drive.SyncDir)
}

// Validates: R-3.3.5
func TestRestoreDriveAddConfigMutation_AddedSectionSkipsConcurrentEdits(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`["personal:user@example.com"]
sync_dir = "~/OneDrive"
display_name = "Concurrent"
`), 0o600))

	err := restoreDriveAddConfigMutation(cfgPath, cid, config.EnsureDriveInConfigResult{
		SyncDir: "~/OneDrive",
		Added:   true,
	})
	require.NoError(t, err)

	cfg, loadErr := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	drive, found := cfg.Drives[cid]
	require.True(t, found)
	assert.Equal(t, "~/OneDrive", drive.SyncDir)
	assert.Equal(t, "Concurrent", drive.DisplayName)
}

// Validates: R-3.3.5
func TestRestoreDriveAddConfigMutation_BackfillSkipsChangedSyncDir(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`["personal:user@example.com"]
sync_dir = "~/Concurrent"
`), 0o600))

	err := restoreDriveAddConfigMutation(cfgPath, cid, config.EnsureDriveInConfigResult{
		SyncDir:                    "~/OneDrive",
		BackfilledSyncDir:          true,
		DriveBeforeSyncDirBackfill: &config.Drive{},
	})
	require.NoError(t, err)

	cfg, loadErr := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	drive, found := cfg.Drives[cid]
	require.True(t, found)
	assert.Equal(t, "~/Concurrent", drive.SyncDir)
}

// Validates: R-3.3.5
func TestRestoreDriveAddConfigMutation_BackfillSkipsConcurrentEditsWithSameSyncDir(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:user@example.com")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`["personal:user@example.com"]
sync_dir = "~/OneDrive"
display_name = "Concurrent"
`), 0o600))

	err := restoreDriveAddConfigMutation(cfgPath, cid, config.EnsureDriveInConfigResult{
		SyncDir:                    "~/OneDrive",
		BackfilledSyncDir:          true,
		DriveBeforeSyncDirBackfill: &config.Drive{},
	})
	require.NoError(t, err)

	cfg, loadErr := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, loadErr)
	drive, found := cfg.Drives[cid]
	require.True(t, found)
	assert.Equal(t, "~/OneDrive", drive.SyncDir)
	assert.Equal(t, "Concurrent", drive.DisplayName)
}

// --- deriveSharedDisplayName ---

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_Basic(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Documents",
		SharedByName:  "John Doe",
		SharedByEmail: "john@example.com",
	}
	name := deriveSharedDisplayName(item, nil)
	assert.Equal(t, "John's Documents", name)
}

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_FirstNameCollision(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Documents",
		SharedByName:  "John Doe",
		SharedByEmail: "john@example.com",
	}
	existing := map[string]bool{"John's Documents": true}
	name := deriveSharedDisplayName(item, existing)
	assert.Equal(t, "John Doe's Documents", name)
}

// Validates: R-3.6.3
func TestDeriveSharedDisplayName_FullNameCollision(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Documents",
		SharedByName:  "John Doe",
		SharedByEmail: "john@example.com",
	}
	existing := map[string]bool{
		"John's Documents":     true,
		"John Doe's Documents": true,
	}
	name := deriveSharedDisplayName(item, existing)
	assert.Equal(t, "John Doe's Documents (john@example.com)", name)
}

func TestDeriveSharedDisplayName_SingleName(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Shared Stuff",
		SharedByName:  "Alice",
		SharedByEmail: "alice@example.com",
	}
	name := deriveSharedDisplayName(item, nil)
	assert.Equal(t, "Alice's Shared Stuff", name)
}

func TestDeriveSharedDisplayName_EmptyOwnerNameWithEmail(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Folder",
		SharedByName:  "",
		SharedByEmail: "unknown@example.com",
	}
	name := deriveSharedDisplayName(item, nil)
	assert.Equal(t, "Folder (shared by unknown@example.com)", name)
}

// Validates: R-3.6.3, R-3.6.4
func TestDeriveSharedDisplayName_NoIdentityFallsBackToRemoteIdentity(t *testing.T) {
	item := sharedDisplayInput{
		Name:          "Folder",
		SharedByName:  "",
		SharedByEmail: "",
		RemoteDriveID: "b!drive123",
		RemoteItemID:  "item456",
	}
	assert.Equal(t, "Folder (shared b!drive123:item456)", deriveSharedDisplayName(item, nil))
}

// --- printDriveListText shared drives ---

// Validates: R-3.6.1
func TestPrintDriveListText_SharedDrive(t *testing.T) {
	available := []driveListEntry{
		{
			CanonicalID:         "shared:user@example.com:drive1:item1",
			State:               "available",
			Source:              "available",
			OwnerEmail:          "alice@example.com",
			OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable,
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
	output := buf.String()
	assert.Contains(t, output, "shared by alice@example.com")
}

func TestPrintDriveListText_SharedAndSharePoint(t *testing.T) {
	available := []driveListEntry{
		{CanonicalID: "sharepoint:u@c.com:site:lib", State: "available", Source: "available", SiteName: "Marketing"},
		{CanonicalID: "shared:u@c.com:d:i", State: "available", Source: "available", OwnerEmail: "bob@example.com", OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
	output := buf.String()
	assert.Contains(t, output, "(Marketing)")
	assert.Contains(t, output, "(shared by bob@example.com)")
}

func TestPrintDriveListText_SharedDriveExplainsRetryableOwnerIdentityGap(t *testing.T) {
	available := []driveListEntry{
		{
			CanonicalID:         "shared:user@example.com:drive1:item1",
			DisplayName:         "Folder (shared drive1:item1)",
			State:               "available",
			Source:              "available",
			OwnerIdentityStatus: sharedOwnerIdentityStatusUnavailableRetryable,
		},
	}
	var buf bytes.Buffer
	require.NoError(t, printDriveListText(&buf, nil, available, nil, nil))
	output := buf.String()
	assert.Contains(t, output, "owner unavailable from Microsoft Graph; try again later")
}

// --- driveListEntry JSON ---

func TestDriveListEntry_SharedFieldsJSON(t *testing.T) {
	entry := driveListEntry{
		CanonicalID:         "shared:user@example.com:driveX:itemY",
		State:               "available",
		Source:              "available",
		OwnerName:           "Alice",
		OwnerEmail:          "alice@example.com",
		OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"owner_name":"Alice"`)
	assert.Contains(t, string(data), `"owner_email":"alice@example.com"`)
	assert.Contains(t, string(data), `"owner_identity_status":"available"`)
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
	assert.NotContains(t, string(data), "owner_identity_status")
}

func TestDriveListEntry_SharedRetryableOwnerIdentityJSON(t *testing.T) {
	entry := driveListEntry{
		CanonicalID:         "shared:user@example.com:driveX:itemY",
		State:               "available",
		Source:              "available",
		OwnerIdentityStatus: sharedOwnerIdentityStatusUnavailableRetryable,
	}
	data, err := json.Marshal(entry)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"owner_identity_status":"unavailable_retryable"`)
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
	originalData, err := localpath.ReadFile(cfgPath)
	require.NoError(t, err)

	require.NoError(t, addSharedDrive(t.Context(), cfgPath, io.Discard, cid, "", testDriveLogger(t), driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t))))

	updatedData, err := localpath.ReadFile(cfgPath)
	require.NoError(t, err)
	assert.Equal(t, string(originalData), string(updatedData))
	assert.Equal(t, 1, strings.Count(string(updatedData), `["shared:user@example.com:driveX:itemY"]`))
}

func TestAddSharedDrive_NoToken(t *testing.T) {
	setTestDriveHome(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	cid := driveid.MustCanonicalID("shared:nobody@example.com:driveX:itemY")

	// Empty config — no primary drive to resolve token from.
	require.NoError(t, os.WriteFile(cfgPath, []byte(""), 0o600))

	err := addSharedDrive(t.Context(), cfgPath, io.Discard, cid, "", testDriveLogger(t), driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token file")
}

// Validates: R-3.3.5
func TestAddSharedDrive_CreatesSyncDir(t *testing.T) {
	setTestDriveHome(t)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")

	ownerCID := driveid.MustCanonicalID("personal:owner@example.com")
	sharedCID := driveid.MustCanonicalID("shared:owner@example.com:driveX:itemY")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_owner@example.com.json")
	seedCatalogAccount(t, ownerCID, nil)

	err := addSharedDrive(
		t.Context(),
		cfgPath,
		io.Discard,
		sharedCID,
		"Shared Folder",
		testDriveLogger(t),
		driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t)),
	)
	require.NoError(t, err)

	home, homeErr := os.UserHomeDir()
	require.NoError(t, homeErr)
	info, statErr := os.Stat(filepath.Join(home, "OneDrive-Shared", "Shared Folder"))
	require.NoError(t, statErr)
	assert.True(t, info.IsDir())
}

func TestAddSharedDrive_ConfigWriteFailureRollsBackCatalogRegistration(t *testing.T) {
	setTestDriveHome(t)

	cfgDir := filepath.Join(t.TempDir(), "readonly-config")
	require.NoError(t, os.MkdirAll(cfgDir, 0o700))
	cfgPath := filepath.Join(cfgDir, "config.toml")

	ownerCID := driveid.MustCanonicalID("personal:owner@example.com")
	sharedCID := driveid.MustCanonicalID("shared:owner@example.com:driveX:itemY")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_owner@example.com.json")

	//nolint:gosec // Test fixture directory must be non-writable to force config write rollback.
	require.NoError(t, os.Chmod(cfgDir, 0o500))
	t.Cleanup(func() {
		//nolint:gosec // Restores the temporary fixture directory so cleanup can proceed.
		require.NoError(t, os.Chmod(cfgDir, 0o700))
	})

	err := addSharedDrive(
		t.Context(),
		cfgPath,
		io.Discard,
		sharedCID,
		"Shared Folder",
		testDriveLogger(t),
		driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t)),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing drive config")

	_, found := loadCatalogDrive(t, sharedCID)
	assert.False(t, found, "shared drive catalog entry should roll back when config write fails")

	account, found := loadCatalogAccount(t, ownerCID)
	require.True(t, found)
	assert.Equal(t, ownerCID.String(), account.CanonicalID)
}

func TestAddSharedDrive_ConfigWriteFailurePreservesPreExistingCatalogDrive(t *testing.T) {
	setTestDriveHome(t)

	cfgDir := filepath.Join(t.TempDir(), "readonly-config")
	require.NoError(t, os.MkdirAll(cfgDir, 0o700))
	cfgPath := filepath.Join(cfgDir, "config.toml")

	ownerCID := driveid.MustCanonicalID("personal:owner@example.com")
	sharedCID := driveid.MustCanonicalID("shared:owner@example.com:driveX:itemY")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_owner@example.com.json")

	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.UpsertDrive(&config.CatalogDrive{
			CanonicalID:           sharedCID.String(),
			OwnerAccountCanonical: ownerCID.String(),
			DriveType:             sharedCID.DriveType(),
			DisplayName:           "Existing Shared Folder",
			RemoteDriveID:         "shared-remote-id",
		})
		return nil
	}))

	//nolint:gosec // Test fixture directory must be non-writable to force config write rollback.
	require.NoError(t, os.Chmod(cfgDir, 0o500))
	t.Cleanup(func() {
		//nolint:gosec // Restores the temporary fixture directory so cleanup can proceed.
		require.NoError(t, os.Chmod(cfgDir, 0o700))
	})

	err := addSharedDrive(
		t.Context(),
		cfgPath,
		io.Discard,
		sharedCID,
		"New Shared Folder",
		testDriveLogger(t),
		driveops.NewSessionRuntime(nil, "test-agent", testDriveLogger(t)),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "writing drive config")

	drive, found := loadCatalogDrive(t, sharedCID)
	require.True(t, found, "pre-existing shared drive catalog entry should survive rollback")
	assert.Equal(t, "Existing Shared Folder", drive.DisplayName)
	assert.Equal(t, ownerCID.String(), drive.OwnerAccountCanonical)
	assert.Equal(t, "shared-remote-id", drive.RemoteDriveID)
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

// setTestDriveHome redirects every local root used by config path discovery.
// Setting HOME alone is not sufficient because XDG roots take precedence on all
// platforms.
func setTestDriveHome(t *testing.T) {
	t.Helper()

	tmpRoot := t.TempDir()
	paths := []string{
		filepath.Join(tmpRoot, "home"),
		filepath.Join(tmpRoot, "data"),
		filepath.Join(tmpRoot, "config"),
		filepath.Join(tmpRoot, "cache"),
	}
	for _, path := range paths {
		require.NoError(t, os.MkdirAll(path, 0o750))
	}

	t.Setenv("HOME", paths[0])
	t.Setenv("XDG_DATA_HOME", paths[1])
	t.Setenv("XDG_CONFIG_HOME", paths[2])
	t.Setenv("XDG_CACHE_HOME", paths[3])
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

	inner := strings.TrimSuffix(strings.TrimPrefix(name, "token_"), ".json")
	parts := strings.SplitN(inner, "_", 2)
	require.Len(t, parts, 2)

	cid, err := driveid.Construct(parts[0], parts[1])
	require.NoError(t, err)

	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID: cid.String(),
			Email:       cid.Email(),
			DriveType:   cid.DriveType(),
		}
		if existing, found := catalog.AccountByCanonicalID(cid); found {
			account = existing
		}
		catalog.UpsertAccount(&account)
		return nil
	}))
}

// --- enrichSharedTarget tests ---

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

func TestEnrichSharedTarget_AlreadyHasEmail(t *testing.T) {
	// No server needed — should return immediately without API call.
	client := newTestGraphClient(t, "http://should-not-be-called")

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		SharedByEmail:       "owner@example.com",
		SharedByName:        "Owner",
		OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable,
		RemoteDriveID:       "b!abc123",
		RemoteItemID:        "01DEFGH",
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	assert.Equal(t, "owner@example.com", item.SharedByEmail)
	assert.Equal(t, "Owner", item.SharedByName)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, item.OwnerIdentityStatus)
}

// Validates: R-3.6.4
func TestEnrichSharedTarget_MissingEmail_GetItemSucceeds(t *testing.T) {
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

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		OwnerIdentityStatus: sharedOwnerIdentityStatusUnavailableRetryable,
		RemoteDriveID:       "b!abc123",
		RemoteItemID:        "01DEFGH",
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	assert.Equal(t, "Bob Jones", item.SharedByName)
	assert.Equal(t, "bob@contoso.com", item.SharedByEmail)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, item.OwnerIdentityStatus)
}

func TestEnrichSharedTarget_MissingEmail_GetItemFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"not found"}}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		SharedByName:        "Partial",
		OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable,
		RemoteDriveID:       "b!abc123",
		RemoteItemID:        "01DEFGH",
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	// Original fields preserved on failure.
	assert.Equal(t, "Partial", item.SharedByName)
	assert.Empty(t, item.SharedByEmail)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, item.OwnerIdentityStatus)
}

func TestEnrichSharedTarget_NoRemoteDriveID(t *testing.T) {
	client := newTestGraphClient(t, "http://should-not-be-called")

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		OwnerIdentityStatus: sharedOwnerIdentityStatusUnavailableRetryable,
		RemoteItemID:        "01DEFGH",
		// RemoteDriveID is empty — should not make API call
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	assert.Empty(t, item.SharedByName)
	assert.Empty(t, item.SharedByEmail)
	assert.Equal(t, sharedOwnerIdentityStatusUnavailableRetryable, item.OwnerIdentityStatus)
}

// Validates: R-3.6.4
func TestEnrichSharedTarget_GetItemReturnsNameOnly(t *testing.T) {
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

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		OwnerIdentityStatus: sharedOwnerIdentityStatusUnavailableRetryable,
		RemoteDriveID:       "b!abc123",
		RemoteItemID:        "01DEFGH",
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	assert.Equal(t, "Bob Jones", item.SharedByName)
	assert.Empty(t, item.SharedByEmail) // only name, no email
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, item.OwnerIdentityStatus)
}

func TestEnrichSharedTarget_AvailableNameOnlyStillFetchesMissingEmail(t *testing.T) {
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
						"displayName": "Bob Jones",
						"email": "bob@contoso.com"
					}
				}
			}
		}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)

	item := &sharedDiscoveryTarget{
		Name:                "Shared Folder",
		SharedByName:        "Bob Jones",
		OwnerIdentityStatus: sharedOwnerIdentityStatusAvailable,
		RemoteDriveID:       "b!abc123",
		RemoteItemID:        "01DEFGH",
	}

	enrichSharedTarget(t.Context(), client, item, slog.Default())

	assert.Equal(t, "Bob Jones", item.SharedByName)
	assert.Equal(t, "bob@contoso.com", item.SharedByEmail)
	assert.Equal(t, sharedOwnerIdentityStatusAvailable, item.OwnerIdentityStatus)
}

// --- searchSharedTargets tests ---

const sharedDiscoveryRemoteItemPath = "/drives/00000b!remote123/items/remote-1"

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
func TestSearchSharedTargets_SearchSucceeds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testDriveSearchAllPath:
			writeTestResponsef(t, w, `{"value": [%s]}`, sharedItemJSON("SearchResult"))
		case sharedDiscoveryRemoteItemPath:
			writeTestResponse(t, w, `{
				"id":"remote-1",
				"name":"SearchResult",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"00000b!remote123"}
			}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items, err := searchSharedTargets(t.Context(), client, "test@example.com", slog.Default())

	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "SearchResult", items[0].Name)
	assert.Equal(t, "shared:test@example.com:00000b!remote123:remote-1", items[0].Selector)
}

// Validates: R-3.6.2
func TestSearchSharedTargets_RemoteFolderPlaceholderIsFolder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value": [{
				"id": "shortcut-placeholder",
				"name": "Shortcut Shared Folder",
				"createdDateTime": "2024-01-15T10:00:00Z",
				"lastModifiedDateTime": "2024-01-15T10:00:00Z",
				"parentReference": {"driveId": "b!abc123"},
				"remoteItem": {
					"id": "remote-1",
					"parentReference": {"driveId": "b!remote123"},
					"folder": {"childCount": 1}
				}
			}]}`)
		case sharedDiscoveryRemoteItemPath:
			writeTestResponse(t, w, `{"id":"remote-1","name":"Shortcut Shared Folder"}`)
		default:
			assert.Failf(t, "unexpected path", "path=%s", r.URL.Path)
		}
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items, err := searchSharedTargets(t.Context(), client, "test@example.com", slog.Default())

	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.True(t, items[0].IsFolder)
}

// Validates: R-3.6.2
func TestSearchSharedTargets_SearchFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		assert.Equal(t, testDriveSearchAllPath, r.URL.Path)
		w.WriteHeader(http.StatusForbidden)
		writeTestResponse(t, w, `{"error":{"code":"generalException","message":"search failed"}}`)
	}))
	defer srv.Close()

	client := newTestGraphClient(t, srv.URL)
	items, err := searchSharedTargets(t.Context(), client, "test@example.com", slog.Default())
	require.Error(t, err)
	assert.Nil(t, items)
}

// Validates: R-3.6.2
func TestSearchSharedTargets_SearchReturnsNoActionableResults_DoesNotFail(t *testing.T) {
	testCases := []struct {
		name string
		body string
	}{
		{
			name: "empty result set",
			body: `{"value": []}`,
		},
		{
			name: "non-actionable shared identity",
			body: `{"value": [{
				"id": "local-item-1",
				"name": "Not Shared",
				"folder": {}
			}]}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				assert.Equal(t, testDriveSearchAllPath, r.URL.Path)
				writeTestResponse(t, w, tc.body)
			}))
			defer srv.Close()

			client := newTestGraphClient(t, srv.URL)
			items, err := searchSharedTargets(t.Context(), client, "test@example.com", slog.Default())

			require.NoError(t, err)
			assert.Empty(t, items)
		})
	}
}

// Validates: R-3.6.5
func TestSharedDiscoveryNoMatchesError_IncludesExternalGuidance(t *testing.T) {
	err := sharedDiscoveryNoMatchesError("marketing", nil, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `no shared folders matching "marketing" found`)
	assert.Contains(t, err.Error(), "Graph shared discovery also checks external shares")
	assert.Contains(t, err.Error(), "cross-org")
	assert.Contains(t, err.Error(), "onedrive-go shared")
	assert.Contains(t, err.Error(), "onedrive-go drive list")
	assert.Contains(t, err.Error(), "drive add <share-url>")
}

// Validates: R-3.3.13
func TestDriveAdd_SharedTargetFolderAddsCanonicalSharedDrive(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var out bytes.Buffer

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":driveTestUserExample,
				"mail":"user@example.com"
			}`)
		case testSharedFolderGetItemPath:
			writeTestResponse(t, w, `{
				"id": "source-item-folder",
				"name": "Shared Folder",
				"size": 0,
				"createdDateTime": "2024-02-01T00:00:00Z",
				"lastModifiedDateTime": "2024-05-01T00:00:00Z",
				"parentReference": {"id": "parent", "driveId": "b!drive1234567890"},
				"folder": {"childCount": 1}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		GraphBaseURL: srv.URL,
		SharedTarget: &sharedTarget{
			Ref:           sharedref.MustParse("shared:user@example.com:b!drive1234567890:source-item-folder"),
			OriginalInput: "https://1drv.ms/f/c/example",
		},
	}

	err := runDriveAddWithContext(context.Background(), cc, []string{"https://1drv.ms/f/c/example"})
	require.NoError(t, err)

	cfg, err := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, err)
	_, exists := cfg.Drives[driveid.MustCanonicalID("shared:user@example.com:b!drive1234567890:source-item-folder")]
	assert.True(t, exists)
	assert.Contains(t, out.String(), "Added drive")
}

// Validates: R-3.3.12
func TestDriveAdd_SharedTargetFileRejectsDirectFileGuidance(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":driveTestUserExample,
				"mail":"user@example.com"
			}`)
		case testSharedFileGetItemPath:
			writeTestResponse(t, w, `{
				"id": "source-item-file",
				"name": "shared-file.docx",
				"size": 2048,
				"createdDateTime": "2024-02-01T00:00:00Z",
				"lastModifiedDateTime": "2024-05-01T00:00:00Z",
				"parentReference": {"id": "parent", "driveId": "b!drive1234567891"},
				"file": {"mimeType": "application/pdf"}
			}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
		SharedTarget: &sharedTarget{
			Ref:           sharedref.MustParse("shared:user@example.com:b!drive1234567891:source-item-file"),
			OriginalInput: "https://1drv.ms/t/c/example",
		},
	}

	err := runDriveAddWithContext(context.Background(), cc, []string{"https://1drv.ms/t/c/example"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shared files are direct stat/get/put targets")
}

// Validates: R-3.6.1, R-3.6.4
func TestDriveList_JSONKeepsSharedEntryWithoutOwnerIdentity(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-personal","name":"OneDrive","driveType":"personal"}]}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-personal","name":"OneDrive","driveType":"personal"}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value":[{
				"id":"local-shared-1",
				"name":"Shared Folder",
				"folder":{"childCount":1},
				"remoteItem":{
					"id":"shared-folder-1",
					"parentReference":{"driveId":"b!drive1234567890"}
				}
			}]}`)
		case "/drives/b!drive1234567890/items/shared-folder-1":
			w.WriteHeader(http.StatusNotFound)
			writeTestResponse(t, w, `{"error":{"code":"itemNotFound","message":"missing"}}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))

	var sharedEntry *driveListEntry
	for i := range decoded.Available {
		if strings.HasPrefix(decoded.Available[i].CanonicalID, "shared:") {
			sharedEntry = &decoded.Available[i]

			break
		}
	}

	require.NotNil(t, sharedEntry)
	assert.Empty(t, sharedEntry.OwnerEmail)
	assert.Empty(t, sharedEntry.OwnerName)
	assert.Equal(t, "Shared Folder (shared b!drive1234567890:shared-folder-1)", sharedEntry.DisplayName)
	assert.Empty(t, decoded.AccountsDegraded)
}

// Validates: R-3.6.5, R-3.6.7
func TestDriveList_JSONIncludesSharedDiscoveryDegradedAccount(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/me/drives":
			writeTestResponse(t, w, `{"value":[{"id":"drive-personal","name":"OneDrive","driveType":"personal"}]}`)
		case "/me/drive":
			writeTestResponse(t, w, `{"id":"drive-personal","name":"OneDrive","driveType":"personal"}`)
		case testDriveSearchAllPath:
			w.WriteHeader(http.StatusForbidden)
			writeTestResponse(t, w, `{"error":{"code":"accessDenied","message":"search unavailable"}}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.AccountsDegraded, 1)
	assert.Equal(t, "user@example.com", decoded.AccountsDegraded[0].Email)
	assert.Equal(t, sharedDiscoveryUnavailableReason, decoded.AccountsDegraded[0].Reason)
	assert.Empty(t, decoded.AccountsRequiringAuth)
}

// Validates: R-3.6.5, R-3.3.10
func TestDriveList_JSONIncludesSharedDiscoveryAuthRequiredAccount(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-personal","name":"OneDrive","driveType":"personal"}]}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-personal","name":"OneDrive","driveType":"personal"}`)
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":driveTestUserExample,
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			w.WriteHeader(http.StatusUnauthorized)
			writeTestResponse(t, w, `{"error":{"code":"unauthenticated","message":"token rejected"}}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	var out bytes.Buffer
	cc := &CLIContext{
		Flags:        CLIFlags{JSON: true},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, "user@example.com", decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, authReasonSyncAuthRejected, decoded.AccountsRequiringAuth[0].Reason)
	assert.Empty(t, decoded.AccountsDegraded)
}

// Validates: R-3.3.6
func TestDriveAdd_SharedNameHonorsAccountFilter(t *testing.T) {
	setTestDriveHome(t)
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_other@example.com.json")

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	var out bytes.Buffer

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case testGraphMePath:
			writeTestResponse(t, w, `{
				"id":"user-123",
				"displayName":driveTestUserExample,
				"mail":"user@example.com"
			}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value":[{
				"id":"local-1",
				"name":"Project Folder",
				"folder":{"childCount":1},
				"remoteItem":{"id":"source-item-user","parentReference":{"driveId":"b!drive-user"}}
			}]}`)
		case "/drives/0000b!drive-user/items/source-item-user":
			writeTestResponse(t, w, `{
				"id":"source-item-user",
				"name":"Project Folder",
				"folder":{"childCount":1},
				"parentReference":{"driveId":"0000b!drive-user"},
				"shared":{"owner":{"user":{"email":"alice@example.com","displayName":"Alice Smith"}}}
			}`)
		default:
			assert.Fail(t, "unexpected graph path", "path=%s", r.URL.Path)
			http.Error(w, "unexpected graph path", http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	cc := &CLIContext{
		Flags:        CLIFlags{Account: "user@example.com"},
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runDriveAddWithContext(context.Background(), cc, []string{"Project Folder"}))

	cfg, err := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, err)
	_, exists := cfg.Drives[driveid.MustCanonicalID("shared:user@example.com:0000b!drive-user:source-item-user")]
	assert.True(t, exists)
	_, otherExists := cfg.Drives[driveid.MustCanonicalID("shared:other@example.com:0000b!drive-user:source-item-user")]
	assert.False(t, otherExists)
}

// Validates: R-3.3.6, R-3.6.5
func TestDriveAdd_SharedNameNoMatchesIncludesBlockedAccounts(t *testing.T) {
	setTestDriveHome(t)

	profileCID := driveid.MustCanonicalID("personal:user@example.com")
	seedCatalogAccount(t, profileCID, func(account *config.CatalogAccount) {
		account.DisplayName = driveTestUserExample
	})

	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &bytes.Buffer{},
		StatusWriter: &bytes.Buffer{},
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
	}

	err := runDriveAddWithContext(context.Background(), cc, []string{"marketing"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Authentication required:")
	assert.Contains(t, err.Error(), "User Example (user@example.com)")
	assert.Contains(t, err.Error(), authReasonText(authReasonMissingLogin))
	assert.Contains(t, err.Error(), `no shared folders matching "marketing" found`)
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
	statePath := config.DriveStatePath(cid)
	var out bytes.Buffer

	// No state DB on disk — should succeed with "no orphaned state" message.
	require.NoError(t, purgeOrphanedDriveState(&out, cid, testDriveLogger(t)))
	assert.Equal(t, "No orphaned state found for personal:user@example.com.\n", out.String())

	_, stateErr := os.Stat(statePath)
	assert.True(t, os.IsNotExist(stateErr), "state DB should remain absent")
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
	assert.Equal(t, string(authReasonMissingLogin), configured[0].AuthReason)
	assert.Equal(t, "required", driveAuthLabel(&configured[0]))
	assert.Equal(t, authStateReady, driveAuthLabel(nil))
	assert.Nil(t, optionalAuthRequirements(nil))

	var buf bytes.Buffer
	require.NoError(t, printDriveListSections(&buf, configured, available, []accountAuthRequirement{{
		Email:  "blocked@example.com",
		Reason: authReasonMissingLogin,
		Action: authAction(authReasonMissingLogin),
	}}, nil))
	assert.Contains(t, buf.String(), "Configured drives:")
	assert.Contains(t, buf.String(), "Available drives (not configured):")
	assert.Contains(t, buf.String(), "Authentication required:")
}

// Validates: R-2.10.47
func TestDriveList_ClearsPersistedAuthScopeAfterSuccessfulDiscovery(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:user@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_user@example.com.json")
	seedAuthScope(t, cid)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case graphDrivesPath:
			writeTestResponse(t, w, `{"value":[{"id":"drive-123","name":"OneDrive","driveType":"personal"}]}`)
		case primaryDrivePath:
			writeTestResponse(t, w, `{"id":"drive-123","name":"OneDrive","driveType":"personal"}`)
		case testDriveSearchAllPath:
			writeTestResponse(t, w, `{"value":[]}`)
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
		CfgPath:      filepath.Join(t.TempDir(), "config.toml"),
		GraphBaseURL: srv.URL,
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))
	assert.False(t, hasPersistedAccountAuthRequirement(t.Context(), cid.Email(), testDriveLogger(t)))
}
