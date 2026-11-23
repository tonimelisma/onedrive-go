package synctree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

const (
	opDetach   = "detach"
	opRemove   = "remove"
	opPreserve = "preserve"
)

func newDetachTree(t *testing.T) (*Root, string) {
	t.Helper()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs"), 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "docs", "a.txt"), []byte("synced\n"), 0o600))
	return root, dir
}

// Validates: R-6.4.9
func TestDetachRegularFile_MovesTheVerifiedFileOut(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	dest := filepath.Join(t.TempDir(), "item")

	require.NoError(t, root.DetachRegularFile("docs/a.txt", nil, dest))

	assert.NoFileExists(t, filepath.Join(dir, "docs", "a.txt"))
	data, err := os.ReadFile(dest) //nolint:gosec // the test reads a file it created
	require.NoError(t, err)
	assert.Equal(t, "synced\n", string(data))
}

// Validates: R-6.2.4
func TestDetachRegularFile_VerifyRefusalLeavesTheFile(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	refused := errors.New("changed")

	err := root.DetachRegularFile("docs/a.txt", func(*os.File, os.FileInfo) error { return refused }, filepath.Join(t.TempDir(), "item"))

	require.ErrorIs(t, err, refused)
	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
}

// An editor's atomic save lands while the old file is being verified: the
// path now names a new inode. Only the verified inode may leave the tree, so
// nothing moves and the new content stays (D-010).
//
// Validates: R-6.2.4
func TestDetachRegularFile_FileReplacedDuringVerifyIsKept(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opRemove, opPreserve} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			target := filepath.Join(dir, "docs", "a.txt")
			swap := func(*os.File, os.FileInfo) error {
				replacement := filepath.Join(dir, "docs", "a.txt.new")
				require.NoError(t, os.WriteFile(replacement, []byte("user's new edit\n"), 0o600))
				require.NoError(t, os.Rename(replacement, target))
				return nil
			}
			dest := filepath.Join(t.TempDir(), "item")

			var err error
			switch operation {
			case opDetach:
				err = root.DetachRegularFile("docs/a.txt", swap, dest)
			case opRemove:
				err = root.RemoveVerifiedRegularFile("docs/a.txt", swap)
			default:
				err = root.PreserveRegularFile("docs/a.txt", swap, dest)
			}

			require.ErrorIs(t, err, ErrDetachRaced)
			data, readErr := os.ReadFile(target) //nolint:gosec // the test reads a file it created
			require.NoError(t, readErr)
			assert.Equal(t, "user's new edit\n", string(data))
			assert.NoFileExists(t, dest)
			entries, readDirErr := os.ReadDir(filepath.Join(dir, "docs"))
			require.NoError(t, readDirErr)
			assert.Len(t, entries, 1, "no set-aside file is left behind")
		})
	}
}

// Validates: R-6.4.9
func TestPreserveRegularFile_KeepsTheFileInPlace(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	dest := filepath.Join(t.TempDir(), "item")

	require.NoError(t, root.PreserveRegularFile("docs/a.txt", nil, dest))

	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
	data, err := os.ReadFile(dest) //nolint:gosec // the test reads a file it created
	require.NoError(t, err)
	assert.Equal(t, "synced\n", string(data))
}

