package localpath

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.4.9
func TestRenameNoReplace_NeverReplacesAnExistingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o600))
	require.NoError(t, os.WriteFile(dst, []byte("current"), 0o600))

	err := RenameNoReplace(src, dst)
	require.ErrorIs(t, err, ErrTargetExists)
	data, readErr := os.ReadFile(dst) //nolint:gosec // test reads its own temp tree
	require.NoError(t, readErr)
	assert.Equal(t, "current", string(data))
	assert.FileExists(t, src)

	require.NoError(t, os.Remove(dst))
	require.NoError(t, RenameNoReplace(src, dst))
	assert.NoFileExists(t, src)
	assert.FileExists(t, dst)
}

// Validates: R-6.4.9
func TestLinkOrCopyNoReplace_FilesystemWithoutHardLinksCopies(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(src, []byte("from trash"), 0o640)) //nolint:gosec // a mode the copy must keep
	ops := defaultNoReplaceOps()
	ops.link = func(string, string) error { return &os.LinkError{Op: "link", Err: syscall.EPERM} }

	require.NoError(t, linkOrCopyNoReplace(src, dst, ops))
	data, err := os.ReadFile(dst) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "from trash", string(data))
	assert.NoFileExists(t, src)
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm(), "the copy keeps the mode")

	// The copy path never replaces either.
	require.NoError(t, os.WriteFile(src, []byte("again"), 0o600))
	err = linkOrCopyNoReplace(src, dst, ops)
	require.ErrorIs(t, err, os.ErrExist)
	data, err = os.ReadFile(dst) //nolint:gosec // test reads its own temp tree
	require.NoError(t, err)
	assert.Equal(t, "from trash", string(data))
}

// Validates: R-6.4.9
//
// A copy that failed leaves no partial file behind, or says it could not
// remove it: a leftover makes the next attempt find the destination taken.
// A partial copy already closed, or already gone, is no further failure.
func TestDiscardPartialCopy_ReportsAPartialCopyItCannotRemove(t *testing.T) {
	t.Parallel()

	errCopy := errors.New("injected copy failure")

	stuck := filepath.Join(t.TempDir(), "stuck")
	out, err := os.Create(stuck) //nolint:gosec // test creates a file in its own temp dir
	require.NoError(t, err)
	err = discardPartialCopy(out, stuck, errCopy, func(string) error { return syscall.EBUSY })
	require.ErrorIs(t, err, errCopy)
	require.ErrorIs(t, err, syscall.EBUSY)
	assert.Contains(t, err.Error(), "removing partial copy "+stuck)

	removed := filepath.Join(t.TempDir(), "removed")
	out, err = os.Create(removed) //nolint:gosec // test creates a file in its own temp dir
	require.NoError(t, err)
	require.NoError(t, out.Close())
	err = discardPartialCopy(out, removed, errCopy, os.Remove)
	require.ErrorIs(t, err, errCopy)
	assert.Equal(t, errCopy.Error(), err.Error(), "a closed, removable partial copy adds nothing")
	assert.NoFileExists(t, removed)
}

// Validates: R-6.4.9
func TestLinkOrCopyNoReplace_SourceThatCannotBeRemovedIsReportedAfterSuccess(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	require.NoError(t, os.WriteFile(src, []byte("x"), 0o600))
	ops := defaultNoReplaceOps()
	ops.remove = func(string) error { return syscall.EBUSY }

	err := linkOrCopyNoReplace(src, dst, ops)
	require.ErrorIs(t, err, ErrSourceNotRemoved)
	assert.FileExists(t, dst, "the file is in place")
}
