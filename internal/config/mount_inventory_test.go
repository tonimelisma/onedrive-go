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
		SchemaVersion: mountsSchemaV6,
		Namespaces: map[string]NamespaceDiscoveryState{
			"personal:owner@example.com": {
				NamespaceID:   "personal:owner@example.com",
				DeltaLink:     "https://graph.microsoft.com/v1.0/drives/drive/root/delta?token=abc",
				DiscoveryMode: DiscoveryModeDelta,
			},
		},
		Mounts: map[string]MountRecord{
			"child-docs": {
				MountID:               "child-docs",
				NamespaceID:           "personal:owner@example.com",
				BindingItemID:         "placeholder-item-id",
				LocalAlias:            "Docs",
				RelativeLocalPath:     "Team/Docs",
				ReservedLocalPaths:    []string{"Team Docs"},
				LocalRootMaterialized: true,
				LocalRootIdentity:     &RootIdentity{Device: 42, Inode: 99},
				TokenOwnerCanonical:   "personal:owner@example.com",
				RemoteDriveID:         "remote-drive",
				RemoteItemID:          "remote-root",
				State:                 MountStateConflict,
				StateReason:           MountStateReasonDuplicateContentRoot,
			},
		},
	})
	require.NoError(t, err)

	loaded, err := LoadMountInventory()
	require.NoError(t, err)
	assert.Equal(t, mountsSchemaV6, loaded.SchemaVersion)
	require.Len(t, loaded.Mounts, 1)
	require.Len(t, loaded.Namespaces, 1)

	record := loaded.Mounts["child-docs"]
	assert.Equal(t, "child-docs", record.MountID)
	assert.Equal(t, "personal:owner@example.com", record.NamespaceID)
	assert.Equal(t, "placeholder-item-id", record.BindingItemID)
	assert.Equal(t, "Docs", record.LocalAlias)
	assert.Equal(t, "Team/Docs", record.RelativeLocalPath)
	assert.Equal(t, []string{"Team Docs"}, record.ReservedLocalPaths)
	assert.True(t, record.LocalRootMaterialized)
	assert.Equal(t, &RootIdentity{Device: 42, Inode: 99}, record.LocalRootIdentity)
	assert.Equal(t, "personal:owner@example.com", record.TokenOwnerCanonical)
	assert.Equal(t, "remote-drive", record.RemoteDriveID)
	assert.Equal(t, "remote-root", record.RemoteItemID)
	assert.Equal(t, MountStateConflict, record.State)
	assert.Equal(t, MountStateReasonDuplicateContentRoot, record.StateReason)
	assert.Equal(t, NamespaceDiscoveryState{
		NamespaceID:   "personal:owner@example.com",
		DeltaLink:     "https://graph.microsoft.com/v1.0/drives/drive/root/delta?token=abc",
		DiscoveryMode: DiscoveryModeDelta,
	}, loaded.Namespaces["personal:owner@example.com"])
}

func TestMountInventory_UnknownFieldRejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 6,
  "namespaces": {},
  "mounts": {},
  "unknown": true
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestMountInventory_ChildPauseFieldRejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 6,
  "mounts": {
    "child-docs": {
      "mount_id": "child-docs",
      "namespace_id": "personal:owner@example.com",
      "binding_item_id": "binding-a",
      "relative_local_path": "Shortcuts/Docs",
      "token_owner_canonical": "personal:owner@example.com",
      "remote_drive_id": "remote-drive",
      "remote_item_id": "remote-root",
      "paused": true
    }
  }
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown field")
}

func TestMountInventory_EmptyMountStateNormalizesActive(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
			},
		},
	})
	require.NoError(t, err)

	loaded, err := LoadMountInventory()
	require.NoError(t, err)
	assert.Equal(t, MountStateActive, loaded.Mounts["docs"].State)
}

func TestMountInventory_InvalidStateRejected(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
				State:               MountState("mystery"),
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported state")
}

func TestMountInventory_TokenOwnerCanonicalRequiredAndValidated(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:           "docs",
				NamespaceID:       "personal:owner@example.com",
				BindingItemID:     "binding-a",
				RelativeLocalPath: "Shared/Docs",
				RemoteDriveID:     "drive-a",
				RemoteItemID:      "root-a",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_owner_canonical")

	err = SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "bad",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "token_owner_canonical")
}

func TestMountInventory_InvalidRelativePathRejected(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"bad": {
				MountID:             "bad",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding",
				RelativeLocalPath:   "../escape",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "remote-drive",
				RemoteItemID:        "remote-root",
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
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
			},
			"nested": {
				MountID:             "nested",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-b",
				RelativeLocalPath:   "Shared/Docs/Deep",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-b",
				RemoteItemID:        "root-b",
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
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "binding_item_id")
}

