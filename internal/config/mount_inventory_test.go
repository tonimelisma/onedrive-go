package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMountInventory_RoundTrip(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		SchemaVersion: mountsSchemaV1,
		Mounts: map[string]MountRecord{
			"child-docs": {
				MountID:           "child-docs",
				ParentMountID:     "personal:owner@example.com",
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
	require.Len(t, loaded.Mounts, 1)

	record := loaded.Mounts["child-docs"]
	assert.Equal(t, "child-docs", record.MountID)
	assert.Equal(t, "personal:owner@example.com", record.ParentMountID)
	assert.Equal(t, "Team/Docs", record.RelativeLocalPath)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Equal(t, "remote-root", record.RemoteRootItemID)
	assert.True(t, record.Paused)
}

func TestMountInventory_UnknownFieldRejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 1,
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
				RelativeLocalPath: "Shared/Docs",
				RemoteDriveID:     "drive-a",
				RemoteRootItemID:  "root-a",
			},
			"nested": {
				MountID:           "nested",
				ParentMountID:     "parent",
				RelativeLocalPath: "Shared/Docs/Deep",
				RemoteDriveID:     "drive-b",
				RemoteRootItemID:  "root-b",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nested child paths")
}

func TestMountStatePath_UsesManagedMountPrefix(t *testing.T) {
	dataDir := setTestDataDir(t)

	path := MountStatePath("parent/shared:docs")
	require.NotEmpty(t, path)
	assert.Equal(t, filepath.Join(dataDir, "state_mount_parent_shared_docs.db"), path)
}
