package sync

import (
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

func shortcutChildRunnerPublicationFromRootsWithParentRoot(
	namespaceID string,
	parentSyncRoot string,
	roots []ShortcutRootRecord,
) ShortcutChildRunnerPublication {
	snapshot := ShortcutChildRunnerPublication{
		NamespaceID: namespaceID,
		RunnerWork: ShortcutChildRunnerWork{
			Children: make([]ShortcutChildRunner, 0, len(roots)),
		},
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		metadata, _ := shortcutRootLifecycleMetadataFor(root.State)
		if metadata.publishesCleanup {
			childMountID := config.ChildMountID(namespaceID, root.BindingItemID)
			snapshot.CleanupWork.Requests = append(snapshot.CleanupWork.Requests, ShortcutChildArtifactCleanupRequest{
				BindingItemID:     root.BindingItemID,
				RelativeLocalPath: root.RelativeLocalPath,
				ChildMountID:      childMountID,
				LocalRoot:         shortcutChildCleanupLocalRoot(parentSyncRoot, root.RelativeLocalPath),
				Reason:            ShortcutChildArtifactCleanupParentRemoved,
			})
			continue
		}
		child := ShortcutChildRunner{
			ChildMountID:      config.ChildMountID(namespaceID, root.BindingItemID),
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			LocalRoot:         shortcutChildLocalRoot(parentSyncRoot, root.RelativeLocalPath),
			DisplayName:       root.LocalAlias,
			RemoteDriveID:     root.RemoteDriveID.String(),
			RemoteItemID:      root.RemoteItemID,
			RunnerAction:      shortcutChildRunnerActionForRoot(root.State),
			LocalRootIdentity: shortcutRootIdentityFromFileIdentity(root.LocalRootIdentity),
		}
		snapshot.RunnerWork.Children = append(snapshot.RunnerWork.Children, child)
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

func shortcutChildRunnerActionForRoot(state ShortcutRootState) ShortcutChildRunnerAction {
	metadata, ok := shortcutRootLifecycleMetadataFor(state)
	if !ok || metadata.runnerAction == "" {
		return ShortcutChildActionSkipParentBlocked
	}
	return metadata.runnerAction
}
