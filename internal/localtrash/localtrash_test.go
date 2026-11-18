package localtrash

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func putEntry(t *testing.T, trash *Trash, syncRoot, rel, content string) Entry {
	t.Helper()

	placement, err := trash.Reserve(syncRoot, rel, ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte(content), 0o600))
	entry, err := placement.Commit()
	require.NoError(t, err)
	return entry
}

// Validates: R-6.4.9
func TestDirForStatePath_IsPerDriveUnderTheDataDir(t *testing.T) {
	t.Parallel()

	dataDir := filepath.Join(string(filepath.Separator), "data")
	assert.Equal(t, filepath.Join(dataDir, "trash", "state_personal_user@example.com"),
		DirForStatePath(filepath.Join(dataDir, "state_personal_user@example.com.db")))
	assert.Empty(t, DirForStatePath(""))
}

// Validates: R-6.4.9
func TestPurge_RemovesOnlyExpiredEntriesAndStaleIncompletePlacements(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	trash := Open(t.TempDir(), clock)
	syncRoot := t.TempDir()

	old := putEntry(t, trash, syncRoot, "old.txt", "old\n")
	now = now.Add(Retention - time.Hour)
	fresh := putEntry(t, trash, syncRoot, "fresh.txt", "fresh\n")
	crashed, err := trash.Reserve(syncRoot, "crashed.txt", ReasonDeleted)
	require.NoError(t, err)

	now = now.Add(2 * time.Hour) // old is now past retention, fresh is not
	inFlight, err := trash.Reserve(syncRoot, "in-flight.txt", ReasonDeleted)
	require.NoError(t, err)
	removed, err := trash.Purge()
	require.NoError(t, err)
	assert.Equal(t, 1, removed)

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, fresh.ID, entries[0].ID)
	assert.NotEqual(t, old.ID, entries[0].ID)
	assert.DirExists(t, filepath.Dir(inFlight.ItemPath), "a placement a worker may still be filling is kept")
	assert.NoDirExists(t, filepath.Dir(crashed.ItemPath), "an incomplete placement from a crash is removed")
}

// Validates: R-6.4.9
func TestRestore_RefusesEntriesThatLeaveTheirSyncFolder(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	entry := putEntry(t, trash, t.TempDir(), "../escape.txt", "x\n")

	_, err := trash.Restore(entry.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "outside its sync folder")

	_, err = trash.Restore("../" + entry.ID)
	require.ErrorIs(t, err, ErrEntryNotFound)
}

// A crash between moving the file into the trash and committing its entry
// left a reservation the old purge treated as debris: it deleted the file.
//
// Validates: R-6.4.9
func TestTrash_CrashBetweenMoveAndCommitIsRecoverable(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	trash := Open(t.TempDir(), func() time.Time { return now })
	syncRoot := t.TempDir()

	placement, err := trash.Reserve(syncRoot, "Docs/a.txt", ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte("kept\n"), 0o600))
	// The process dies here: no Commit.

	now = now.Add(Retention + 24*time.Hour)
	removed, err := trash.Purge()
	require.NoError(t, err)
	assert.Zero(t, removed)

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, StatePending, entries[0].State)
	assert.Equal(t, "Docs/a.txt", entries[0].Path)

	target, err := trash.Restore(entries[0].ID)
	require.NoError(t, err)
	data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "kept\n", string(data))
}

// Validates: R-6.4.9
func TestTrash_FailedCommitLeavesARecoverablePendingEntry(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	placement, err := trash.Reserve(t.TempDir(), "a.txt", ReasonReplaced)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte("old version\n"), 0o600))
	entryDir := filepath.Dir(placement.ItemPath)
	require.NoError(t, os.Chmod(entryDir, 0o500)) //nolint:gosec // a read-only directory makes the commit fail
	_, err = placement.Commit()
	require.NoError(t, os.Chmod(entryDir, 0o700)) //nolint:gosec // restores the entry directory's permissions
	require.Error(t, err)

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, StatePending, entries[0].State)
	assert.Equal(t, "a.txt", entries[0].Path)
	assert.Error(t, placement.Abandon(), "an entry holding a file is never abandoned")
}

