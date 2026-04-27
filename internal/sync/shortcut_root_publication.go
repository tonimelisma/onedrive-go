package sync

func shortcutChildRunnerPublicationFromRoots(namespaceID string, roots []ShortcutRootRecord) ShortcutChildRunnerPublication {
	snapshot := ShortcutChildRunnerPublication{
		NamespaceID: namespaceID,
		Children:    make([]ShortcutChildRunner, 0, len(roots)),
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		if root.State == ShortcutRootStateRemovedChildCleanupPending {
			snapshot.CleanupRequests = append(snapshot.CleanupRequests, ShortcutChildArtifactCleanupRequest{
				BindingItemID:     root.BindingItemID,
				RelativeLocalPath: root.RelativeLocalPath,
				Reason:            ShortcutChildArtifactCleanupParentRemoved,
			})
			continue
		}
		child := ShortcutChildRunner{
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			LocalAlias:        root.LocalAlias,
			RemoteDriveID:     root.RemoteDriveID.String(),
			RemoteItemID:      root.RemoteItemID,
			RemoteIsFolder:    root.RemoteIsFolder,
			RunnerAction:      shortcutChildRunnerActionForRoot(root.State),
			RunnerDetail:      root.BlockedDetail,
			LocalRootIdentity: shortcutRootIdentityFromFileIdentity(root.LocalRootIdentity),
		}
		snapshot.Children = append(snapshot.Children, child)
	}
	return snapshot
}

func shortcutChildRunnerActionForRoot(state ShortcutRootState) ShortcutChildRunnerAction {
	switch state {
	case "", ShortcutRootStateActive:
		return ShortcutChildActionRun
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateSamePathReplacementWaiting:
		return ShortcutChildActionFinalDrain
	case ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateDuplicateTarget:
		return ShortcutChildActionSkipParentBlocked
	default:
		return ShortcutChildActionSkipParentBlocked
	}
}
