//go:build e2e && e2e_full

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

type w7LiveStatusOutput struct {
	Accounts []struct {
		Email      string `json:"email"`
		DriveType  string `json:"drive_type"`
		AuthState  string `json:"auth_state"`
		LiveDrives []struct {
			DriveType string `json:"drive_type"`
		} `json:"live_drives"`
	} `json:"accounts"`
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
	AccountsDegraded      []struct {
		Email     string `json:"email"`
		DriveType string `json:"drive_type"`
		Reason    string `json:"reason"`
	} `json:"accounts_degraded"`
}

func catalogPathForDataHome(dataHome string) string {
	return filepath.Join(dataHome, "onedrive-go", "catalog.json")
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
	catalogPath := catalogPathForDataHome(dataHome)

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", transientGraphRetryTimeout, "status", "--json",
	)

	var beforeLogout w7LiveStatusOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &beforeLogout))
	var configuredBeforeLogout *struct {
		Email      string `json:"email"`
		DriveType  string `json:"drive_type"`
		AuthState  string `json:"auth_state"`
		LiveDrives []struct {
			DriveType string `json:"drive_type"`
		} `json:"live_drives"`
	}
	for i := range beforeLogout.Accounts {
		account := &beforeLogout.Accounts[i]
		if account.Email == email && account.AuthState == "ready" {
			configuredBeforeLogout = account
			break
		}
	}
	require.NotNil(t, configuredBeforeLogout, "pre-logout status should still include the configured ready account")
	assert.NotEmpty(t, configuredBeforeLogout.LiveDrives, "pre-logout status should list the live drive catalog")

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "logout")
	require.NoErrorf(t, err, "logout should succeed\nstdout: %s\nstderr: %s", stdout, stderr)
	combinedLogout := stdout + stderr
	assert.Contains(t, combinedLogout, "Token removed")
	assert.Contains(t, combinedLogout, "State databases kept")
	assert.Contains(t, combinedLogout, "Sync directories untouched")

	_, tokenErr := os.Stat(tokenPath)
	assert.True(t, os.IsNotExist(tokenErr), "logout should remove the token file")

	_, catalogErr := os.Stat(catalogPath)
	require.NoError(t, catalogErr, "logout should preserve the managed catalog")

	cfgBytes, readErr := os.ReadFile(cfgPath)
	require.NoError(t, readErr)
	assert.NotContains(t, string(cfgBytes), drive, "logout should remove the drive config section")

	stdout, stderr, err = runCLICore(t, cfgPath, env, "", "status", "--json")
	require.NoErrorf(t, err, "offline status should still succeed after logout\nstdout: %s\nstderr: %s", stdout, stderr)

	var statusAfterLogout w7LiveStatusOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &statusAfterLogout))
	var loggedOutAccountFound bool
	for i := range statusAfterLogout.Accounts {
		account := statusAfterLogout.Accounts[i]
		if account.Email != email {
			continue
		}

		loggedOutAccountFound = true
		assert.Equal(t, strings.SplitN(drive, ":", 2)[0], account.DriveType)
		assert.Equal(t, "authentication_required", account.AuthState)
		assert.Empty(t, account.LiveDrives)
	}
	assert.True(t, loggedOutAccountFound, "status should retain the logged-out account in the offline catalog view")

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
	rawLink := requireSharedFileLink(t)
	registerLogDump(t)

	fixture := resolveSharedFileFixture(t, rawLink)
	cfgPath, env := writeSyncConfigForDriveID(t, fixture.RecipientDriveID, t.TempDir())

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t,
		cfgPath,
		env,
		"",
		transientGraphRetryTimeout,
		"--account",
		fixture.RecipientEmail,
		"shared",
		"--json",
	)

	var listing w7LiveSharedOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing))
	assert.Empty(t, listing.AccountsRequiringAuth)
	if len(listing.Items) == 0 {
		assert.Empty(t, listing.AccountsDegraded, "empty shared discovery should be an honest best-effort result, not degraded")
		return
	}

	for i := range listing.Items {
		assert.NotEmpty(t, listing.Items[i].Selector)
		assert.NotEmpty(t, listing.Items[i].RemoteDriveID)
		assert.NotEmpty(t, listing.Items[i].RemoteItemID)
		assert.Equal(t, fixture.RecipientEmail, listing.Items[i].AccountEmail)
		assert.Contains(t, []string{"file", "folder"}, listing.Items[i].Type)
	}
}