func TestLoadMountInventory_OlderSchemaRejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 1,
  "mounts": {}
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 1")
	assert.FileExists(t, path)
}

// Validates: R-2.8.1, R-4.1.4
func TestLoadMountInventory_SchemaV3Rejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 3,
  "mounts": {}
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 3")
}

// Validates: R-2.8.1, R-4.1.4
func TestLoadMountInventory_SchemaV4Rejected(t *testing.T) {
	dataDir := setTestDataDir(t)
	path := MountsPathForDataDir(dataDir)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, []byte(`{
  "schema_version": 4,
  "mounts": {}
}`), 0o600))

	_, err := LoadMountInventoryForDataDir(dataDir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported schema version 4")
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_UnavailableShortcutBindingMayOmitRemoteTarget(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				State:               MountStateUnavailable,
				StateReason:         MountStateReasonShortcutBindingUnavailable,
			},
		},
	})
	require.NoError(t, err)

	loaded, err := LoadMountInventory()
	require.NoError(t, err)
	record := loaded.Mounts["docs"]
	assert.Equal(t, MountStateUnavailable, record.State)
	assert.Equal(t, MountStateReasonShortcutBindingUnavailable, record.StateReason)
	assert.Empty(t, record.RemoteDriveID)
	assert.Empty(t, record.RemoteItemID)
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_RemoteTargetRequiredForOtherLifecycleStates(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				State:               MountStateUnavailable,
				StateReason:         MountStateReasonLocalProjectionUnavailable,
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "remote_drive_id")
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_LocalRootReasonsValidate(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"collision": {
				MountID:             "collision",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Collision",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
				State:               MountStateConflict,
				StateReason:         MountStateReasonLocalRootCollision,
			},
			"unavailable": {
				MountID:             "unavailable",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-b",
				RelativeLocalPath:   "Shared/Unavailable",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-b",
				RemoteItemID:        "root-b",
				State:               MountStateUnavailable,
				StateReason:         MountStateReasonLocalRootUnavailable,
			},
			"rename-conflict": {
				MountID:             "rename-conflict",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-c",
				RelativeLocalPath:   "Shared/RenameConflict",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-c",
				RemoteItemID:        "root-c",
				State:               MountStateConflict,
				StateReason:         MountStateReasonLocalAliasRenameConflict,
			},
			"rename-unavailable": {
				MountID:             "rename-unavailable",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-d",
				RelativeLocalPath:   "Shared/RenameUnavailable",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-d",
				RemoteItemID:        "root-d",
				State:               MountStateUnavailable,
				StateReason:         MountStateReasonLocalAliasRenameUnavailable,
			},
			"delete-unavailable": {
				MountID:             "delete-unavailable",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-e",
				RelativeLocalPath:   "Shared/DeleteUnavailable",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-e",
				RemoteItemID:        "root-e",
				State:               MountStateUnavailable,
				StateReason:         MountStateReasonLocalAliasDeleteUnavailable,
			},
		},
	})
	require.NoError(t, err)
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_RejectsCaseFoldSiblingChildPathCollision(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"child-a": {
				MountID:             "child-a",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shortcuts/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
				State:               MountStateActive,
			},
			"child-b": {
				MountID:             "child-b",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-b",
				RelativeLocalPath:   "shortcuts/docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-b",
				RemoteItemID:        "root-b",
				State:               MountStateActive,
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "duplicate child path")
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_InvalidStateReasonRejected(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
				State:               MountStateActive,
				StateReason:         "mystery",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported state_reason")
}

// Validates: R-2.8.1, R-4.1.4
func TestMountInventory_ReservedLocalPathsNormalizeAndValidate(t *testing.T) {
	setTestDataDir(t)

	err := SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"docs": {
				MountID:             "docs",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-a",
				RelativeLocalPath:   "Shared/Docs",
				ReservedLocalPaths:  []string{" Shared/Old Docs ", "Shared/Old Docs", "Shared/Docs"},
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-a",
				RemoteItemID:        "root-a",
			},
		},
	})
	require.NoError(t, err)

	loaded, err := LoadMountInventory()
	require.NoError(t, err)
	assert.Equal(t, []string{"Shared/Old Docs"}, loaded.Mounts["docs"].ReservedLocalPaths)

	err = SaveMountInventory(&MountInventory{
		Mounts: map[string]MountRecord{
			"bad": {
				MountID:             "bad",
				NamespaceID:         "personal:owner@example.com",
				BindingItemID:       "binding-b",
				RelativeLocalPath:   "Shared/Bad",
				ReservedLocalPaths:  []string{"../escape"},
				TokenOwnerCanonical: "personal:owner@example.com",
				RemoteDriveID:       "drive-b",
				RemoteItemID:        "root-b",
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "reserved_local_paths")
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
