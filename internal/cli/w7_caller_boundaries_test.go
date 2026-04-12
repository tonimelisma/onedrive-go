package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func setupConfiguredInvalidSavedLogin(t *testing.T) (string, driveid.CanonicalID) {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("business:blocked@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive - Contoso"))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Blocked User",
	}))
	writeInvalidSavedLoginFile(t, cid)
	require.NoError(t, touchStateDBForAccount(t, cid))

	return cfgPath, cid
}

func setupUnconfiguredInvalidSavedLogin(t *testing.T) driveid.CanonicalID {
	t.Helper()

	return setupUnconfiguredInvalidSavedLoginCID(
		t,
		driveid.MustCanonicalID("business:blocked@example.com"),
		"Blocked User",
	)
}

func setupUnconfiguredInvalidSavedLoginCID(
	t *testing.T,
	cid driveid.CanonicalID,
	displayName string,
) driveid.CanonicalID {
	t.Helper()

	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: displayName,
	}))
	writeInvalidSavedLoginFile(t, cid)
	require.NoError(t, touchStateDBForAccount(t, cid))

	return cid
}

func writeInvalidSavedLoginFile(t *testing.T, cid driveid.CanonicalID) {
	t.Helper()

	tokenPath := config.DriveTokenPath(cid)
	require.NotEmpty(t, tokenPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(tokenPath), 0o700))
	require.NoError(t, os.WriteFile(tokenPath, []byte("{invalid"), 0o600))
}

// Validates: R-3.1.3, R-2.10.47
func TestLogoutCommand_PreservesOfflineState(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("business:alice@contoso.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive - Contoso"))
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Alice Smith",
		OrgName:     "Contoso",
	}))
	require.NoError(t, config.SaveDriveMetadata(cid, &config.DriveMetadata{
		DriveID:  "drive-123",
		CachedAt: time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_alice@contoso.com.json")
	seedAuthScope(t, cid)

	syncDir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))
	require.NoError(t, config.SetDriveKey(cfgPath, cid, "sync_dir", syncDir))

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)

	require.NoError(t, runLogoutWithContext(cc, false))

	cfg, err := config.LoadOrDefault(cfgPath, testDriveLogger(t))
	require.NoError(t, err)
	assert.Empty(t, cfg.Drives, "plain logout should remove the drive config section")

	_, tokenErr := os.Stat(config.DriveTokenPath(cid))
	assert.True(t, os.IsNotExist(tokenErr), "plain logout should remove the account token")

	_, stateErr := os.Stat(config.DriveStatePath(cid))
	require.NoError(t, stateErr, "plain logout must preserve the state DB")

	_, metaErr := os.Stat(config.DriveMetadataPath(cid))
	require.NoError(t, metaErr, "plain logout must preserve drive metadata")

	profile, found, profileErr := config.LookupAccountProfile(cid)
	require.NoError(t, profileErr)
	require.True(t, found, "plain logout must preserve the account profile")
	assert.Equal(t, "Alice Smith", profile.DisplayName)

	_, syncDirErr := os.Stat(syncDir)
	require.NoError(t, syncDirErr, "plain logout must leave sync directories untouched")
	assert.False(t, hasPersistedAuthScope(t.Context(), cid.Email(), testDriveLogger(t)))

	assert.Contains(t, out.String(), "Token removed for alice@contoso.com.")
	assert.Contains(t, out.String(), "State databases kept.")
	assert.Contains(t, out.String(), "Sync directories untouched")
}

// Validates: R-3.1.5, R-3.1.6
func TestRunWhoamiWithContext_AuthRequiredOnlyJSON(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:orphan@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Orphan User",
	}))
	require.NoError(t, touchStateDBForAccount(t, cid))

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      filepath.Join(t.TempDir(), "missing-config.toml"),
		Flags: CLIFlags{
			JSON: true,
		},
	}

	require.NoError(t, runWhoamiWithContext(t.Context(), cc))

	var decoded whoamiOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Nil(t, decoded.User)
	assert.Empty(t, decoded.Drives)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, "orphan@example.com", decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, authReasonMissingLogin, decoded.AccountsRequiringAuth[0].Reason)
	assert.Equal(t, 1, decoded.AccountsRequiringAuth[0].StateDBs)
}

