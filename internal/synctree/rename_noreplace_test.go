package synctree

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// noReplaceMode is one system a no-replace rename can meet: one with the
// no-replace flag, one that refuses the flag but has hard links (link then
// unlink), and one that has neither (inside the root a check and then a plain
// rename, from outside it an exclusive copy).
type noReplaceMode struct {
	name      string
	configure func(root *Root)
}

func refuseExclusiveRename(root *Root) {
	root.ops.renameExclusive = func(*os.File, string, *os.File, string) (bool, error) {
		return true, syscall.EINVAL
	}
}

func refuseExclusiveRenameAndLinks(root *Root) {
	*root = *root.WithoutNoReplaceFlagOrHardLinks()
}

// skipWithoutHardLinks skips a test that needs a rename to link its entry and
// then unlink the source, on a temporary filesystem without hard links: there
// the rename takes its check and plain rename, and the case under test never
// happens.
func skipWithoutHardLinks(t *testing.T) {
	t.Helper()

	probe := filepath.Join(t.TempDir(), "probe")
	require.NoError(t, os.WriteFile(probe, nil, 0o600))
	err := os.Link(probe, probe+"-link")
	if localpath.IsLinkUnsupported(err) {
		t.Skipf("the temporary filesystem has no hard links, which the link and unlink under test need: %v", err)
	}
	require.NoError(t, err)
}

func noReplaceModes() []noReplaceMode {
	return []noReplaceMode{
		{name: "the no-replace flag", configure: func(*Root) {}},
		{name: "flag refused, link then unlink", configure: refuseExclusiveRename},
		{name: "flag refused, no hard links", configure: refuseExclusiveRenameAndLinks},
	}
}

// noReplaceSource is an entry a no-replace rename moves: a file or a folder.
type noReplaceSource struct {
	name   string
	create func(t *testing.T, path string)
	// check asserts that path holds the entry create made.
	check func(t *testing.T, path string)
}

func noReplaceSources() []noReplaceSource {
	return []noReplaceSource{
		{
			name: "a file",
			create: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("moving"), 0o600))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				requireFileContent(t, path, "moving")
			},
		},
		{
			name: "a folder",
			create: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Mkdir(path, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(path, "a.jpg"), []byte("moving"), 0o600))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				requireFileContent(t, filepath.Join(path, "a.jpg"), "moving")
			},
		},
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

func openTestRoot(t *testing.T) (*Root, string) {
	t.Helper()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	return root, dir
}

func requireFileContent(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, want, string(got))
}

// Validates: R-6.4.8, R-2.17.10, R-6.4.9
//
// A no-replace rename never replaces what is at its destination: a file and
// an empty folder there are each refused with ErrTargetExists, the error the
// conflict copy and the trash restore branch on, and both names are left as
// they were. It holds for a file and a folder, on a system with the flag, on
// one with only hard links, and on one with neither; a free destination then
// takes the move.
func TestRoot_RenameNoReplace_NeverReplacesAnExistingEntry(t *testing.T) {
	t.Parallel()

	destinations := []struct {
		name   string
		create func(t *testing.T, path string)
		check  func(t *testing.T, path string)
	}{
		{
			name: "a file",
			create: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.WriteFile(path, []byte("current"), 0o600))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				requireFileContent(t, path, "current")
			},
		},
		{
			name: "an empty folder",
			create: func(t *testing.T, path string) {
				t.Helper()
				require.NoError(t, os.Mkdir(path, 0o700))
			},
			check: func(t *testing.T, path string) {
				t.Helper()
				entries, err := os.ReadDir(path)
				require.NoError(t, err)
				assert.Empty(t, entries, "nothing was moved into or onto the folder")
			},
		},
	}

	for _, mode := range noReplaceModes() {
		for _, src := range noReplaceSources() {
			for _, dest := range destinations {
				t.Run(mode.name+"/"+src.name+"/onto "+dest.name, func(t *testing.T) {
					t.Parallel()

					root, dir := openTestRoot(t)
					mode.configure(root)
					src.create(t, filepath.Join(dir, "src"))
					dest.create(t, filepath.Join(dir, "dst"))

					err := root.RenameNoReplace("src", "dst")
					require.ErrorIs(t, err, ErrTargetExists)
					src.check(t, filepath.Join(dir, "src"))
					dest.check(t, filepath.Join(dir, "dst"))

					require.NoError(t, os.MkdirAll(filepath.Join(dir, "sub"), 0o700))
					require.NoError(t, root.RenameNoReplace("src", filepath.Join("sub", "free")))
					assert.NoFileExists(t, filepath.Join(dir, "src"))
					assert.NoDirExists(t, filepath.Join(dir, "src"))
					src.check(t, filepath.Join(dir, "sub", "free"))
				})
			}
		}
	}
}