// Validates: R-6.4.9
func TestTrash_OrphanItemIsListedAndKept(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	trash := Open(dir, nil)
	orphanDir := filepath.Join(dir, "20200101T000000.000000000Z-abcd")
	require.NoError(t, os.MkdirAll(orphanDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(orphanDir, "item"), []byte("x"), 0o600))

	removed, err := trash.Purge()
	require.NoError(t, err)
	assert.Zero(t, removed)
	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, StateOrphan, entries[0].State)

	_, err = trash.Restore(entries[0].ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), filepath.Join(orphanDir, "item"), "the user is told where the file is")
}

// forceCrossFilesystem makes every move of trash into a sync folder fail as a
// move across filesystems does, so a restore takes its copy path.
func forceCrossFilesystem(trash *Trash) {
	trash.moveIn = func(_ *synctree.Root, src, rel string) error {
		return &os.LinkError{Op: "rename", Old: src, New: rel, Err: syscall.EXDEV}
	}
}

// Validates: R-6.4.9
func TestTrash_RestoreAcrossFilesystemsCopies(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	forceCrossFilesystem(trash)
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")

	target, err := trash.Restore(entry.ID)
	require.NoError(t, err)

	data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "content\n", string(data))
	entries, err := trash.List()
	require.NoError(t, err)
	assert.Empty(t, entries)
	leftovers, err := filepath.Glob(filepath.Join(syncRoot, "Docs", restoreArtifactPrefix+"*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
}

// Validates: R-6.4.9, R-6.5.4
//
// A restore that copies across filesystems is only durable once the target's
// folder is: the rename that gives the copy its name is a directory update,
// and the trashed original is removed right after it.
func TestTrash_CrossFilesystemRestoreSyncsTheTargetFolder(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	forceCrossFilesystem(trash)
	var synced []string
	trash.syncDir = func(tree *synctree.Root, rel string) error {
		synced = append(synced, rel)
		return tree.SyncDir(rel)
	}
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")

	target, err := trash.Restore(entry.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"Docs"}, synced, "the folder the copy was renamed into is synced once")
	assert.FileExists(t, target)
}

// Validates: R-6.4.9
//
// A cross-filesystem placement that a crash interrupted leaves the entry with
// a `.copy-*` file and no `item`. That is the only copy the trash has, so the
// entry holds a file: it is listed, never purged as an empty reservation,
// never expired, and not restored as if it were the complete file.
func TestTrash_IncompleteCrossFilesystemCopyIsNeverPurged(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	trash := Open(t.TempDir(), func() time.Time { return now })
	syncRoot := t.TempDir()

	placement, err := trash.Reserve(syncRoot, "Docs/a.txt", ReasonDeleted)
	require.NoError(t, err)
	entryDir := filepath.Dir(placement.ItemPath)
	debris := filepath.Join(entryDir, ".copy-abc")
	require.NoError(t, os.WriteFile(debris, []byte("copied before the power went\n"), 0o600))
	require.Error(t, placement.Abandon(), "an entry holding a file is not removed as unused")

	now = now.Add(2 * incompletePlacementGrace)
	removed, err := trash.Purge()
	require.NoError(t, err)
	assert.Zero(t, removed, "not an empty reservation")
	assert.DirExists(t, entryDir)

	now = now.Add(Retention + 24*time.Hour)
	removed, err = trash.Purge()
	require.NoError(t, err)
	assert.Zero(t, removed, "a pending entry never expires")
	assert.FileExists(t, debris)

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, StatePending, entries[0].State)
	assert.Equal(t, "Docs/a.txt", entries[0].Path)
	assert.Equal(t, debris, entries[0].ItemPath, "the user is told where the bytes are")

	_, err = trash.Restore(entries[0].ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), debris)
	assert.FileExists(t, debris, "a refused restore leaves the copy where it was")
}

// The folders a restore creates for the file are in the user's sync folder,
// not in the trash: 0777 minus the umask, like every folder the client makes
// there, never the trash's own 0700.
//
// Validates: R-6.4.9, R-5.10.2
func TestTrash_RestoreCreatesMissingFoldersThatFollowTheUmask(t *testing.T) {
	t.Parallel()
	if !synctest.RunUnderUmask(t, synctest.GroupWritableUmask) {
		return
	}

	trash := Open(t.TempDir(), nil)
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/Notes/a.txt", "content\n")

	target, err := trash.Restore(entry.ID)
	require.NoError(t, err)
	assert.FileExists(t, target)

	for _, rel := range []string{"Docs", filepath.Join("Docs", "Notes")} {
		info, statErr := os.Stat(filepath.Join(syncRoot, rel))
		require.NoError(t, statErr)
		assert.Equal(t, os.FileMode(0o775), info.Mode().Perm(), "%s is 0777 minus the umask", rel)
	}
}

