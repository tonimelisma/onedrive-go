//go:build e2e && e2e_full

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The user-facing safety mechanisms against the live service: the delete
// hold and its confirmation, the local trash, and the sync-folder identity
// refusal with `drive rebind-root`. Each runs in an isolated shared root so
// the baseline holds only the test's own files.

const safetyFileCount = 8

// seedSafetySyncRoot writes count small files under syncDir/folder and syncs
// them up, returning the folder name and the content by file name.
func seedSafetySyncRoot(t *testing.T, cfgPath string, env map[string]string, syncDir, folder string, count int) map[string]string {
	t.Helper()

	localDir := filepath.Join(syncDir, folder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	contents := make(map[string]string, count)
	for i := range count {
		name := fmt.Sprintf("f%02d.txt", i)
		contents[name] = fmt.Sprintf("safety %s %d\n", folder, i)
		require.NoError(t, os.WriteFile(filepath.Join(localDir, name), []byte(contents[name]), 0o600))
	}
	runSyncThroughTransientFailures(t, cfgPath, env)
	for name := range contents {
		waitForRemoteReadContains(t, cfgPath, env, "", name, pollTimeout, "ls", "/"+folder)
	}

	return contents
}

// remoteFileNames lists the file names `ls --json` reports under remotePath.
func remoteFileNames(t *testing.T, cfgPath string, env map[string]string, remotePath string) []string {
	t.Helper()

	stdout, _, err := runCLIWithConfigAllowError(t, cfgPath, env, "ls", remotePath, "--json")
	if err != nil {
		return nil
	}
	var items []struct {
		Name string `json:"name"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &items), "ls --json: %s", stdout)
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name)
	}

	return names
}

// Validates: R-6.2.5, R-6.6.28
//
// Removing every synced file locally is held, OneDrive keeps the files, and
// `sync --confirm-deletes` applies exactly that set. A pass that holds the
// deletes exits 0: the hold waits for the user on purpose.
func TestE2E_SyncFull_DeleteHoldKeepsOneDriveUntilConfirmed(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	folder := fmt.Sprintf("e2e-safety-hold-%d", time.Now().UnixNano())
	contents := seedSafetySyncRoot(t, cfgPath, env, syncDir, folder, safetyFileCount)

	require.NoError(t, os.RemoveAll(filepath.Join(syncDir, folder)))

	// Single pass by design: this one pass must hold the deletes. Deletes held
	// for confirmation are a deliberate hold, not work left, so it exits 0
	// (R-6.6.28).
	stdout, stderr, syncErr := runCLIWithConfigAllowError(t, cfgPath, env, "sync")
	combined := stdout + stderr
	require.NoErrorf(t, syncErr, "a pass that only holds deletes exits 0: %s", combined)
	assert.NotRegexp(t, syncLeftWorkPattern, stdout)
	assert.Contains(t, combined, "deletes for confirmation", "the deletes are held: %s", combined)
	assert.Contains(t, combined, "sync --confirm-deletes", "the summary names the command: %s", combined)
	assert.ElementsMatch(t, keysOf(contents), remoteFileNames(t, cfgPath, env, "/"+folder),
		"nothing is deleted on OneDrive while the hold stands")

	status := readStatus(t, cfgPath, env)
	driveStatus := requireStatusDriveByIdentity(t, status, resolveDriveSelection(env, ""))
	require.NotNil(t, driveStatus.SyncState)
	heldIssue := false
	for _, issue := range driveStatus.SyncState.Issues {
		if issue.Type == "deletes_held" {
			heldIssue = true
			// The folder's own delete is held with its files (folder deletes
			// count like file deletes, R-6.2.5).
			assert.Equal(t, safetyFileCount+1, issue.Count, "status counts the held deletes, the folder included")
		}
	}
	assert.True(t, heldIssue, "status lists the held deletes: %+v", driveStatus.SyncState.Issues)

	// A second ordinary pass holds them again: the hold is derived from
	// current truth, not from a one-shot flag. It exits 0 too.
	stdout, stderr, syncErr = runCLIWithConfigAllowError(t, cfgPath, env, "sync")
	require.NoErrorf(t, syncErr, "a pass that only holds deletes exits 0: %s", stdout+stderr)
	assert.Contains(t, stdout+stderr, "deletes for confirmation")
	assert.Len(t, remoteFileNames(t, cfgPath, env, "/"+folder), safetyFileCount)

	runSyncThroughTransientFailures(t, cfgPath, env, "--confirm-deletes")
	assert.Eventually(t, func() bool {
		return len(remoteFileNames(t, cfgPath, env, "/"+folder)) == 0
	}, pollTimeout, 2*time.Second, "the confirmed deletes are applied on OneDrive")

	// Single pass by design: the one pass after the confirmed deletes must
	// hold nothing; it may leave work a transient failure causes.
	stdout, stderr = runSyncPass(t, cfgPath, env)
	assert.NotContains(t, stdout+stderr, "deletes for confirmation", "nothing is held once applied")
}

// Validates: R-6.4.9
//
// A file deleted on OneDrive leaves this computer through the local trash,
// where `trash list` shows it and `trash restore` puts it back byte for byte.
func TestE2E_SyncFull_RemoteDeleteGoesToLocalTrashAndRestores(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	syncDir := t.TempDir()
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	folder := fmt.Sprintf("e2e-safety-trash-%d", time.Now().UnixNano())
	contents := seedSafetySyncRoot(t, cfgPath, env, syncDir, folder, 2)

	victim := "f00.txt"
	localVictim := filepath.Join(syncDir, folder, victim)
	runCLIWithConfig(t, cfgPath, env, "rm", "/"+folder+"/"+victim)

	// The delete reaches sync through delta, which can lag the rm.
	requireSyncEventuallyConverges(
		t,
		cfgPath,
		env,
		remoteDeletePropagationTimeout,
		"the remote delete is applied locally",
		func(result syncAttemptResult) bool {
			_, err := os.Stat(localVictim)
			return result.Err == nil && os.IsNotExist(err)
		},
	)
	assert.FileExists(t, filepath.Join(syncDir, folder, "f01.txt"), "the other file stays")

	stdout, _ := runCLIWithConfig(t, cfgPath, env, "trash", "list", "--json")
	var listing struct {
		Entries []struct {
			ID     string `json:"id"`
			Path   string `json:"path"`
			Reason string `json:"reason"`
		} `json:"entries"`
	}
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing), "trash list --json: %s", stdout)
	require.Len(t, listing.Entries, 1, "exactly the deleted file is in the trash: %s", stdout)
	entry := listing.Entries[0]
	assert.Equal(t, folder+"/"+victim, entry.Path)
	assert.Equal(t, "deleted", entry.Reason)

	runCLIWithConfig(t, cfgPath, env, "trash", "restore", entry.ID)
	require.FileExists(t, localVictim, "the file is back where sync removed it")
	assert.Equal(t, contents[victim], string(mustReadFile(t, localVictim)), "byte for byte")

	stdout, _ = runCLIWithConfig(t, cfgPath, env, "trash", "list", "--json")
	require.NoError(t, json.Unmarshal([]byte(stdout), &listing))
	assert.Empty(t, listing.Entries, "the restored entry has left the trash")
}

// Validates: R-6.2.12, R-6.6.28
//
// A different folder at the sync path is refused, with no delete anywhere,
// until the user runs `drive rebind-root`; a rebound empty folder then
// holds the deletes it would imply instead of running them.
func TestE2E_SyncFull_ReplacedSyncFolderIsRefusedUntilRebound(t *testing.T) {
	t.Parallel()
	registerLogDump(t)

	parent := t.TempDir()
	syncDir := filepath.Join(parent, "sync")
	require.NoError(t, os.MkdirAll(syncDir, 0o700))
	cfgPath, env := writeIsolatedSharedRootSyncConfig(t, syncDir)
	folder := fmt.Sprintf("e2e-safety-rebind-%d", time.Now().UnixNano())
	contents := seedSafetySyncRoot(t, cfgPath, env, syncDir, folder, safetyFileCount)

	// The folder goes away and an empty one takes its place: a restored or
	// re-created sync folder, or a mount that did not come up.
	require.NoError(t, os.Rename(syncDir, syncDir+"-moved"))
	require.NoError(t, os.MkdirAll(syncDir, 0o700))

	refusal := runCLIWithConfigExpectError(t, cfgPath, env, "sync")
	assert.Contains(t, refusal, "rebind-root", "the refusal names the command: %s", refusal)
	assert.ElementsMatch(t, keysOf(contents), remoteFileNames(t, cfgPath, env, "/"+folder),
		"a refused sync deletes nothing on OneDrive")
	assert.NoFileExists(t, filepath.Join(syncDir, folder, "f00.txt"), "and downloads nothing into the stranger")

	stdout, stderr := runCLIWithConfig(t, cfgPath, env, "drive", "rebind-root", "--yes")
	t.Logf("rebind-root: %s%s", stdout, stderr)

	// Single pass by design: this one pass must hold the deletes, and a pass
	// that only holds deletes exits 0 (R-6.6.28).
	stdout, stderr, syncErr := runCLIWithConfigAllowError(t, cfgPath, env, "sync")
	require.NoErrorf(t, syncErr, "the rebound folder's held deletes are not work left: %s", stdout+stderr)
	assert.Contains(t, stdout+stderr, "deletes for confirmation",
		"the rebound empty folder holds the deletes it implies: %s", stdout+stderr)
	assert.ElementsMatch(t, keysOf(contents), remoteFileNames(t, cfgPath, env, "/"+folder),
		"still nothing deleted on OneDrive")
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	return keys
}
