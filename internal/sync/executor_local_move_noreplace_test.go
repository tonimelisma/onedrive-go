package sync

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// localMoveEntry is what a local move of these tests moves: a file a.txt to
// b.txt, or a folder a, holding child.txt, to b.
type localMoveEntry struct {
	name     string
	src, dst string
	itemType ItemType
}

func localMoveEntries() []localMoveEntry {
	return []localMoveEntry{
		{name: "a file", src: "a.txt", dst: "b.txt", itemType: ItemTypeFile},
		{name: "a folder", src: "a", dst: "b", itemType: ItemTypeFolder},
	}
}

// create makes the entry at its source in syncRoot.
func (m localMoveEntry) create(t *testing.T, syncRoot string) {
	t.Helper()

	if m.itemType == ItemTypeFile {
		writeExecTestFile(t, syncRoot, m.src, "moving\n")
		return
	}
	writeExecTestFile(t, syncRoot, filepath.Join(m.src, "child.txt"), "moving\n")
}

// requireAtSource asserts that the entry is still at its source, as made.
func (m localMoveEntry) requireAtSource(t *testing.T, syncRoot string) {
	t.Helper()

	if m.itemType == ItemTypeFile {
		requireExecFileContent(t, syncRoot, m.src, "moving\n")
		return
	}
	requireExecFileContent(t, syncRoot, filepath.Join(m.src, "child.txt"), "moving\n")
}

// requireTrashHoldsOnce asserts that trash holds exactly one entry, for path,
// whose file holds content.
func requireTrashHoldsOnce(t *testing.T, trash *localtrash.Trash, path, content string) localtrash.Entry {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1, "the displaced file is in the trash exactly once")
	assert.Equal(t, path, entries[0].Path)
	data, err := localpath.ReadFile(entries[0].ItemPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	return entries[0]
}

// Validates: R-6.4.8, R-2.17.9
//
// A local move never replaces what it did not expect at its destination: an
// entry that takes the destination after the precondition found it free is
// kept, the moved entry stays at its source, and the move ends as a stale
// precondition, which replans it. The precondition misses the entry through
// a sync tree whose lstat of the destination answers ENOENT, the state a
// check sees when the name is taken right after it. An empty folder is what
// a plain rename of a folder would replace. A folder is refused by the
// system's no-replace flag, and where the system has none, by the check of
// the destination on its folder's descriptor that the rename makes. A move
// to another spelling of the same name is refused the same way on a volume
// that tells the spellings apart (and skipped where it cannot hold both).
func TestExecutor_LocalMove_DestinationTakenAfterTheCheckIsKept(t *testing.T) {
	t.Parallel()

	entries := append(localMoveEntries(),
		localMoveEntry{name: "a file to another spelling", src: "Report.txt", dst: "report.txt", itemType: ItemTypeFile},
		localMoveEntry{name: "a folder to another spelling", src: "Docs", dst: "docs", itemType: ItemTypeFolder},
	)
	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()
			if isLocalSpellingOnlyMove(entry.src, entry.dst) {
				skipUnlessNamesCoexist(t, entry.src, entry.dst)
			}

			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
			e := newExecution(cfg, emptyBaseline())
			entry.create(t, syncRoot)
			if entry.itemType == ItemTypeFile {
				writeExecTestFile(t, syncRoot, entry.dst, "appeared\n")
			} else {
				require.NoError(t, os.Mkdir(filepath.Join(syncRoot, entry.dst), 0o700))
			}
			shim := synctest.NewErrnoShim(synctest.ErrnoRule{Op: synctest.FSOpStat, Path: entry.dst, Errno: syscall.ENOENT})
			e.syncTree = e.syncTree.Interpose(shim.Wrap)

			o := e.ExecuteMove(t.Context(), &Action{
				Type: ActionLocalMove, Path: entry.dst, OldPath: entry.src,
				View: &pathView{Path: entry.dst},
			})

			requireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, errActionPreconditionChanged)
			require.ErrorIs(t, o.Error, synctree.ErrTargetExists)
			entry.requireAtSource(t, syncRoot)
			if entry.itemType == ItemTypeFile {
				requireExecFileContent(t, syncRoot, entry.dst, "appeared\n")
			} else {
				dstEntries, err := os.ReadDir(filepath.Join(syncRoot, entry.dst))
				require.NoError(t, err)
				assert.Empty(t, dstEntries, "nothing was moved onto the folder")
			}
		})
	}
}