// A restore into a sync folder that is gone is refused, and the file stays
// in the trash: creating the sync folder again would make an ordinary folder
// the drive's sync then refuses as not the one its state belongs to.
//
// Validates: R-6.4.9
func TestTrash_RestoreRefusesAMissingSyncFolder(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	syncRoot := filepath.Join(t.TempDir(), "OneDrive")
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")

	_, err := trash.Restore(entry.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not exist")
	assert.NoDirExists(t, syncRoot)
	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, entry.ID, entries[0].ID)
}

// Validates: R-6.4.9
func TestRefuseSymlinkedAncestors_RefusesATargetItCannotPlaceInTheSyncFolder(t *testing.T) {
	t.Parallel()

	err := refuseSymlinkedAncestors("relative-sync-folder", filepath.Join(t.TempDir(), "a.txt"))
	require.Error(t, err, "a target the sync folder cannot be related to is not inside it")
	assert.Contains(t, err.Error(), "not inside its sync folder")
}

// Validates: R-6.4.9
func TestTrash_RestoreRefusesSymlinkedAncestor(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	syncRoot := t.TempDir()
	outside := t.TempDir()
	require.NoError(t, os.Symlink(outside, filepath.Join(syncRoot, "Docs")))
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")

	_, err := trash.Restore(entry.ID)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "symbolic link")
	assert.NoFileExists(t, filepath.Join(outside, "a.txt"))
}

// swappedRestoreFixture is a trash entry for Docs/a.txt whose target folder
// was replaced by a symlink to a folder outside the sync folder after the
// restore's own symlink check passed.
type swappedRestoreFixture struct {
	trash    *Trash
	syncRoot string
	outside  string
	itemPath string
}

func newSwappedRestoreFixture(t *testing.T) swappedRestoreFixture {
	t.Helper()

	trash := Open(t.TempDir(), nil)
	syncRoot := t.TempDir()
	outside := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")
	if err := os.Symlink(outside, filepath.Join(syncRoot, "Docs")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	return swappedRestoreFixture{
		trash:    trash,
		syncRoot: syncRoot,
		outside:  outside,
		itemPath: filepath.Join(trash.Dir(), entry.ID, itemName),
	}
}

// putBack runs the restore's move past Restore's lexical symlink check and
// past the creation of the target's folder, as a symlink swapped in after
// both leaves it, and asserts that it names no restored file.
func (f swappedRestoreFixture) putBack(t *testing.T) error {
	t.Helper()

	tree, err := synctree.Open(f.syncRoot)
	require.NoError(t, err)
	rel := filepath.Join("Docs", "a.txt")

	restored, err := f.trash.moveNoReplace(tree, f.itemPath, restoreDest{rel: rel, abs: filepath.Join(f.syncRoot, rel)})
	assert.Empty(t, restored, "a move that failed restored nothing")

	return err
}

func (f swappedRestoreFixture) requireNothingOutside(t *testing.T) {
	t.Helper()

	entries, err := os.ReadDir(f.outside)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing is restored, copied or created outside the sync folder")
	assert.FileExists(t, f.itemPath, "the trashed file stays in the trash")
}

// Validates: R-6.4.8, R-6.4.9
//
// A restore moves the file through descriptors opened inside the sync folder.
// Its target folder swapped for a symlink out of the sync folder after the
// lexical check refuses the move.
func TestTrash_RestoreThroughASwappedFolderStaysInside(t *testing.T) {
	t.Parallel()

	f := newSwappedRestoreFixture(t)
	require.Error(t, f.putBack(t))
	f.requireNothingOutside(t)
}

// Validates: R-6.4.8, R-6.4.9
//
// Across filesystems the restore copies into a temporary file created inside
// the sync folder, so a swapped target folder takes neither the temporary
// copy nor the file outside.
func TestTrash_CrossFilesystemRestoreThroughASwappedFolderStaysInside(t *testing.T) {
	t.Parallel()

	f := newSwappedRestoreFixture(t)
	forceCrossFilesystem(f.trash)
	require.Error(t, f.putBack(t))
	f.requireNothingOutside(t)
}

// Validates: R-6.4.9
//
// A cross-filesystem restore whose copy fails removes the partial copy, and
// says so when it cannot: a leftover is a restore artifact sync never sees,
// so the user is the only one who can remove it.
func TestTrash_FailedRestoreCopyIsRemovedOrReported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		removeFails   bool
		wantLeftovers int
	}{
		{name: "the partial copy is removed"},
		{name: "a partial copy that cannot be removed is reported", removeFails: true, wantLeftovers: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			trash := Open(t.TempDir(), nil)
			forceCrossFilesystem(trash)
			syncRoot := t.TempDir()
			rel := filepath.Join("Docs", "a.txt")
			entry := putEntry(t, trash, syncRoot, rel, "content\n")
			rules := []synctest.ErrnoRule{{Op: synctest.FSOpChtimes, Path: "Docs", Errno: syscall.EIO}}
			if tt.removeFails {
				rules = append(rules, synctest.ErrnoRule{Op: synctest.FSOpRemove, Path: "Docs", Errno: syscall.EACCES})
			}
			tree, err := synctree.Open(syncRoot)
			require.NoError(t, err)
			tree = tree.Interpose(synctest.NewErrnoShim(rules...).Wrap)
			itemPath := filepath.Join(trash.Dir(), entry.ID, itemName)

			restored, err := trash.putBack(tree, itemPath, restoreDest{rel: rel, abs: filepath.Join(syncRoot, rel)})
			require.ErrorIs(t, err, syscall.EIO)
			assert.Empty(t, restored, "a copy that failed restored nothing")
			if tt.removeFails {
				require.ErrorIs(t, err, syscall.EACCES)
				assert.Contains(t, err.Error(), "removing the partial restore copy")
			}
			entries, err := os.ReadDir(filepath.Join(syncRoot, "Docs"))
			require.NoError(t, err)
			assert.Len(t, entries, tt.wantLeftovers)
			assert.FileExists(t, itemPath, "the trashed file stays in the trash")
		})
	}
}

