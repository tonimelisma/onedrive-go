package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// rewriteKeepingMtime rewrites a file in place and restores its mtime, as
// `cp -p`, `touch -r` and `rsync -t --inplace` do.
func rewriteKeepingMtime(t *testing.T, path, content string) {
	t.Helper()

	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Len(t, content, int(info.Size()), "the edit keeps the size")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
}

// Validates: R-2.2.4
func TestRunOnce_InPlaceEditWithRestoredMtimeUploads(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-1", "root", "doc.txt", "synced version")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.Equal(t, 1, runScenarioPass(t, eng).Downloads)

	rewriteKeepingMtime(t, filepath.Join(syncRoot, "doc.txt"), "EDITED version")

	// The edit used to be invisible: size and mtime matched the baseline, so
	// the scan reused the synced hash and nothing was ever uploaded.
	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.Uploads, "the in-place edit is uploaded: %+v", report)
	assert.Equal(t, "EDITED version", remote.contents["item-1"])
	requireScenarioQuietPlan(t, eng)
}

// Validates: R-2.2.4, R-2.2.5
func TestRunOnce_InPlaceEditWithRestoredMtimeConflictsWithRemoteEdit(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-1", "root", "doc.txt", "synced version")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.Equal(t, 1, runScenarioPass(t, eng).Downloads)

	rewriteKeepingMtime(t, filepath.Join(syncRoot, "doc.txt"), "EDITED version")
	remote.setContent(t, "item-1", "remote version")

	// With the stale hash the change looked remote-only, and the download
	// either replaced the edit or was superseded on every pass.
	report := runScenarioPass(t, eng)
	assert.Equal(t, 1, report.ConflictCopies, "an edit on both sides is a conflict: %+v", report)

	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	var contents []string
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		data, readErr := localpath.ReadFile(filepath.Join(syncRoot, entry.Name()))
		require.NoError(t, readErr)
		contents = append(contents, string(data))
	}
	assert.Contains(t, contents, "EDITED version", "the local edit survives")
	assert.Contains(t, contents, "remote version", "the remote edit arrives")
}

// The baseline must hold the ctime and identity of what the executor wrote
// and uploaded, or the hash fast path never applies and every scan re-reads
// every file.
//
// Validates: R-2.2.2, R-2.2.4
func TestRunOnce_TransfersRecordLocalCtimeAndIdentity(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "item-1", "root", "down.txt", "downloaded")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	writeTestFile(t, syncRoot, "up.txt", "uploaded")
	report := runScenarioPass(t, eng)
	require.Equal(t, 1, report.Downloads)
	require.Equal(t, 1, report.Uploads)

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)
	for _, name := range []string{"down.txt", "up.txt"} {
		entry, found := bl.GetByPath(name)
		require.True(t, found, name)
		info, statErr := os.Lstat(filepath.Join(syncRoot, name))
		require.NoError(t, statErr)
		ctime, ok := synctree.ChangeTimeFromFileInfo(info)
		require.True(t, ok)
		assert.Equal(t, ctime, entry.LocalCtime, "%s baseline ctime", name)
		assert.True(t, entry.LocalHasIdentity, "%s baseline identity", name)
		assert.True(t, canReuseBaselineHash(info, entry, ctime+2*nanosPerSecond), "%s fast path applies", name)
	}
}
