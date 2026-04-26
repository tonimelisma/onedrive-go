package sync

import "fmt"

type shortcutRootLifecycleEvent string

const (
	shortcutRootEventRemoteUpsert               shortcutRootLifecycleEvent = "remote_upsert"
	shortcutRootEventRemoteDelete               shortcutRootLifecycleEvent = "remote_delete"
	shortcutRootEventRemoteUnavailable          shortcutRootLifecycleEvent = "remote_unavailable"
	shortcutRootEventCompleteOmission           shortcutRootLifecycleEvent = "complete_omission"
	shortcutRootEventSamePathReplacement        shortcutRootLifecycleEvent = "same_path_replacement"
	shortcutRootEventProtectedPathConflict      shortcutRootLifecycleEvent = "protected_path_conflict"
	shortcutRootEventLocalRootReady             shortcutRootLifecycleEvent = "local_root_ready"
	shortcutRootEventLocalPathBlocked           shortcutRootLifecycleEvent = "local_path_blocked"
	shortcutRootEventAliasMutationSucceeded     shortcutRootLifecycleEvent = "alias_mutation_succeeded"
	shortcutRootEventAliasMutationFailed        shortcutRootLifecycleEvent = "alias_mutation_failed"
	shortcutRootEventAliasRenameAmbiguous       shortcutRootLifecycleEvent = "alias_rename_ambiguous"
	shortcutRootEventChildFinalDrainClean       shortcutRootLifecycleEvent = "child_final_drain_clean"
	shortcutRootEventProjectionCleanupFailed    shortcutRootLifecycleEvent = "projection_cleanup_failed"
	shortcutRootEventProjectionCleanupSucceeded shortcutRootLifecycleEvent = "projection_cleanup_succeeded"
	shortcutRootEventWaitingReplacementPromote  shortcutRootLifecycleEvent = "waiting_replacement_promote"
	shortcutRootEventChildArtifactsPurged       shortcutRootLifecycleEvent = "child_artifacts_purged"
	shortcutRootEventDuplicateTargetDetected    shortcutRootLifecycleEvent = "duplicate_target_detected"
	shortcutRootEventDuplicateTargetResolved    shortcutRootLifecycleEvent = "duplicate_target_resolved"
)