// Validates: R-3.5.1, R-3.1.5
func TestRunWhoamiWithContext_InvalidDriveSelectorReturnsMatchError(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	cid := driveid.MustCanonicalID("personal:ready@example.com")
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, "~/OneDrive"))
	writeTestTokenFile(t, config.DefaultDataDir(), "token_personal_ready@example.com.json")

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		Flags: CLIFlags{
			Drive: []string{"missing"},
		},
	}

	err := runWhoamiWithContext(t.Context(), cc)
	require.Error(t, err)
	assert.EqualError(t, err, `no drive matching "missing"`)
}

// Validates: R-3.5.1
func TestRunWhoamiWithContext_MultipleConfiguredDrivesRequireSelector(t *testing.T) {
	setTestDriveHome(t)

	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(
		cfgPath,
		driveid.MustCanonicalID("personal:alice@example.com"),
		"~/OneDrive Alice",
	))
	require.NoError(t, config.AppendDriveSection(
		cfgPath,
		driveid.MustCanonicalID("business:bob@contoso.com"),
		"~/OneDrive Bob",
	))

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}

	err := runWhoamiWithContext(t.Context(), cc)
	require.Error(t, err)
	assert.EqualError(t, err, "multiple drives configured — specify with --drive")
}

// Validates: R-3.3.2
func TestDriveList_TextSurfacesConfiguredAuthRequirement(t *testing.T) {
	setTestDriveHome(t)

	cfgPath, _ := setupConfiguredInvalidSavedLogin(t)

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))
	assert.Contains(t, out.String(), "Configured drives:")
	assert.Contains(t, out.String(), "required")
	assert.Contains(t, out.String(), "Authentication required:")
	assert.Contains(t, out.String(), "blocked@example.com")
	assert.Contains(t, out.String(), "invalid or unreadable")
}

// Validates: R-3.3.10
func TestDriveList_JSONSurfacesConfiguredAuthRequirement(t *testing.T) {
	setTestDriveHome(t)

	cfgPath, cid := setupConfiguredInvalidSavedLogin(t)

	var out bytes.Buffer
	cc := &CLIContext{
		Logger:       testDriveLogger(t),
		OutputWriter: &out,
		StatusWriter: &out,
		CfgPath:      cfgPath,
		Flags: CLIFlags{
			JSON: true,
		},
	}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	require.Len(t, decoded.Configured, 1)
	assert.Equal(t, cid.String(), decoded.Configured[0].CanonicalID)
	assert.Equal(t, authStateAuthenticationNeeded, decoded.Configured[0].AuthState)
	assert.Empty(t, decoded.Available)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, "blocked@example.com", decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, authReasonInvalidSavedLogin, decoded.AccountsRequiringAuth[0].Reason)
	assert.Equal(t, 1, decoded.AccountsRequiringAuth[0].StateDBs)
}

// Validates: R-3.3.2, R-3.3.10
func TestDriveList_JSONSurfacesOrphanedMissingLogin(t *testing.T) {
	setTestDriveHome(t)

	cid := driveid.MustCanonicalID("personal:orphan@example.com")
	require.NoError(t, config.SaveAccountProfile(cid, &config.AccountProfile{
		DisplayName: "Orphan User",
	}))
	require.NoError(t, touchStateDBForAccount(t, cid))

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))
	cc.Flags = CLIFlags{JSON: true}

	require.NoError(t, runDriveListWithContext(t.Context(), cc, false))

	var decoded driveListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Empty(t, decoded.Configured)
	assert.Empty(t, decoded.Available)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, cid.Email(), decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, driveid.DriveTypePersonal, decoded.AccountsRequiringAuth[0].DriveType)
	assert.Equal(t, authReasonMissingLogin, decoded.AccountsRequiringAuth[0].Reason)
	assert.Equal(t, 1, decoded.AccountsRequiringAuth[0].StateDBs)
}