// Validates: R-6.4.8, R-2.17.10
//
// A rename inside the sync folder moves the entry itself, a file and a folder
// alike, on every system: the entry at the new name is the one that was at the
// old name, and no second entry is written. Where the system offers neither
// the no-replace flag nor a hard link, the rename checks that the destination
// is free and then renames; it never copies.
func TestRoot_RenameNoReplace_MovesTheEntryItselfOnEverySystem(t *testing.T) {
	t.Parallel()

	for _, mode := range noReplaceModes() {
		for _, src := range noReplaceSources() {
			t.Run(mode.name+"/"+src.name, func(t *testing.T) {
				t.Parallel()

				root, dir := openTestRoot(t)
				mode.configure(root)
				src.create(t, filepath.Join(dir, "src"))
				before, err := os.Lstat(filepath.Join(dir, "src"))
				require.NoError(t, err)

				require.NoError(t, root.RenameNoReplace("src", "dst"))
				after, err := os.Lstat(filepath.Join(dir, "dst"))
				require.NoError(t, err)
				assert.True(t, os.SameFile(before, after), "the entry is renamed, not copied")
				assert.Equal(t, []string{"dst"}, entryNames(t, dir), "no second entry is written")
				src.check(t, filepath.Join(dir, "dst"))
			})
		}
	}
}

// Validates: R-6.4.9
//
// A move into the sync folder on a system with neither the no-replace flag
// nor hard links copies, and only a regular file can be copied: a folder
// fails with the rename's own errno, also matching localpath.ErrNotRegularFile,
// and stays where it was.
func TestRoot_MoveInNoReplace_CopiesOnlyARegularFile(t *testing.T) {
	t.Parallel()

	root, dir := openTestRoot(t)
	refuseExclusiveRenameAndLinks(root)
	src := filepath.Join(t.TempDir(), "photos")
	require.NoError(t, os.Mkdir(src, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(src, "a.jpg"), []byte("a"), 0o600))

	err := root.MoveInNoReplace(src, "photos")
	require.ErrorIs(t, err, localpath.ErrNotRegularFile)
	require.ErrorIs(t, err, syscall.EINVAL, "the rename's own errno is reported")
	requireFileContent(t, filepath.Join(src, "a.jpg"), "a")
	assert.NoDirExists(t, filepath.Join(dir, "photos"))
}

// subSecondNanos is the sub-second part of subSecondModTime, which a copy
// whose times were cut to the microsecond would not keep.
const subSecondNanos = 123456789

func subSecondModTime() time.Time {
	return time.Date(2025, 5, 6, 7, 8, 9, subSecondNanos, time.UTC)
}

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
// A move into the sync folder on a system without hard links copies, and the
// exclusive copy keeps the file's mode and its modification time to the
// precision the filesystem keeps.
func TestRoot_MoveInNoReplace_CopyKeepsModeAndTime(t *testing.T) {
	t.Parallel()

	root, dir := openTestRoot(t)
	refuseExclusiveRenameAndLinks(root)
	src := filepath.Join(t.TempDir(), "src.txt")
	require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o600))
	require.NoError(t, os.Chmod(src, 0o640)) //nolint:gosec // a mode the copy must keep
	modTime := subSecondModTime()
	require.NoError(t, os.Chtimes(src, modTime, modTime))
	want := keptModTime(t, dir, modTime)

	require.NoError(t, root.MoveInNoReplace(src, "dst.txt"))
	assert.NoFileExists(t, src)
	requireFileContent(t, filepath.Join(dir, "dst.txt"), "from trash")
	info, err := os.Stat(filepath.Join(dir, "dst.txt"))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	assert.Equal(t, want.UnixNano(), info.ModTime().UnixNano(), "the copy keeps the modification time")
}

// Validates: R-6.4.9
//
// The exclusive copy removes the source only once the folder holding the copy
// is synced: a power cut before the folder's entry is durable must not leave
// neither name. A folder sync that fails removes the copy and keeps the
// source; one the filesystem does not support is no failure.
func TestRoot_MoveInNoReplace_CopyKeepsTheSourceUntilTheFolderIsSynced(t *testing.T) {
	t.Parallel()

	t.Run("sync fails", func(t *testing.T) {
		t.Parallel()

		root, dir := openTestRoot(t)
		refuseExclusiveRenameAndLinks(root)
		root.ops.syncDirFile = func(*os.File) error { return syscall.EIO }
		src := filepath.Join(t.TempDir(), "src.txt")
		require.NoError(t, os.WriteFile(src, []byte("moving"), 0o600))

		err := root.MoveInNoReplace(src, "dst.txt")
		require.ErrorIs(t, err, syscall.EIO)
		require.NotErrorIs(t, err, ErrSourceNotRemoved)
		requireFileContent(t, src, "moving")
		assert.NoFileExists(t, filepath.Join(dir, "dst.txt"), "the copy whose name is not durable is removed")
	})

	t.Run("sync unsupported", func(t *testing.T) {
		t.Parallel()

		root, dir := openTestRoot(t)
		refuseExclusiveRenameAndLinks(root)
		root.ops.syncDirFile = func(*os.File) error { return syscall.EINVAL }
		src := filepath.Join(t.TempDir(), "src.txt")
		require.NoError(t, os.WriteFile(src, []byte("moving"), 0o600))

		require.NoError(t, root.MoveInNoReplace(src, "dst.txt"))
		assert.NoFileExists(t, src)
		requireFileContent(t, filepath.Join(dir, "dst.txt"), "moving")
	})
}

