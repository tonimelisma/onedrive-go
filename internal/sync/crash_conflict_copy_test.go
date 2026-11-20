package sync

import (
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The two edits of one file that conflict in conflictCopyUploadCutAfterCreate.
const (
	conflictCrashPath          = "c.md"
	conflictCrashLocalContent  = "c local\n"
	conflictCrashRemoteContent = "c remote\n"
)

// A conflict copy reaches OneDrive as a new file, so its upload can be cut
// at upload.created_unrecorded: OneDrive holds the copy's empty item and the
// state database the create's intent. After a crash the intent claims that
// item and the restart finishes the copy's upload into it (R-5.6.15). When
// the WAL is lost too (the crash harness's lost-WAL variant), the intent goes
// with it: nothing then tells the empty item from another client's file, so
// the restart keeps both versions (R-2.3.5), the copy's content moves on to a
// second copy, and the empty item is downloaded at the first copy's name.
// Either way nothing is lost and every invariant holds.
//
// Validates: R-5.6.15, R-2.3.5, R-6.5.2
func TestCrashRecovery_ConflictCopyWhoseUploadWasCutAfterTheCreate(t *testing.T) {
	t.Parallel()

	for _, lostWAL := range []bool{false, true} {
		t.Run(crashCaseName(syncdomain.CrashPointUploadCreatedUnrecorded, lostWAL), func(t *testing.T) {
			t.Parallel()

			placeholder, placeholderID, recovered := conflictCopyUploadCutAfterCreate(t, lostWAL)
			require.True(t, assertSyncInvariants(t, recovered), "state:%s", syncStateDump(t, recovered.eng))

			local := harnessLocalTree(t, recovered.syncRoot)
			assert.Equal(t, conflictCrashRemoteContent, local[conflictCrashPath], "the OneDrive edit keeps the original name")
			kept := pathsHolding(local, conflictCrashLocalContent)
			require.Len(t, kept, 1, "the local edit is kept exactly once: %v", local)
			if !lostWAL {
				assert.Equal(t, placeholder, kept[0], "the local edit stays at the copy's name")
				assert.Empty(t, pathsHolding(local, ""), "no empty file is left")
				item, err := recovered.remote.getItemByPath(placeholder)
				require.NoError(t, err)
				assert.Equal(t, placeholderID, item.ID, "the copy's upload finished into the cut create's own empty item")
				return
			}
			assert.Equal(t, map[string]bool{placeholderID: true}, recovered.placeholdersWithoutIntent,
				"the lost WAL took the intent: nothing names the empty item as this client's")
			assert.Equal(t, []string{placeholder}, pathsHolding(local, ""), "the empty item is downloaded at the first copy's name")
			stem := strings.TrimSuffix(placeholder, filepath.Ext(placeholder))
			assert.True(t, strings.HasPrefix(kept[0], stem+modelConflictMarker),
				"the local edit is in a conflict copy of the first copy: %s", kept[0])
		})
	}
}

// conflictCopyUploadCutAfterCreate syncs one file, edits it on both sides,
// and syncs with a capture armed at upload.created_unrecorded: the first pass
// makes the conflict copy, the next uploads it and reaches the point. It
// returns the path and item ID of the empty item the cut create left on
// OneDrive and the engine that recovered from the capture, converged.
func conflictCopyUploadCutAfterCreate(t *testing.T, lostWAL bool) (string, string, *syncHarness) {
	t.Helper()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-c", "root", conflictCrashPath, "c\n")
	h := newSyncHarness(t, remote)
	h.eng.serializeWorkers = true
	runScenarioPass(t, h.eng)
	// The synced state reaches the main database file; the round's commits,
	// the create's intent among them, stay in the WAL, which the lost-WAL
	// variant then loses.
	_, err := h.eng.baseline.RawDBForTest().ExecContext(t.Context(), `PRAGMA wal_checkpoint(TRUNCATE)`)
	require.NoError(t, err)

	syncfixture.WriteLocalFile(t, h.syncRoot, conflictCrashPath, conflictCrashLocalContent)
	remote.putFileAt(t, conflictCrashPath, conflictCrashRemoteContent)
	capture := &crashCapture{
		point: syncdomain.CrashPointUploadCreatedUnrecorded, lostWAL: lostWAL,
		srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.execCfg.SetCrashHookForTest(capture.hook)
	h.eng.baseline.SetCrashHookForTest(capture.hook)
	h.beginRound(t)
	syncHarnessUntilQuiet(t, h)
	require.True(t, capture.captured, "the conflict copy's upload reached %s", capture.point)

	placeholders := capture.remoteCopy.emptyFiles()
	require.Len(t, placeholders, 1, "the cut create left one empty item on OneDrive")
	var placeholder, placeholderID string
	for p, itemID := range placeholders {
		placeholder, placeholderID = p, itemID
	}
	require.Contains(t, placeholder, modelConflictMarker, "the cut create was the conflict copy's")

	recovered := capture.reopen(t)
	recovered.held = h.held
	syncHarnessUntilQuiet(t, recovered)

	return placeholder, placeholderID, recovered
}

// pathsHolding is every path of tree whose content is content, sorted.
func pathsHolding(tree map[string]string, content string) []string {
	var paths []string
	for p, held := range tree {
		if held == content {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)

	return paths
}
