package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.10.60, R-2.10.61, R-2.10.62, R-2.11.8
func TestClassifyLocalError_Table(t *testing.T) {
	t.Parallel()

	wrapped := func(errno syscall.Errno) error {
		return fmt.Errorf("sync: download: %w", &os.PathError{Op: "write", Path: "/sync/a", Err: errno})
	}
	testCases := []struct {
		name string
		err  error
		want LocalErrorKind
	}{
		{name: "nil", err: nil, want: LocalErrorOther},
		{name: "reserve sentinel", err: fmt.Errorf("x: %w", ErrDiskFull), want: LocalErrorNoSpace},
		{name: "enospc", err: wrapped(syscall.ENOSPC), want: LocalErrorNoSpace},
		{name: "edquot", err: wrapped(syscall.EDQUOT), want: LocalErrorNoSpace},
		{name: "erofs", err: wrapped(syscall.EROFS), want: LocalErrorReadOnly},
		{name: "efbig", err: wrapped(syscall.EFBIG), want: LocalErrorFileTooLarge},
		{name: "ebusy", err: wrapped(syscall.EBUSY), want: LocalErrorBusy},
		{name: "etxtbsy", err: wrapped(syscall.ETXTBSY), want: LocalErrorBusy},
		{name: "estale", err: wrapped(syscall.ESTALE), want: LocalErrorBusy},
		{name: "exdev", err: wrapped(syscall.EXDEV), want: LocalErrorCrossDevice},
		{name: "enametoolong", err: wrapped(syscall.ENAMETOOLONG), want: LocalErrorNameTooLong},
		{name: "permission is not this table's", err: wrapped(syscall.EACCES), want: LocalErrorOther},
		{name: "plain error", err: errors.New("boom"), want: LocalErrorOther},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, ClassifyLocalError(tc.err))
		})
	}
}

// Validates: R-2.10.60
func TestDownloadToFile_WriteOutOfSpaceReturnsErrDiskFull(t *testing.T) {
	t.Parallel()

	dl := &tmSimpleDownloader{
		downloadFn: func(context.Context, driveid.ID, string, io.Writer) (int64, error) {
			// What io.Copy returns when the partial file's write hits a full disk.
			return 0, &os.PathError{Op: "write", Path: "x.partial", Err: syscall.ENOSPC}
		},
	}
	tm := newTestTM(dl, &tmMockUploader{}, nil)
	targetPath := filepath.Join(t.TempDir(), "big.iso")

	_, err := tm.DownloadToFile(t.Context(), driveid.New("d1"), "item1", targetPath, DownloadOpts{})

	require.Error(t, err)
	require.ErrorIs(t, err, ErrDiskFull, "a mid-write ENOSPC is the same condition as the reserve pre-check")
	require.ErrorIs(t, err, syscall.ENOSPC)
	assert.Contains(t, err.Error(), "no space left on the local disk")
}

// Validates: R-2.10.60
func TestWrapLocalWriteError_NamesQuotaAndLeavesOthersAlone(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	quota := wrapLocalWriteError(dir, &os.PathError{Op: "write", Path: "a", Err: syscall.EDQUOT})
	require.ErrorIs(t, quota, ErrDiskFull)
	assert.Contains(t, quota.Error(), "disk quota")

	other := errors.New("connection reset")
	assert.Same(t, other, wrapLocalWriteError(dir, other))

	already := fmt.Errorf("pre-check: %w", ErrDiskFull)
	assert.Same(t, already, wrapLocalWriteError(dir, already), "never double-wrap the reserve error")
}

// Validates: R-2.10.60
func TestInodesExhausted_RealFilesystemHasFreeInodes(t *testing.T) {
	t.Parallel()

	free, total, err := inodeCounts(t.TempDir())
	require.NoError(t, err)
	if total == 0 {
		// Dynamic-inode filesystems report no total; that is never exhaustion.
		assert.False(t, inodesExhausted(t.TempDir()))
		return
	}
	assert.Positive(t, free)
	assert.False(t, inodesExhausted(t.TempDir()))
}
