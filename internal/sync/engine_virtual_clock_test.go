package sync

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.17.2, R-2.15.3
//
// A tool that deletes a synced folder online on every poll must not cause a
// full enumeration per poll. A folder deletion makes the full refresh due
// FullRefreshAfterFolderDeleteDelay later, not at once, so the deletions of
// one burst share one refresh and refreshes stay that far apart; the item
// moved into a deleted folder (model test seed 28) is still resolved, by the
// deferred refresh.
func TestRunOnce_VirtualTime_FolderDeletesSpaceFullRefreshes(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	syncfixture.InVirtualTime(t, func(t *testing.T) { //nolint:thelper // a test body run in a synctest bubble, like a t.Run subtest
		remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
		const folders = 6
		for i := range folders {
			id := fmt.Sprintf("item-f%d", i)
			remote.addFolder(id, "root", fmt.Sprintf("f%d", i))
			remote.addFile(t, id+"-in", id, "in.txt", fmt.Sprintf("in %d\n", i))
		}
		remote.addFile(t, "item-moved", "root", "moved.txt", "moved\n")
		remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
		h := newSyncHarness(t, remote)
		runScenarioPass(t, h.eng)
		start := time.Now()

		remote.move("item-moved", "item-f0", "moved.txt")
		for minute := range 40 {
			if minute < folders {
				remote.deleteRemotely(fmt.Sprintf("item-f%d", minute))
			}
			_, err := h.eng.RunOnce(t.Context(), syncdomain.SyncBidirectional, RunOptions{ConfirmDeletes: true})
			require.NoError(t, err)
			syncfixture.VirtualWait(time.Minute)
		}

		var refreshes []time.Time
		for _, at := range remote.fullDeltaAt {
			if at.After(start) {
				refreshes = append(refreshes, at)
			}
		}
		require.NotEmpty(t, refreshes, "the deferred full refresh ran")
		for i := 1; i < len(refreshes); i++ {
			assert.GreaterOrEqual(t, refreshes[i].Sub(refreshes[i-1]), syncdomain.FullRefreshAfterFolderDeleteDelay,
				"full refreshes %d and %d are too close", i-1, i)
		}
		assert.NoFileExists(t, filepath.Join(h.syncRoot, "moved.txt"), "the item moved into a deleted folder is resolved")
		assertSyncInvariants(t, h)
	})
}
