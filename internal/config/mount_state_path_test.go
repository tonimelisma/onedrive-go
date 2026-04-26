package config

import (
	"os"
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

// Validates: R-2.4.8
func TestPurgeManagedChildMountArtifacts_RemovesStateFamilyAndChildCatalogRecord(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	childID := ChildMountID("parent", "shortcut-item")
	statePath := MountStatePath(childID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	for _, path := range []string{statePath, statePath + "-wal", statePath + "-shm", statePath + "-journal"} {
		require.NoError(t, os.WriteFile(path, []byte("state"), 0o600))
	}
	require.NoError(t, UpdateCatalog(func(catalog *Catalog) error {
		catalog.Drives[childID] = CatalogDrive{CanonicalID: childID, DisplayName: "Managed child"}
		catalog.Drives["business:user@example.com"] = CatalogDrive{
			CanonicalID: "business:user@example.com",
			DisplayName: "Explicit shared drive",
		}
		return nil
	}))

	require.NoError(t, PurgeManagedChildMountArtifacts(childID))

	for _, path := range []string{statePath, statePath + "-wal", statePath + "-shm", statePath + "-journal"} {
		assert.NoFileExists(t, path)
	}
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	assert.NotContains(t, catalog.Drives, childID)
	assert.Contains(t, catalog.Drives, "business:user@example.com")
}

// Validates: R-2.4.8
func TestPurgeManagedChildMountArtifacts_IgnoresExplicitMountID(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	mountID := "business:user@example.com"
	statePath := MountStatePath(mountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	require.NoError(t, UpdateCatalog(func(catalog *Catalog) error {
		catalog.Drives[mountID] = CatalogDrive{CanonicalID: mountID, DisplayName: "Explicit shared drive"}
		return nil
	}))

	require.NoError(t, PurgeManagedChildMountArtifacts(mountID))

	assert.FileExists(t, statePath)
	catalog, err := LoadCatalog()
	require.NoError(t, err)
	assert.Contains(t, catalog.Drives, mountID)
}
