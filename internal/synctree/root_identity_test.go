package synctree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.12
func TestSameRoot_MatchesByFileSystemIDAcrossDeviceRenumbering(t *testing.T) {
	t.Parallel()

	recorded := RootIdentity{Device: 2049, Inode: 42, FileSystemID: 0xabc}

	assert.True(t, SameRoot(recorded, RootIdentity{Device: 2065, Inode: 42, FileSystemID: 0xabc}),
		"the same filesystem after a device-number reshuffle is the same root")
	assert.False(t, SameRoot(recorded, RootIdentity{Device: 2049, Inode: 42, FileSystemID: 0xdef}),
		"a different filesystem at the same device number is a different root")
	assert.False(t, SameRoot(recorded, RootIdentity{Device: 2049, Inode: 43, FileSystemID: 0xabc}))
	assert.True(t, SameRoot(RootIdentity{Device: 7, Inode: 9}, RootIdentity{Device: 7, Inode: 9, FileSystemID: 0xabc}),
		"without an fsid on one side the device number decides")
	assert.False(t, SameRoot(RootIdentity{Device: 7, Inode: 9}, RootIdentity{Device: 8, Inode: 9}))
}

// Validates: R-6.2.12
func TestRootIdentity_DistinguishesReplacedFolder(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.Mkdir(dir, 0o750))
	root, err := Open(dir)
	require.NoError(t, err)

	before, err := root.RootIdentity()
	require.NoError(t, err)
	again, err := root.RootIdentity()
	require.NoError(t, err)
	assert.True(t, SameRoot(before, again))

	require.NoError(t, os.Rename(dir, dir+".old"))
	require.NoError(t, os.Mkdir(dir, 0o750))
	after, err := root.RootIdentity()
	require.NoError(t, err)
	assert.False(t, SameRoot(before, after), "a new folder at the same path is a different root")
}
