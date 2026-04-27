package sync

type ShortcutRootIssueClass string

const (
	ShortcutRootIssueNone                       ShortcutRootIssueClass = ""
	ShortcutRootIssueTargetUnavailable          ShortcutRootIssueClass = "target_unavailable"
	ShortcutRootIssueLocalRootUnavailable       ShortcutRootIssueClass = "local_root_unavailable"
	ShortcutRootIssueBlockedPath                ShortcutRootIssueClass = "blocked_path"
	ShortcutRootIssueRenameAmbiguous            ShortcutRootIssueClass = "rename_ambiguous"
	ShortcutRootIssueAliasMutationBlocked       ShortcutRootIssueClass = "alias_mutation_blocked"
	ShortcutRootIssueRemovedFinalDrain          ShortcutRootIssueClass = "removed_final_drain"
	ShortcutRootIssueRemovedReleasePending      ShortcutRootIssueClass = "removed_release_pending"
	ShortcutRootIssueRemovedCleanupBlocked      ShortcutRootIssueClass = "removed_cleanup_blocked"
	ShortcutRootIssueRemovedChildCleanupPending ShortcutRootIssueClass = "removed_child_cleanup_pending"
	ShortcutRootIssueSamePathReplacementWaiting ShortcutRootIssueClass = "same_path_replacement_waiting"
	ShortcutRootIssueDuplicateTarget            ShortcutRootIssueClass = "duplicate_target"
	ShortcutRootIssueParentRecovery             ShortcutRootIssueClass = "parent_recovery"
)

type ShortcutRootRecoveryClass string

const (
	ShortcutRootRecoveryNone                       ShortcutRootRecoveryClass = ""
	ShortcutRootRecoveryRestoreTargetOrRemoveAlias ShortcutRootRecoveryClass = "restore_target_or_remove_alias"
	ShortcutRootRecoveryRestoreLocalRootOrDiscard  ShortcutRootRecoveryClass = "restore_local_root_or_discard"
	ShortcutRootRecoveryClearBlockedPath           ShortcutRootRecoveryClass = "clear_blocked_path"
	ShortcutRootRecoveryDisambiguateAliasRename    ShortcutRootRecoveryClass = "disambiguate_alias_rename"
	ShortcutRootRecoveryFixAliasMutation           ShortcutRootRecoveryClass = "fix_alias_mutation"
	ShortcutRootRecoveryRestoreTargetOrDiscard     ShortcutRootRecoveryClass = "restore_target_or_discard"
	ShortcutRootRecoveryWaitForRetry               ShortcutRootRecoveryClass = "wait_for_retry"
	ShortcutRootRecoveryRemoveDuplicateAlias       ShortcutRootRecoveryClass = "remove_duplicate_alias"
)

type ShortcutRootStatusMetadata struct {
	DisplayState   string
	StateReason    string
	IssueClass     ShortcutRootIssueClass
	Issue          string
	RecoveryClass  ShortcutRootRecoveryClass
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
		DisplayState:  string(state),
		StateReason:   string(state),
		IssueClass:    ShortcutRootIssueParentRecovery,
		Issue:         "The shortcut alias is waiting for parent-engine recovery.",
		RecoveryClass: ShortcutRootRecoveryWaitForRetry,
		AutoRetry:     true,
		ProtectsPath:  true,
	}
}

