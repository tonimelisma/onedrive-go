package synctree_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/text/unicode/norm"

	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// caseInsensitiveTree is a sync tree on the emulated case-insensitive
// volume (synctest.CaseInsensitive), with the folder it sits on.
func caseInsensitiveTree(t *testing.T) (*synctree.Root, string) {
	t.Helper()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	return tree.Interpose(synctest.CaseInsensitive), dir
}

// skipUnlessByteExactVolume skips on macOS, whose volumes find an entry by
// another case or Unicode spelling of its name.
func skipUnlessByteExactVolume(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "darwin" {
		t.Skip("macOS volumes fold case and Unicode form")
	}
}

func namesIn(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}

	return names
}

// Validates: R-2.10, R-5.10.2
func TestRoot_MkdirExact_CreatesTheFolderAndItsParents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)

	onDisk, err := tree.MkdirExact(filepath.Join("a", "b"))

	require.NoError(t, err)
	assert.Equal(t, filepath.Join("a", "b"), onDisk)
	assert.DirExists(t, filepath.Join(dir, "a", "b"))
}

// A folder create on a volume that folds case finds an unrelated folder of
// another spelling where it creates: that folder is not adopted, the create
// fails as a name taken, and nothing is made beside or inside it.
//
// Validates: R-2.12.10
func TestRoot_MkdirExact_RefusesACaseVariantOfAnotherFolder(t *testing.T) {
	t.Parallel()

	tree, dir := caseInsensitiveTree(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "x2"), 0o700))

	_, err := tree.MkdirExact("X2")

	require.ErrorIs(t, err, synctree.ErrTargetExists)
	assert.Equal(t, []string{"x2"}, namesIn(t, dir))
	assert.Empty(t, namesIn(t, filepath.Join(dir, "x2")))
}

// A folder spelled exactly as the create asks for, left by a create that
// crashed before its outcome was recorded, is the folder: it is adopted.
//
// Validates: R-2.5.1, R-2.12.10
func TestRoot_MkdirExact_AdoptsTheFolderSpelledExactly(t *testing.T) {
	t.Parallel()

	for name, wrap := range map[string]func(synctree.Handle) synctree.Handle{
		"exact volume":            func(h synctree.Handle) synctree.Handle { return h },
		"case-insensitive volume": synctest.CaseInsensitive,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			base, err := synctree.Open(dir)
			require.NoError(t, err)
			tree := base.Interpose(wrap)
			require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", "X2"), 0o700))

			onDisk, err := tree.MkdirExact(filepath.Join("docs", "X2"))

			require.NoError(t, err)
			assert.Equal(t, filepath.Join("docs", "X2"), onDisk)
		})
	}
}

// A folder whose name on disk is the NFD spelling of the stored NFC name is
// the folder the create asks for (R-2.13.2): it is adopted, under the
// spelling it has on disk.
//
// Validates: R-2.12.10, R-2.13.2
func TestRoot_MkdirExact_AdoptsTheNFDSpellingOfTheStoredName(t *testing.T) {
	t.Parallel()

	tree, dir := caseInsensitiveTree(t)
	nfd := norm.NFD.String("Café")
	require.NoError(t, os.Mkdir(filepath.Join(dir, nfd), 0o700))

	onDisk, err := tree.MkdirExact(norm.NFC.String("Café"))

	require.NoError(t, err)
	assert.Equal(t, nfd, onDisk)
	assert.Equal(t, []string{nfd}, namesIn(t, dir))
}

// On a volume that stores names byte for byte, mkdir of the stored NFC name
// succeeds beside its NFD spelling, which OneDrive takes for the same name.
// Made, the two folders would be one OneDrive name, a local collision
// (R-2.12.3); the create refuses as a name taken instead, and nothing is
// made beside the entry.
//
// Validates: R-2.12.3, R-2.12.10, R-2.13.2
func TestRoot_MkdirExact_RefusesANormalizationTwinOnAByteExactVolume(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactVolume(t)

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)
	nfd := norm.NFD.String("Café")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "docs", nfd), 0o700))

	_, err = tree.MkdirExact(filepath.Join("docs", norm.NFC.String("Café")))

	require.ErrorIs(t, err, synctree.ErrTargetExists)
	assert.Contains(t, err.Error(), nfd, "the refusal names the entry that holds the name")
	assert.Equal(t, []string{nfd}, namesIn(t, filepath.Join(dir, "docs")))
	assert.Empty(t, namesIn(t, filepath.Join(dir, "docs", nfd)))

	twin, found, err := tree.NormalizationTwin(filepath.Join("docs", norm.NFC.String("Café")))
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, filepath.Join("docs", nfd), twin)
}