// The version a download replaces is kept by a hard link where one can be
// made, and by a copy through the verified descriptor where it cannot: across
// filesystems, or where the filesystem refuses the link (no hard links, a
// FUSE filesystem without a link operation, too many links already). Either
// way the file stays where it is, unchanged. Any other refusal fails the
// preserve and copies nothing.
//
// Validates: R-6.4.9
func TestPreserveRegularFile_CopiesWhenTheLinkCannotBeMade(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		errno  unix.Errno
		copies bool
	}{
		{name: "EXDEV", errno: unix.EXDEV, copies: true},
		{name: "EPERM", errno: unix.EPERM, copies: true},
		{name: "EMLINK", errno: unix.EMLINK, copies: true},
		{name: "ENOTSUP", errno: unix.ENOTSUP, copies: true},
		{name: "EOPNOTSUPP", errno: unix.EOPNOTSUPP, copies: true},
		{name: "ENOSYS", errno: unix.ENOSYS, copies: true},
		{name: "EIO", errno: unix.EIO},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			source := filepath.Join(dir, "docs", "a.txt")
			before, err := os.Lstat(source)
			require.NoError(t, err)
			root.ops.link = func(*os.File, string, *os.File, string) error { return tt.errno }
			dest := filepath.Join(t.TempDir(), "item")

			err = root.PreserveRegularFile("docs/a.txt", nil, dest)

			requireFileContent(t, source, "synced\n")
			after, statErr := os.Lstat(source)
			require.NoError(t, statErr)
			assert.True(t, os.SameFile(before, after), "the file stays where it was")
			if !tt.copies {
				require.ErrorIs(t, err, tt.errno)
				assert.NoFileExists(t, dest)
				return
			}
			require.NoError(t, err)
			requireFileContent(t, dest, "synced\n")
			kept, keptErr := os.Lstat(dest)
			require.NoError(t, keptErr)
			assert.False(t, os.SameFile(before, kept), "the kept version is a copy")
		})
	}
}

// A destination that fails is told apart from a file that is gone: when the
// folder the file was to land in is missing, before the destination is
// checked or after the check and before the move, the take-out fails with
// ErrDestinationFailed, which does not match os.ErrNotExist, and the file
// stays where it was, with no set-aside sibling left.
//
// Validates: R-6.4.9
func TestDetachRegularFile_ADestinationThatFailsIsNotAFileThatIsGone(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name                 string
		removedBeforeTheMove bool
	}{
		{name: "missing before the check"},
		{name: "removed before the move", removedBeforeTheMove: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			folder := filepath.Join(t.TempDir(), "entry")
			var verify VerifyFunc
			if tt.removedBeforeTheMove {
				require.NoError(t, os.Mkdir(folder, 0o700))
				verify = func(*os.File, os.FileInfo) error {
					require.NoError(t, os.Remove(folder))
					return nil
				}
			}

			err := root.DetachRegularFile("docs/a.txt", verify, filepath.Join(folder, "item"))

			require.ErrorIs(t, err, ErrDestinationFailed)
			require.NotErrorIs(t, err, os.ErrNotExist)
			requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
			assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")))
		})
	}
}

// detachOrPreserve runs operation (opDetach or opPreserve) on docs/a.txt.
func detachOrPreserve(root *Root, operation string, verify VerifyFunc, dest string) error {
	if operation == opDetach {
		return root.DetachRegularFile("docs/a.txt", verify, dest)
	}

	return root.PreserveRegularFile("docs/a.txt", verify, dest)
}

// takeOutCase is one way a file leaves the tree for a place outside it, or is
// kept there: taken out or preserved, on the tree's filesystem or across two.
type takeOutCase struct {
	name              string
	operation         string
	acrossFilesystems bool
}

func takeOutCases() []takeOutCase {
	return []takeOutCase{
		{name: "detach", operation: opDetach},
		{name: "detach by copy", operation: opDetach, acrossFilesystems: true},
		{name: "preserve", operation: opPreserve},
		{name: "preserve by copy", operation: opPreserve, acrossFilesystems: true},
	}
}

// tree returns a new detach tree (newDetachTree) whose take-outs cross
// filesystems when the case does (acrossFilesystems).
func (c takeOutCase) tree(t *testing.T) (*Root, string) {
	t.Helper()

	root, dir := newDetachTree(t)
	if c.acrossFilesystems {
		acrossFilesystems(root)
	}

	return root, dir
}

// acrossFilesystems makes every rename and link of root between two different
// folders fail with EXDEV, as when the place a file is taken or copied out to
// is on another filesystem than the tree: a detach's move out and a
// preserve's link then copy. A rename or link within one folder, the set-aside
// and the copy's rename to its final name among them, is made as before.
func acrossFilesystems(root *Root) {
	renameExclusive, link := root.ops.renameExclusive, root.ops.link
	root.ops.renameExclusive = func(fromDir *os.File, from string, toDir *os.File, to string) (bool, error) {
		if fromDir != toDir {
			return false, unix.EXDEV
		}
		return renameExclusive(fromDir, from, toDir, to)
	}
	root.ops.link = func(fromDir *os.File, from string, toDir *os.File, to string) error {
		if fromDir != toDir {
			return unix.EXDEV
		}
		return link(fromDir, from, toDir, to)
	}
}