// Validates: R-3.3.9
func TestDriveSearch_TextSurfacesUnconfiguredAuthRequirement(t *testing.T) {
	setTestDriveHome(t)

	cid := setupUnconfiguredInvalidSavedLogin(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))

	require.NoError(t, runDriveSearchWithContext(t.Context(), cc, "marketing"))
	assert.Contains(t, out.String(), "Authentication required:")
	assert.Contains(t, out.String(), cid.Email())
	assert.Contains(t, out.String(), "invalid or unreadable")
	assert.Contains(t, out.String(), `No SharePoint sites found matching "marketing" in searchable business accounts.`)
	assert.NotContains(t, out.String(), "no business accounts found")
}

// Validates: R-3.3.9, R-3.3.11
func TestDriveSearch_JSONSurfacesUnconfiguredAuthRequirementForAccountFilter(t *testing.T) {
	setTestDriveHome(t)

	cid := setupUnconfiguredInvalidSavedLogin(t)

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))
	cc.Flags = CLIFlags{
		Account: cid.Email(),
		JSON:    true,
	}

	require.NoError(t, runDriveSearchWithContext(t.Context(), cc, "marketing"))

	var decoded driveSearchJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Empty(t, decoded.Results)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, cid.Email(), decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, driveid.DriveTypeBusiness, decoded.AccountsRequiringAuth[0].DriveType)
	assert.Equal(t, authReasonInvalidSavedLogin, decoded.AccountsRequiringAuth[0].Reason)
	assert.Equal(t, 1, decoded.AccountsRequiringAuth[0].StateDBs)
}

// Validates: R-3.6.6
func TestSharedList_TextSurfacesUnconfiguredAuthRequirement(t *testing.T) {
	setTestDriveHome(t)

	cid := setupUnconfiguredInvalidSavedLoginCID(
		t,
		driveid.MustCanonicalID("personal:blocked@example.com"),
		"Blocked Personal User",
	)

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))

	require.NoError(t, runSharedList(t.Context(), cc))
	assert.Contains(t, out.String(), "Authentication required:")
	assert.Contains(t, out.String(), cid.Email())
	assert.Contains(t, out.String(), "invalid or unreadable")
	assert.NotContains(t, out.String(), "No shared items found.")
}

// Validates: R-3.6.6, R-3.6.7
func TestSharedList_JSONSurfacesUnconfiguredAuthRequirementForAccountFilter(t *testing.T) {
	setTestDriveHome(t)

	cid := setupUnconfiguredInvalidSavedLoginCID(
		t,
		driveid.MustCanonicalID("personal:blocked@example.com"),
		"Blocked Personal User",
	)

	var out bytes.Buffer
	cc := newCommandContext(&out, filepath.Join(t.TempDir(), "missing-config.toml"))
	cc.Flags = CLIFlags{
		Account: cid.Email(),
		JSON:    true,
	}

	require.NoError(t, runSharedList(t.Context(), cc))

	var decoded sharedListJSONOutput
	require.NoError(t, json.Unmarshal(out.Bytes(), &decoded))
	assert.Empty(t, decoded.Items)
	require.Len(t, decoded.AccountsRequiringAuth, 1)
	assert.Equal(t, cid.Email(), decoded.AccountsRequiringAuth[0].Email)
	assert.Equal(t, driveid.DriveTypePersonal, decoded.AccountsRequiringAuth[0].DriveType)
	assert.Equal(t, authReasonInvalidSavedLogin, decoded.AccountsRequiringAuth[0].Reason)
	assert.Equal(t, 1, decoded.AccountsRequiringAuth[0].StateDBs)
}