// Validates: R-6.4.9
//
// A cross-filesystem restore whose copy got its name but whose temporary name
// cannot be removed is a restore that happened: the entry leaves the trash as
// for a clean restore, Restore returns where the file is, and its error,
// matching ErrRestoreLeftover, names the copy left behind, which sync never
// sees and so only the user can delete.
func TestTrash_RestoreThatLeavesItsTemporaryCopyFinishesAndNamesIt(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	trash := Open(t.TempDir(), nil)
	forceCrossFilesystem(trash)
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")
	leftover := leaveTheTemporaryCopy(t, trash, syncRoot)

	target, err := trash.Restore(entry.ID)
	require.ErrorIs(t, err, ErrRestoreLeftover)
	require.ErrorIs(t, err, syscall.EACCES)
	assert.Contains(t, err.Error(), *leftover)
	requireRestoredAt(t, target, filepath.Join(syncRoot, "Docs", "a.txt"))
	requireRestoredEntryGone(t, trash, entry.ID)
	assert.FileExists(t, *leftover)
}

// leaveTheTemporaryCopy makes a cross-filesystem restore's final rename put
// the copy at its name and keep its temporary name, which the sync folder
// then refuses to remove, as a rename that linked the copy and could not
// unlink it does. It returns where the temporary name is, set once the
// rename ran.
func leaveTheTemporaryCopy(t *testing.T, trash *Trash, syncRoot string) *string {
	t.Helper()

	var leftover string
	trash.renameIn = func(_ *synctree.Root, srcRel, dstRel string) error {
		leftover = filepath.Join(syncRoot, srcRel)
		require.NoError(t, os.Link(leftover, filepath.Join(syncRoot, dstRel)))
		return fmt.Errorf("renaming %s to %s: %w: %w", srcRel, dstRel, synctree.ErrSourceNotRemoved, syscall.EBUSY)
	}
	removeFails := synctest.NewErrnoShim(synctest.ErrnoRule{Op: synctest.FSOpRemove, Path: "Docs", Errno: syscall.EACCES})
	trash.openTree = func(dir string) (*synctree.Root, error) {
		tree, err := synctree.Open(dir)
		require.NoError(t, err)
		return tree.Interpose(removeFails.Wrap), nil
	}

	return &leftover
}

