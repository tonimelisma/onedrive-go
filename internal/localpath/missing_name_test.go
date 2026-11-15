package localpath

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.4.10, R-6.4.8
//
// Only the filesystem's own answer says a name is missing: ENOENT, or ENOTDIR
// for a folder on the way that is a file, however it is wrapped. Every other
// errno, and a failure without an errno (a path escape, the bare sentinel),
// is not evidence of absence.
func TestIsMissingName_OnlyTheErrnoOfAMissingNameCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "ENOENT", err: syscall.ENOENT, want: true},
		{name: "ENOTDIR", err: syscall.ENOTDIR, want: true},
		{name: "path error with ENOENT", err: &os.PathError{Op: "openat", Path: "a", Err: syscall.ENOENT}, want: true},
		{name: "link error with ENOTDIR", err: &os.LinkError{Op: "renameat", Old: "a", New: "b", Err: syscall.ENOTDIR}, want: true},
		{name: "wrapped ENOENT", err: fmt.Errorf("stating a: %w", &os.PathError{Op: "lstat", Path: "a", Err: syscall.ENOENT}), want: true},
		{name: "EACCES", err: &os.PathError{Op: "openat", Path: "a", Err: syscall.EACCES}, want: false},
		{name: "ENOSPC", err: syscall.ENOSPC, want: false},
		{name: "EEXIST", err: syscall.EEXIST, want: false},
		{name: "bare not-exist sentinel", err: fs.ErrNotExist, want: false},
		{name: "wrapped not-exist sentinel", err: fmt.Errorf("opening a: %w", os.ErrNotExist), want: false},
		{name: "path escape without an errno", err: errors.New("openat a/b: path escapes from parent"), want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			assert.Equal(t, tc.want, IsMissingName(tc.err))
		})
	}
}
