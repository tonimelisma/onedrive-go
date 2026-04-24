package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMountInventory_RoundTrip(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		SchemaVersion: mountsSchemaV2,
		Parents: map[string]ParentDiscoveryState{
			"personal:owner@example.com": {
				ParentMountID: "personal:owner@example.com",
				DeltaLink:     "https://graph.microsoft.com/v1.0/drives/drive/root/delta?token=abc",
				DiscoveryMode: DiscoveryModeDelta,
			},
		},
		Mounts: map[string]MountRecord{
			"child-docs": {
				MountID:           "child-docs",
				ParentMountID:     "personal:owner@example.com",
				BindingItemID:     "placeholder-item-id",
				DisplayName:       "Docs",
				RelativeLocalPath: "Team/Docs",
				RemoteDriveID:     "remote-drive",
				RemoteRootItemID:  "remote-root",
				Paused:            true,
			},
		},
	})
	require.NoError(t, err)

	loaded, err := LoadMountInventory()
	require.NoError(t, err)
	assert.Equal(t, mountsSchemaV2, loaded.SchemaVersion)
	require.Len(t, loaded.Mounts, 1)
	require.Len(t, loaded.Parents, 1)

	record := loaded.Mounts["child-docs"]
	assert.Equal(t, "child-docs", record.MountID)
	assert.Equal(t, "personal:owner@example.com", record.ParentMountID)
	assert.Equal(t, "placeholder-item-id", record.BindingItemID)
	assert.Equal(t, "Team/Docs", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Equal(t, "remote-root", record.RemoteRootItemID)
	assert.True(t, record.Paused)
	assert.Equal(t, ParentDiscoveryState{
		ParentMountID: "personal:owner@example.com",
		DeltaLink:     "https://graph.microsoft.com/v1.0/drives/drive/root/delta?token=abc",
		DiscoveryMode: DiscoveryModeDelta,
	}, loaded.Parents["personal:owner@example.com"])
}

func TestMountInventory_UnknownFieldRejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 2,
  "parents": {},
  "mounts": {},
  "unknown": true
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestMountInventory_InvalidRelativePathRejected(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"bad": {
				MountID:           "bad",
				ParentMountID:     "parent",
				BindingItemID:     "binding",
				RelativeLocalPath: "../escape",
				RemoteDriveID:     "remote-drive",
				RemoteRootItemID:  "remote-root",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "relative_local_path")
}

func TestMountInventory_NestedSiblingPathsRejected(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:           "docs",
				ParentMountID:     "parent",
				BindingItemID:     "binding-a",
				RelativeLocalPath: "Shared/Docs",
				RemoteDriveID:     "drive-a",
				RemoteRootItemID:  "root-a",
			},
			"nested": {
				MountID:           "nested",
				ParentMountID:     "parent",
				BindingItemID:     "binding-b",
				RelativeLocalPath: "Shared/Docs/Deep",
				RemoteDriveID:     "drive-b",
				RemoteRootItemID:  "root-b",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested child paths")
}

func TestMountInventory_BindingItemIDRequired(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:           "docs",
				ParentMountID:     "parent",
				RelativeLocalPath: "Shared/Docs",
				RemoteDriveID:     "drive-a",
				RemoteRootItemID:  "root-a",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binding_item_id")
}

func TestLoadMountInventory_V1MovesAsideAndReturnsEmptyV2(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 1,
  "mounts": {
    "child-docs": {
      "mount_id": "child-docs",
      "parent_mount_id": "personal:owner@example.com",
      "display_name": "Docs",
      "relative_local_path": "Team/Docs",
      "remote_drive_id": "remote-drive",
      "remote_root_item_id": "remote-root"
    }
  }
}`), 0o600))

	loaded, err := LoadMountInventoryForDataDir(dataDir)
	require.NoError(t, err)
	assert.Equal(t, mountsSchemaV2, loaded.SchemaVersion)
	assert.Empty(t, loaded.Mounts)
	assert.Empty(t, loaded.Parents)

	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)

	backupPath := path + ".v1.bak"
	assert.FileExists(t, backupPath)
}

// Validates: R-2.8.1
func TestMountStatePath_UsesManagedMountPrefix(t *testing.T) {
	dataDir := setTestDataDir(t)

	path := MountStatePath("parent/shared:docs")
	require.NotEmpty(t, path)
	assert.Equal(t, dataDir, filepath.Dir(path))
	base := filepath.Base(path)
	assert.True(t, strings.HasPrefix(base, stateMountPrefix))
	assert.True(t, strings.HasSuffix(base, ".db"))
	assert.Len(t, base, len(stateMountPrefix)+43+len(".db"))
	assert.LessOrEqual(t, len(base), 255)
}

// Validates: R-2.8.1
func TestMountStatePath_EncodesManagedMountIDWithoutCollisions(t *testing.T) {
	dataDir := setTestDataDir(t)

	paths := []string{
		MountStatePath("parent/a/b"),
		MountStatePath("parent/a:b"),
		MountStatePath("parent/a|b"),
		MountStatePath("parent/a b"),
	}

	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		require.NotEmpty(t, path)
		assert.Equal(t, dataDir, filepath.Dir(path))
		base := filepath.Base(path)
		assert.True(t, strings.HasPrefix(base, stateMountPrefix))
		assert.True(t, strings.HasSuffix(base, ".db"))
		if _, exists := seen[path]; exists {
			assert.Failf(t, "duplicate state path", "path %q should be unique", path)
		}
		seen[path] = struct{}{}
	}
}

// Validates: R-2.8.1
func TestMountStatePath_LongIDUsesBoundedFilename(t *testing.T) {
	dataDir := setTestDataDir(t)

	path := MountStatePath(strings.Repeat("parent/binding:", 100))
	require.NotEmpty(t, path)
	assert.Equal(t, dataDir, filepath.Dir(path))
	base := filepath.Base(path)
	assert.Len(t, base, len(stateMountPrefix)+43+len(".db"))
	assert.LessOrEqual(t, len(base), 255)
}

// Validates: R-2.8.1
func TestMountStatePath_EmptyIDReturnsEmpty(t *testing.T) {
	setTestDataDir(t)

	assert.Empty(t, MountStatePath(""))
}