// requireRestoredAt asserts that a restore returned want and that the
// restored file is there with its content.
func requireRestoredAt(t *testing.T, target, want string) {
	t.Helper()

	require.Equal(t, want, target)
	data, err := os.ReadFile(target) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "content\n", string(data))
}

// requireRestoredEntryGone asserts that entry id left the trash with its
// restore: it is not listed, and restoring it again finds no entry rather
// than the file it restored in the way.
func requireRestoredEntryGone(t *testing.T, trash *Trash, id string) {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	assert.Empty(t, entries, "the entry leaves the trash as for a clean restore")
	_, err = trash.Restore(id)
	require.ErrorIs(t, err, ErrEntryNotFound)
	require.NotErrorIs(t, err, ErrRestoreTargetExists)
}

// requireEntryKeptWhole asserts that entry is still in the trash as it was
// before a restore that placed its file and then stopped: listed, its
// trashed file there with its content, and its record readable.
func requireEntryKeptWhole(t *testing.T, trash *Trash, entry *Entry) {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1, "the entry stays listed")
	assert.Equal(t, entry.ID, entries[0].ID)
	data, err := os.ReadFile(filepath.Join(trash.Dir(), entry.ID, itemName))
	require.NoError(t, err)
	assert.Equal(t, "content\n", string(data), "the trash keeps its copy")
	record, ok := trash.readRecord(entry.ID)
	require.True(t, ok, "the record stays readable")
	assert.Equal(t, entry.Path, record.Path)
}

// requireRestoringAgainFindsTheFile asserts that restoring entry again finds
// the file the first restore placed in the way and leaves the entry listed.
func requireRestoringAgainFindsTheFile(t *testing.T, trash *Trash, entry *Entry) {
	t.Helper()

	_, err := trash.Restore(entry.ID)
	require.ErrorIs(t, err, ErrRestoreTargetExists)
	requireEntryKeptWhole(t, trash, entry)
}

// Validates: R-6.4.9
//
// A cross-filesystem restore whose copy got its name and kept its temporary
// name, and whose folder then could not be synced, has placed the file with
// two things wrong: the error matches ErrRestoreNotDurable and names the copy
// left behind as text, so it does not match ErrRestoreLeftover, Restore still
// returns where the file is, and the trash keeps the entry whole.
func TestTrash_RestoreThatLeavesItsTemporaryCopyAndThenFailsNamesBoth(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	trash := Open(t.TempDir(), nil)
	forceCrossFilesystem(trash)
	trash.syncDir = func(*synctree.Root, string) error { return syscall.EIO }
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")
	leftover := leaveTheTemporaryCopy(t, trash, syncRoot)

	target, err := trash.Restore(entry.ID)
	require.ErrorIs(t, err, ErrRestoreNotDurable)
	require.ErrorIs(t, err, syscall.EIO)
	require.NotErrorIs(t, err, ErrRestoreLeftover)
	assert.Contains(t, err.Error(), "its temporary copy was left too: "+*leftover)
	assert.Contains(t, err.Error(), "the trash keeps its copy as entry "+entry.ID)
	assert.NotContains(t, err.Error(), "\n")
	requireRestoredAt(t, target, filepath.Join(syncRoot, "Docs", "a.txt"))
	requireEntryKeptWhole(t, trash, &entry)
}