// swapFolderForLink is a verify function that removes folder, the empty
// folder a destination lies in, and puts a symbolic link to target in its
// place: the trash entry swapped for a link after the destination was checked
// and before the file moves.
func swapFolderForLink(t *testing.T, folder, target string) VerifyFunc {
	t.Helper()

	return func(*os.File, os.FileInfo) error {
		require.NoError(t, os.Remove(folder))
		require.NoError(t, os.Symlink(target, folder))
		return nil
	}
}

// Validates: R-6.4.8
//
// The folder a file is taken or copied out to is opened once, and every step
// is made on that descriptor: the trash entry swapped for a link into the
// sync folder after the check sends nothing through the link, by rename,
// link or copy. The take-out or copy-out fails as a destination that failed,
// and the file is at its name with its content.
func TestDetachRegularFile_ADestinationFolderSwappedForALinkIsNotFollowed(t *testing.T) {
	t.Parallel()

	for _, tc := range takeOutCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, dir := tc.tree(t)
			folder := filepath.Join(t.TempDir(), "entry")
			require.NoError(t, os.Mkdir(folder, 0o700))

			err := detachOrPreserve(root, tc.operation, swapFolderForLink(t, folder, filepath.Join(dir, "docs")), filepath.Join(folder, "item"))

			require.ErrorIs(t, err, ErrDestinationFailed)
			require.NotErrorIs(t, err, os.ErrNotExist)
			requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
			assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")),
				"nothing reaches the sync folder through the link, and no set-aside file is left")
		})
	}
}

// Validates: R-6.4.8
//
// The destination's folder is opened without following a symbolic link in
// its own name: a destination whose folder is a link, whether it leads out of
// the sync folder or into it, is a destination that failed, and nothing
// moves.
func TestDetachRegularFile_ADestinationFolderThatIsALinkIsNotFollowed(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opPreserve} {
		for _, leadsIntoTheTree := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/into the sync folder %t", operation, leadsIntoTheTree), func(t *testing.T) {
				t.Parallel()

				root, dir := newDetachTree(t)
				target := t.TempDir()
				if leadsIntoTheTree {
					target = filepath.Join(dir, "docs")
				}
				folder := filepath.Join(t.TempDir(), "entry")
				require.NoError(t, os.Symlink(target, folder))

				err := detachOrPreserve(root, operation, nil, filepath.Join(folder, "item"))

				require.ErrorIs(t, err, ErrDestinationFailed)
				requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
				assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")))
				if !leadsIntoTheTree {
					assert.Empty(t, entryNames(t, target), "nothing is taken out through the link")
				}
			})
		}
	}
}

// Validates: R-6.4.9
//
// A take-out or copy-out never replaces what it finds at the destination's
// name: the rename, the copy's rename and the link all refuse a name that is
// taken, the entry there is left as it was, and the file is at its own name
// with its content.
func TestDetachRegularFile_ANameTakenAtTheDestinationIsNotReplaced(t *testing.T) {
	t.Parallel()

	for _, tc := range takeOutCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, dir := tc.tree(t)
			dest := filepath.Join(t.TempDir(), "item")
			require.NoError(t, os.WriteFile(dest, []byte("already here\n"), 0o600))

			err := detachOrPreserve(root, tc.operation, nil, dest)

			require.ErrorIs(t, err, ErrDestinationFailed)
			requireFileContent(t, dest, "already here\n")
			assert.Equal(t, []string{"item"}, entryNames(t, filepath.Dir(dest)), "no copy is left beside it")
			requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
			assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")))
		})
	}
}

