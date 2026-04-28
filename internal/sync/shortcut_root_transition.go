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

func shortcutRootTransitionTable() map[ShortcutRootState]map[shortcutRootLifecycleEvent][]ShortcutRootState {
	table := shortcutRootLifecycleMetadataTable()
	transitions := make(map[ShortcutRootState]map[shortcutRootLifecycleEvent][]ShortcutRootState, len(table))
	for state := range table {
		transitions[state] = cloneShortcutRootTransitionTargets(table[state].transitions)
	}
	return transitions
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

func shortcutRootWithTransition(
	record *ShortcutRootRecord,
	event shortcutRootLifecycleEvent,
	to ShortcutRootState,
	detail string,
) (ShortcutRootRecord, error) {
	if record == nil {
		return ShortcutRootRecord{}, fmt.Errorf("sync: shortcut root transition %s requires record", event)
	}
	next := *record
	if err := validateShortcutRootTransition(next.State, event, to); err != nil {
		return next, err
	}
	next.State = to
	next.BlockedDetail = detail
	next.ProtectedPaths = protectedPathsForShortcutRoot(next.RelativeLocalPath, next.ProtectedPaths)
	return next, nil
}

func plannedShortcutRootTransition(
	record *ShortcutRootRecord,
	event shortcutRootLifecycleEvent,
	to ShortcutRootState,
	detail string,
) ShortcutRootRecord {
	next, err := shortcutRootWithTransition(record, event, to, detail)
	if err == nil {
		return next
	}
	if record == nil {
		return ShortcutRootRecord{State: ShortcutRootStateBlockedPath, BlockedDetail: err.Error()}
	}
	fallback := *record
	fallback.State = normalizeShortcutRootState(fallback.State)
	fallback.BlockedDetail = err.Error()
	fallback.ProtectedPaths = protectedPathsForShortcutRoot(fallback.RelativeLocalPath, fallback.ProtectedPaths)
	return fallback
}
