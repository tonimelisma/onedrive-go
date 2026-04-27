package sync

type ShortcutRootStatusMetadata struct {
	DisplayState   string
	StateReason    string
	Issue          string
	RecoveryAction string
	AutoRetry      bool
	ProtectsPath   bool
}

func ShortcutRootStatus(state ShortcutRootState) ShortcutRootStatusMetadata {
	if state == "" || state == ShortcutRootStateActive {
		return ShortcutRootStatusMetadata{}
	}
	if entry, ok := shortcutRootStatusMetadataTable()[state]; ok {
		return entry
	}
	return ShortcutRootStatusMetadata{
		DisplayState: string(state),
		StateReason:  string(state),
		Issue:        "The shortcut alias is waiting for parent-engine recovery.",
		AutoRetry:    true,
		ProtectsPath: true,
	}
}

func shortcutRootStatusMetadataTable() map[ShortcutRootState]ShortcutRootStatusMetadata {
	return map[ShortcutRootState]ShortcutRootStatusMetadata{
		ShortcutRootStateTargetUnavailable: {
			DisplayState:   string(ShortcutRootStateTargetUnavailable),
			StateReason:    string(ShortcutRootStateTargetUnavailable),
			Issue:          "The shortcut target is unavailable.",
			RecoveryAction: "Restore target access or remove the shortcut alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateLocalRootUnavailable: {
			DisplayState:   string(ShortcutRootStateLocalRootUnavailable),
			StateReason:    string(ShortcutRootStateLocalRootUnavailable),
			Issue:          "The shortcut alias local root is unavailable.",
			RecoveryAction: "Restore the local shortcut directory or delete it to discard unresolved local state.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateBlockedPath: {
			DisplayState:   string(ShortcutRootStateBlockedPath),
			StateReason:    string(ShortcutRootStateBlockedPath),
			Issue:          "The shortcut alias path is blocked.",
			RecoveryAction: "Clear the blocking local path.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRenameAmbiguous: {
			DisplayState:   string(ShortcutRootStateRenameAmbiguous),
			StateReason:    string(ShortcutRootStateRenameAmbiguous),
			Issue:          "Multiple same-folder shortcut alias rename candidates were found.",
			RecoveryAction: "Keep exactly one renamed shortcut alias or restore the original name.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateAliasMutationBlocked: {
			DisplayState:   string(ShortcutRootStateAliasMutationBlocked),
			StateReason:    string(ShortcutRootStateAliasMutationBlocked),
			Issue:          "The parent engine cannot update the shortcut alias in OneDrive.",
			RecoveryAction: "Fix account, network, or permission access, or restore the local alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedFinalDrain: {
			DisplayState:   string(ShortcutRootStateRemovedFinalDrain),
			StateReason:    string(ShortcutRootStateRemovedFinalDrain),
			Issue:          "The shortcut alias was removed; child sync is finishing before release.",
			RecoveryAction: "Restore shared-folder access, or delete the local shortcut directory to discard dirty local state.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedReleasePending: {
			DisplayState: string(ShortcutRootStateRemovedReleasePending),
			StateReason:  string(ShortcutRootStateRemovedReleasePending),
			Issue:        "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
			AutoRetry:    true,
			ProtectsPath: true,
		},
		ShortcutRootStateRemovedCleanupBlocked: {
			DisplayState:   string(ShortcutRootStateRemovedCleanupBlocked),
			StateReason:    string(ShortcutRootStateRemovedCleanupBlocked),
			Issue:          "The parent engine cannot release the protected shortcut alias path.",
			RecoveryAction: "Clear the local filesystem blocker.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedChildCleanupPending: {
			DisplayState: string(ShortcutRootStateRemovedChildCleanupPending),
			StateReason:  string(ShortcutRootStateRemovedChildCleanupPending),
			Issue:        "The shortcut alias was released; child cleanup is finishing.",
			AutoRetry:    true,
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			DisplayState: string(ShortcutRootStateSamePathReplacementWaiting),
			StateReason:  string(ShortcutRootStateSamePathReplacementWaiting),
			Issue:        "A new shortcut is waiting for the old child sync to finish.",
			AutoRetry:    true,
			ProtectsPath: true,
		},
		ShortcutRootStateDuplicateTarget: {
			DisplayState:   string(ShortcutRootStateDuplicateTarget),
			StateReason:    string(ShortcutRootStateDuplicateTarget),
			Issue:          "Another shortcut alias in this parent already projects the same target.",
			RecoveryAction: "Remove or rename the duplicate shortcut alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
	}
}