// Validates: R-6.4.9
//
// Across filesystems a detach copies the file through its verified descriptor
// into the destination's folder, gives the copy the destination's name, and
// only then removes the file from the tree.
func TestDetachRegularFile_CopiesAcrossFilesystems(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	acrossFilesystems(root)
	before, err := os.Lstat(filepath.Join(dir, "docs", "a.txt"))
	require.NoError(t, err)
	dest := filepath.Join(t.TempDir(), "item")

	require.NoError(t, root.DetachRegularFile("docs/a.txt", nil, dest))

	assert.Empty(t, entryNames(t, filepath.Join(dir, "docs")), "the file and its set-aside name are gone from the tree")
	requireFileContent(t, dest, "synced\n")
	kept, err := os.Lstat(dest)
	require.NoError(t, err)
	assert.False(t, os.SameFile(before, kept), "the file taken out is a copy")
	assert.Equal(t, []string{"item"}, entryNames(t, filepath.Dir(dest)), "no temporary copy is left beside it")
}

// Validates: R-6.4.9
//
// A preserve removes only its own link: an entry another program put at the
// destination's name between the link and the check that the link names the
// verified file is left as it is, and the preserve reports the race.
func TestPreserveRegularFile_LeavesAnEntryThatReplacedItsLink(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	dest := filepath.Join(t.TempDir(), "item")
	link := root.ops.link
	root.ops.link = func(fromDir *os.File, from string, toDir *os.File, to string) error {
		if err := link(fromDir, from, toDir, to); err != nil {
			return err
		}
		replacement := dest + ".new"
		require.NoError(t, os.WriteFile(replacement, []byte("another program's file\n"), 0o600))
		require.NoError(t, os.Rename(replacement, dest))
		return nil
	}

	err := root.PreserveRegularFile("docs/a.txt", nil, dest)

	require.ErrorIs(t, err, ErrDetachRaced)
	requireFileContent(t, dest, "another program's file\n")
	requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
}

// writtenInPlaceOnceSetAside makes root's set-aside rename append to the file
// right after it, in folder, the tree folder that holds it: an in-place write
// that lands between setting the file aside and the check that it is still
// the verified file.
func writtenInPlaceOnceSetAside(t *testing.T, root *Root, folder string) {
	t.Helper()

	renameAt := root.ops.renameAt
	root.ops.renameAt = func(fromDir *os.File, from string, toDir *os.File, to string) error {
		if err := renameAt(fromDir, from, toDir, to); err != nil || !IsDetachArtifactName(to) {
			return err
		}
		f, err := os.OpenFile(filepath.Join(folder, to), os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // the test writes a file it created
		require.NoError(t, err)
		_, err = f.WriteString("appended by the user\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		return nil
	}
}

// Validates: R-6.4.9
//
// On a filesystem without hard links a set-aside file goes back to its name
// by a rename that replaces nothing, so a file written while it was set
// aside, or one whose destination failed, is back at its name with its
// content instead of staying under its temporary one.
func TestDetachRegularFile_PutsTheFileBackWithoutHardLinks(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name      string
		operation string
		// destinationFails removes the destination's folder during the
		// check; otherwise the file is written in place once it is set aside.
		destinationFails bool
	}{
		{name: "written once set aside, detach", operation: opDetach},
		{name: "written once set aside, remove", operation: opRemove},
		{name: "destination failed", operation: opDetach, destinationFails: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			root = root.WithoutNoReplaceFlagOrHardLinks()
			folder := filepath.Join(t.TempDir(), "entry")
			require.NoError(t, os.Mkdir(folder, 0o700))
			var verify VerifyFunc
			want, wantErr := "synced\nappended by the user\n", ErrDetachRaced
			if tt.destinationFails {
				verify = func(*os.File, os.FileInfo) error {
					require.NoError(t, os.Remove(folder))
					return nil
				}
				want, wantErr = "synced\n", ErrDestinationFailed
			} else {
				writtenInPlaceOnceSetAside(t, root, filepath.Join(dir, "docs"))
			}

			var err error
			if tt.operation == opRemove {
				err = root.RemoveVerifiedRegularFile("docs/a.txt", verify)
			} else {
				err = root.DetachRegularFile("docs/a.txt", verify, filepath.Join(folder, "item"))
			}

			require.ErrorIs(t, err, wantErr)
			requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), want)
			assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")), "no set-aside file is left")
		})
	}
}

