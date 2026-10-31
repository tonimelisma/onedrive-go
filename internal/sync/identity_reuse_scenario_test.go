package sync

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// Inode reuse (D-057, R-2.17.1). ext4 and tmpfs readily give a just-freed
// inode number to the next entry created, so a deleted synced item and an
// unrelated new one can share (device, inode). The entry's birth time tells
// them apart where the filesystem records one. The kernel cannot be made to
// reuse an inode on demand, so these scenarios run over synctest's identity
// shim, which reports the new entry with the deleted entry's inode.

const (
	reuseOldBirthTime = int64(1_700_000_000_000_000_001)
	reuseNewBirthTime = int64(1_700_000_000_000_000_002)
)

// realInode is the inode number the real filesystem gave rel.
func realInode(t *testing.T, syncRoot, rel string) uint64 {
	t.Helper()

	info, err := os.Lstat(filepath.Join(syncRoot, filepath.FromSlash(rel)))
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "the test filesystem reports inodes")

	return stat.Ino
}

// newIdentityReuseHarness syncs remote once over shim and returns the
// harness. keep.txt at the root keeps the local side from looking emptied,
// which the delete-safety hold would hold for confirmation instead.
func newIdentityReuseHarness(t *testing.T, remote *moveScenarioRemote, shim *synctest.IdentityShim) *syncHarness {
	t.Helper()

	remote.addFile(t, "item-keep", "root", "keep.txt", "keep\n")
	eng, syncRoot := newTestEngineOnFS(t, remote.client(t), shim.Wrap)
	h := &syncHarness{eng: eng, remote: remote, syncRoot: syncRoot}
	runScenarioPass(t, eng)

	return h
}

// Validates: R-2.17.1, R-2.2.6
//
// The user deletes a synced folder and creates an unrelated one elsewhere,
// which the filesystem hands the freed inode. It is a delete and a create:
// renaming the old OneDrive folder into the new one would give the new
// folder the old one's sharing links, permissions and history.
func TestRunOnce_DeletedFolderAndNewFolderWithReusedInodeAreNotAMove(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	remote.addFolder("item-old", "root", "old")
	remote.addFolder("item-elsewhere", "root", "elsewhere")
	shim := synctest.NewIdentityShim(synctest.IdentityRule{Path: "old", BirthTime: reuseOldBirthTime})
	h := newIdentityReuseHarness(t, remote, shim)

	oldInode := realInode(t, h.syncRoot, "old")
	require.NoError(t, os.Remove(filepath.Join(h.syncRoot, "old")))
	require.NoError(t, os.Mkdir(filepath.Join(h.syncRoot, "elsewhere", "new"), 0o750))
	shim.Set(synctest.IdentityRule{Path: "elsewhere/new", Inode: oldInode, BirthTime: reuseNewBirthTime})

	h.beginRound(t)
	runScenarioPass(t, h.eng)

	assert.Empty(t, remote.moves, "a reused inode is not the deleted folder")
	assert.Contains(t, remote.deletes, "item-old", "the deleted folder goes to the recycle bin")
	assert.Contains(t, remote.folderCreates, "new", "the new folder is a new item")
	newItem, ok := remote.itemAt("elsewhere/new")
	require.True(t, ok)
	assert.NotEqual(t, "item-old", newItem.ID)
	assertSyncInvariants(t, h)
}