// Validates: R-6.4.9
//
// A restore that placed the file but could not make it durable, or could not
// remove the trashed copy, has placed the file: Restore returns where it is
// with the reason on one line, and the trash keeps the entry whole, so a
// power cut cannot leave neither copy, and the entry is never left without
// its record. Restoring it again finds the file in the way.
func TestTrash_RestoreThatFailsAfterPlacingTheFileReturnsWhereItIs(t *testing.T) {
	t.Parallel()

	failures := []struct {
		name    string
		fail    func(t *testing.T, trash *Trash, entry *Entry)
		reason  error
		message string
	}{
		{
			name: "the folder cannot be synced",
			fail: func(_ *testing.T, trash *Trash, _ *Entry) {
				forceCrossFilesystem(trash)
				trash.syncDir = func(*synctree.Root, string) error { return syscall.EIO }
			},
			reason:  ErrRestoreNotDurable,
			message: "the trash keeps its copy as entry ",
		},
		{
			name: "the trashed copy of a copy across filesystems cannot be removed",
			fail: func(t *testing.T, trash *Trash, entry *Entry) {
				t.Helper()
				forceCrossFilesystem(trash)
				lockEntryDir(t, trash, entry)
			},
			reason:  syscall.EACCES,
			message: "stays in the trash as entry ",
		},
		{
			name: "the source an exclusive copy left in the entry cannot be removed",
			fail: func(t *testing.T, trash *Trash, entry *Entry) {
				t.Helper()
				trash.moveIn = func(tree *synctree.Root, src, rel string) error {
					return tree.WithoutNoReplaceFlagOrHardLinks().MoveInNoReplace(src, rel)
				}
				lockEntryDir(t, trash, entry)
			},
			reason:  syscall.EACCES,
			message: "stays in the trash as entry ",
		},
	}
	for _, failure := range failures {
		t.Run(failure.name, func(t *testing.T) {
			t.Parallel()

			trash := Open(t.TempDir(), nil)
			syncRoot := t.TempDir()
			entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")
			failure.fail(t, trash, &entry)

			target, err := trash.Restore(entry.ID)
			require.ErrorIs(t, err, failure.reason)
			assert.Contains(t, err.Error(), failure.message+entry.ID)
			assert.NotContains(t, err.Error(), "\n")
			requireRestoredAt(t, target, filepath.Join(syncRoot, "Docs", "a.txt"))
			requireRestoringAgainFindsTheFile(t, trash, &entry)
		})
	}
}

// lockEntryDir takes the write permission away from entry's directory until
// the test ends, so nothing in it can be removed, as for a trashed file that
// really cannot be. It skips when the test runs as root, which permissions do
// not stop.
func lockEntryDir(t *testing.T, trash *Trash, entry *Entry) {
	t.Helper()

	if os.Geteuid() == 0 {
		t.Skip("root removes entries from a folder without write permission")
	}
	entryDir := filepath.Join(trash.Dir(), entry.ID)
	require.NoError(t, os.Chmod(entryDir, readOnlyDirPerm))
	t.Cleanup(func() {
		assert.NoError(t, os.Chmod(entryDir, entryDirPerm))
	})
}

// readOnlyDirPerm lets the owner list and enter a folder but not change it.
const readOnlyDirPerm = 0o500

// Validates: R-6.4.9
//
// A move into the sync folder that placed the file but left its source in the
// entry has restored it: the trash removes that source and then the entry,
// and the restore reports nothing wrong.
func TestTrash_RestoreWhoseMoveLeftItsSourceRemovesTheEntry(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	trash := Open(t.TempDir(), nil)
	trash.moveIn = func(tree *synctree.Root, src, rel string) error {
		return tree.WithoutNoReplaceFlagAndUnremovableName(itemName).MoveInNoReplace(src, rel)
	}
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "Docs/a.txt", "content\n")

	target, err := trash.Restore(entry.ID)
	require.NoError(t, err)
	requireRestoredAt(t, target, filepath.Join(syncRoot, "Docs", "a.txt"))
	requireRestoredEntryGone(t, trash, entry.ID)
	assert.NoDirExists(t, filepath.Join(trash.Dir(), entry.ID))
}

// Validates: R-6.4.8, R-6.4.9
//
// The restore makes the target's missing folders without following a
// symbolic link: with Docs swapped for one after Restore's own check, putting
// back Docs/Sub/a.txt creates nothing where the link leads, out of the sync
// folder or to another folder inside it, and the file stays in the trash.
func TestTrash_PutBackMakesNoFolderThroughASwappedFolder(t *testing.T) {
	t.Parallel()

	// linkTarget makes the folder the link leads to and returns the link's
	// value and the folder's path.
	tests := []struct {
		name       string
		linkTarget func(t *testing.T, syncRoot string) (string, string)
	}{
		{name: "a folder outside the sync folder", linkTarget: func(t *testing.T, _ string) (string, string) {
			t.Helper()
			outside := t.TempDir()
			return outside, outside
		}},
		{name: "another folder inside the sync folder", linkTarget: func(t *testing.T, syncRoot string) (string, string) {
			t.Helper()
			require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "Elsewhere"), 0o700))
			return "Elsewhere", filepath.Join(syncRoot, "Elsewhere")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			trash := Open(t.TempDir(), nil)
			syncRoot := t.TempDir()
			rel := filepath.Join("Docs", "Sub", "a.txt")
			entry := putEntry(t, trash, syncRoot, rel, "content\n")
			linkValue, linkTarget := tt.linkTarget(t, syncRoot)
			require.NoError(t, os.Symlink(linkValue, filepath.Join(syncRoot, "Docs")))
			tree, err := synctree.Open(syncRoot)
			require.NoError(t, err)
			itemPath := filepath.Join(trash.Dir(), entry.ID, itemName)

			restored, err := trash.putBack(tree, itemPath, restoreDest{rel: rel, abs: filepath.Join(syncRoot, rel)})
			require.ErrorIs(t, err, synctree.ErrUnsafePath)
			assert.Empty(t, restored, "a refused restore restored nothing")
			entries, err := os.ReadDir(linkTarget)
			require.NoError(t, err)
			assert.Empty(t, entries, "nothing is created where the link leads")
			assert.FileExists(t, itemPath, "the trashed file stays in the trash")
		})
	}
}