// Validates: R-6.4.8
//
// A file taken out of the sync folder, or copied out of it, goes to a place
// outside the folder: a destination inside it, or the folder itself, is
// refused before anything moves, so the file is neither renamed inside the
// tree nor linked beside itself.
func TestDetachRegularFile_RefusesADestinationInsideTheSyncFolder(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opPreserve} {
		for _, place := range []string{"inside", "the folder itself"} {
			t.Run(operation+"/"+place, func(t *testing.T) {
				t.Parallel()

				root, dir := newDetachTree(t)
				dest := filepath.Join(dir, "docs", "moved.txt")
				if place == "the folder itself" {
					dest = dir
				}

				require.ErrorIs(t, detachOrPreserve(root, operation, nil, dest), ErrUnsafePath)
				requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
				assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")))
				assert.Equal(t, []string{"docs"}, entryNames(t, dir))
			})
		}
	}
}

// Validates: R-6.4.8
//
// Whether a destination lies in the sync folder is decided by the folder it
// resolves to, not by its text: a destination whose folder lies below a link
// in the sync folder that leads outside it is taken, and the file lands
// outside, while one whose folder lies below a link outside that leads into
// the sync folder is refused before anything moves.
func TestDetachRegularFile_DecidesTheDestinationByTheFolderItResolvesTo(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opPreserve} {
		t.Run(operation+"/below a link in the folder to outside it", func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			outside := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(outside, "entry"), 0o700))
			require.NoError(t, os.Symlink(outside, filepath.Join(dir, "elsewhere")))

			dest := filepath.Join(dir, "elsewhere", "entry", "taken-out")

			require.NoError(t, detachOrPreserve(root, operation, nil, dest))
			requireFileContent(t, filepath.Join(outside, "entry", filepath.Base(dest)), "synced\n")
			if operation == opPreserve {
				requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
			} else {
				assert.NoFileExists(t, filepath.Join(dir, "docs", "a.txt"))
			}
		})
		t.Run(operation+"/below a link outside to a folder in it", func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			intoTheTree := filepath.Join(t.TempDir(), "into-the-tree")
			require.NoError(t, os.Symlink(dir, intoTheTree))

			require.ErrorIs(t, detachOrPreserve(root, operation, nil, filepath.Join(intoTheTree, "docs", "moved.txt")), ErrUnsafePath)
			requireFileContent(t, filepath.Join(dir, "docs", "a.txt"), "synced\n")
			assert.Equal(t, []string{"a.txt"}, entryNames(t, filepath.Join(dir, "docs")))
		})
	}
}

// lstatReportsFolder answers a lstat of name with folder, a real folder's
// stat, whatever is there: the parent the ancestor walk checked as a folder
// and that was swapped for something else before it was opened.
type lstatReportsFolder struct {
	Handle
	name   string
	folder os.FileInfo
}

func (h lstatReportsFolder) Lstat(name string) (os.FileInfo, error) {
	if name == h.name {
		return h.folder, nil
	}

	return h.Handle.Lstat(name) //nolint:wrapcheck // passes the wrapped handle's answer through
}

// Validates: R-6.4.8
//
// A parent folder swapped for a FIFO after the ancestor walk checked it fails
// the removal without blocking: the parent is opened as a folder and never
// waits for a writer.
func TestDetachRegularFile_AFIFOInPlaceOfTheParentFailsWithoutBlocking(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opRemove, opPreserve} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			root, dir := openTestRoot(t)
			require.NoError(t, unix.Mkfifo(filepath.Join(dir, "fifo"), 0o600))
			folder, err := os.Lstat(t.TempDir())
			require.NoError(t, err)
			root = root.Interpose(func(h Handle) Handle {
				return lstatReportsFolder{Handle: h, name: "fifo", folder: folder}
			})
			rel := filepath.Join("fifo", "a.txt")
			dest := filepath.Join(t.TempDir(), "item")

			err = returnsWithoutWaitingOnFIFO(t, filepath.Join(dir, "fifo"), func() error {
				switch operation {
				case opDetach:
					return root.DetachRegularFile(rel, nil, dest)
				case opRemove:
					return root.RemoveVerifiedRegularFile(rel, nil)
				default:
					return root.PreserveRegularFile(rel, nil, dest)
				}
			})

			require.ErrorIs(t, err, unix.ENOTDIR)
			assert.NoFileExists(t, dest)
		})
	}
}