//nolint:funlen // The shortcut lifecycle table is intentionally centralized for state-machine review.
func shortcutRootTransitionTable() map[ShortcutRootState]map[shortcutRootLifecycleEvent][]ShortcutRootState {
	return map[ShortcutRootState]map[shortcutRootLifecycleEvent][]ShortcutRootState{
		ShortcutRootStateActive: {
			shortcutRootEventRemoteUpsert:          {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:          {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventRemoteUnavailable:     {ShortcutRootStateTargetUnavailable},
			shortcutRootEventCompleteOmission:      {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventProtectedPathConflict: {ShortcutRootStateBlockedPath},
			shortcutRootEventLocalRootReady:        {ShortcutRootStateActive},
			shortcutRootEventLocalPathBlocked:      {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationSucceeded: {
				ShortcutRootStateActive,
				ShortcutRootStateRemovedFinalDrain,
			},
			shortcutRootEventAliasMutationFailed:  {ShortcutRootStateAliasMutationBlocked},
			shortcutRootEventAliasRenameAmbiguous: {ShortcutRootStateRenameAmbiguous},
			shortcutRootEventDuplicateTargetDetected: {
				ShortcutRootStateDuplicateTarget,
			},
		},
		ShortcutRootStateTargetUnavailable: {
			shortcutRootEventRemoteUpsert:          {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:          {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventRemoteUnavailable:     {ShortcutRootStateTargetUnavailable},
			shortcutRootEventCompleteOmission:      {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventProtectedPathConflict: {ShortcutRootStateBlockedPath},
			shortcutRootEventLocalRootReady:        {ShortcutRootStateActive},
			shortcutRootEventLocalPathBlocked:      {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationSucceeded: {
				ShortcutRootStateActive,
				ShortcutRootStateRemovedFinalDrain,
			},
			shortcutRootEventDuplicateTargetDetected: {
				ShortcutRootStateDuplicateTarget,
			},
		},
		ShortcutRootStateBlockedPath: {
			shortcutRootEventRemoteUpsert:          {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:          {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventRemoteUnavailable:     {ShortcutRootStateTargetUnavailable},
			shortcutRootEventCompleteOmission:      {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventProtectedPathConflict: {ShortcutRootStateBlockedPath},
			shortcutRootEventLocalRootReady:        {ShortcutRootStateActive},
			shortcutRootEventLocalPathBlocked:      {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationSucceeded: {
				ShortcutRootStateActive,
				ShortcutRootStateRemovedFinalDrain,
			},
			shortcutRootEventAliasMutationFailed:  {ShortcutRootStateAliasMutationBlocked},
			shortcutRootEventAliasRenameAmbiguous: {ShortcutRootStateRenameAmbiguous},
			shortcutRootEventDuplicateTargetDetected: {
				ShortcutRootStateDuplicateTarget,
			},
		},
		ShortcutRootStateRenameAmbiguous: {
			shortcutRootEventRemoteUpsert:     {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:     {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventCompleteOmission: {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventLocalRootReady:   {ShortcutRootStateActive},
			shortcutRootEventLocalPathBlocked: {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationSucceeded: {
				ShortcutRootStateActive,
				ShortcutRootStateRemovedFinalDrain,
			},
			shortcutRootEventAliasMutationFailed:  {ShortcutRootStateAliasMutationBlocked},
			shortcutRootEventAliasRenameAmbiguous: {ShortcutRootStateRenameAmbiguous},
			shortcutRootEventDuplicateTargetDetected: {
				ShortcutRootStateDuplicateTarget,
			},
		},
		ShortcutRootStateAliasMutationBlocked: {
			shortcutRootEventRemoteUpsert:     {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:     {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventCompleteOmission: {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventLocalRootReady:   {ShortcutRootStateActive},
			shortcutRootEventLocalPathBlocked: {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationSucceeded: {
				ShortcutRootStateActive,
				ShortcutRootStateRemovedFinalDrain,
			},
			shortcutRootEventAliasMutationFailed:  {ShortcutRootStateAliasMutationBlocked},
			shortcutRootEventAliasRenameAmbiguous: {ShortcutRootStateRenameAmbiguous},
			shortcutRootEventDuplicateTargetDetected: {
				ShortcutRootStateDuplicateTarget,
			},
		},
		ShortcutRootStateRemovedFinalDrain: {
			shortcutRootEventRemoteUpsert:              {ShortcutRootStateActive},
			shortcutRootEventSamePathReplacement:       {ShortcutRootStateSamePathReplacementWaiting},
			shortcutRootEventChildFinalDrainClean:      {ShortcutRootStateRemovedReleasePending},
			shortcutRootEventProjectionCleanupFailed:   {ShortcutRootStateRemovedCleanupBlocked},
			shortcutRootEventWaitingReplacementPromote: {ShortcutRootStateActive},
		},
		ShortcutRootStateRemovedReleasePending: {
			shortcutRootEventProjectionCleanupFailed:    {ShortcutRootStateRemovedCleanupBlocked},
			shortcutRootEventProjectionCleanupSucceeded: {ShortcutRootStateRemovedChildCleanupPending},
			shortcutRootEventWaitingReplacementPromote:  {ShortcutRootStateActive},
		},
		ShortcutRootStateRemovedCleanupBlocked: {
			shortcutRootEventRemoteUpsert:               {ShortcutRootStateActive},
			shortcutRootEventSamePathReplacement:        {ShortcutRootStateSamePathReplacementWaiting},
			shortcutRootEventChildFinalDrainClean:       {ShortcutRootStateRemovedReleasePending},
			shortcutRootEventProjectionCleanupFailed:    {ShortcutRootStateRemovedCleanupBlocked},
			shortcutRootEventProjectionCleanupSucceeded: {ShortcutRootStateRemovedChildCleanupPending},
			shortcutRootEventWaitingReplacementPromote:  {ShortcutRootStateActive},
		},
		ShortcutRootStateRemovedChildCleanupPending: {
			shortcutRootEventRemoteUpsert:         {ShortcutRootStateActive},
			shortcutRootEventChildArtifactsPurged: {},
		},
		ShortcutRootStateSamePathReplacementWaiting: {
			shortcutRootEventRemoteUpsert:              {ShortcutRootStateActive},
			shortcutRootEventSamePathReplacement:       {ShortcutRootStateSamePathReplacementWaiting},
			shortcutRootEventChildFinalDrainClean:      {ShortcutRootStateRemovedReleasePending},
			shortcutRootEventProjectionCleanupFailed:   {ShortcutRootStateRemovedCleanupBlocked},
			shortcutRootEventWaitingReplacementPromote: {ShortcutRootStateActive},
		},
		ShortcutRootStateDuplicateTarget: {
			shortcutRootEventRemoteUpsert:            {ShortcutRootStateActive},
			shortcutRootEventRemoteDelete:            {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventRemoteUnavailable:       {ShortcutRootStateTargetUnavailable},
			shortcutRootEventCompleteOmission:        {ShortcutRootStateRemovedFinalDrain},
			shortcutRootEventDuplicateTargetDetected: {ShortcutRootStateDuplicateTarget},
			shortcutRootEventDuplicateTargetResolved: {ShortcutRootStateActive},
			shortcutRootEventProtectedPathConflict:   {ShortcutRootStateBlockedPath},
			shortcutRootEventLocalRootReady:          {ShortcutRootStateDuplicateTarget},
			shortcutRootEventLocalPathBlocked:        {ShortcutRootStateBlockedPath},
			shortcutRootEventAliasMutationFailed:     {ShortcutRootStateAliasMutationBlocked},
			shortcutRootEventAliasRenameAmbiguous:    {ShortcutRootStateRenameAmbiguous},
		},
	}
}

func validateShortcutRootTransition(
	from ShortcutRootState,
	event shortcutRootLifecycleEvent,
	to ShortcutRootState,
) error {
	from = normalizeShortcutRootState(from)
	to = normalizeShortcutRootState(to)
	targets, ok := shortcutRootTransitionTable()[from][event]
	if !ok {
		return fmt.Errorf("sync: shortcut root transition %s from %s is not allowed", event, from)
	}
	for _, target := range targets {
		if target == to {
			return nil
		}
	}
	return fmt.Errorf("sync: shortcut root transition %s from %s to %s is not allowed", event, from, to)
}

func normalizeShortcutRootState(state ShortcutRootState) ShortcutRootState {
	if state == "" {
		return ShortcutRootStateActive
	}
	return state
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func shortcutRootWithTransition(
	record ShortcutRootRecord,
	event shortcutRootLifecycleEvent,
	to ShortcutRootState,
	detail string,
) (ShortcutRootRecord, error) {
	if err := validateShortcutRootTransition(record.State, event, to); err != nil {
		return record, err
	}
	record.State = to
	record.BlockedDetail = detail
	record.ProtectedPaths = protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
	return record, nil
}

//nolint:gocritic // ShortcutRootRecord is an immutable planner value at this boundary.
func plannedShortcutRootTransition(
	record ShortcutRootRecord,
	event shortcutRootLifecycleEvent,
	to ShortcutRootState,
	detail string,
) ShortcutRootRecord {
	next, err := shortcutRootWithTransition(record, event, to, detail)
	if err == nil {
		return next
	}
	record.State = normalizeShortcutRootState(record.State)
	record.BlockedDetail = err.Error()
	record.ProtectedPaths = protectedPathsForShortcutRoot(record.RelativeLocalPath, record.ProtectedPaths)
	return record
}
