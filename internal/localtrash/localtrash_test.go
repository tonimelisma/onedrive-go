package localtrash

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, os.Chmod(entryDir, 0o500))
	_, err = placement.Commit()
	require.NoError(t, os.Chmod(entryDir, 0o700))
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

// Validates: R-6.4.9
func TestTrash_RestoreAcrossFilesystemsCopies(t *testing.T) {
	t.Parallel()

	trash := Open(t.TempDir(), nil)
	trash.renameNoReplace = func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EXDEV}
	}
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
	leftovers, err := filepath.Glob(filepath.Join(syncRoot, "Docs", copyTempGlob))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
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