// fifoWaitBound is how long a call that must not open a FIFO for reading gets
// to return before the test takes it as waiting for a writer. The call makes
// a few system calls, so the bound only has to outlast a loaded machine.
const fifoWaitBound = 30 * time.Second

// returnsWithoutWaitingOnFIFO runs call and returns its result. A call that
// has not returned within fifoWaitBound is taken as waiting in an open of
// fifo for a writer: the test opens fifo for writing itself, which releases
// that open, and fails, so a regression fails this test instead of hanging
// the package until its deadline.
func returnsWithoutWaitingOnFIFO(t *testing.T, fifo string, call func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() {
		done <- call()
	}()

	bound := time.NewTimer(fifoWaitBound)
	defer bound.Stop()
	select {
	case err := <-done:
		return err
	case <-bound.C:
	}

	// O_RDWR opens a FIFO without waiting for a reader, and its writing end
	// releases a reader that waits in open.
	writer, err := localpath.OpenFile(fifo, os.O_RDWR, 0)
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	select {
	case <-done:
		require.FailNow(t, "the call waited in an open of the FIFO in place of its parent folder until the test opened it for writing")
	case <-time.After(fifoWaitBound):
		require.FailNow(t, "the call waited on the FIFO in place of its parent folder and still had not returned after the test opened it for writing")
	}

	return nil
}

// Validates: R-6.4.8
func TestDetachRegularFile_RefusesSymlinks(t *testing.T) {
	t.Parallel()

	root, dir := newDetachTree(t)
	require.NoError(t, os.Symlink(filepath.Join(dir, "docs", "a.txt"), filepath.Join(dir, "link.txt")))

	err := root.DetachRegularFile("link.txt", nil, filepath.Join(t.TempDir(), "item"))
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(dir, "docs", "a.txt"))
}

// An in-place write (same inode) between the content check and the move:
// the inode check alone would let it through and the new bytes would leave
// the tree as if they were the verified ones.
//
// Validates: R-6.2.4
func TestDetachRegularFile_InPlaceWriteAfterVerifyIsKept(t *testing.T) {
	t.Parallel()

	for _, operation := range []string{opDetach, opRemove, opPreserve} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()

			root, dir := newDetachTree(t)
			target := filepath.Join(dir, "docs", "a.txt")
			writeInPlace := func(*os.File, os.FileInfo) error {
				f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // the test writes a file it created
				require.NoError(t, err)
				_, err = f.WriteString("appended by the user\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
				return nil
			}
			dest := filepath.Join(t.TempDir(), "item")

			var err error
			switch operation {
			case opDetach:
				err = root.DetachRegularFile("docs/a.txt", writeInPlace, dest)
			case opRemove:
				err = root.RemoveVerifiedRegularFile("docs/a.txt", writeInPlace)
			default:
				err = root.PreserveRegularFile("docs/a.txt", writeInPlace, dest)
			}

			require.ErrorIs(t, err, ErrDetachRaced)
			data, readErr := os.ReadFile(target) //nolint:gosec // the test reads a file it created
			require.NoError(t, readErr)
			assert.Equal(t, "synced\nappended by the user\n", string(data))
			assert.NoFileExists(t, dest)
		})
	}
}

// The cross-filesystem path copies through the verified descriptor; a write
// that lands while the copy runs makes the copy untrustworthy.
//
// Validates: R-6.2.4
func TestCopyVerified_WriteDuringCopyDiscardsTheCopy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("synced\n"), 0o600))
	file, err := os.Open(path) //nolint:gosec // the test opens a file it created
	require.NoError(t, err)
	defer file.Close()
	stamp, err := stampOf(file)
	require.NoError(t, err)

	require.NoError(t, os.WriteFile(path, []byte("rewritten in place\n"), 0o600))
	root, _ := openTestRoot(t)
	out, dest := openTestDestination(t, root)

	err = root.copyVerified(file, stamp, out)
	require.ErrorIs(t, err, ErrDetachRaced)
	assert.NoFileExists(t, dest)
}

