package syncfixture

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// RequireTrashHoldsOnce asserts that trash holds exactly one entry, for path,
// whose file holds content.
func RequireTrashHoldsOnce(t *testing.T, trash *localtrash.Trash, path, content string) localtrash.Entry {
	t.Helper()

	entries, err := trash.List()
	require.NoError(t, err)
	require.Len(t, entries, 1, "the displaced file is in the trash exactly once")
	assert.Equal(t, path, entries[0].Path)
	data, err := localpath.ReadFile(entries[0].ItemPath)
	require.NoError(t, err)
	assert.Equal(t, content, string(data))

	return entries[0]
}

// EntryIdentity is the device and inode of the entry at path, not followed.
func EntryIdentity(t *testing.T, path string) synctree.FileIdentity {
	t.Helper()

	info, err := os.Lstat(path)
	require.NoError(t, err)
	identity, ok := synctree.IdentityFromFileInfo(info)
	require.True(t, ok)

	return synctree.FileIdentity{Device: identity.Device, Inode: identity.Inode}
}