// Validates: R-6.4.8, R-6.4.9
//
// On a system with neither the no-replace flag nor hard links, the plain
// rename that follows the check finds the destination taken when an entry was
// put there after the check: ENOTEMPTY and EEXIST, and EISDIR and ENOTDIR for
// an entry of the other kind, are ErrTargetExists, as the flag's refusal is,
// and the source stays where it was.
func TestRoot_RenameNoReplace_DestinationTakenAfterTheCheckIsReportedAsExisting(t *testing.T) {
	t.Parallel()

	for _, errno := range []syscall.Errno{syscall.ENOTEMPTY, syscall.EEXIST, syscall.EISDIR, syscall.ENOTDIR} {
		t.Run(errno.Error(), func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			refuseExclusiveRenameAndLinks(root)
			root.ops.renameAt = func(*os.File, string, *os.File, string) error { return errno }
			require.NoError(t, os.Mkdir(filepath.Join(dir, "photos"), 0o700))

			err := root.RenameNoReplace("photos", "photos.copy")
			require.ErrorIs(t, err, ErrTargetExists)
			assert.DirExists(t, filepath.Join(dir, "photos"))
		})
	}
}

// Validates: R-6.4.9
//
// A fallback that put the file in place but could not remove the source
// reports it after the fact: the move itself succeeded.
func TestRoot_RenameNoReplace_SourceThatCannotBeRemovedIsReportedAfterSuccess(t *testing.T) {
	t.Parallel()
	skipWithoutHardLinks(t)

	root, dir := openTestRoot(t)
	refuseExclusiveRename(root)
	root.ops.unlink = func(*os.File, string) error { return syscall.EBUSY }
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("x"), 0o600))

	err := root.RenameNoReplace("src.txt", "dst.txt")
	require.ErrorIs(t, err, ErrSourceNotRemoved)
	require.ErrorIs(t, err, syscall.EBUSY)
	requireFileContent(t, filepath.Join(dir, "dst.txt"), "x")
}

// sourceNameChange is something another program does to the source name of
// a no-replace rename right after the entry is linked at its destination.
type sourceNameChange struct {
	name   string
	change func(t *testing.T, srcPath string)
	// check asserts what srcPath holds after the rename.
	check func(t *testing.T, srcPath string)
}

func sourceNameChanges() []sourceNameChange {
	return []sourceNameChange{
		{
			name: "a save renames a new file onto it",
			change: func(t *testing.T, srcPath string) {
				t.Helper()
				require.NoError(t, os.WriteFile(srcPath+".save", []byte("saved"), 0o600))
				require.NoError(t, os.Rename(srcPath+".save", srcPath))
			},
			check: func(t *testing.T, srcPath string) {
				t.Helper()
				requireFileContent(t, srcPath, "saved")
			},
		},
		{
			name: "it is removed",
			change: func(t *testing.T, srcPath string) {
				t.Helper()
				require.NoError(t, os.Remove(srcPath))
			},
			check: func(t *testing.T, srcPath string) {
				t.Helper()
				assert.NoFileExists(t, srcPath)
			},
		},
	}
}

// Validates: R-6.4.8, R-6.4.9
//
// Where the flag is refused and the entry is linked at its destination and
// then unlinked from its source, a source name that no longer holds the
// linked entry is not unlinked: a program that saved over it by renaming a
// new file onto it keeps its save, and a source already gone needs nothing.
// Either is the move done, as if the change had come a moment later, for a
// rename inside the root and for a move into it alike.
func TestRoot_NoReplace_ASourceNameChangedAfterTheLinkIsNotUnlinked(t *testing.T) {
	t.Parallel()
	skipWithoutHardLinks(t)

	moves := []struct {
		name         string
		sourceFolder func(t *testing.T, rootDir string) string
		move         func(root *Root, srcPath string) error
	}{
		{
			name:         "inside the root",
			sourceFolder: func(_ *testing.T, rootDir string) string { return rootDir },
			move:         func(root *Root, _ string) error { return root.RenameNoReplace("src.txt", "dst.txt") },
		},
		{
			name: "into the root",
			sourceFolder: func(t *testing.T, _ string) string {
				t.Helper()
				return t.TempDir()
			},
			move: func(root *Root, srcPath string) error { return root.MoveInNoReplace(srcPath, "dst.txt") },
		},
	}
	for _, change := range sourceNameChanges() {
		for _, move := range moves {
			t.Run(change.name+", "+move.name, func(t *testing.T) {
				t.Parallel()

				root, dir := openTestRoot(t)
				refuseExclusiveRename(root)
				srcPath := filepath.Join(move.sourceFolder(t, dir), "src.txt")
				require.NoError(t, os.WriteFile(srcPath, []byte("original"), 0o600))
				link := root.ops.link
				root.ops.link = func(fromDir *os.File, from string, toDir *os.File, to string) error {
					err := link(fromDir, from, toDir, to)
					if err == nil {
						change.change(t, srcPath)
					}
					return err
				}

				require.NoError(t, move.move(root, srcPath))
				requireFileContent(t, filepath.Join(dir, "dst.txt"), "original")
				change.check(t, srcPath)
			})
		}
	}
}