// openTestDestination opens, as a take-out does, the destination named item
// in a new folder outside root, and returns it with its path.
func openTestDestination(t *testing.T, root *Root) (*destination, string) {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "entry", "item")
	require.NoError(t, os.Mkdir(filepath.Dir(dest), 0o700))
	out, err := root.openDestination(dest)
	require.NoError(t, err)
	t.Cleanup(out.close)

	return out, dest
}

// openVerifiedCopySource writes a file and opens it the way the detach path
// does, returning the open descriptor and its stamp.
func openVerifiedCopySource(t *testing.T) (*os.File, fileStamp) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("synced\n"), 0o600))
	file, err := os.Open(path) //nolint:gosec // the test opens a file it created
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, file.Close()) })
	stamp, err := stampOf(file)
	require.NoError(t, err)
	return file, stamp
}

// The cross-filesystem copy is only durable once the trash entry's directory
// is: the rename that gives the copy its name is a directory update, and the
// source is unlinked right after. Without the sync a power cut left the entry
// with only a `.copy-*` file and no original anywhere.
//
// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_SyncsTheDestinationFolder(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	root, _ := openTestRoot(t)
	out, dest := openTestDestination(t, root)

	var synced []string
	root.ops.syncDirFile = func(dir *os.File) error {
		synced = append(synced, dir.Name())
		// The sync comes after the rename: it is the rename it makes durable.
		data, err := os.ReadFile(dest) //nolint:gosec // the test reads the copy it made
		require.NoError(t, err)
		assert.Equal(t, "synced\n", string(data))
		return dir.Sync()
	}

	require.NoError(t, root.copyVerified(file, stamp, out))
	assert.Equal(t, []string{filepath.Dir(dest)}, synced, "the entry directory is synced once, after the rename")
}

// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_DirectorySyncFailureLeavesNothingAtTheDestination(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	root, _ := openTestRoot(t)
	out, dest := openTestDestination(t, root)
	root.ops.syncDirFile = func(*os.File) error { return fmt.Errorf("syncing directory: %w", unix.EIO) }

	err := root.copyVerified(file, stamp, out)
	require.ErrorIs(t, err, ErrDestinationFailed)
	require.ErrorContains(t, err, unix.EIO.Error())
	assert.NoFileExists(t, dest, "a copy whose placement is not durable is not left for the caller to unlink the source over")
	entries, readErr := os.ReadDir(filepath.Dir(dest))
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no temporary copy is left behind")
}

// A copy discarded after a failed directory sync is removed only while its
// name still holds it: a file put at the name since stays where it is.
//
// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_DiscardLeavesAFileThatReplacedTheCopy(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	root, _ := openTestRoot(t)
	out, dest := openTestDestination(t, root)
	root.ops.syncDirFile = func(*os.File) error {
		// Another file is renamed over the placed copy before the discard.
		other := filepath.Join(filepath.Dir(dest), "other")
		require.NoError(t, os.WriteFile(other, []byte("someone else's\n"), 0o600))
		require.NoError(t, os.Rename(other, dest))
		return fmt.Errorf("syncing directory: %w", unix.EIO)
	}

	err := root.copyVerified(file, stamp, out)
	require.ErrorIs(t, err, ErrDestinationFailed)
	require.ErrorContains(t, err, "no longer holds the copy")
	data, readErr := os.ReadFile(dest) //nolint:gosec // the test reads the file it planted
	require.NoError(t, readErr)
	assert.Equal(t, "someone else's\n", string(data), "the file at the name is not the copy, so it stays")
}

// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_ToleratesUnsupportedDirectorySync(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	root, _ := openTestRoot(t)
	out, dest := openTestDestination(t, root)
	root.ops.syncDirFile = func(*os.File) error { return fmt.Errorf("syncing directory: %w", unix.ENOTSUP) }

	require.NoError(t, root.copyVerified(file, stamp, out))
	assert.FileExists(t, dest)
}

// Validates: R-6.4.9
func TestIsDetachArtifactName(t *testing.T) {
	t.Parallel()

	name, err := detachTempName()
	require.NoError(t, err)
	assert.True(t, IsDetachArtifactName(name))
	assert.False(t, IsDetachArtifactName(".onedrive-go.abc.partial"), "a download partial is a different artifact")
	assert.False(t, IsDetachArtifactName("notes.detach"))
}