//nolint:funlen // The metadata table is intentionally centralized for state review.
func shortcutRootStatusMetadataTable() map[ShortcutRootState]ShortcutRootStatusMetadata {
	return map[ShortcutRootState]ShortcutRootStatusMetadata{
		ShortcutRootStateTargetUnavailable: {
			DisplayState:   string(ShortcutRootStateTargetUnavailable),
			StateReason:    string(ShortcutRootStateTargetUnavailable),
			IssueClass:     ShortcutRootIssueTargetUnavailable,
			Issue:          "The shortcut target is unavailable.",
			RecoveryClass:  ShortcutRootRecoveryRestoreTargetOrRemoveAlias,
			RecoveryAction: "Restore target access or remove the shortcut alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateLocalRootUnavailable: {
			DisplayState:   string(ShortcutRootStateLocalRootUnavailable),
			StateReason:    string(ShortcutRootStateLocalRootUnavailable),
			IssueClass:     ShortcutRootIssueLocalRootUnavailable,
			Issue:          "The shortcut alias local root is unavailable.",
			RecoveryClass:  ShortcutRootRecoveryRestoreLocalRootOrDiscard,
			RecoveryAction: "Restore the local shortcut directory or delete it to discard unresolved local state.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateBlockedPath: {
			DisplayState:   string(ShortcutRootStateBlockedPath),
			StateReason:    string(ShortcutRootStateBlockedPath),
			IssueClass:     ShortcutRootIssueBlockedPath,
			Issue:          "The shortcut alias path is blocked.",
			RecoveryClass:  ShortcutRootRecoveryClearBlockedPath,
			RecoveryAction: "Clear the blocking local path.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRenameAmbiguous: {
			DisplayState:   string(ShortcutRootStateRenameAmbiguous),
			StateReason:    string(ShortcutRootStateRenameAmbiguous),
			IssueClass:     ShortcutRootIssueRenameAmbiguous,
			Issue:          "Multiple same-folder shortcut alias rename candidates were found.",
			RecoveryClass:  ShortcutRootRecoveryDisambiguateAliasRename,
			RecoveryAction: "Keep exactly one renamed shortcut alias or restore the original name.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateAliasMutationBlocked: {
			DisplayState:   string(ShortcutRootStateAliasMutationBlocked),
			StateReason:    string(ShortcutRootStateAliasMutationBlocked),
			IssueClass:     ShortcutRootIssueAliasMutationBlocked,
			Issue:          "The parent engine cannot update the shortcut alias in OneDrive.",
			RecoveryClass:  ShortcutRootRecoveryFixAliasMutation,
			RecoveryAction: "Fix account, network, or permission access, or restore the local alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedFinalDrain: {
			DisplayState:   string(ShortcutRootStateRemovedFinalDrain),
			StateReason:    string(ShortcutRootStateRemovedFinalDrain),
			IssueClass:     ShortcutRootIssueRemovedFinalDrain,
			Issue:          "The shortcut alias was removed; child sync is finishing before release.",
			RecoveryClass:  ShortcutRootRecoveryRestoreTargetOrDiscard,
			RecoveryAction: "Restore shared-folder access, or delete the local shortcut directory to discard dirty local state.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedReleasePending: {
			DisplayState:  string(ShortcutRootStateRemovedReleasePending),
			StateReason:   string(ShortcutRootStateRemovedReleasePending),
			IssueClass:    ShortcutRootIssueRemovedReleasePending,
			Issue:         "Child sync finished; the parent engine is releasing the protected shortcut alias path.",
			RecoveryClass: ShortcutRootRecoveryWaitForRetry,
			AutoRetry:     true,
			ProtectsPath:  true,
		},
		ShortcutRootStateRemovedCleanupBlocked: {
			DisplayState:   string(ShortcutRootStateRemovedCleanupBlocked),
			StateReason:    string(ShortcutRootStateRemovedCleanupBlocked),
			IssueClass:     ShortcutRootIssueRemovedCleanupBlocked,
			Issue:          "The parent engine cannot release the protected shortcut alias path.",
			RecoveryClass:  ShortcutRootRecoveryClearBlockedPath,
			RecoveryAction: "Clear the local filesystem blocker.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
		ShortcutRootStateRemovedChildCleanupPending: {
			DisplayState:  string(ShortcutRootStateRemovedChildCleanupPending),
			StateReason:   string(ShortcutRootStateRemovedChildCleanupPending),
			IssueClass:    ShortcutRootIssueRemovedChildCleanupPending,
			Issue:         "The shortcut alias was released; child cleanup is finishing.",
			RecoveryClass: ShortcutRootRecoveryWaitForRetry,
			AutoRetry:     true,
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			DisplayState:  string(ShortcutRootStateSamePathReplacementWaiting),
			StateReason:   string(ShortcutRootStateSamePathReplacementWaiting),
			IssueClass:    ShortcutRootIssueSamePathReplacementWaiting,
			Issue:         "A new shortcut is waiting for the old child sync to finish.",
			RecoveryClass: ShortcutRootRecoveryWaitForRetry,
			AutoRetry:     true,
			ProtectsPath:  true,
		},
		ShortcutRootStateDuplicateTarget: {
			DisplayState:   string(ShortcutRootStateDuplicateTarget),
			StateReason:    string(ShortcutRootStateDuplicateTarget),
			IssueClass:     ShortcutRootIssueDuplicateTarget,
			Issue:          "Another shortcut alias in this parent already projects the same target.",
			RecoveryClass:  ShortcutRootRecoveryRemoveDuplicateAlias,
			RecoveryAction: "Remove or rename the duplicate shortcut alias.",
			AutoRetry:      true,
			ProtectsPath:   true,
		},
	}
}
