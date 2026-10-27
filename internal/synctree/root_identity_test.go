package synctree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// Validates: R-6.2.12
func TestSameRoot_TokenDecidesWhenRecorded(t *testing.T) {
	t.Parallel()

	recorded := RootIdentity{Device: 7, Inode: 42, FileSystemID: 0xabc, Token: "t1"}

	assert.True(t, SameRoot(recorded, RootIdentity{Device: 9, Inode: 99, FileSystemID: 0xdef, Token: "t1"}),
		"a matching token is the same folder even after FUSE or SMB inode churn")
	assert.False(t, SameRoot(recorded, RootIdentity{Device: 7, Inode: 42, FileSystemID: 0xabc}),
		"rm -rf and mkdir can reuse the inode; a folder without the token is a different folder")
	assert.False(t, SameRoot(recorded, RootIdentity{Device: 7, Inode: 42, FileSystemID: 0xabc, Token: "t2"}))
}

// Validates: R-6.2.12
func TestSameRoot_BtrfsSubvolumeRootAlsoNeedsDevice(t *testing.T) {
	t.Parallel()

	recorded := RootIdentity{Device: 40, Inode: btrfsSubvolumeRootInode, FileSystemID: 0xabc}
	assert.False(t, SameRoot(recorded, RootIdentity{Device: 41, Inode: btrfsSubvolumeRootInode, FileSystemID: 0xabc}),
		"another subvolume of the same btrfs filesystem shares fsid and root inode")
	assert.True(t, SameRoot(recorded, RootIdentity{Device: 40, Inode: btrfsSubvolumeRootInode, FileSystemID: 0xabc}))
	assert.True(t, SameRoot(RootIdentity{Device: 40, Inode: 300, FileSystemID: 0xabc},
		RootIdentity{Device: 41, Inode: 300, FileSystemID: 0xabc}), "an ordinary folder still survives renumbering")
}

// Validates: R-6.2.12
func TestMarkRoot_WritesTokenThatRootIdentityReads(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	token, err := root.MarkRoot()
	require.NoError(t, err)
	require.NotEmpty(t, token)
	identity, err := root.RootIdentity()
	require.NoError(t, err)
	assert.Equal(t, token, identity.Token)
}

// Validates: R-6.2.12
func TestMarkRoot_FilesystemWithoutXattrsFallsBack(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)
	root.ops.setRootMark = func(*os.File, string) error { return unix.ENOTSUP }
	root.ops.getRootMark = func(*os.File) (string, error) { return "", unix.ENOTSUP }

	_, err = root.MarkRoot()
	require.ErrorIs(t, err, ErrRootMarkUnsupported)
	identity, err := root.RootIdentity()
	require.NoError(t, err)
	assert.Empty(t, identity.Token, "no token: inode and fsid decide")
	assert.NotZero(t, identity.Inode)
}
