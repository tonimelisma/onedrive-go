package cli

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// An email change would rename a drive's state DB; while a sync still has it
// open, nothing is renamed and the command says to stop that sync and run
// again. Renaming it under the engine split the store from its log.
//
// Validates: R-3.7.2, R-6.3.9
func TestReconcileGraphUser_RefusesWhileASyncUsesTheState(t *testing.T) {
	setTestDriveHome(t)

	const userID = "user-123"
	oldCID := driveid.MustCanonicalID("business:user@example.com")
	newCID := driveid.MustCanonicalID("business:renamed@example.com")
	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_user@example.com.json")
	seedCatalogAccount(t, oldCID, func(account *config.CatalogAccount) {
		account.UserID = userID
	})
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, oldCID, t.TempDir()))
	statePath := config.DriveStatePath(oldCID)
	require.NoError(t, os.WriteFile(statePath, []byte(purgeTestStateContent), 0o600))
	holdDriveState(t, statePath)

	var out bytes.Buffer
	cc := newCommandContext(&out, cfgPath)
	_, err := cc.reconcileGraphUser(t.Context(), newCID, &graph.User{ID: userID, Email: newCID.Email()})

	require.ErrorIs(t, err, syncengine.ErrSyncStoreInUse)
	require.ErrorContains(t, err, "stop that sync, then run the command again")
	assert.FileExists(t, statePath, "the state DB keeps its name")
	assert.FileExists(t, config.DriveTokenPath(oldCID), "so does the token")
	assert.NoFileExists(t, emailChangeJournalPathForTest(), "a refused change is never put on record")
	assert.NotContains(t, out.String(), "Account email changed")
}

const emailChangeTestUserID = "user-123"

// emailChangeJournalPathForTest is where internal/config keeps the journal of
// an unfinished account email change.
func emailChangeJournalPathForTest() string {
	return filepath.Join(config.DefaultDataDir(), "email-change", "journal.json")
}

// seedInterruptedEmailChange leaves the change of the business account
// user@example.com to renamed@example.com as a kill right after it was
// recorded leaves it: the journal, and every name old. It returns the config
// path.
func seedInterruptedEmailChange(t *testing.T, oldCID, newCID driveid.CanonicalID) string {
	t.Helper()

	writeTestTokenFile(t, config.DefaultDataDir(), "token_business_user@example.com.json")
	seedCatalogAccount(t, oldCID, func(account *config.CatalogAccount) {
		account.UserID = emailChangeTestUserID
	})
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	require.NoError(t, config.AppendDriveSection(cfgPath, oldCID, t.TempDir()))
	require.NoError(t, os.WriteFile(config.DriveStatePath(oldCID), []byte(purgeTestStateContent), 0o600))

	killed := errors.New("killed")
	_, err := config.ReconcileAccountEmail(cfgPath, newCID, emailChangeTestUserID, newCID.Email(),
		func(_ []config.StateDBMove, begin func() error) error {
			require.NoError(t, begin())
			return killed
		}, slog.New(slog.DiscardHandler))
	require.ErrorIs(t, err, killed)
	require.FileExists(t, emailChangeJournalPathForTest())
	require.FileExists(t, config.DriveStatePath(oldCID))

	return cfgPath
}

