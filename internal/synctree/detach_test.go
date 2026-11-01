package synctree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	opDetach = "detach"
	opRemove = "remove"
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

	for _, operation := range []string{opDetach, opRemove, "preserve"} {
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

	for _, operation := range []string{opDetach, opRemove, "preserve"} {
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
	dest := filepath.Join(t.TempDir(), "item")

	err = copyVerified(file, stamp, dest, syncDirectory)
	require.ErrorIs(t, err, ErrDetachRaced)
	assert.NoFileExists(t, dest)
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
	dest := filepath.Join(t.TempDir(), "entry", "item")

	var synced []string
	recordingSync := func(dir string) error {
		synced = append(synced, dir)
		// The sync comes after the rename: it is the rename it makes durable.
		data, err := os.ReadFile(dest) //nolint:gosec // the test reads the copy it made
		require.NoError(t, err)
		assert.Equal(t, "synced\n", string(data))
		return syncDirectory(dir)
	}

	require.NoError(t, copyVerified(file, stamp, dest, recordingSync))
	assert.Equal(t, []string{filepath.Dir(dest)}, synced, "the entry directory is synced once, after the rename")
}

// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_DirectorySyncFailureLeavesNothingAtTheDestination(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	dest := filepath.Join(t.TempDir(), "entry", "item")
	failingSync := func(string) error { return fmt.Errorf("syncing directory: %w", unix.EIO) }

	err := copyVerified(file, stamp, dest, failingSync)
	require.ErrorIs(t, err, unix.EIO)
	assert.NoFileExists(t, dest, "a copy whose placement is not durable is not left for the caller to unlink the source over")
	entries, readErr := os.ReadDir(filepath.Dir(dest))
	require.NoError(t, readErr)
	assert.Empty(t, entries, "no temporary copy is left behind")
}

// Validates: R-6.4.9, R-6.5.4
func TestCopyVerified_ToleratesUnsupportedDirectorySync(t *testing.T) {
	t.Parallel()

	file, stamp := openVerifiedCopySource(t)
	dest := filepath.Join(t.TempDir(), "entry", "item")
	unsupportedSync := func(string) error { return fmt.Errorf("syncing directory: %w", unix.ENOTSUP) }

	require.NoError(t, copyVerified(file, stamp, dest, unsupportedSync))
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
