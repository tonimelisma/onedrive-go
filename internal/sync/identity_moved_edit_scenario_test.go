package sync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Validates: R-2.17.1, R-2.17.2
//
// A file renamed and edited inside one observation window keeps its OneDrive
// item only where its birth time proves it is the same entry: the same
// device and inode, and a birth time both the baseline and the new path
// know and agree on. Then the rename goes up as a move and the edit as an
// upload to the same item, which keeps its sharing links and history, and
// OneDrive keeps the replaced version. Where either side lacks a birth time
// a reused inode cannot be told from a rename, so the edited file is a new
// file (the content proof, unchanged), and an inode a new file reused with
// another birth time is never the item.
func TestRunOnce_MovedAndEditedFileKeepsItsItemOnlyWhenBirthTimeProvesIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// oldBirth and newBirth are the birth times the source and the
		// destination report; zero reports a filesystem that records none.
		oldBirth, newBirth int64
		// reuse deletes old.txt and writes renamed.txt as a new file that
		// reports old.txt's inode; otherwise old.txt is renamed.
		reuse    bool
		wantMove bool
	}{
		{name: "birth time known on both sides", oldBirth: reuseOldBirthTime, newBirth: reuseOldBirthTime, wantMove: true},
		{name: "no birth time recorded"},
		{name: "reused inode with another birth time", oldBirth: reuseOldBirthTime, newBirth: reuseNewBirthTime, reuse: true},
		{name: "birth time known at the source only", oldBirth: reuseOldBirthTime},
		{name: "birth time known at the destination only", newBirth: reuseOldBirthTime},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
			remote.addFile(t, "item-old", "root", "old.txt", "old content\n")
			shim := synctest.NewIdentityShim(synctest.IdentityRule{Path: "old.txt", BirthTime: tc.oldBirth})
			h := newIdentityReuseHarness(t, remote, shim)

			oldInode := realInode(t, h.syncRoot, "old.txt")
			oldPath, newPath := filepath.Join(h.syncRoot, "old.txt"), filepath.Join(h.syncRoot, "renamed.txt")
			if tc.reuse {
				require.NoError(t, os.Remove(oldPath))
				syncfixture.WriteLocalFile(t, h.syncRoot, "renamed.txt", "edited content!\n")
				shim.Set(synctest.IdentityRule{Path: "renamed.txt", Inode: oldInode, BirthTime: tc.newBirth})
			} else {
				require.NoError(t, os.Rename(oldPath, newPath))
				require.NoError(t, os.WriteFile(newPath, []byte("edited content!\n"), 0o600))
				shim.Set(synctest.IdentityRule{Path: "renamed.txt", BirthTime: tc.newBirth})
			}
			h.beginRound(t)
			runScenarioPass(t, h.eng)

			item, ok := remote.itemAt("renamed.txt")
			require.True(t, ok)
			assert.Equal(t, "edited content!\n", remote.tree()["renamed.txt"], "the edit is on OneDrive")
			assert.Contains(t, remote.retainedContents(), "old content\n", "OneDrive keeps the synced content")
			if tc.wantMove {
				assert.Equal(t, []string{"item-old->renamed.txt"}, remote.moves)
				assert.Empty(t, remote.deletes)
				assert.Equal(t, "item-old", item.ID, "the edit is uploaded to the same item")
			} else {
				assert.Empty(t, remote.moves, "an edited file is not proven the item")
				assert.Contains(t, remote.deletes, "item-old")
				assert.NotEqual(t, "item-old", item.ID)
			}
			assertSyncInvariants(t, h)
		})
	}
}

// Validates: R-2.17.1, R-2.3.1
//
// OneDrive edits the item while this computer moves and edits it, the birth
// time proving the moved file is the item: the move goes up alone and the
// two edits meet at the moved path as a content conflict that keeps both.
// The upload of the local edit never replaces OneDrive's.
func TestRunOnce_MovedAndEditedFileWithBirthTimeAndARemoteEditKeepsBoth(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(syncfixture.EngineTestDriveID))
	remote.addFile(t, "item-old", "root", "old.txt", "old content\n")
	shim := synctest.NewIdentityShim(synctest.IdentityRule{Path: "old.txt", BirthTime: reuseOldBirthTime})
	h := newIdentityReuseHarness(t, remote, shim)

	newPath := filepath.Join(h.syncRoot, "renamed.txt")
	require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "old.txt"), newPath))
	require.NoError(t, os.WriteFile(newPath, []byte("edited here!\n"), 0o600))
	shim.Set(synctest.IdentityRule{Path: "renamed.txt", BirthTime: reuseOldBirthTime})
	remote.setContent(t, "item-old", "edited on OneDrive\n")
	h.beginRound(t)
	syncHarnessUntilQuiet(t, h)

	assert.Equal(t, []string{"item-old->renamed.txt"}, remote.moves, "the birth time proves the move")
	assert.Equal(t, "edited on OneDrive\n", remote.tree()["renamed.txt"], "OneDrive's edit keeps the item")
	contents := map[string]bool{}
	for _, content := range remote.tree() {
		contents[content] = true
	}
	assert.True(t, contents["edited here!\n"], "the local edit is kept as a conflict copy")
	assertSyncInvariants(t, h)
}
