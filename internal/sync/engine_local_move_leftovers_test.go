package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// leftoverMoveHarness is a synced docs/a.txt whose engine runs on a system
// that refuses the no-replace flag and cannot unlink a.txt, and a OneDrive
// rename of it to docs/b.txt, which the next pass applies as a local move
// that links b.txt and leaves a.txt behind.
func leftoverMoveHarness(t *testing.T) (*moveScenarioRemote, *syncHarness) {
	t.Helper()
	synctest.SkipWithoutHardLinks(t)

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFolder("item-docs", "root", "docs")
	remote.addFile(t, "item-a", "item-docs", "a.txt", "alpha\n")
	eng, syncRoot := newTestEngineInDirWith(t, t.Context(), remote.client(t), t.TempDir(), func(inputs *engineInputs) {
		inputs.hooks.adaptSyncTree = func(tree *synctree.Root) *synctree.Root {
			return tree.WithoutNoReplaceFlagAndUnremovableName("a.txt")
		}
	})
	eng.hooks.serializeWorkers = true
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	runScenarioPass(t, h.eng)
	require.FileExists(t, filepath.Join(syncRoot, "docs", "a.txt"))
	remote.moveAt(t, "docs/a.txt", "docs/b.txt")

	return remote, h
}

// requireMovedOnce holds that the move ended with one file on each side, at
// its new name: the old name the move left was removed, not uploaded again.
func requireMovedOnce(t *testing.T, h *syncHarness) {
	t.Helper()

	assert.Equal(t, map[string]string{"docs": syncfixture.ModelFolderSentinel, "docs/b.txt": "alpha\n"},
		harnessLocalTree(t, h.syncRoot), "the old local name is gone")
	assert.Equal(t, map[string]string{"docs": syncfixture.ModelFolderSentinel, "docs/b.txt": "alpha\n"},
		h.remote.tree(), "OneDrive holds the file once")
}

// A local move whose old name cannot be removed (a link where the system
// refuses the no-replace flag) leaves that name hidden from the planner, so
// none of the run's passes uploads it as a new file, and the next start
// removes it: it is a second name of the moved file, so nothing goes with it.
//
// Validates: R-2.12.9
func TestLocalMoveLeftover_TheOldNameIsNeverUploadedAndTheNextStartRemovesIt(t *testing.T) {
	t.Parallel()

	_, h := leftoverMoveHarness(t)
	for range modelMaxSyncPasses {
		_, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
		require.NoError(t, err)
	}

	requireMovedOnce(t, h)
	intents, err := h.eng.baseline.ListLocalMoveIntents(t.Context())
	require.NoError(t, err)
	assert.Empty(t, intents, "the settled move's intent is cleared")
}

// The process dies right after the rename that left the old name, before the
// move's outcome is committed. The intent recorded before the rename names
// the old name to the next engine, which removes it and converges with the
// file once on each side.
//
// Validates: R-2.12.9
func TestLocalMoveLeftover_ACrashAfterTheRenameStillRemovesTheOldName(t *testing.T) {
	t.Parallel()

	remote, h := leftoverMoveHarness(t)
	capture := &crashCapture{
		point: syncdomain.CrashPointLocalMoveDone, srcDir: filepath.Dir(h.syncRoot), dstDir: t.TempDir(), remote: remote,
	}
	h.eng.armCrash(capture.hook)
	_, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{})
	require.NoError(t, err)

	recovered := capture.reopen(t)
	require.FileExists(t, filepath.Join(recovered.syncRoot, "docs", "a.txt"), "the crash kept the old name")
	syncHarnessUntilQuiet(t, recovered)

	requireMovedOnce(t, recovered)
}

// What the next start does with each shape a move's intent can find: a
// second link of the moved file and a copy of the same content are removed;
// an old name that now holds other content is the user's and kept, said
// once; an old name already gone, or a new name that is not there (the
// process died before the rename), needs nothing. Every intent is settled.
//
// Validates: R-2.12.9
func TestSettleLocalMoveLeftovers_RemovesOnlyANameThatHoldsTheMovedFile(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	logger, logs := syncfixture.NewCapturedLogger()
	eng, root := newTestEngineWithLogger(t, &syncfixture.EngineMockClient{}, logger)
	write := func(rel, content string) {
		require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte(content), 0o600))
	}
	write("linked-new.txt", "linked\n")
	require.NoError(t, os.Link(filepath.Join(root, "linked-new.txt"), filepath.Join(root, "linked-old.txt")))
	write("copy-new.txt", "copied\n")
	write("copy-old.txt", "copied\n")
	write("changed-new.txt", "moved\n")
	write("changed-old.txt", "edited since\n")
	write("gone-new.txt", "moved\n")
	write("unmoved-old.txt", "still here\n")
	for _, pair := range [][2]string{
		{"linked-old.txt", "linked-new.txt"},
		{"copy-old.txt", "copy-new.txt"},
		{"changed-old.txt", "changed-new.txt"},
		{"gone-old.txt", "gone-new.txt"},
		{"unmoved-old.txt", "unmoved-new.txt"},
	} {
		require.NoError(t, eng.baseline.RecordLocalMoveIntent(t.Context(), pair[0], pair[1]))
	}

	eng.settleLocalMoveLeftovers(t.Context())

	assert.NoFileExists(t, filepath.Join(root, "linked-old.txt"))
	assert.NoFileExists(t, filepath.Join(root, "copy-old.txt"))
	for _, kept := range []string{"linked-new.txt", "copy-new.txt", "changed-old.txt", "changed-new.txt", "gone-new.txt", "unmoved-old.txt"} {
		assert.FileExists(t, filepath.Join(root, kept), "%s is kept", kept)
	}
	assert.Equal(t, 1, syncfixture.CountLevel(logs.String(), "WARN"), logs.String())
	assert.Contains(t, logs.String(), "Kept changed-old.txt")
	intents, err := eng.baseline.ListLocalMoveIntents(t.Context())
	require.NoError(t, err)
	assert.Empty(t, intents)
}
