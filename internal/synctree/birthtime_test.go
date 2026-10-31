package synctree

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.17.1
func TestSameIdentity_BirthTimeDecidesWhenBothSidesKnowIt(t *testing.T) {
	t.Parallel()

	const (
		device = 7
		inode  = 42
		born   = int64(1_700_000_000_000_000_001)
		reborn = int64(1_700_000_000_000_000_002)
	)
	for _, tc := range []struct {
		name string
		a, b FileIdentity
		same bool
	}{
		{name: "same entry", a: FileIdentity{Device: device, Inode: inode, BirthTime: born}, b: FileIdentity{Device: device, Inode: inode, BirthTime: born}, same: true},
		{name: "reused inode", a: FileIdentity{Device: device, Inode: inode, BirthTime: born}, b: FileIdentity{Device: device, Inode: inode, BirthTime: reborn}},
		{name: "no birth time on either side", a: FileIdentity{Device: device, Inode: inode}, b: FileIdentity{Device: device, Inode: inode}, same: true},
		{name: "birth time on one side only", a: FileIdentity{Device: device, Inode: inode, BirthTime: born}, b: FileIdentity{Device: device, Inode: inode}, same: true},
		{name: "other inode", a: FileIdentity{Device: device, Inode: inode, BirthTime: born}, b: FileIdentity{Device: device, Inode: inode + 1, BirthTime: born}},
		{name: "other device", a: FileIdentity{Device: device, Inode: inode}, b: FileIdentity{Device: device + 1, Inode: inode}},
	} {
		assert.Equal(t, tc.same, SameIdentity(tc.a, tc.b), tc.name)
		assert.Equal(t, tc.same, SameIdentity(tc.b, tc.a), "%s, reversed", tc.name)
	}
}

// Validates: R-2.17.1
//
// Every way the sync tree reads an entry (rooted Lstat and Stat, a walk, a
// listing) and watch-mode's AttachBirthTime report one identity, birth time
// included where this filesystem records one; a rename keeps it.
func TestIdentityFromFileInfo_EveryReadReportsTheSameBirthTime(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o600))

	lstat, err := tree.Lstat("a.txt")
	require.NoError(t, err)
	want, ok := IdentityFromFileInfo(lstat)
	require.True(t, ok)

	stat, err := tree.Stat("a.txt")
	require.NoError(t, err)
	fromStat, _ := IdentityFromFileInfo(stat)
	assert.Equal(t, want, fromStat, "Stat")

	entries, err := tree.ReadDir(".")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	entryInfo, err := entries[0].Info()
	require.NoError(t, err)
	fromListing, _ := IdentityFromFileInfo(entryInfo)
	assert.Equal(t, want, fromListing, "ReadDir")

	var fromWalk FileIdentity
	require.NoError(t, tree.WalkDir(func(path string, d fs.DirEntry, walkErr error) error {
		require.NoError(t, walkErr)
		if filepath.Base(path) == "a.txt" {
			info, infoErr := d.Info()
			require.NoError(t, infoErr)
			fromWalk, _ = IdentityFromFileInfo(info)
		}
		return nil
	}))
	assert.Equal(t, want, fromWalk, "WalkDir")

	plain, err := os.Lstat(filepath.Join(dir, "a.txt"))
	require.NoError(t, err)
	attached, _ := IdentityFromFileInfo(AttachBirthTime(plain, filepath.Join(dir, "a.txt")))
	assert.Equal(t, want, attached, "AttachBirthTime")

	require.NoError(t, os.Rename(filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")))
	renamed, err := tree.IdentityNoFollow("b.txt")
	require.NoError(t, err)
	assert.Equal(t, want, renamed, "a rename keeps the identity, birth time included")
	assert.True(t, SameFile(lstat, mustLstat(t, tree, "b.txt")))
}

func mustLstat(t *testing.T, tree *Root, rel string) fs.FileInfo {
	t.Helper()

	info, err := tree.Lstat(rel)
	require.NoError(t, err)

	return info
}
