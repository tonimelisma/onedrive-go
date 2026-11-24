package syncexec

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.17.1
//
// A remote move of a file this computer moved and edited (paired because its
// birth time proves it is the item) publishes the new path and the moved
// file's identity, while its local content facts stay the baseline's: the
// edit is synced only when its upload records it. Recorded here, a remote
// edit met no local change on the next pass and came down over the edit.
// A destination whose hash is unknown keeps today's outcome.
func TestExecutor_MoveOutcome_AnEditedMovedFileKeepsItsEditPending(t *testing.T) {
	t.Parallel()

	cfg, _ := newTestExecutorConfig(t, &syncfixture.ExecutorMockItemClient{}, &syncfixture.ExecutorMockDownloader{}, &syncfixture.ExecutorMockUploader{})
	e := NewExecution(cfg, syncfixture.EmptyBaseline())
	baseline := &syncdomain.BaselineEntry{
		ItemType: syncdomain.ItemTypeFile, LocalHash: "synced", RemoteHash: "synced",
		LocalSize: 4, LocalSizeKnown: true, LocalMtime: 10, LocalCtime: 11,
		LocalDevice: 1, LocalInode: 2, LocalBirthTime: 3, LocalHasIdentity: true,
	}
	move := func(movedLocal *syncdomain.LocalState) syncdomain.ActionOutcome {
		return e.moveOutcome(&syncdomain.Action{
			Type: syncdomain.ActionRemoteMove, Path: "dst", OldPath: "src", ItemID: "item1",
			DriveID: driveid.New(synctest.TestDriveID), MovedLocal: movedLocal,
			View: &syncdomain.PathView{Path: "src", Baseline: baseline},
		})
	}

	edited := move(&syncdomain.LocalState{
		ItemType: syncdomain.ItemTypeFile, Hash: "edited", Size: 9, Mtime: 20, LocalCtime: 21,
		LocalDevice: 1, LocalInode: 2, LocalBirthTime: 3, LocalHasIdentity: true,
	})
	assert.Equal(t, "synced", edited.LocalHash, "the edit stays pending")
	assert.Equal(t, int64(4), edited.LocalSize)
	assert.Equal(t, int64(10), edited.LocalMtime)
	assert.Equal(t, int64(11), edited.LocalCtime)
	assert.Equal(t, uint64(2), edited.LocalInode, "the identity is the moved file's")
	assert.Equal(t, int64(3), edited.LocalBirthTime)

	unchanged := move(&syncdomain.LocalState{
		ItemType: syncdomain.ItemTypeFile, Hash: "synced", Size: 4, Mtime: 10, LocalCtime: 30,
		LocalDevice: 1, LocalInode: 2, LocalBirthTime: 3, LocalHasIdentity: true,
	})
	assert.Equal(t, "synced", unchanged.LocalHash)
	assert.Equal(t, int64(30), unchanged.LocalCtime, "an unedited move records what it observed")

	hashless := move(&syncdomain.LocalState{
		ItemType: syncdomain.ItemTypeFile, Size: 4, Mtime: 10, LocalCtime: 30,
		LocalDevice: 1, LocalInode: 2, LocalHasIdentity: true,
	})
	assert.Empty(t, hashless.LocalHash, "an unknown destination hash stays unknown")
}