// Validates: R-6.4.8, R-6.4.9
//
// On a system with neither the flag nor hard links, the exclusive copy into
// the root removes its source only while the source name still holds the
// file it copied: a program that saved over the source by renaming a new
// file onto it after the copy keeps its save, and a source already gone
// needs nothing. Either is the move done.
func TestRoot_MoveInNoReplace_ASourceNameChangedAfterTheCopyIsNotUnlinked(t *testing.T) {
	t.Parallel()

	for _, change := range sourceNameChanges() {
		t.Run(change.name, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			refuseExclusiveRenameAndLinks(root)
			srcPath := filepath.Join(t.TempDir(), "src.txt")
			require.NoError(t, os.WriteFile(srcPath, []byte("original"), 0o600))
			syncDirFile := root.ops.syncDirFile
			root.ops.syncDirFile = func(dir *os.File) error {
				err := syncDirFile(dir)
				change.change(t, srcPath)
				return err
			}

			require.NoError(t, root.MoveInNoReplace(srcPath, "dst.txt"))
			requireFileContent(t, filepath.Join(dir, "dst.txt"), "original")
			change.check(t, srcPath)
		})
	}
}

// Validates: R-6.4.8, R-6.4.9
//
// A fallback that placed the entry unlinks the source only once a stat of
// each name shows the source still holds that entry. When a stat fails for
// another reason than absence (of the destination or of the source after a
// link, of the source after an exclusive copy), the source is kept, and the
// move reports it left (ErrSourceNotRemoved) with the stat's reason.
func TestRoot_NoReplace_ASourceThatCannotBeCheckedIsNotUnlinked(t *testing.T) {
	t.Parallel()

	checks := []struct {
		name        string
		configure   func(root *Root)
		failing     string
		fromOutside bool
	}{
		{name: "a link whose destination cannot be checked", configure: refuseExclusiveRename, failing: "dst.txt"},
		{name: "a link whose source cannot be checked", configure: refuseExclusiveRename, failing: "src.txt"},
		{
			name: "an exclusive copy whose source cannot be checked", configure: refuseExclusiveRenameAndLinks,
			failing: "src.txt", fromOutside: true,
		},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			if !check.fromOutside {
				skipWithoutHardLinks(t)
			}

			root, dir := openTestRoot(t)
			check.configure(root)
			lstatEntry := root.ops.lstatEntry
			root.ops.lstatEntry = func(folder *os.File, name string) (unix.Stat_t, error) {
				if name == check.failing {
					return unix.Stat_t{}, syscall.EIO
				}
				return lstatEntry(folder, name)
			}
			srcPath := filepath.Join(dir, "src.txt")
			move := func() error { return root.RenameNoReplace("src.txt", "dst.txt") }
			if check.fromOutside {
				srcPath = filepath.Join(t.TempDir(), "src.txt")
				move = func() error { return root.MoveInNoReplace(srcPath, "dst.txt") }
			}
			require.NoError(t, os.WriteFile(srcPath, []byte("moving"), 0o600))

			err := move()
			require.ErrorIs(t, err, ErrSourceNotRemoved)
			require.ErrorIs(t, err, syscall.EIO)
			requireFileContent(t, srcPath, "moving")
			requireFileContent(t, filepath.Join(dir, "dst.txt"), "moving")
		})
	}
}

// Validates: R-6.4.9
//
// A copy that failed leaves no partial file behind, or says it could not
// remove it: a leftover makes the next attempt find the destination taken.
// A partial copy already closed, or already gone, is no further failure.
func TestDiscardPartialCopy_ReportsAPartialCopyItCannotRemove(t *testing.T) {
	t.Parallel()

	errCopy := errors.New("injected copy failure")
	dir := t.TempDir()
	parent, err := os.Open(dir) //nolint:gosec // test opens its own temp dir
	require.NoError(t, err)
	defer closeDirectory(parent)

	stuck, err := os.Create(filepath.Join(dir, "stuck")) //nolint:gosec // test creates a file in its own temp dir
	require.NoError(t, err)
	err = discardPartialCopy(stuck, parent, "stuck", errCopy, func(*os.File, string) error { return syscall.EBUSY })
	require.ErrorIs(t, err, errCopy)
	require.ErrorIs(t, err, syscall.EBUSY)
	assert.Contains(t, err.Error(), "removing partial copy stuck")

	removed, err := os.Create(filepath.Join(dir, "removed")) //nolint:gosec // test creates a file in its own temp dir
	require.NoError(t, err)
	require.NoError(t, removed.Close())
	err = discardPartialCopy(removed, parent, "removed", errCopy, unlinkDescriptor)
	require.ErrorIs(t, err, errCopy)
	assert.Equal(t, errCopy.Error(), err.Error(), "a closed, removable partial copy adds nothing")
	assert.NoFileExists(t, filepath.Join(dir, "removed"))
}