// Validates: R-6.4.8, R-2.17.9
//
// A move that replaces its destination (OneDrive moved an item over a file
// it deleted) takes the verified file out into the trash first and then
// renames without replacing: a file that takes the freed name between the
// two steps is kept, the moved entry stays at its source, the move ends as a
// stale precondition, and the displaced file stays in the trash, once. The
// crash-point hook between the two steps is where the file appears.
func TestExecutor_LocalMove_ReplacingMoveKeepsAFileThatTakesTheFreedName(t *testing.T) {
	t.Parallel()

	for _, entry := range localMoveEntries() {
		t.Run(entry.name, func(t *testing.T) {
			t.Parallel()

			cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
			trash := localtrash.Open(filepath.Join(t.TempDir(), "trash"), nil)
			cfg.SetTrash(trash)
			cfg.crashAt = func(point string) error {
				if point == crashPointLocalMoveDestinationTrashed {
					writeExecTestFile(t, syncRoot, entry.dst, "saved meanwhile\n")
				}
				return nil
			}
			e := newExecution(cfg, emptyBaseline())
			entry.create(t, syncRoot)
			writeExecTestFile(t, syncRoot, entry.dst, "bravo\n")
			planned := plannedDestination(t, e, syncRoot, entry.dst)

			o := e.ExecuteMove(t.Context(), &Action{
				Type: ActionLocalMove, Path: entry.dst, OldPath: entry.src, ReplaceLocalTarget: true,
				View: &pathView{
					Path: entry.dst, Local: planned,
					Baseline: &BaselineEntry{Path: entry.src, ItemType: entry.itemType},
				},
			})

			requireOutcomeFailure(t, &o)
			require.ErrorIs(t, o.Error, errActionPreconditionChanged)
			require.ErrorIs(t, o.Error, synctree.ErrTargetExists)
			requireExecFileContent(t, syncRoot, entry.dst, "saved meanwhile\n")
			entry.requireAtSource(t, syncRoot)
			requireTrashHoldsOnce(t, trash, entry.dst, "bravo\n")
		})
	}
}

// Validates: R-6.4.8
//
// A local move whose rename placed the file at its destination but could not
// remove the old name (a system without the no-replace flag, where the file
// is linked and then unlinked) happened: it succeeds and records the move
// from the old name to the new one, the destination holds the file, the old
// name is left as a second name of that same file, and a warning names the
// move.
func TestExecutor_LocalMove_OldNameThatCannotBeRemovedIsAMoveThatHappened(t *testing.T) {
	t.Parallel()
	synctest.SkipWithoutHardLinks(t)

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	capture, logger := synctest.NewLogCapture()
	e.logger = logger
	e.syncTree = e.syncTree.WithoutNoReplaceFlagAndUnremovableName("a.txt")
	writeExecTestFile(t, syncRoot, "a.txt", "moving\n")
	moving := entryIdentity(t, filepath.Join(syncRoot, "a.txt"))

	o := e.ExecuteMove(t.Context(), &Action{
		Type: ActionLocalMove, Path: "b.txt", OldPath: "a.txt", View: &pathView{Path: "b.txt"},
	})

	requireOutcomeSuccess(t, &o)
	assert.Equal(t, ActionLocalMove, o.Action)
	assert.Equal(t, "b.txt", o.Path)
	assert.Equal(t, "a.txt", o.OldPath)
	requireExecFileContent(t, syncRoot, "b.txt", "moving\n")
	assert.Equal(t, moving, entryIdentity(t, filepath.Join(syncRoot, "b.txt")))
	assert.Equal(t, moving, entryIdentity(t, filepath.Join(syncRoot, "a.txt")), "the old name is a second name of the moved file")
	warnings := capture.AtLeast(slog.LevelWarn)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0].Message, "Moved b.txt, but could not remove the name it was moved from")
}

// Validates: R-6.3.6, R-6.4.8
//
// The hash that checks the file a replacing local move is about to take out
// of its way stops when the move's context ends instead of reading the whole
// file: with the context ended, the check fails with the context's error,
// even for a file that is still the version the plan saw.
func TestExecutor_LocalMove_DestinationHashStopsWhenTheContextEnds(t *testing.T) {
	t.Parallel()

	cfg, syncRoot := newTestExecutorConfig(t, &executorMockItemClient{}, &executorMockDownloader{}, &executorMockUploader{})
	e := newExecution(cfg, emptyBaseline())
	writeExecTestFile(t, syncRoot, "b.txt", "bravo\n")
	planned := plannedDestination(t, e, syncRoot, "b.txt")
	file, err := localpath.OpenRegular(filepath.Join(syncRoot, "b.txt"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, file.Close()) })
	info, err := file.Stat()
	require.NoError(t, err)
	ended, cancel := context.WithCancel(t.Context())
	cancel()

	verify := e.existingTargetVerifier(ended, &Action{Type: ActionLocalMove, Path: "b.txt"}, planned, localMoveDestinationNoun)

	require.ErrorIs(t, verify(file, info), context.Canceled)
}

// entryIdentity is the device and inode of the entry at path, not followed.
func entryIdentity(t *testing.T, path string) synctree.FileIdentity {
	t.Helper()

	info, err := os.Lstat(path)
	require.NoError(t, err)
	identity, ok := synctree.IdentityFromFileInfo(info)
	require.True(t, ok)

	return synctree.FileIdentity{Device: identity.Device, Inode: identity.Inode}
}

// plannedDestination is the local state a plan saw of the file at rel.
func plannedDestination(t *testing.T, e *executor, syncRoot, rel string) *localState {
	t.Helper()

	hash, err := e.hashFunc(t.Context(), filepath.Join(syncRoot, rel))
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(syncRoot, rel))
	require.NoError(t, err)

	return &localState{ItemType: ItemTypeFile, Hash: hash, Size: info.Size()}
}
