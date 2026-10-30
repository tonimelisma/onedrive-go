//go:build linux

package synctree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// Validates: R-2.17.1
//
// On Linux the birth time is statx's STATX_BTIME, recorded only when the
// filesystem sets the mask bit; where it does not, identity has none.
func TestIdentityFromFileInfo_LinuxBirthTimeIsStatxBtime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "folder"), 0o700))

	identity, err := tree.IdentityNoFollow("folder")
	require.NoError(t, err)

	var stx unix.Statx_t
	require.NoError(t, unix.Statx(unix.AT_FDCWD, filepath.Join(dir, "folder"), unix.AT_SYMLINK_NOFOLLOW, unix.STATX_BTIME, &stx))
	if stx.Mask&unix.STATX_BTIME == 0 {
		assert.Zero(t, identity.BirthTime, "a filesystem without birth times records none")
		t.Skip("the test filesystem reports no birth time")
	}
	assert.Equal(t, stx.Btime.Sec*int64(1e9)+int64(stx.Btime.Nsec), identity.BirthTime)
}
