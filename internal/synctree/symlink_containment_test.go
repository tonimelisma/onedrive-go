package synctree

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testFixedTimeUnix keeps Chtimes assertions deterministic.
const testFixedTimeUnix = 1700000000

func testFixedTime() time.Time { return time.Unix(testFixedTimeUnix, 0) }

// escapeFixture builds a sync root containing "alias", a symlink to a
// directory OUTSIDE the root. Mutating anything under "alias/" is lexically
// inside the root but physically outside it, which is the escape these tests
// pin closed.
type escapeFixture struct {
	root    *Root
	rootDir string
	outside string
}

func newEscapeFixture(t *testing.T) escapeFixture {
	t.Helper()

	base := t.TempDir()
	rootDir := filepath.Join(base, "syncroot")
	outside := filepath.Join(base, "outside")
	require.NoError(t, os.MkdirAll(rootDir, 0o700))
	require.NoError(t, os.MkdirAll(outside, 0o700))

	if err := os.Symlink(outside, filepath.Join(rootDir, "alias")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	root, err := Open(rootDir)
	require.NoError(t, err)

	return escapeFixture{root: root, rootDir: rootDir, outside: outside}
}

func (f escapeFixture) writeOutside(t *testing.T, name, content string) {
	t.Helper()

	path := filepath.Join(f.outside, name)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
}

func (f escapeFixture) requireOutsideContent(t *testing.T, name, want string) {
	t.Helper()

	//nolint:gosec // test fixture reads a path it just created under t.TempDir.
	got, err := os.ReadFile(filepath.Join(f.outside, name))
	require.NoError(t, err, "file outside the sync root must survive")
	assert.Equal(t, want, string(got), "content outside the sync root must be unmodified")
}

// Validates: R-2.4.6, R-6.2
func TestRoot_Rename_ThroughSymlinkedAncestorFailsAndPreservesTarget(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "src.txt", "SOURCE")
	f.writeOutside(t, "victim.txt", "PRECIOUS")

	err := f.root.Rename("alias/src.txt", "alias/victim.txt")
	require.Error(t, err, "rename through a symlinked ancestor must fail closed")

	f.requireOutsideContent(t, "victim.txt", "PRECIOUS")
	f.requireOutsideContent(t, "src.txt", "SOURCE")
}

// Validates: R-2.4.6, R-6.2
func TestRoot_MkdirAll_ThroughSymlinkedAncestorFails(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	err := f.root.MkdirAll("alias/created", 0o700)
	require.Error(t, err, "mkdirall through a symlinked ancestor must fail closed")
	assert.NoDirExists(t, filepath.Join(f.outside, "created"))
}

// Validates: R-2.4.6, R-6.2
func TestRoot_RemoveAll_ThroughSymlinkedAncestorFails(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "tree/file.txt", "PRECIOUS")

	err := f.root.RemoveAll("alias/tree")
	require.Error(t, err, "removeall through a symlinked ancestor must fail closed")
	f.requireOutsideContent(t, "tree/file.txt", "PRECIOUS")
}

// Validates: R-2.4.6, R-6.2
func TestRoot_Remove_ThroughSymlinkedAncestorFails(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "file.txt", "PRECIOUS")

	err := f.root.Remove("alias/file.txt")
	require.Error(t, err, "remove through a symlinked ancestor must fail closed")
	f.requireOutsideContent(t, "file.txt", "PRECIOUS")
}

// Validates: R-2.4.6, R-6.2
func TestRoot_Chtimes_ThroughSymlinkedAncestorFails(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)
	f.writeOutside(t, "file.txt", "PRECIOUS")

	err := f.root.Chtimes("alias/file.txt", testFixedTime(), testFixedTime())
	require.Error(t, err, "chtimes through a symlinked ancestor must fail closed")
}

// Validates: R-2.4.6, R-6.2
//
// A rooted stat that fails because the path escapes the root must never be
// reported as os.ErrNotExist: callers branch on ErrNotExist to mean "nothing
// is there, safe to create", which turns a containment failure into a write
// outside the sync root.
func TestRoot_Lstat_ThroughSymlinkedAncestorIsNotReportedAsNotExist(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	_, err := f.root.Lstat("alias/missing.txt")
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist,
		"containment failure must not be normalized into ErrNotExist")
	assert.ErrorIs(t, err, ErrUnsafePath)
}

// Validates: R-2.4.6, R-6.2
func TestRoot_Stat_ThroughSymlinkedAncestorIsNotReportedAsNotExist(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	_, err := f.root.Stat("alias/missing.txt")
	require.Error(t, err)
	require.NotErrorIs(t, err, os.ErrNotExist)
	assert.ErrorIs(t, err, ErrUnsafePath)
}

// Validates: R-2.4.6, R-6.2
func TestRoot_OpenFile_ThroughSymlinkedAncestorFails(t *testing.T) {
	t.Parallel()

	f := newEscapeFixture(t)

	file, err := f.root.OpenFile("alias/new.txt", os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		require.NoError(t, file.Close())
	}
	require.Error(t, err, "creating a file through a symlinked ancestor must fail closed")
	assert.NoFileExists(t, filepath.Join(f.outside, "new.txt"))
}

// Validates: R-2.4.6, R-6.2
//
// normalizeNotExist consults ValidateNoSymlinkAncestors, which for a depth-1
// path validates the sync root itself. This pins that the call terminates
// instead of recursing through validateRootDirectoryNoFollow.
func TestRoot_NormalizeNotExist_DepthOnePathTerminates(t *testing.T) {
	t.Parallel()

	root, err := Open(t.TempDir())
	require.NoError(t, err)

	_, err = root.Lstat("missing.txt")
	require.Error(t, err)
	assert.ErrorIs(t, err, os.ErrNotExist,
		"a genuinely absent depth-1 path must still normalize to ErrNotExist")
}

// Validates: R-2.4.6, R-6.2
//
// Containment must not regress ordinary in-root behavior.
func TestRoot_MutationsWithinRootStillSucceed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, root.MkdirAll("a/b", 0o700))
	assert.DirExists(t, filepath.Join(dir, "a", "b"))

	require.NoError(t, os.WriteFile(filepath.Join(dir, "a", "b", "f.txt"), []byte("data"), 0o600))

	require.NoError(t, root.Rename("a/b/f.txt", "a/b/g.txt"))
	assert.FileExists(t, filepath.Join(dir, "a", "b", "g.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "a", "b", "f.txt"))

	require.NoError(t, root.Chtimes("a/b/g.txt", testFixedTime(), testFixedTime()))

	require.NoError(t, root.Remove("a/b/g.txt"))
	assert.NoFileExists(t, filepath.Join(dir, "a", "b", "g.txt"))

	require.NoError(t, root.RemoveAll("a"))
	assert.NoDirExists(t, filepath.Join(dir, "a"))
}

// Validates: R-2.4.6, R-6.2
//
// A symlink that stays inside the sync root is ordinary content: the rooted
// boundary constrains escapes, not symlinks as such.
func TestRoot_MutationsThroughInRootSymlinkSucceed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "real"), 0o700))
	if err := os.Symlink("real", filepath.Join(dir, "inner")); err != nil {
		t.Skipf("symlink not available on this filesystem: %v", err)
	}

	root, err := Open(dir)
	require.NoError(t, err)

	require.NoError(t, root.MkdirAll("inner/sub", 0o700))
	assert.DirExists(t, filepath.Join(dir, "real", "sub"))
}
