//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
		Email          string `json:"email"`
		SignInRequired *struct {
			Reason string `json:"reason"`
			Action string `json:"action"`
		} `json:"sign_in_required,omitempty"`
		Drives []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"drives"`
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

	requireReadyAccountBeforeLogout(t, cfgPath, env, email)

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "logout")
	require.NoErrorf(t, err, "logout should succeed\nstdout: %s\nstderr: %s", printable(stdout), printable(stderr))
	logoutOutput := stdout + stderr
	assertOutputContains(t, logoutOutput, "Token removed")
	assertOutputContains(t, logoutOutput, "State databases kept")
	assertOutputContains(t, logoutOutput, "Sync directories untouched")

	_, tokenErr := os.Stat(tokenPath)
	assert.True(t, os.IsNotExist(tokenErr), "logout should remove the token file")

	_, catalogErr := os.Stat(catalogPath)
	require.NoError(t, catalogErr, "logout should preserve the managed catalog")

	cfgBytes := mustReadFile(t, cfgPath)
	assertOutputNotContains(t, string(cfgBytes), drive, "logout should remove the drive config section")

	stdout, stderr, err = runCLICore(t, cfgPath, env, "", "status", "--json")
	require.NoErrorf(t, err, "offline status should still succeed after logout\nstdout: %s\nstderr: %s", printable(stdout), printable(stderr))

	var statusAfterLogout w7LiveStatusOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &statusAfterLogout))
	var loggedOutAccountFound bool
	for i := range statusAfterLogout.Accounts {
		account := statusAfterLogout.Accounts[i]
		if account.Email != email {
			continue
		}

		loggedOutAccountFound = true
		require.NotNil(t, account.SignInRequired)
		assertOutputContains(t, account.SignInRequired.Action, "login")
		driveCount := len(account.Drives)
		assert.Zerof(t, driveCount, "the logged-out account lists no drives")
	}
	assert.True(t, loggedOutAccountFound, "status should retain the logged-out account in the offline catalog view")

	assertDriveListRequiresLoginAfterLogout(t, cfgPath, env, email)
}

// requireReadyAccountBeforeLogout checks that status --json lists email as a
// ready account with its configured drive.
func requireReadyAccountBeforeLogout(t *testing.T, cfgPath string, env map[string]string, email string) {
	t.Helper()

	stdout, _ := pollCLIWithConfigRetryingTransientGraphFailures(
		t, cfgPath, env, "", "status", "--json",
	)

	var beforeLogout w7LiveStatusOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &beforeLogout))
	var configuredBeforeLogout *struct {
		Email          string `json:"email"`
		SignInRequired *struct {
			Reason string `json:"reason"`
			Action string `json:"action"`
		} `json:"sign_in_required,omitempty"`
		Drives []struct {
			Name  string `json:"name"`
			State string `json:"state"`
		} `json:"drives"`
	}
	for i := range beforeLogout.Accounts {
		account := &beforeLogout.Accounts[i]
		if account.Email == email && account.SignInRequired == nil {
			configuredBeforeLogout = account
			break
		}
	}
	require.NotNil(t, configuredBeforeLogout, "pre-logout status should still include the configured ready account")
	assert.NotEmpty(t, configuredBeforeLogout.Drives, "pre-logout status should list the configured drive")
}

// assertDriveListRequiresLoginAfterLogout checks that drive list --json keeps
// the logged-out account, with no configured drives, under
// accounts_requiring_auth.
func assertDriveListRequiresLoginAfterLogout(t *testing.T, cfgPath string, env map[string]string, email string) {
	t.Helper()

	stdout, stderr, err := runCLICore(t, cfgPath, env, "", "drive", "list", "--json")
	require.NoErrorf(t, err, "drive list should still succeed after logout\nstdout: %s\nstderr: %s", printable(stdout), printable(stderr))

	var driveListAfterLogout w7LiveDriveListOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &driveListAfterLogout))
	// A count, not the list: its entries name the drives.
	configuredAfterLogout := len(driveListAfterLogout.Configured)
	assert.Zero(t, configuredAfterLogout, "logout leaves no configured drive")
	require.NotEmpty(t, driveListAfterLogout.AccountsRequiringAuth)
	var foundDriveListAccount bool
	for i := range driveListAfterLogout.AccountsRequiringAuth {
		if driveListAfterLogout.AccountsRequiringAuth[i].Email != email {
			continue
		}

		foundDriveListAccount = true
		assertOutputEqual(t, "missing_login", driveListAfterLogout.AccountsRequiringAuth[i].Reason)
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
		"--account",
		fixture.RecipientEmail,
		"shared",
		"--json",
	)

	var listing w7LiveSharedOutput
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing))
	// Counts, not the lists: their entries name the accounts.
	requiringAuthCount := len(listing.AccountsRequiringAuth)
	assert.Zerof(t, requiringAuthCount, "no account needs a sign-in")
	if len(listing.Items) == 0 {
		degradedCount := len(listing.AccountsDegraded)
		assert.Zerof(t, degradedCount, "empty shared discovery should be an honest best-effort result, not degraded")
		return
	}

	for i := range listing.Items {
		assert.NotEmpty(t, listing.Items[i].Selector)
		assert.NotEmpty(t, listing.Items[i].RemoteDriveID)
		assert.NotEmpty(t, listing.Items[i].RemoteItemID)
		assertSameIdentity(t, "a shared item's account email", fixture.RecipientEmail, listing.Items[i].AccountEmail)
		itemType := listing.Items[i].Type
		assert.Truef(t, slices.Contains([]string{"file", "folder"}, itemType),
			"a shared item is a file or a folder, not %s", printable(itemType))
	}
}
