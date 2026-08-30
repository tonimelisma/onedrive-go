package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.8
func TestMountStatePath_UsesManagedMountPrefix(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)
	path := MountStatePath("parent/shared:docs")

	assert.True(t, strings.HasPrefix(filepath.Base(path), stateMountPrefix))
	assert.True(t, strings.HasSuffix(path, ".db"))
	assert.Equal(t, DefaultDataDir(), filepath.Dir(path))
}

// Validates: R-2.4.8
func TestMountStatePath_EncodesManagedMountIDWithoutCollisions(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	paths := []string{
		MountStatePath("parent/a/b"),
		MountStatePath("parent/a:b"),
		MountStatePath("parent/a|b"),
		MountStatePath("parent/a b"),
	}
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		require.NotEmpty(t, path)
		assert.NotContains(t, filepath.Base(path), "/")
		assert.NotContains(t, filepath.Base(path), ":")
		assert.NotContains(t, filepath.Base(path), "|")
		if _, found := seen[path]; found {
			require.Fail(t, "duplicate state path", path)
		}
		seen[path] = struct{}{}
	}
}

// Validates: R-2.4.8
func TestMountStatePath_LongIDUsesBoundedFilename(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataDir)

	path := MountStatePath(strings.Repeat("parent/binding:", 100))

	assert.LessOrEqual(t, len(filepath.Base(path)), len(stateMountPrefix)+43+len(".db"))
}

// Validates: R-2.4.8
func TestMountStatePath_EmptyIDReturnsEmpty(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	assert.Empty(t, MountStatePath(""))
}

// Validates: R-2.4.8
func TestChildMountID_UsesParentAndBinding(t *testing.T) {
	assert.Equal(t, "parent|binding:shortcut-item", ChildMountID("parent", "shortcut-item"))
	assert.Empty(t, ChildMountID("", "shortcut-item"))
	assert.Empty(t, ChildMountID("parent", ""))
}

// Validates: R-2.2, R-4.1
//
// The mount-ref digest names a child mount's state database on disk. Changing
// how the input is derived would rename existing state files and orphan their
// contents, so the exact bytes hashed are pinned here.
func TestMountRef_MatchesStateFileDigest(t *testing.T) {
	t.Parallel()

	const mountID = "personal:user@example.com|binding:ABC!s123"

	ref := MountRef(mountID)
	require.NotEmpty(t, ref)

	path := MountStatePathForDataDir("/data", mountID)
	assert.Equal(t, "/data/state_mount_"+ref+".db", path,
		"the state file name must remain the mount ref digest")
}

// Validates: R-4.1
func TestMountRef_IsStableUniqueAndOpaque(t *testing.T) {
	t.Parallel()

	const mountID = "personal:user@example.com|binding:ABC!s123"

	ref := MountRef(mountID)
	assert.Equal(t, ref, MountRef(mountID), "ref must be stable across calls")
	assert.NotEqual(t, ref, MountRef(mountID+"x"), "distinct mounts must not collide")

	// The point of the ref is that it reveals none of its inputs.
	assert.NotContains(t, ref, "user@example.com")
	assert.NotContains(t, ref, "ABC!s123")
	assert.NotContains(t, ref, "personal:")
}

// Validates: R-4.1
func TestMountRef_EmptyForBlankMountID(t *testing.T) {
	t.Parallel()

	assert.Empty(t, MountRef(""))
	assert.Empty(t, MountRef("   "))
}
