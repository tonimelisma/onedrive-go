package synctree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	// spellingNFC is "café" precomposed; spellingNFD is the same name as
	// e + U+0301 COMBINING ACUTE ACCENT.
	spellingNFC = "café"
	spellingNFD = "café"
)

// skipUnlessByteExactNames skips on macOS, whose volumes find an entry by
// either Unicode spelling, so there is nothing to resolve.
func skipUnlessByteExactNames(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "darwin" {
		t.Skip("macOS volumes are normalization-insensitive")
	}
}

func spellingTestRoot(t *testing.T, files ...string) (*Root, string) {
	t.Helper()

	dir := t.TempDir()
	for _, file := range files {
		abs := filepath.Join(dir, filepath.FromSlash(file))
		require.NoError(t, os.MkdirAll(filepath.Dir(abs), 0o700))
		require.NoError(t, os.WriteFile(abs, []byte("x"), 0o600))
	}
	root, err := Open(dir)
	require.NoError(t, err)

	return root, dir
}

// Validates: R-2.13.2
func TestRoot_SpellingFindsTheNameOnDisk(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	for _, tc := range []struct {
		name  string
		files []string
		query string
		want  string
	}{
		{
			name:  "exact hit is returned as given",
			files: []string{"docs/" + spellingNFC + ".txt"},
			query: "docs/" + spellingNFC + ".txt",
			want:  filepath.Join("docs", spellingNFC+".txt"),
		},
		{
			name:  "NFD file on disk",
			files: []string{"docs/" + spellingNFD + ".txt"},
			query: "docs/" + spellingNFC + ".txt",
			want:  filepath.Join("docs", spellingNFD+".txt"),
		},
		{
			name:  "NFD folders and file on disk",
			files: []string{spellingNFD + "/" + spellingNFD + "/" + spellingNFD + ".txt"},
			query: spellingNFC + "/" + spellingNFC + "/" + spellingNFC + ".txt",
			want:  filepath.Join(spellingNFD, spellingNFD, spellingNFD+".txt"),
		},
		{
			name:  "missing entry under an NFD folder keeps its own name",
			files: []string{spellingNFD + "/other.txt"},
			query: spellingNFC + "/new.txt",
			want:  filepath.Join(spellingNFD, "new.txt"),
		},
		{
			name:  "missing parent keeps the whole path",
			files: []string{"other.txt"},
			query: "missing/" + spellingNFC + ".txt",
			want:  filepath.Join("missing", spellingNFC+".txt"),
		},
		{
			// U+1E17 has two non-NFC spellings: fully decomposed, and
			// e-macron with a combining acute. Both on disk is a local name
			// collision (R-2.12.3); neither is picked.
			name:  "ambiguous match is not resolved",
			files: []string{"ḗ.txt", "ḗ.txt"},
			query: "ḗ.txt",
			want:  "ḗ.txt",
		},
		{
			name:  "a file is not a folder",
			files: []string{spellingNFD},
			query: spellingNFC + "/child.txt",
			want:  filepath.Join(spellingNFD, "child.txt"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			root, _ := spellingTestRoot(t, tc.files...)
			assert.Equal(t, tc.want, root.Spelling(tc.query))
		})
	}
}

// Validates: R-2.13.2
func TestRoot_SpellingReturnsUnusablePathsAsGiven(t *testing.T) {
	t.Parallel()

	root, _ := spellingTestRoot(t)
	assert.Equal(t, "../outside.txt", root.Spelling("../outside.txt"))
	assert.Empty(t, root.Spelling(""))
	assert.Equal(t, ".", root.Spelling("."))

	gone, err := Open(filepath.Join(t.TempDir(), "gone"))
	require.NoError(t, err)
	assert.Equal(t, spellingNFC, gone.Spelling(spellingNFC), "an unavailable sync folder resolves nothing")
}

// Validates: R-2.13.2
func TestRoot_SpellingOfParentResolvesOnlyTheParents(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	root, _ := spellingTestRoot(t, spellingNFD+"/"+spellingNFD+".txt")

	assert.Equal(t, filepath.Join(spellingNFD, spellingNFC+".txt"), root.SpellingOfParent(spellingNFC+"/"+spellingNFC+".txt"),
		"the new entry's own name is never looked up")
	assert.Equal(t, spellingNFC+".txt", root.SpellingOfParent(spellingNFC+".txt"))
	assert.Equal(t, "../x", root.SpellingOfParent("../x"))
}

// Validates: R-2.13.2
//
// The resolved spelling is what the rooted operations need: stat, rename and
// remove by the stored NFC path fail on a byte-exact filesystem.
func TestRoot_SpellingAddressesTheEntryForRootedOperations(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactNames(t)

	root, dir := spellingTestRoot(t, spellingNFD+"/"+spellingNFD+".txt")
	stored := spellingNFC + "/" + spellingNFC + ".txt"

	_, err := root.Lstat(stored)
	require.ErrorIs(t, err, os.ErrNotExist, "premise: the stored spelling is not on disk")

	local := root.Spelling(stored)
	_, err = root.Lstat(local)
	require.NoError(t, err)
	require.NoError(t, root.Rename(local, root.SpellingOfParent(spellingNFC+"/renamed.txt")))
	assert.FileExists(t, filepath.Join(dir, spellingNFD, "renamed.txt"))
	require.NoError(t, root.Remove(root.Spelling(spellingNFC+"/renamed.txt")))
	assert.NoFileExists(t, filepath.Join(dir, spellingNFD, "renamed.txt"))
}