// Validates: R-6.4.8, R-6.4.9
//
// A file moves into the sync folder from outside it under the same
// no-replace rule, on every system, the exclusive copy of one without hard
// links included: a file and an empty folder at the destination are refused.
// A source inside the sync folder is refused: inside it, both names go
// through the root (RenameNoReplace).
func TestRoot_MoveInNoReplace_NeverReplacesAndTakesOnlyOutsideSources(t *testing.T) {
	t.Parallel()

	for _, mode := range noReplaceModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			mode.configure(root)
			src := filepath.Join(t.TempDir(), "item")
			require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "taken.txt"), []byte("current"), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(dir, "folder"), 0o700))

			require.ErrorIs(t, root.MoveInNoReplace(src, "taken.txt"), ErrTargetExists)
			requireFileContent(t, filepath.Join(dir, "taken.txt"), "current")
			require.ErrorIs(t, root.MoveInNoReplace(src, "folder"), ErrTargetExists)
			assert.Empty(t, entryNames(t, filepath.Join(dir, "folder")), "nothing was moved onto the folder")
			requireFileContent(t, src, "from trash")

			require.NoError(t, root.MoveInNoReplace(src, filepath.Join("folder", "restored.txt")))
			assert.NoFileExists(t, src)
			requireFileContent(t, filepath.Join(dir, "folder", "restored.txt"), "from trash")

			require.ErrorIs(t, root.MoveInNoReplace(filepath.Join(dir, "taken.txt"), "moved.txt"), ErrUnsafePath)
			require.ErrorIs(t, root.MoveInNoReplace("relative.txt", "moved.txt"), ErrUnsafePath)
			require.ErrorIs(t, root.RenameNoReplace("taken.txt", "."), ErrUnsafePath)
			requireFileContent(t, filepath.Join(dir, "taken.txt"), "current")
		})
	}
}

// Validates: R-6.4.8
//
// Whether the file to move in lies outside the sync folder is decided by the
// folder its path resolves to, not by the path's text: a trash path that
// reaches the sync folder through a symbolic link, to the folder itself or to
// a folder below it, names a file of the tree. Moving it "in" would move it
// to another name of the tree, so it is refused and both names stay.
func TestRoot_MoveInNoReplace_RefusesASourceThatResolvesIntoTheSyncFolder(t *testing.T) {
	t.Parallel()

	for _, target := range []string{".", "docs"} {
		t.Run("a link to "+target, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			require.NoError(t, os.Mkdir(filepath.Join(dir, "docs"), 0o700))
			inTree := filepath.Join(dir, target, "a.txt")
			require.NoError(t, os.WriteFile(inTree, []byte("in the tree"), 0o600))
			outside := t.TempDir()
			require.NoError(t, os.Symlink(filepath.Join(dir, target), filepath.Join(outside, "trash")))

			err := root.MoveInNoReplace(filepath.Join(outside, "trash", "a.txt"), "moved.txt")

			require.ErrorIs(t, err, ErrUnsafePath)
			requireFileContent(t, inTree, "in the tree")
			assert.NoFileExists(t, filepath.Join(dir, "moved.txt"))
		})
	}
}

// Validates: R-6.4.8, R-6.4.9
//
// A move into the sync folder from another filesystem fails with an error
// matching syscall.EXDEV, whether the no-replace rename or the link of its
// fallback says so, and leaves both names as they were: the restore branches
// on EXDEV to copy instead.
func TestRoot_MoveInNoReplace_AcrossFilesystemsLeavesBothNames(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		configure func(root *Root)
	}{
		{name: "the rename says so", configure: func(root *Root) {
			root.ops.renameExclusive = func(*os.File, string, *os.File, string) (bool, error) {
				return false, syscall.EXDEV
			}
		}},
		{name: "flag refused, the link says so", configure: func(root *Root) {
			refuseExclusiveRename(root)
			root.ops.link = func(*os.File, string, *os.File, string) error { return syscall.EXDEV }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			tt.configure(root)
			src := filepath.Join(t.TempDir(), "item")
			require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o600))

			err := root.MoveInNoReplace(src, "restored.txt")
			require.ErrorIs(t, err, syscall.EXDEV)
			requireFileContent(t, src, "from trash")
			assert.NoFileExists(t, filepath.Join(dir, "restored.txt"))
		})
	}
}

// Validates: R-6.4.8, R-6.4.9
//
// A destination whose final name is a symlink to a file outside the sync
// folder is an entry like any other: the move fails with ErrTargetExists and
// the link is never followed, so the file outside keeps its content, for the
// system's flag and both fallbacks.
func TestRoot_NoReplace_ADestinationThatIsASymlinkIsNeverFollowed(t *testing.T) {
	t.Parallel()

	for _, mode := range noReplaceModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			mode.configure(root)
			outsideFile := filepath.Join(t.TempDir(), "outside.txt")
			require.NoError(t, os.WriteFile(outsideFile, []byte("OUTSIDE"), 0o600))
			dst := filepath.Join(dir, "dst")
			require.NoError(t, os.Symlink(outsideFile, dst))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("moving"), 0o600))
			trashed := filepath.Join(t.TempDir(), "item")
			require.NoError(t, os.WriteFile(trashed, []byte("from trash"), 0o600))

			require.ErrorIs(t, root.RenameNoReplace("src.txt", "dst"), ErrTargetExists)
			require.ErrorIs(t, root.MoveInNoReplace(trashed, "dst"), ErrTargetExists)

			requireFileContent(t, outsideFile, "OUTSIDE")
			requireFileContent(t, filepath.Join(dir, "src.txt"), "moving")
			requireFileContent(t, trashed, "from trash")
			info, err := os.Lstat(dst)
			require.NoError(t, err)
			assert.NotZero(t, info.Mode()&os.ModeSymlink, "the link is left as it was")
		})
	}
}

