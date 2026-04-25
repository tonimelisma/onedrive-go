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
			RecoveryAction:   "Resolve the listed child mount conflict.",
			StatusDetail:     "The child mount is blocked by a protected-path conflict.",
		}, true
	case MountStateUnavailable:
		return MountLifecycleDetail{
			RequiredState:    MountStateUnavailable,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			RecoveryAction:   "Restore access to the listed shortcut alias or child projection.",
			StatusDetail:     "The child mount is unavailable and will be retried.",
		}, true
	case MountStatePendingRemoval:
		return MountLifecycleDetail{
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			RecoveryAction:   "No action is needed while the removed shortcut finishes child sync and cleanup.",
			StatusDetail:     "The shortcut was removed and the child projection remains protected until release is safe.",
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
		MountStateReasonShortcutBindingUnavailable: lifecycleReasonDetail(
			MountStateUnavailable,
			"OneDrive did not return a usable shortcut target. "+
				"The child path remains reserved and will retry when the parent observes a complete shortcut target.",
			"Restore target access or recreate the OneDrive shortcut alias if it no longer resolves.",
		),
		MountStateReasonLocalProjectionUnavailable: lifecycleReasonDetail(
			MountStateUnavailable,
			"The local shortcut projection could not be moved or inspected safely. "+
				"The reservation stays active and the move will retry.",
			"Fix the filesystem permission, lock, or disk issue for the protected path.",
		),
		MountStateReasonLocalAliasRenameUnavailable: lifecycleReasonDetail(
			MountStateUnavailable,
			"The local shortcut rename could not be applied to the OneDrive shortcut placeholder. "+
				"The reservation stays active and the rename will retry.",
			"Fix auth, network, permission, or filesystem issues, or rename the shortcut alias directory back.",
		),
		MountStateReasonLocalAliasDeleteUnavailable: lifecycleReasonDetail(
			MountStateUnavailable,
			"The local shortcut delete could not be applied to the OneDrive shortcut placeholder. "+
				"The reservation stays active and the delete will retry.",
			"Fix auth, network, or permission issues, or recreate the shortcut alias directory.",
		),
		MountStateReasonLocalRootUnavailable: lifecycleReasonDetail(
			MountStateUnavailable,
			"The child mount path could not be created, checked, or identified. "+
				"The path remains reserved and will be rechecked.",
			"Restore the protected path or fix local filesystem permissions.",
		),
	}
}

func shortcutConflictLifecycleDetails() map[MountStateReason]MountLifecycleDetail {
	return map[MountStateReason]MountLifecycleDetail{
		MountStateReasonDuplicateContentRoot: lifecycleReasonDetail(
			MountStateConflict,
			"Another child shortcut owns this same content root. "+
				"The conflicting child paths stay reserved until the parent topology changes.",
			"Remove, move, or rename one duplicate OneDrive shortcut alias.",
		),
		MountStateReasonExplicitStandaloneContentRoot: lifecycleReasonDetail(
			MountStateConflict,
			"This content root is already configured as a standalone mount. "+
				"The shortcut path stays reserved while the explicit mount owns the content root.",
			"Remove the OneDrive shortcut alias or remove the configured standalone shared-folder drive.",
		),
		MountStateReasonLocalProjectionConflict: lifecycleReasonDetail(
			MountStateConflict,
			"The old and new shortcut paths both exist locally with content that cannot be collapsed safely. "+
				"Both paths remain reserved.",
			"Leave only one safe listed child projection path.",
		),
		MountStateReasonLocalAliasRenameConflict: lifecycleReasonDetail(
			MountStateConflict,
			"The local shortcut rename is ambiguous because multiple same-folder candidates match the stored root identity. "+
				"All plausible paths stay reserved.",
			"Keep exactly one listed same-folder renamed directory, or restore the original shortcut alias name.",
		),
		MountStateReasonLocalRootCollision: lifecycleReasonDetail(
			MountStateConflict,
			"The child mount path collides with a local file, symlink, or unsafe path. "+
				"The path remains reserved and will be rechecked.",
			"Remove or rename the listed blocking local object.",
		),
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
				"The child sync runs one final pass before the protected projection is released.",
			RecoveryAction: "No action is needed unless the child sync reports blocked content.",
		},
		MountStateReasonRemovedProjectionDirty: {
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The shortcut was removed, but the child projection is not ready for release. " +
				"The parent keeps this path protected to avoid recreating the shortcut content.",
			RecoveryAction: "Leave the listed child projection empty, or wait for child sync to finish.",
		},
		MountStateReasonRemovedProjectionUnavailable: {
			RequiredState:    MountStatePendingRemoval,
			KeepsReservation: true,
			StartsChild:      false,
			AutoRetry:        true,
			StatusDetail: "The shortcut was removed, but the local projection or child state could not be checked or removed. " +
				"The reservation stays active until cleanup can prove safety.",
			RecoveryAction: "Fix the filesystem, lock, disk, or child state-file issue for the protected path.",
		},
	}
}

func lifecycleReasonDetail(state MountState, statusDetail string, recoveryAction string) MountLifecycleDetail {
	return MountLifecycleDetail{
		RequiredState:    state,
		KeepsReservation: true,
		StartsChild:      false,
		AutoRetry:        true,
		StatusDetail:     statusDetail,
		RecoveryAction:   recoveryAction,
	}
}
