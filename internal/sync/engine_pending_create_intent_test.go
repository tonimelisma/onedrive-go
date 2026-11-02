package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-5.6.15
//
// Live, 2026-11-01: the watch client was killed with uploads in flight.
// OneDrive created one file's empty item two seconds after the kill, so the
// record of it was never written, and the next pass planned a create
// conflict: the user's file was renamed to a conflict copy and the empty
// item downloaded in its place. The intent recorded before the request is
// what the recovered engine recognizes the empty item by.
func TestRunOnce_EmptyItemACrashLeftUnrecordedIsFinishedNotConflicted(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFile(t, "orphan", "root", "doc.txt", "")
	eng, syncRoot := newTestEngine(t, remote.client(t))
	require.NoError(t, eng.baseline.RecordPendingCreateIntent(t.Context(), &PendingCreate{
		DriveID: driveid.New(engineTestDriveID), Path: "doc.txt", ParentID: "root",
	}))
	require.NoError(t, os.WriteFile(filepath.Join(syncRoot, "doc.txt"), []byte("the user's content\n"), 0o600))

	report := runScenarioPass(t, eng)

	assert.Equal(t, "the user's content\n", remote.tree()["doc.txt"], "the content goes into the empty item")
	assert.Equal(t, 1, report.Uploads)
	assert.Zero(t, report.Downloads, "the empty item is not downloaded over the user's file")
	entries, err := os.ReadDir(syncRoot)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.Contains(entry.Name(), ".conflict-"), "no conflict copy: %s", entry.Name())
	}
	assertSyncInvariants(t, &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot})
}