// A name no entry holds in either Unicode form is made as asked on a
// byte-exact volume, beside entries of other names. A case variant is one of
// them: the create checks normalization forms only, and the two local
// entries of one OneDrive name are the scanner's collision to report
// (TestFullScan_CaseCollision_InSkippedNotEvents).
//
// Validates: R-2.12.3, R-2.12.10
func TestRoot_MkdirExact_MakesANameNoNormalizationTwinHolds(t *testing.T) {
	t.Parallel()
	skipUnlessByteExactVolume(t)

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "x1"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "x2"), 0o700))

	onDisk, err := tree.MkdirExact("X2")

	require.NoError(t, err)
	assert.Equal(t, "X2", onDisk)
	assert.ElementsMatch(t, []string{"x1", "x2", "X2"}, namesIn(t, dir))
	_, found, err := tree.NormalizationTwin("X3")
	require.NoError(t, err)
	assert.False(t, found)
}

// A file, or a symbolic link, at the name is not a folder the create can
// adopt.
//
// Validates: R-2.12.10, R-6.4.8
func TestRoot_MkdirExact_RefusesAFileOrALinkAtTheName(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "X2"), []byte("keep"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0o700))
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "link")))

	_, fileErr := tree.MkdirExact("X2")
	_, linkErr := tree.MkdirExact("link")

	require.ErrorIs(t, fileErr, synctree.ErrTargetExists)
	require.ErrorIs(t, linkErr, synctree.ErrTargetExists)
	content, err := os.ReadFile(filepath.Join(dir, "X2")) //nolint:gosec // the test reads a file it created
	require.NoError(t, err)
	assert.Equal(t, "keep", string(content))
}

// Validates: R-2.4.6, R-6.2
func TestRoot_MkdirExact_RefusesASymlinkedParent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	outside := t.TempDir()
	tree, err := synctree.Open(dir)
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "real"), 0o700))
	require.NoError(t, os.Symlink("real", filepath.Join(dir, "inner")))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "alias")))

	_, innerErr := tree.MkdirExact(filepath.Join("inner", "sub"))
	_, aliasErr := tree.MkdirExact(filepath.Join("alias", "sub"))

	require.ErrorIs(t, innerErr, synctree.ErrUnsafePath)
	require.ErrorIs(t, aliasErr, synctree.ErrUnsafePath)
	assert.NoDirExists(t, filepath.Join(dir, "real", "sub"))
	assert.NoDirExists(t, filepath.Join(outside, "sub"))
}

// HoldsName tells the entry that is a name from a case variant an lstat
// finds on a volume that folds case, and finds the NFD spelling of a stored
// NFC name.
//
// Validates: R-2.12.10, R-2.13.2
func TestRoot_HoldsName_TellsTheNameFromACaseVariant(t *testing.T) {
	t.Parallel()

	tree, dir := caseInsensitiveTree(t)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "Shared", "x2"), 0o700))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "Shared", norm.NFD.String("Café")), 0o700))

	_, err := tree.Lstat(filepath.Join("Shared", "X2"))
	require.NoError(t, err, "the volume finds the case variant")

	for rel, want := range map[string]bool{
		filepath.Join("Shared", "x2"):                    true,
		filepath.Join("Shared", "X2"):                    false,
		filepath.Join("Shared", norm.NFC.String("Café")): true,
		filepath.Join("Missing", "X2"):                   false,
	} {
		holds, err := tree.HoldsName(rel)
		require.NoError(t, err, rel)
		assert.Equal(t, want, holds, rel)
	}
}
