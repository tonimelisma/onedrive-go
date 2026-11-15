package synctree

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// absenceProbeHandle fails every Remove with removeErr and answers an Lstat
// with lstatErr, or with the filesystem's own answer when lstatErr is nil.
type absenceProbeHandle struct {
	Handle
	removeErr error
	lstatErr  error
}

func (h absenceProbeHandle) Remove(string) error { return h.removeErr }

func (h absenceProbeHandle) Lstat(name string) (os.FileInfo, error) {
	if h.lstatErr != nil {
		return nil, h.lstatErr
	}

	return h.Handle.Lstat(name) //nolint:wrapcheck // stands in for the filesystem
}

func pathErr(errno syscall.Errno) error {
	return &os.PathError{Op: "probe", Path: "probe", Err: errno}
}

// Validates: R-6.4.8, R-2.4.6
//
// A failure reads as "does not exist" only when two answers agree: the
// failure itself carries an errno saying the name is missing (ENOENT, or
// ENOTDIR for a folder on the way that is a file), and a lstat through the
// same rooted handle finds the name missing too. A failure without such an
// errno, a path escape among them, keeps its own error whatever is at the
// path; so does a missing-name failure the rooted lstat cannot confirm.
func TestRoot_NotExistNeedsTheFailureAndTheRootedLstatToAgree(t *testing.T) {
	t.Parallel()

	errNoErrno := errors.New("refused without an errno")
	errEscape := errors.New("path escapes from parent")
	tests := []struct {
		name         string
		path         string
		removeErr    error
		lstatErr     error
		wantNotExist bool
	}{
		{name: "missing name, lstat agrees", path: "missing.txt", removeErr: pathErr(syscall.ENOENT), wantNotExist: true},
		{name: "folder on the way is a file, lstat agrees", path: "missing.txt", removeErr: pathErr(syscall.ENOTDIR), lstatErr: pathErr(syscall.ENOTDIR), wantNotExist: true},
		{name: "missing errno but the name is there", path: "present.txt", removeErr: pathErr(syscall.ENOENT)},
		{name: "not a directory but the name is there", path: "present.txt", removeErr: pathErr(syscall.ENOTDIR)},
		{name: "permission denied on a missing name", path: "missing.txt", removeErr: pathErr(syscall.EACCES)},
		{name: "failure without an errno on a missing name", path: "missing.txt", removeErr: errNoErrno},
		{name: "path escape on a missing name", path: "missing.txt", removeErr: errEscape},
		{name: "rooted lstat cannot tell", path: "missing.txt", removeErr: pathErr(syscall.ENOENT), lstatErr: pathErr(syscall.EACCES)},
		{name: "rooted lstat fails without an errno", path: "missing.txt", removeErr: pathErr(syscall.ENOENT), lstatErr: errEscape},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "present.txt"), []byte("x"), 0o600))
			root, err := Open(dir)
			require.NoError(t, err)
			root = root.Interpose(func(h Handle) Handle {
				return absenceProbeHandle{Handle: h, removeErr: tc.removeErr, lstatErr: tc.lstatErr}
			})

			err = root.Remove(tc.path)
			require.Error(t, err)
			if tc.wantNotExist {
				require.ErrorIs(t, err, os.ErrNotExist)
				require.NotErrorIs(t, err, tc.removeErr, "the failure is replaced by the absence answer")
				return
			}
			require.ErrorIs(t, err, tc.removeErr, "the failure keeps its own error")
			if !errors.Is(tc.removeErr, os.ErrNotExist) {
				require.NotErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}