// Validates: R-2.17.1, R-2.2.6
//
// The same for a file. Identical content is the case content corroboration
// cannot catch: the copy matches the deleted file's size and hash, and only
// the birth time shows it is another file. Different content was already
// refused by corroboration and must stay refused.
func TestRunOnce_DeletedFileAndNewFileWithReusedInodeAreNotAMove(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "identical content", content: "old content\n"},
		{name: "different content", content: "new content\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFile(t, "item-old", "root", "old.txt", "old content\n")
			shim := synctest.NewIdentityShim(synctest.IdentityRule{Path: "old.txt", BirthTime: reuseOldBirthTime})
			h := newIdentityReuseHarness(t, remote, shim)

			oldInode := realInode(t, h.syncRoot, "old.txt")
			require.NoError(t, os.Remove(filepath.Join(h.syncRoot, "old.txt")))
			writeLocalFile(t, h.syncRoot, "new.txt", tc.content)
			shim.Set(synctest.IdentityRule{Path: "new.txt", Inode: oldInode, BirthTime: reuseNewBirthTime})

			h.beginRound(t)
			runScenarioPass(t, h.eng)

			assert.Empty(t, remote.moves, "a reused inode is not the deleted file")
			assert.Contains(t, remote.deletes, "item-old", "the deleted file goes to the recycle bin")
			newItem, ok := remote.itemAt("new.txt")
			require.True(t, ok)
			assert.NotEqual(t, "item-old", newItem.ID, "the new file is uploaded as a new item")
			assert.Contains(t, remote.retainedContents(), "old content\n")
			assertSyncInvariants(t, h)
		})
	}
}

// Validates: R-2.17.1
//
// A genuine rename keeps the inode and the birth time, and still moves the
// OneDrive item, for a folder and for a file.
func TestRunOnce_RenameWithMatchingBirthTimeIsStillAMove(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		isFolder bool
	}{
		{name: "folder", isFolder: true},
		{name: "file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			if tc.isFolder {
				remote.addFolder("item-old", "root", "old")
			} else {
				remote.addFile(t, "item-old", "root", "old", "content\n")
			}
			shim := synctest.NewIdentityShim(
				synctest.IdentityRule{Path: "old", BirthTime: reuseOldBirthTime},
				synctest.IdentityRule{Path: "renamed", BirthTime: reuseOldBirthTime},
			)
			h := newIdentityReuseHarness(t, remote, shim)

			require.NoError(t, os.Rename(filepath.Join(h.syncRoot, "old"), filepath.Join(h.syncRoot, "renamed")))
			h.beginRound(t)
			runScenarioPass(t, h.eng)

			assert.Equal(t, []string{"item-old->renamed"}, remote.moves)
			assert.Empty(t, remote.deletes)
			assertSyncInvariants(t, h)
		})
	}
}

// Validates: R-2.17.1
//
// Where the filesystem reports no birth time, identity is (device, inode) as
// before: a rename moves, and a reused inode cannot be told from one, so a
// deleted folder and a new folder holding its inode still pair as a move.
func TestRunOnce_WithoutBirthTimeIdentityIsDeviceAndInode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		// reuse deletes old and creates elsewhere/new with old's inode;
		// otherwise old is renamed to elsewhere/new.
		reuse bool
	}{
		{name: "rename"},
		{name: "reused inode", reuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
			remote.addFolder("item-old", "root", "old")
			remote.addFolder("item-elsewhere", "root", "elsewhere")
			shim := synctest.NewIdentityShim(
				synctest.IdentityRule{Path: "old"},
				synctest.IdentityRule{Path: "elsewhere/new"},
			)
			h := newIdentityReuseHarness(t, remote, shim)

			oldPath, newPath := filepath.Join(h.syncRoot, "old"), filepath.Join(h.syncRoot, "elsewhere", "new")
			if tc.reuse {
				oldInode := realInode(t, h.syncRoot, "old")
				require.NoError(t, os.Remove(oldPath))
				require.NoError(t, os.Mkdir(newPath, 0o750))
				shim.Set(synctest.IdentityRule{Path: "elsewhere/new", Inode: oldInode})
			} else {
				require.NoError(t, os.Rename(oldPath, newPath))
			}
			h.beginRound(t)
			runScenarioPass(t, h.eng)

			assert.Equal(t, []string{"item-old->new"}, remote.moves)
			assert.Empty(t, remote.deletes)
			assertSyncInvariants(t, h)
		})
	}
}
