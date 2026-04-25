package config

// MountLifecycleDetail is the shared presentation and retry contract for a
// durable child-mount lifecycle state. Config owns the durable state vocabulary;
// multisync remains the owner of transitions and side effects.
type MountLifecycleDetail struct {
	RequiredState    MountState
	KeepsReservation bool
	StartsChild      bool
	AutoRetry        bool
	StatusDetail     string
	RecoveryAction   string
}

func MountLifecycleDetailFor(state MountState, reason MountStateReason) (MountLifecycleDetail, bool) {
	if reason != "" {
		detail, ok := mountLifecycleDetailForReason(reason)
		if !ok || detail.RequiredState != state {
			return MountLifecycleDetail{}, false
		}
		return detail, ok
	}

	switch state {
	case MountStateActive:
		return MountLifecycleDetail{
			RequiredState:    MountStateActive,
			KeepsReservation: true,
			StartsChild:      true,
		}, true
	case MountStateConflict:
		return MountLifecycleDetail{
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			RecoveryAction:   "Resolve the child mount conflict, then rerun sync.",
			StatusDetail:     "Resolve the child mount conflict, then rerun sync.",
		}, true
	case MountStateUnavailable:
		return MountLifecycleDetail{
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			RecoveryAction:   "Wait for the shortcut or local projection to become available, then rerun sync.",
			StatusDetail:     "Wait for the shortcut or local projection to become available, then rerun sync.",
		}, true
	case MountStatePendingRemoval:
		return MountLifecycleDetail{
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			RecoveryAction:   "Wait for runner stop and child state cleanup to finish.",
			StatusDetail:     "Wait for runner stop and child state cleanup to finish.",
		}, true
	default:
		return MountLifecycleDetail{}, false
	}
}

func mountLifecycleDetailForReason(reason MountStateReason) (MountLifecycleDetail, bool) {
	detail, ok := mountLifecycleDetails()[reason]
	return detail, ok
}

func AllMountStateReasons() []MountStateReason {
	reasons := make([]MountStateReason, 0, len(mountLifecycleDetails()))
	for reason := range mountLifecycleDetails() {
		reasons = append(reasons, reason)
	}
	return reasons
}

func mountLifecycleDetails() map[MountStateReason]MountLifecycleDetail {
	details := map[MountStateReason]MountLifecycleDetail{}
	for _, group := range []map[MountStateReason]MountLifecycleDetail{
		shortcutUnavailableLifecycleDetails(),
		shortcutConflictLifecycleDetails(),
		shortcutPendingRemovalLifecycleDetails(),
	} {
		for reason, detail := range group {
			details[reason] = detail
		}
	}
	return details
}

func shortcutUnavailableLifecycleDetails() map[MountStateReason]MountLifecycleDetail {
	return map[MountStateReason]MountLifecycleDetail{
		MountStateReasonShortcutBindingUnavailable: {
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "OneDrive did not return a usable shortcut target. " +
				"The child path remains reserved and will retry when the parent observes a complete shortcut target.",
			RecoveryAction: "Restore target access or recreate the OneDrive shortcut if it no longer resolves.",
		},
		MountStateReasonLocalProjectionUnavailable: {
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The local shortcut projection could not be moved or inspected safely. " +
				"The reservation stays active and the move will retry.",
			RecoveryAction: "Fix the filesystem, permission, lock, or disk issue, then rerun sync.",
		},
		MountStateReasonLocalAliasRenameUnavailable: {
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The local shortcut rename could not be applied to the OneDrive shortcut placeholder. " +
				"The reservation stays active and the rename will retry.",
			RecoveryAction: "Fix auth, network, permission, or filesystem issues, or rename the directory back.",
		},
		MountStateReasonLocalAliasDeleteUnavailable: {
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The local shortcut delete could not be applied to the OneDrive shortcut placeholder. " +
				"The reservation stays active and the delete will retry.",
			RecoveryAction: "Fix auth, network, or permission issues, or recreate the local root.",
		},
		MountStateReasonLocalRootUnavailable: {
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The child mount path could not be created, checked, or identified. " +
				"The path remains reserved and will be rechecked.",
			RecoveryAction: "Restore the path or fix local filesystem permissions, then rerun sync.",
		},
	}
}

func shortcutConflictLifecycleDetails() map[MountStateReason]MountLifecycleDetail {
	return map[MountStateReason]MountLifecycleDetail{
		MountStateReasonDuplicateContentRoot: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "Another child shortcut owns this same content root. " +
				"The conflicting child paths stay reserved until the parent topology changes.",
			RecoveryAction: "Remove, move, or rename one duplicate OneDrive shortcut.",
		},
		MountStateReasonExplicitStandaloneContentRoot: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "This content root is already configured as a standalone mount. " +
				"The shortcut path stays reserved while the explicit mount owns the content root.",
			RecoveryAction: "Remove the OneDrive shortcut or remove the configured standalone shared-folder drive.",
		},
		MountStateReasonLocalProjectionConflict: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The old and new shortcut paths both exist locally with content that cannot be collapsed safely. " +
				"Both paths remain reserved.",
			RecoveryAction: "Merge, move, or remove one path, then rerun sync.",
		},
		MountStateReasonLocalAliasRenameConflict: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The local shortcut rename is ambiguous because multiple same-folder candidates match the stored root identity. " +
				"All plausible paths stay reserved.",
			RecoveryAction: "Keep exactly one renamed directory, or restore the original directory name, then rerun sync.",
		},
		MountStateReasonPathReservedByPendingRemoval: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "A new shortcut is waiting for an older removed shortcut at the same local path to finish cleanup. " +
				"The shared path remains reserved.",
			RecoveryAction: "Clean the old removed projection if it still contains local content.",
		},
		MountStateReasonLocalRootCollision: {
			RequiredState:    MountStateConflict,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The child mount path collides with a local file, symlink, or unsafe path. " +
				"The path remains reserved and will be rechecked.",
			RecoveryAction: "Remove or rename the blocking local object, then rerun sync.",
		},
	}
}

func shortcutPendingRemovalLifecycleDetails() map[MountStateReason]MountLifecycleDetail {
	return map[MountStateReason]MountLifecycleDetail{
		MountStateReasonShortcutRemoved: {
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The shortcut was removed. " +
				"The local projection remains reserved until cleanup proves it is missing or empty.",
			RecoveryAction: "No action is needed unless the local projection still contains content.",
		},
		MountStateReasonRemovedProjectionDirty: {
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The shortcut was removed, but the local projection still contains content. " +
				"The parent will keep reserving this path to avoid re-uploading it.",
			RecoveryAction: "Move or remove that local projection content, then rerun sync.",
		},
		MountStateReasonRemovedProjectionUnavailable: {
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The shortcut was removed, but the local projection or child state could not be checked or removed. " +
				"The reservation stays active until cleanup can prove safety.",
			RecoveryAction: "Fix the filesystem, lock, disk, or state-file issue, then rerun sync.",
		},
	}
}