// Validates: R-6.4.8
//
// A parent folder replaced by a FIFO fails the move as a parent that is not a
// folder, without blocking: the parent is opened as a folder and never waits
// for a writer.
func TestRoot_NoReplace_AFIFOInPlaceOfAParentFolderFailsWithoutBlocking(t *testing.T) {
	t.Parallel()

	root, dir := openTestRoot(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "src.txt"), []byte("moving"), 0o600))
	trashed := filepath.Join(t.TempDir(), "item")
	require.NoError(t, os.WriteFile(trashed, []byte("from trash"), 0o600))
	beneath := filepath.Join("fifo", "x")

	require.ErrorIs(t, root.RenameNoReplace(beneath, "moved.txt"), ErrUnsafePath)
	require.ErrorIs(t, root.RenameNoReplace("src.txt", beneath), ErrUnsafePath)
	require.ErrorIs(t, root.MoveInNoReplace(trashed, beneath), ErrUnsafePath)
	requireFileContent(t, filepath.Join(dir, "src.txt"), "moving")
	requireFileContent(t, trashed, "from trash")
}

// Validates: R-6.4.8
//
// The rename's own failure takes the two-answer rule: ENOTDIR reads as
// absence only when a lstat through the root finds the source missing too.
func TestRoot_RenameNoReplace_FailureReadsAsAbsenceOnlyWhenTheRootedLstatAgrees(t *testing.T) {
	t.Parallel()

	root, dir := openTestRoot(t)
	root.ops.renameExclusive = func(*os.File, string, *os.File, string) (bool, error) {
		return false, syscall.ENOTDIR
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "present.txt"), []byte("x"), 0o600))

	err := root.RenameNoReplace("missing.txt", "dst.txt")
	require.ErrorIs(t, err, os.ErrNotExist, "a source the rooted lstat finds missing is absent")

	err = root.RenameNoReplace("present.txt", "dst.txt")
	require.ErrorIs(t, err, syscall.ENOTDIR)
	require.NotErrorIs(t, err, os.ErrNotExist, "a source the rooted lstat finds is not absent")
}

// lstatAnswers answers a lstat of name with errno whatever is there: with
// ENOENT, the state a check sees when the name is taken right after it.
type lstatAnswers struct {
	Handle
	name  string
	errno syscall.Errno
}

func (h lstatAnswers) Lstat(name string) (os.FileInfo, error) {
	if name == h.name {
		return nil, &os.PathError{Op: "lstat", Path: name, Err: h.errno}
	}

	return h.Handle.Lstat(name) //nolint:wrapcheck // passes the wrapped handle's answer through
}

// temporarySiblingFixture is a file Docs to be renamed to docs through the
// temporary sibling .tmp-docs.
type temporarySiblingFixture struct {
	root *Root
	dir  string
}

const (
	siblingSource = "Docs"
	siblingDest   = "docs"
	siblingTemp   = ".tmp-docs"
)

func newTemporarySiblingFixture(t *testing.T) temporarySiblingFixture {
	t.Helper()

	root, dir := openTestRoot(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, siblingSource), []byte("moving"), 0o600))

	return temporarySiblingFixture{root: root, dir: dir}
}

// skipWhereTheTemporaryFolderFoldsCase skips a test that needs siblingSource
// and siblingDest as two entries at once, on a temporary folder that folds
// case (the default on macOS), where they are one name: it creates the one
// name and looks for the other.
func skipWhereTheTemporaryFolderFoldsCase(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, siblingSource), nil, 0o600))
	_, err := os.Lstat(filepath.Join(dir, siblingDest))
	if err == nil {
		t.Skipf("the temporary folder folds case, so %s and %s are one name there", siblingSource, siblingDest)
	}
	require.ErrorIs(t, err, os.ErrNotExist)
}

// namesAppearBeforeTheSecondStep makes a file appear at each of names just
// before the temporary sibling is renamed onto the destination. On a volume
// that folds case the destination and the source are one name, so a file
// that takes the destination takes the source's name too: passing both
// models that volume on one that does not fold case.
func (f temporarySiblingFixture) namesAppearBeforeTheSecondStep(t *testing.T, names ...string) {
	t.Helper()

	appear := func(from, to string) {
		if from != siblingTemp || to != siblingDest {
			return
		}
		for _, name := range names {
			require.NoError(t, os.WriteFile(filepath.Join(f.dir, name), []byte("appeared"), 0o600))
		}
	}
	renameAt := f.root.ops.renameAt
	f.root.ops.renameAt = func(fromDir *os.File, from string, toDir *os.File, to string) error {
		appear(from, to)
		return renameAt(fromDir, from, toDir, to)
	}
	renameExclusive := f.root.ops.renameExclusive
	f.root.ops.renameExclusive = func(fromDir *os.File, from string, toDir *os.File, to string) (bool, error) {
		appear(from, to)
		return renameExclusive(fromDir, from, toDir, to)
	}
}

// Validates: R-2.13.2, R-6.4.8
//
// The first step never replaces a temporary name that is taken, even when a
// check of it would say it is free: it moves on to the next candidate.
func TestRoot_RenameWithTemporarySibling_TakenTemporaryNameIsNeverReplaced(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(f.dir, siblingTemp), []byte("taken"), 0o600))
	root := f.root.Interpose(func(h Handle) Handle { return lstatAnswers{Handle: h, name: siblingTemp, errno: syscall.ENOENT} })

	require.NoError(t, root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3))
	requireFileContent(t, filepath.Join(f.dir, siblingTemp), "taken")
	requireFileContent(t, filepath.Join(f.dir, siblingDest), "moving")
	assert.NoFileExists(t, filepath.Join(f.dir, siblingTemp+"-1"))
}

