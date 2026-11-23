package localtrash

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// orphanEntryName is the directory name of an entry a crash left with a file
// and no record.
const orphanEntryName = "20260901T000000.000000000Z-0badf00d"

// childTrashFixture is a drive's trash and the trash of one of its shortcut
// children, on one clock the test moves.
type childTrashFixture struct {
	now      *time.Time
	parent   *Trash
	child    *Trash
	syncRoot string
}

func newChildTrashFixture(t *testing.T) childTrashFixture {
	t.Helper()

	dataDir := t.TempDir()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	parentDir := DirForStatePath(filepath.Join(dataDir, "state_personal_user@example.com.db"))
	childDir := ChildDir(parentDir, filepath.Join(dataDir, "state_mount_child.db"))

	return childTrashFixture{
		now:      &now,
		parent:   Open(parentDir, clock),
		child:    Open(childDir, clock),
		syncRoot: filepath.Join(t.TempDir(), "Shared"),
	}
}

// fillWithEveryKindOfEntry puts a committed entry, a pending one that holds
// its file, an orphan and a reservation whose file never arrived in trash,
// and returns the entries List shows, by ID.
func (f childTrashFixture) fillWithEveryKindOfEntry(t *testing.T, trash *Trash) map[string]Entry {
	t.Helper()

	putEntry(t, trash, f.syncRoot, "committed.txt", "committed\n")
	pending, err := trash.Reserve(f.syncRoot, "pending.txt", ReasonReleased)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(pending.ItemPath, []byte("pending\n"), 0o600))
	_, err = trash.Reserve(f.syncRoot, "never-arrived.txt", ReasonDeleted)
	require.NoError(t, err)
	orphanDir := filepath.Join(trash.Dir(), orphanEntryName)
	require.NoError(t, os.MkdirAll(orphanDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(orphanDir, itemName), []byte("orphan\n"), 0o600))

	return entriesByID(t, trash)
}

func entriesByID(t *testing.T, trash *Trash) map[string]Entry {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	byID := make(map[string]Entry, len(entries))
	for i := range entries {
		entry := entries[i]
		entry.ItemPath = ""
		byID[entry.ID] = entry
	}
	return byID
}

func entryDirNames(t *testing.T, dir string) []string {
	t.Helper()

	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	names := make([]string, 0, len(dirEntries))
	for _, dirEntry := range dirEntries {
		names = append(names, dirEntry.Name())
	}
	sort.Strings(names)
	return names
}

// A shortcut child's trash sits under its parent drive's name, outside the
// parent's own entries: the parent finds it, and nothing the parent does to
// its own entries touches it.
//
// Validates: R-6.4.12
func TestChildDir_IsReachedFromTheParentAndOutsideItsEntries(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)
	assert.Equal(t,
		filepath.Join(filepath.Dir(f.parent.Dir()), childrenDirName, filepath.Base(f.parent.Dir()), "state_mount_child"),
		f.child.Dir())
	assert.Empty(t, ChildDir("", "state_mount_child.db"))
	assert.Empty(t, ChildDir(f.parent.Dir(), ""))

	children, err := f.parent.Children()
	require.NoError(t, err)
	assert.Empty(t, children, "a drive with no child trash has none")

	childEntry := putEntry(t, f.child, f.syncRoot, "a.txt", "a\n")
	putEntry(t, f.parent, f.syncRoot, "own.txt", "own\n")
	children, err = f.parent.Children()
	require.NoError(t, err)
	require.Len(t, children, 1)
	assert.Equal(t, f.child.Dir(), children[0].Dir())

	removed, err := f.parent.Empty()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the parent's own trash holds only its own entry")
	assert.Contains(t, entriesByID(t, f.child), childEntry.ID, "emptying the parent's own trash leaves the child's")
}

// The hand-over moves every entry as it is: a committed entry, a pending one,
// an orphan and a reservation whose file never arrived each keep their ID,
// time, reason and state, and the child's trash directory is gone. The
// parent's purge then expires the committed one after the retention, as its
// own, and keeps what was never committed.
//
// Validates: R-6.4.12
func TestHandOverTo_MovesEveryEntryAsItIs(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)
	before := f.fillWithEveryKindOfEntry(t, f.child)
	require.Len(t, before, 3, "the committed, pending and orphan entries are listed")
	dirsBefore := entryDirNames(t, f.child.Dir())
	require.Len(t, dirsBefore, 4, "the reservation without a file is an entry directory too")

	moved, err := f.child.HandOverTo(f.parent)
	require.NoError(t, err)

	assert.Equal(t, 4, moved)
	assert.NoDirExists(t, f.child.Dir())
	assert.Equal(t, before, entriesByID(t, f.parent), "every entry is in the parent's trash as it was")
	assert.Equal(t, dirsBefore, entryDirNames(t, f.parent.Dir()))
	children, err := f.parent.Children()
	require.NoError(t, err)
	assert.Empty(t, children, "no trash is left that only the child reached")

	*f.now = f.now.Add(Retention + time.Hour)
	removed, err := f.parent.Purge()
	require.NoError(t, err)
	assert.Equal(t, 1, removed, "the committed entry expires with the parent's own")
	after := entriesByID(t, f.parent)
	assert.Len(t, after, 2, "the pending entry and the orphan are never purged")
	for id := range after {
		assert.NotEqual(t, StateCommitted, after[id].State)
	}
}