// An account email change cut short is finished at the CLI's entry, before
// the command resolves its config: a command run with the drive's old name
// resolves it under the new one, and says once that the email changed.
//
// Validates: R-3.7.3
func TestInitializeCLIContext_FinishesAnInterruptedEmailChangeBeforeTheCommandResolvesItsDrive(t *testing.T) {
	setTestDriveHome(t)
	oldCID := driveid.MustCanonicalID("business:user@example.com")
	newCID := driveid.MustCanonicalID("business:renamed@example.com")
	cfgPath := seedInterruptedEmailChange(t, oldCID, newCID)

	var status bytes.Buffer
	root := newRootCmdWithWriters(io.Discard, &status)
	var resolved driveid.CanonicalID
	root.AddCommand(&cobra.Command{
		Use: "resolved-drive",
		RunE: func(cmd *cobra.Command, _ []string) error {
			resolved = mustCLIContext(cmd.Context()).Cfg.CanonicalID
			return nil
		},
	})
	root.SetArgs([]string{"--config", cfgPath, "--drive", oldCID.String(), "resolved-drive"})
	require.NoError(t, root.Execute())
	require.NoError(t, closeRootCommandLogger(root))

	assert.Equal(t, newCID, resolved, "the command resolved its drive under the new name")
	assert.Equal(t, 1, strings.Count(status.String(),
		"Account email changed from user@example.com to renamed@example.com"))
	assert.NoFileExists(t, emailChangeJournalPathForTest())
	assert.NoFileExists(t, config.DriveStatePath(oldCID))
	assert.FileExists(t, config.DriveStatePath(newCID))
	assert.NoFileExists(t, config.DriveTokenPath(oldCID))
	assert.FileExists(t, config.DriveTokenPath(newCID))
}

// An email change remaps this invocation's exact old IDs to the new ones,
// whether the drive was named by --drive or by ONEDRIVE_GO_DRIVE, and says
// once that the email changed; a selector that is not an old ID stays.
//
// Validates: R-3.7.2, R-3.7.3
func TestApplyEmailReconcileResult_RemapsEveryExactSelectorOfTheInvocation(t *testing.T) {
	t.Parallel()

	oldCID := driveid.MustCanonicalID("business:user@example.com")
	newCID := driveid.MustCanonicalID("business:renamed@example.com")
	var status bytes.Buffer
	cc := &CLIContext{
		StatusWriter: &status,
		Flags:        CLIFlags{Account: oldCID.Email(), Drive: []string{oldCID.String(), "Work"}},
		Env:          config.EnvOverrides{Drive: oldCID.String()},
	}
	result := config.EmailReconcileResult{
		AccountRenames: []config.CanonicalIDRename{{From: oldCID, To: newCID}},
		DriveRenames:   []config.CanonicalIDRename{{From: oldCID, To: newCID}},
	}

	cc.applyEmailReconcileResult(&result)
	cc.applyEmailReconcileResult(&result)

	assert.Equal(t, newCID.Email(), cc.Flags.Account)
	assert.Equal(t, []string{newCID.String(), "Work"}, cc.Flags.Drive)
	assert.Equal(t, newCID.String(), cc.Env.Drive)
	assert.Equal(t, 1, strings.Count(status.String(), "Account email changed"))
}

// A change that cannot be finished fails every command that uses an account,
// with the reason; help and the completion scripts read and write nothing of
// an account, so they skip the change and still work.
//
// Validates: R-3.7.3
func TestInitializeCLIContext_AChangeThatCannotBeFinishedFailsOnlyCommandsThatUseAnAccount(t *testing.T) {
	setTestDriveHome(t)
	journalPath := emailChangeJournalPathForTest()
	require.NoError(t, os.MkdirAll(filepath.Dir(journalPath), 0o700))
	require.NoError(t, os.WriteFile(journalPath, []byte("{not a journal"), 0o600))

	for _, args := range [][]string{
		{"help"},
		{"help", "ls"},
		{"completion", "bash"},
		{cobra.ShellCompRequestCmd, "l"},
	} {
		var out bytes.Buffer
		root := newRootCmdWithWriters(&out, io.Discard)
		root.SetOut(&out)
		root.SetErr(io.Discard)
		root.SetArgs(args)
		require.NoError(t, root.Execute(), "%v", args)
		assert.NotEmpty(t, out.String(), "%v", args)
	}

	root := newRootCmdWithWriters(io.Discard, io.Discard)
	root.SetArgs([]string{"drive", "list"})
	err := root.Execute()
	require.ErrorContains(t, err, "finish an interrupted account email change")
	require.ErrorContains(t, err, journalPath)
	require.NoError(t, closeRootCommandLogger(root))
	assert.FileExists(t, journalPath, "a journal that cannot be read is kept")
}