// Validates: R-2.13.2, R-6.4.8
//
// A destination that appears between the two steps is never replaced: the
// rename reports it as existing.
func TestRoot_RenameWithTemporarySibling_DestinationThatAppearsIsNeverReplaced(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	f.namesAppearBeforeTheSecondStep(t, siblingDest)

	err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, ErrTargetExists)
	requireFileContent(t, filepath.Join(f.dir, siblingDest), "appeared")
}

// Validates: R-2.13.2, R-6.4.8
//
// When the second step fails and the source's name is free, as it is where
// the two names differ, the entry is put back under its old name and no
// temporary name is left behind.
func TestRoot_RenameWithTemporarySibling_SourceIsPutBackWhenItsNameIsFree(t *testing.T) {
	t.Parallel()
	skipWhereTheTemporaryFolderFoldsCase(t)

	f := newTemporarySiblingFixture(t)
	f.namesAppearBeforeTheSecondStep(t, siblingDest)

	require.Error(t, f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3))
	requireFileContent(t, filepath.Join(f.dir, siblingSource), "moving")
	assert.NoFileExists(t, filepath.Join(f.dir, siblingTemp))
}

// Validates: R-2.13.2, R-6.4.8
//
// On a volume that folds case the destination and the source are one name,
// so when the second step finds the destination taken, the put-back finds
// the same name taken: the entry is kept under the temporary name, and the
// error matches ErrTargetExists and says where the entry is kept.
func TestRoot_RenameWithTemporarySibling_EntryThatCannotBePutBackIsKeptAsTheTemporaryName(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	f.namesAppearBeforeTheSecondStep(t, siblingDest, siblingSource)

	err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, ErrTargetExists)
	require.NotErrorIs(t, err, ErrSourceNotRemoved)
	assert.Contains(t, err.Error(), "kept as "+filepath.Join(f.dir, siblingTemp))
	requireFileContent(t, filepath.Join(f.dir, siblingTemp), "moving")
	requireFileContent(t, filepath.Join(f.dir, siblingDest), "appeared")
	requireFileContent(t, filepath.Join(f.dir, siblingSource), "appeared")
}

// Validates: R-2.13.2, R-6.4.8
//
// A step whose fallback put the entry at its destination but could not
// remove the name it came from is a step taken: the rename goes on, the entry
// ends at the new spelling, the old spelling is not put back, and the error
// matches ErrSourceNotRemoved and names each name left behind, on one line.
func TestRoot_RenameWithTemporarySibling_ANameThatCannotBeRemovedIsLeftAndReported(t *testing.T) {
	t.Parallel()
	skipWithoutHardLinks(t)

	for _, leftovers := range [][]string{{siblingSource}, {siblingTemp}, {siblingSource, siblingTemp}} {
		t.Run(strings.Join(leftovers, " and "), func(t *testing.T) {
			t.Parallel()
			if slices.Contains(leftovers, siblingSource) {
				skipWhereTheTemporaryFolderFoldsCase(t)
			}

			f := newTemporarySiblingFixture(t)
			refuseExclusiveRename(f.root)
			unlink := f.root.ops.unlink
			f.root.ops.unlink = func(dir *os.File, name string) error {
				if slices.Contains(leftovers, name) {
					return syscall.EBUSY
				}
				return unlink(dir, name)
			}

			err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
			require.ErrorIs(t, err, ErrSourceNotRemoved)
			require.NotErrorIs(t, err, ErrTargetExists)
			assert.NotContains(t, err.Error(), "\n", "every name left behind is reported on one line")
			for _, leftover := range leftovers {
				assert.Contains(t, err.Error(), "renaming "+leftover+" to ")
				requireFileContent(t, filepath.Join(f.dir, leftover), "moving")
			}
			requireFileContent(t, filepath.Join(f.dir, siblingDest), "moving")
			assert.ElementsMatch(t, append([]string{siblingDest}, leftovers...), entryNames(t, f.dir))
		})
	}
}

// Validates: R-2.13.2, R-6.4.8
//
// A first step that placed the entry but could not remove the source's name
// leaves that name taken, as on a volume that folds case a destination that
// appeared would take it: when the second step then finds the destination
// taken, the put-back finds the source's name taken too. The entry is kept
// under the temporary name, and the error names the source's name as left.
func TestRoot_RenameWithTemporarySibling_SourceNameLeftByTheFirstStepIsNamedWhenTheSecondFails(t *testing.T) {
	t.Parallel()
	skipWithoutHardLinks(t)
	skipWhereTheTemporaryFolderFoldsCase(t)

	f := newTemporarySiblingFixture(t)
	*f.root = *f.root.WithoutNoReplaceFlagAndUnremovableName(siblingSource)
	f.namesAppearBeforeTheSecondStep(t, siblingDest)

	err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, ErrTargetExists)
	require.NotErrorIs(t, err, ErrSourceNotRemoved)
	assert.Contains(t, err.Error(), siblingSource+" was left too")
	assert.Contains(t, err.Error(), "kept as "+filepath.Join(f.dir, siblingTemp))
	requireFileContent(t, filepath.Join(f.dir, siblingTemp), "moving")
	requireFileContent(t, filepath.Join(f.dir, siblingSource), "moving")
	requireFileContent(t, filepath.Join(f.dir, siblingDest), "appeared")
}

