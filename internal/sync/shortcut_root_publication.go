package sync

import (
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func shortcutChildProcessSnapshotFromRootsWithParentRoot(
	namespaceID string,
	parentSyncRoot string,
	roots []ShortcutRootRecord,
) ShortcutChildProcessSnapshot {
	snapshot := ShortcutChildProcessSnapshot{
		NamespaceID: namespaceID,
		RunCommands: make([]ShortcutChildRunCommand, 0, len(roots)),
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		metadata, _ := shortcutRootLifecycleMetadataFor(root.State)
		if metadata.publishesCleanup {
			childMountID := config.ChildMountID(namespaceID, root.BindingItemID)
			snapshot.Cleanups = append(snapshot.Cleanups, ShortcutChildCleanupCommand{
				ChildMountID: childMountID,
				LocalRoot:    shortcutChildCleanupLocalRoot(parentSyncRoot, root.RelativeLocalPath),
				Reason:       ShortcutChildArtifactCleanupParentRemoved,
				AckRef:       NewShortcutChildAckRef(root.BindingItemID),
			})
			continue
		}
		if metadata.runMode == "" {
			continue
		}
		child := ShortcutChildRunCommand{
			ChildMountID: config.ChildMountID(namespaceID, root.BindingItemID),
			DisplayName:  root.LocalAlias,
			Engine: ShortcutChildEngineSpec{
				LocalRoot:         shortcutChildLocalRoot(parentSyncRoot, root.RelativeLocalPath),
				RemoteDriveID:     root.RemoteDriveID.String(),
				RemoteItemID:      root.RemoteItemID,
				LocalRootIdentity: shortcutRootIdentityFromFileIdentity(root.LocalRootIdentity),
			},
			Mode:   metadata.runMode,
			AckRef: NewShortcutChildAckRef(root.BindingItemID),
		}
		snapshot.RunCommands = append(snapshot.RunCommands, child)
	}
	return snapshot
}

func shortcutChildCleanupLocalRoot(parentSyncRoot string, relativeLocalPath string) string {
	return shortcutChildLocalRoot(parentSyncRoot, relativeLocalPath)
}

func shortcutChildLocalRoot(parentSyncRoot string, relativeLocalPath string) string {
	if parentSyncRoot == "" || relativeLocalPath == "" {
		return ""
	}
	return filepath.Join(parentSyncRoot, filepath.FromSlash(relativeLocalPath))
}

func shortcutChildRunModeForRoot(state ShortcutRootState) ShortcutChildRunMode {
	metadata, ok := shortcutRootLifecycleMetadataFor(state)
	if !ok {
		return ""
	}
	return metadata.runMode
}