// A hand-over stopped part way leaves every entry in exactly one trash, and
// the child's trash directory stays while it holds any; run again, it moves
// the rest.
//
// Validates: R-6.4.12
func TestHandOverTo_StoppedPartWayAndRunAgainHoldsEachEntryOnce(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)
	before := f.fillWithEveryKindOfEntry(t, f.child)
	dirsBefore := entryDirNames(t, f.child.Dir())

	for stopAfter := range len(dirsBefore) {
		t.Run(fmt.Sprintf("stopped at entry %d", stopAfter), func(t *testing.T) {
			calls := 0
			stopped := errors.New("interrupted")
			f.child.renameEntry = func(src, dst string) error {
				call := calls
				calls++
				if call == stopAfter {
					return stopped
				}
				return os.Rename(src, dst)
			}
			moved, err := f.child.HandOverTo(f.parent)
			require.ErrorIs(t, err, stopped)
			assert.Equal(t, len(dirsBefore)-1, moved, "only the stopped entry stays")
			assert.DirExists(t, f.child.Dir(), "a trash that still holds an entry stays")

			inParent := entryDirNames(t, f.parent.Dir())
			inChild := entryDirNames(t, f.child.Dir())
			both := append(append([]string(nil), inParent...), inChild...)
			sort.Strings(both)
			assert.Equal(t, dirsBefore, both, "every entry is in exactly one trash")

			f.child.renameEntry = os.Rename
			_, err = f.child.HandOverTo(f.parent)
			require.NoError(t, err)
			assert.NoDirExists(t, f.child.Dir())
			assert.Equal(t, dirsBefore, entryDirNames(t, f.parent.Dir()))
			assert.Equal(t, before, entriesByID(t, f.parent))

			// Put everything back in the child for the next stop.
			for _, name := range dirsBefore {
				require.NoError(t, os.MkdirAll(f.child.Dir(), 0o700))
				require.NoError(t, os.Rename(filepath.Join(f.parent.Dir(), name), filepath.Join(f.child.Dir(), name)))
			}
		})
	}
}

// An entry whose ID the parent's trash already holds is not moved onto it:
// both stay, and so does the child's trash directory, and the hand-over says
// so.
//
// Validates: R-6.4.12
func TestHandOverTo_KeepsAnEntryWhoseIDIsTaken(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)
	entry := putEntry(t, f.child, f.syncRoot, "a.txt", "a\n")
	require.NoError(t, os.MkdirAll(filepath.Join(f.parent.Dir(), entry.ID), 0o700))

	moved, err := f.child.HandOverTo(f.parent)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "already holds an entry with that ID")
	assert.Zero(t, moved)
	assert.Contains(t, entriesByID(t, f.child), entry.ID, "the child's entry is kept whole")
}

// A hand-over whose receiving trash cannot be created moves nothing and
// leaves the child's trash as it was.
//
// Validates: R-6.4.12
func TestHandOverTo_ThatCannotReachTheParentLeavesTheChildAsItWas(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)
	before := f.fillWithEveryKindOfEntry(t, f.child)
	require.NoError(t, os.MkdirAll(filepath.Dir(f.parent.Dir()), 0o700))
	require.NoError(t, os.WriteFile(f.parent.Dir(), []byte("not a folder"), 0o600))

	moved, err := f.child.HandOverTo(f.parent)

	require.Error(t, err)
	assert.Zero(t, moved)
	assert.Equal(t, before, entriesByID(t, f.child))
}

// A child with no trash has nothing to hand over.
//
// Validates: R-6.4.12
func TestHandOverTo_WithoutAChildTrashDoesNothing(t *testing.T) {
	t.Parallel()

	f := newChildTrashFixture(t)

	moved, err := f.child.HandOverTo(f.parent)

	require.NoError(t, err)
	assert.Zero(t, moved)
	assert.NoDirExists(t, f.parent.Dir())
}
