package sync

type ShortcutRootStatusMetadata struct {
	DisplayState   string
	Issue          string
	RecoveryAction string
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
		Issue:        "The shortcut alias is waiting for parent-engine recovery.",
		ProtectsPath: true,
	}
}

func shortcutRootStatusMetadataTable() map[ShortcutRootState]ShortcutRootStatusMetadata {
	return map[ShortcutRootState]ShortcutRootStatusMetadata{
		ShortcutRootStateTargetUnavailable: {
			DisplayState:   string(ShortcutRootStateTargetUnavailable),
			Issue:          "The shortcut target is unavailable.",
			RecoveryAction: "Restore target access or remove the shortcut alias.",
			ProtectsPath:   true,
		},
		ShortcutRootStateBlockedPath: {
			DisplayState:   string(ShortcutRootStateBlockedPath),
			Issue:          "The shortcut alias path is blocked.",
			RecoveryAction: "Clear the blocking local path.",
			ProtectsPath:   true,
		},
		ShortcutRootStateRenameAmbiguous: {
			DisplayState:   string(ShortcutRootStateRenameAmbiguous),
			Issue:          "Multiple same-folder shortcut alias rename candidates were found.",
			RecoveryAction: "Keep exactly one renamed shortcut alias or restore the original name.",
			ProtectsPath:   true,
		},
		ShortcutRootStateAliasMutationBlocked: {
			DisplayState:   string(ShortcutRootStateAliasMutationBlocked),
			Issue:          "The parent engine cannot update the shortcut alias in OneDrive.",
			RecoveryAction: "Fix account, network, or permission access, or restore the local alias.",
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedFinalDrain: {
			DisplayState:   string(ShortcutRootStateRemovedFinalDrain),
			Issue:          "The shortcut alias was removed; child sync is finishing before release.",
			RecoveryAction: "Restore access to the shared folder if final drain is blocked.",
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedReleasePending: {
			DisplayState: string(ShortcutRootStateRemovedReleasePending),
			Issue:        "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
			ProtectsPath: true,
		},
		ShortcutRootStateRemovedCleanupBlocked: {
			DisplayState:   string(ShortcutRootStateRemovedCleanupBlocked),
			Issue:          "The parent engine cannot release the protected shortcut alias path.",
			RecoveryAction: "Clear the local filesystem blocker.",
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedChildCleanupPending: {
			DisplayState: string(ShortcutRootStateRemovedChildCleanupPending),
			Issue:        "The shortcut alias was released; child cleanup is finishing.",
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			DisplayState: string(ShortcutRootStateSamePathReplacementWaiting),
			Issue:        "A new shortcut is waiting for the old child sync to finish.",
			ProtectsPath: true,
		},
		ShortcutRootStateDuplicateTarget: {
			DisplayState:   string(ShortcutRootStateDuplicateTarget),
			Issue:          "Another shortcut alias in this parent already projects the same target.",
			RecoveryAction: "Remove or rename the duplicate shortcut alias.",
			ProtectsPath:   true,
		},
	}
}
