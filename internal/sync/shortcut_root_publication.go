package sync

func shortcutChildTopologyFromRoots(namespaceID string, roots []ShortcutRootRecord) ShortcutChildTopologySnapshot {
	snapshot := ShortcutChildTopologySnapshot{
		NamespaceID: namespaceID,
		Children:    make([]ShortcutChildTopology, 0, len(roots)),
	}
	for i := range roots {
		root := normalizeShortcutRootRecord(roots[i])
		if root.NamespaceID != "" && root.NamespaceID != namespaceID {
			continue
		}
		if root.State == ShortcutRootStateRemovedChildCleanupPending {
			snapshot.CleanupRequests = append(snapshot.CleanupRequests, ShortcutChildArtifactCleanupRequest{
				BindingItemID: root.BindingItemID,
				Reason:        ShortcutChildArtifactCleanupParentRemoved,
			})
			continue
		}
		child := ShortcutChildTopology{
			BindingItemID:     root.BindingItemID,
			RelativeLocalPath: root.RelativeLocalPath,
			LocalAlias:        root.LocalAlias,
			RemoteDriveID:     root.RemoteDriveID.String(),
			RemoteItemID:      root.RemoteItemID,
			RemoteIsFolder:    root.RemoteIsFolder,
			RunnerAction:      shortcutChildRunnerActionForRoot(root.State),
			BlockedDetail:     root.BlockedDetail,
			ProtectedPaths:    protectedPathsForShortcutRoot(root.RelativeLocalPath, root.ProtectedPaths),
			LocalRootIdentity: shortcutRootIdentityFromFileIdentity(root.LocalRootIdentity),
		}
		if root.Waiting != nil {
			waiting := shortcutChildTopologyFromReplacement(*root.Waiting)
			child.Waiting = &waiting
		}
		snapshot.Children = append(snapshot.Children, child)
	}
	return snapshot
}

func shortcutChildTopologyFromReplacement(replacement ShortcutRootReplacement) ShortcutChildTopology {
	return ShortcutChildTopology{
		BindingItemID:     replacement.BindingItemID,
		RelativeLocalPath: replacement.RelativeLocalPath,
		LocalAlias:        replacement.LocalAlias,
		RemoteDriveID:     replacement.RemoteDriveID.String(),
		RemoteItemID:      replacement.RemoteItemID,
		RemoteIsFolder:    replacement.RemoteIsFolder,
		RunnerAction:      ShortcutChildActionSkipWaitingReplacement,
		ProtectedPaths:    []string{replacement.RelativeLocalPath},
	}
}

func shortcutChildRunnerActionForRoot(state ShortcutRootState) ShortcutChildRunnerAction {
	switch state {
	case "", ShortcutRootStateActive:
		return ShortcutChildActionRun
	case ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateSamePathReplacementWaiting:
		return ShortcutChildActionFinalDrain
	case ShortcutRootStateTargetUnavailable,
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