// Validates: R-2.13.2, R-6.4.8
//
// A put-back that placed the entry under its old name but could not remove
// the temporary name says both: the entry is back, and the temporary name is
// left.
func TestRoot_RenameWithTemporarySibling_PutBackThatLeavesTheTemporaryNameSaysSo(t *testing.T) {
	t.Parallel()
	skipWithoutHardLinks(t)
	skipWhereTheTemporaryFolderFoldsCase(t)

	f := newTemporarySiblingFixture(t)
	*f.root = *f.root.WithoutNoReplaceFlagAndUnremovableName(siblingTemp)
	f.namesAppearBeforeTheSecondStep(t, siblingDest)

	err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, ErrTargetExists)
	require.NotErrorIs(t, err, ErrSourceNotRemoved)
	assert.Contains(t, err.Error(), "it is back as "+siblingSource+", and "+siblingTemp+" was left")
	requireFileContent(t, filepath.Join(f.dir, siblingSource), "moving")
	requireFileContent(t, filepath.Join(f.dir, siblingTemp), "moving")
	requireFileContent(t, filepath.Join(f.dir, siblingDest), "appeared")
}

// Validates: R-2.13.2, R-6.4.8
//
// An entry that another program removes from the temporary name before the
// second step is found by neither the second step nor the put-back: the
// error says it is under neither name, never that it is kept under the
// temporary one.
func TestRoot_RenameWithTemporarySibling_EntryGoneFromTheTemporaryNameIsUnderNeitherName(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	renameExclusive := f.root.ops.renameExclusive
	f.root.ops.renameExclusive = func(fromDir *os.File, from string, toDir *os.File, to string) (bool, error) {
		if from == siblingTemp && to == siblingDest {
			require.NoError(t, os.Remove(filepath.Join(f.dir, siblingTemp)))
		}
		return renameExclusive(fromDir, from, toDir, to)
	}

	err := f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "it was found under neither name")
	assert.NotContains(t, err.Error(), "kept as")
	assert.Equal(t, 1, strings.Count(err.Error(), siblingSource), "the old name is named once: %v", err)
	assert.Empty(t, entryNames(t, f.dir))
}

// Validates: R-2.13.2, R-6.4.8
//
// A put-back that failed, followed by a lstat of the temporary name that
// fails for another reason than absence, is no evidence that the entry is
// gone: the error says it is kept under the temporary name, with the lstat's
// reason, and never that it is under neither name.
func TestRoot_RenameWithTemporarySibling_TemporaryNameThatCannotBeCheckedIsReportedAsKept(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	f.namesAppearBeforeTheSecondStep(t, siblingDest, siblingSource)
	root := f.root.Interpose(func(h Handle) Handle { return lstatAnswers{Handle: h, name: siblingTemp, errno: syscall.EIO} })

	err := root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3)
	require.ErrorIs(t, err, ErrTargetExists)
	assert.Contains(t, err.Error(), "kept as "+filepath.Join(f.dir, siblingTemp))
	assert.Contains(t, err.Error(), syscall.EIO.Error())
	assert.NotContains(t, err.Error(), "neither name")
	requireFileContent(t, filepath.Join(f.dir, siblingTemp), "moving")
}

// Validates: R-2.13.2, R-6.4.8
//
// On a system with neither the no-replace flag nor hard links a spelling-only
// rename of a file renames the file itself in both steps: it keeps its inode,
// and no copy is written.
func TestRoot_RenameWithTemporarySibling_WithoutTheFlagOrLinksRenamesTheFileItself(t *testing.T) {
	t.Parallel()

	f := newTemporarySiblingFixture(t)
	refuseExclusiveRenameAndLinks(f.root)
	before, err := os.Lstat(filepath.Join(f.dir, siblingSource))
	require.NoError(t, err)

	require.NoError(t, f.root.RenameWithTemporarySibling(siblingSource, siblingDest, siblingTemp, 3))
	after, err := os.Lstat(filepath.Join(f.dir, siblingDest))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after), "the file is renamed, not copied")
	assert.Equal(t, []string{siblingDest}, entryNames(t, f.dir))
}

// Validates: R-2.13.2
//
// A folder on a system without the no-replace flag, which a hard link cannot
// move, still takes both steps.
func TestRoot_RenameWithTemporarySibling_FolderWithoutTheFlagStillMoves(t *testing.T) {
	t.Parallel()

	root, dir := openTestRoot(t)
	refuseExclusiveRename(root)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "Photos"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Photos", "a.jpg"), []byte("a"), 0o600))

	require.NoError(t, root.RenameWithTemporarySibling("Photos", "photos", ".tmp-photos", 3))
	requireFileContent(t, filepath.Join(dir, "photos", "a.jpg"), "a")
	assert.NoDirExists(t, filepath.Join(dir, ".tmp-photos"))
}
