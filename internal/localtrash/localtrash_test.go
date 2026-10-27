package localtrash

import (
	"os"
	"path/filepath"
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

	assert.Equal(t, filepath.Join("/data", "trash", "state_personal_user@example.com"),
		DirForStatePath(filepath.Join("/data", "state_personal_user@example.com.db")))
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
