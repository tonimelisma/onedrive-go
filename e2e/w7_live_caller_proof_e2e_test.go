//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/testutil"
)

type w7LiveAuthRequirement struct {
	Email     string `json:"email"`
	DriveType string `json:"drive_type"`
	Reason    string `json:"reason"`
	StateDBs  int    `json:"state_dbs"`
}

type w7LiveWhoamiOutput struct {
	User *struct {
		Email string `json:"email"`
	} `json:"user"`
	Drives []struct {
		DriveType string `json:"drive_type"`
	} `json:"drives"`
	AccountsRequiringAuth []w7LiveAuthRequirement `json:"accounts_requiring_auth"`
}

type w7LiveDriveListOutput struct {
	Configured            []struct{}              `json:"configured"`
	Available             []struct{}              `json:"available"`
	AccountsRequiringAuth []w7LiveAuthRequirement `json:"accounts_requiring_auth"`
}

type w7LiveSharedOutput struct {
	Items []struct {
		Selector      string `json:"selector"`
		Type          string `json:"type"`
		AccountEmail  string `json:"account_email"`
		RemoteDriveID string `json:"remote_drive_id"`
		RemoteItemID  string `json:"remote_item_id"`
	} `json:"items"`
	AccountsRequiringAuth []w7LiveAuthRequirement `json:"accounts_requiring_auth"`
}

func accountProfilePathForDriveID(dataHome, driveID string) string {
	parts := strings.SplitN(driveID, ":", 2)
	if len(parts) != 2 {
		return ""
	}

	return filepath.Join(dataHome, "onedrive-go", "account_"+parts[0]+"_"+parts[1]+".json")
}

func driveMetadataPathForDriveID(dataHome, driveID string) string {
	return filepath.Join(
		dataHome,
		"onedrive-go",
		"drive_"+strings.ReplaceAll(driveID, ":", "_")+".json",
	)
}

// Validates: R-3.1.3, R-3.1.5, R-3.1.6, R-3.3.2, R-3.3.10
func TestE2E_Logout_PreservesOfflineAccountCatalog(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeSyncConfig(t, syncDir)
	email := strings.SplitN(drive, ":", 2)[1]
	dataHome := env["XDG_DATA_HOME"]
	tokenPath := filepath.Join(dataHome, "onedrive-go", testutil.TokenFileName(drive))
	accountProfilePath := accountProfilePathForDriveID(dataHome, drive)
	driveMetadataPath := driveMetadataPathForDriveID(dataHome, drive)

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", transientGraphRetryTimeout, "whoami", "--json",
	)

	var beforeLogout w7LiveWhoamiOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &beforeLogout))
	require.NotNil(t, beforeLogout.User, "pre-logout whoami should authenticate against Graph")
	assert.Equal(t, email, beforeLogout.User.Email)
	assert.NotEmpty(t, beforeLogout.Drives, "pre-logout whoami should list the live drive catalog")

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "logout")
	require.NoErrorf(t, err, "logout should succeed\nstdout: %s\nstderr: %s", stdout, stderr)
	combinedLogout := stdout + stderr
	assert.Contains(t, combinedLogout, "Token removed")
	assert.Contains(t, combinedLogout, "State databases kept")
	assert.Contains(t, combinedLogout, "Sync directories untouched")

	_, tokenErr := os.Stat(tokenPath)
	assert.True(t, os.IsNotExist(tokenErr), "logout should remove the token file")

	_, profileErr := os.Stat(accountProfilePath)
	require.NoError(t, profileErr, "logout should preserve the account profile")

	_, metadataErr := os.Stat(driveMetadataPath)
	require.NoError(t, metadataErr, "logout should preserve drive metadata")

	cfgBytes, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(cfgBytes), drive, "logout should remove the drive config section")

	stdout, stderr, err = runCLICore(t, cfgPath, env, "", "whoami", "--json")
	require.NoErrorf(t, err, "offline whoami should still succeed after logout\nstdout: %s\nstderr: %s", stdout, stderr)

	var whoamiAfterLogout w7LiveWhoamiOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &whoamiAfterLogout))
	assert.Nil(t, whoamiAfterLogout.User)
	assert.Empty(t, whoamiAfterLogout.Drives)
	require.NotEmpty(t, whoamiAfterLogout.AccountsRequiringAuth)
	var foundWhoamiAccount bool
	for i := range whoamiAfterLogout.AccountsRequiringAuth {
		if whoamiAfterLogout.AccountsRequiringAuth[i].Email != email {
			continue
		}

		foundWhoamiAccount = true
		assert.Equal(t, strings.SplitN(drive, ":", 2)[0], whoamiAfterLogout.AccountsRequiringAuth[i].DriveType)
		assert.Equal(t, "missing_login", whoamiAfterLogout.AccountsRequiringAuth[i].Reason)
	}
	assert.True(t, foundWhoamiAccount, "offline whoami should retain the logged-out account in accounts_requiring_auth")

	stdout, stderr, err = runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoErrorf(t, err, "drive list should still succeed after logout\nstdout: %s\nstderr: %s", stdout, stderr)

	var driveListAfterLogout w7LiveDriveListOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &driveListAfterLogout))
	assert.Empty(t, driveListAfterLogout.Configured)
	require.NotEmpty(t, driveListAfterLogout.AccountsRequiringAuth)
	var foundDriveListAccount bool
	for i := range driveListAfterLogout.AccountsRequiringAuth {
		if driveListAfterLogout.AccountsRequiringAuth[i].Email != email {
			continue
		}

		foundDriveListAccount = true
		assert.Equal(t, "missing_login", driveListAfterLogout.AccountsRequiringAuth[i].Reason)
	}
	assert.True(t, foundDriveListAccount, "drive list should retain the logged-out account in accounts_requiring_auth")
}

// Validates: R-3.6.6, R-3.6.7
func TestE2E_Shared_JSON_RecipientListingUsesLiveAccountCatalog(t *testing.T) {
	t.Parallel()
	requireDrive2Shared(t)
	registerLogDump(t)

	recipientEmail := recipientEmailFromDriveID(t, drive2)
	cfgPath, env := writeSyncConfigForDrive2(t, t.TempDir())

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", transientGraphRetryTimeout, "--account", recipientEmail, "shared", "--json",
	)

	var listing w7LiveSharedOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing))
	assert.Empty(t, listing.AccountsRequiringAuth)
	require.NotEmpty(t, listing.Items, "drive2 shared test setup should expose at least one shared item")

	for i := range listing.Items {
		assert.NotEmpty(t, listing.Items[i].Selector)
		assert.NotEmpty(t, listing.Items[i].RemoteDriveID)
		assert.NotEmpty(t, listing.Items[i].RemoteItemID)
		assert.Equal(t, recipientEmail, listing.Items[i].AccountEmail)
		assert.Contains(t, []string{"file", "folder"}, listing.Items[i].Type)
	}
}