// restoreModTimeNanos is the sub-second part of the restored file's
// modification time, which a copy whose times were cut to the microsecond
// would not keep.
const restoreModTimeNanos = 123456789

// keptModTime is what the filesystem holding dir keeps of mtime: a control
// file there is given it with os.Chtimes, which sets nanoseconds.
func keptModTime(t *testing.T, dir string, mtime time.Time) time.Time {
	t.Helper()

	control := filepath.Join(dir, "control")
	require.NoError(t, os.WriteFile(control, nil, 0o600))
	require.NoError(t, os.Chtimes(control, mtime, mtime))
	info, err := os.Stat(control)
	require.NoError(t, err)
	require.NoError(t, os.Remove(control))

	return info.ModTime()
}

// Validates: R-6.4.9
//
// A restore across filesystems keeps the file's mode and its modification
// time to the precision the sync folder's filesystem keeps.
func TestTrash_CrossFilesystemRestoreKeepsModeAndTime(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	forceCrossFilesystem(trash)
	syncRoot := t.TempDir()
	entry := putEntry(t, trash, syncRoot, "a.txt", "content\n")
	modTime := time.Date(2025, 5, 6, 7, 8, 9, restoreModTimeNanos, time.UTC)
	want := keptModTime(t, syncRoot, modTime)
	itemPath := filepath.Join(trash.Dir(), entry.ID, "item")
	require.NoError(t, os.Chmod(itemPath, 0o640)) //nolint:gosec // a mode the restore must keep
	require.NoError(t, os.Chtimes(itemPath, modTime, modTime))

	target, err := trash.Restore(entry.ID)
	require.NoError(t, err)

	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	assert.Equal(t, want.UnixNano(), info.ModTime().UnixNano())
}

// Validates: R-6.4.9
func TestTrash_UncheckableItemIsNeverPurged(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Parallel()

	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	trash := Open(dir, func() time.Time { return now })
	placement, err := trash.Reserve(t.TempDir(), "a.txt", ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(placement.ItemPath, []byte("x"), 0o600))
	entryDir := filepath.Dir(placement.ItemPath)
	// Without search permission the item cannot be stat'ed: unknown, so kept.
	require.NoError(t, os.Chmod(entryDir, 0o600))
	t.Cleanup(func() { assert.NoError(t, os.Chmod(entryDir, 0o700)) }) //nolint:gosec // restores permissions

	now = now.Add(2 * incompletePlacementGrace)
	removed, err := trash.Purge()
	require.NoError(t, os.Chmod(entryDir, 0o700)) //nolint:gosec // restores permissions before checking
	require.NoError(t, err, "an entry kept as unknown is not a purge failure")
	assert.Zero(t, removed)
	assert.FileExists(t, placement.ItemPath, "an entry whose file cannot be checked is never purged as empty")
}

// An entry whose folder is gone, removed after its reservation, holds no
// file: abandoning it succeeds instead of refusing as if it held its item.
//
// Validates: R-6.4.9
func TestPlacement_AbandonOfAnEntryWhoseFolderIsGoneSucceeds(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	placement, err := trash.Reserve(t.TempDir(), "a.txt", ReasonDeleted)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(placement.ItemPath)))

	require.NoError(t, placement.Abandon())
	assert.NoDirExists(t, filepath.Dir(placement.ItemPath))
}
