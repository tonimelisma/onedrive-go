//go:build linux

package synctree

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func memoryRoot(t *testing.T, caps Capabilities) *Root {
	t.Helper()

	root, err := OpenMemory(caps)
	require.NoError(t, err)

	return root
}

func writeMemoryFile(t *testing.T, root *Root, rel, content string) {
	t.Helper()

	require.NoError(t, root.MkdirAll(filepath.Dir(rel)))
	file, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	require.NoError(t, err)
	_, err = io.WriteString(file, content)
	require.NoError(t, err)
	require.NoError(t, file.Close())
}

// LI-20261029-01 on a byte-exact in-memory volume, so it replays on any
// host: the NFD-named file is missing under the NFC spelling every stored
// path has, which is what made the upload read its source as gone, and
// Spelling finds it.
func TestMemoryVolume_NFDNamedFileIsMissingUnderItsStoredSpelling(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{})
	stored := filepath.Join("corpus", spellingNFC+" nfd.txt")
	onDisk := filepath.Join("corpus", spellingNFD+" nfd.txt")
	writeMemoryFile(t, root, onDisk, "corpus file")

	_, err := root.Lstat(stored)
	require.ErrorIs(t, err, os.ErrNotExist, "a byte-exact volume does not find the NFD name by its NFC spelling")

	resolved := root.Spelling(stored)
	assert.Equal(t, onDisk, resolved)
	info, err := root.Lstat(resolved)
	require.NoError(t, err)
	assert.Equal(t, int64(len("corpus file")), info.Size())
	_, statErr := os.Lstat(root.Path())
	assert.True(t, errors.Is(statErr, os.ErrNotExist), "the in-memory root must not exist on disk")
}

// The same file on a volume that folds normalization, as APFS does: the
// stored spelling finds it directly, which is why the incident never showed
// on macOS.
func TestMemoryVolume_NormalizationFoldingVolumeFindsBothSpellings(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{FoldNormalization: true})
	writeMemoryFile(t, root, spellingNFD+".txt", "x")

	info, err := root.Lstat(spellingNFC + ".txt")
	require.NoError(t, err)
	assert.Equal(t, spellingNFD+".txt", info.Name(), "the entry keeps the spelling it was created with")
}

func TestMemoryVolume_CaseFoldingRenameRespellsTheEntry(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{FoldCase: true})
	writeMemoryFile(t, root, "Report.txt", "x")
	before, err := root.IdentityNoFollow("report.TXT")
	require.NoError(t, err)

	require.NoError(t, root.RenameNoReplace("Report.txt", "report.txt"))
	entries, err := root.ReadDir(".")
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "report.txt", entries[0].Name())
	after, err := root.IdentityNoFollow("report.txt")
	require.NoError(t, err)
	assert.True(t, SameIdentity(before, after), "a rename keeps the entry's identity")
}

func TestMemoryVolume_NoReplaceRenameAndRmdirKeepTheirRefusals(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{})
	writeMemoryFile(t, root, "a.txt", "a")
	writeMemoryFile(t, root, "dir/b.txt", "b")

	require.ErrorIs(t, root.RenameNoReplace("a.txt", "dir/b.txt"), ErrTargetExists)
	require.Error(t, root.RemoveEmptyDirNoFollow("dir"))
	require.NoError(t, root.Remove("dir/b.txt"))
	require.NoError(t, root.RemoveEmptyDirNoFollow("dir"))
	require.NoError(t, root.RenameNoReplace("a.txt", "c.txt"))
	_, err := root.Lstat("a.txt")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestMemoryVolume_RootIdentityAndMarkAreKept(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{})
	token, err := root.MarkRoot()
	require.NoError(t, err)
	identity, err := root.RootIdentity()
	require.NoError(t, err)
	assert.Equal(t, token, identity.Token)
	again, err := root.RootIdentity()
	require.NoError(t, err)
	assert.True(t, SameRoot(identity, again))
}

func TestMemoryVolume_RemoveVerifiedRegularFileRemovesOnlyTheApprovedFile(t *testing.T) {
	t.Parallel()

	root := memoryRoot(t, Capabilities{})
	writeMemoryFile(t, root, "keep.txt", "old")

	refused := errors.New("refused")
	require.ErrorIs(t, root.RemoveVerifiedRegularFile("keep.txt", func(*os.File, os.FileInfo) error { return refused }), refused)
	_, err := root.Lstat("keep.txt")
	require.NoError(t, err)

	require.NoError(t, root.RemoveVerifiedRegularFile("keep.txt", nil))
	_, err = root.Lstat("keep.txt")
	require.ErrorIs(t, err, os.ErrNotExist)
}
