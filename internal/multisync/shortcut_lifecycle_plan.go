package multisync

import (
	"fmt"
	"reflect"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

type shortcutLifecycleEffect string

const (
	shortcutEffectPersistInventoryBeforeProjectionCleanup shortcutLifecycleEffect = "persist_inventory_before_projection_cleanup"
	shortcutEffectPersistInventoryBeforeRunnerRestart     shortcutLifecycleEffect = "persist_inventory_before_runner_restart"
	shortcutEffectStopChildBeforeGraphAliasMutation       shortcutLifecycleEffect = "stop_child_before_graph_alias_mutation"
	shortcutEffectStopChildBeforeProjectionMove           shortcutLifecycleEffect = "stop_child_before_projection_move"
	shortcutEffectRecompileAfterLifecycleMutation         shortcutLifecycleEffect = "recompile_after_lifecycle_mutation"
	shortcutEffectKeepReservationsActive                  shortcutLifecycleEffect = "keep_reservations_active"
	shortcutEffectPurgeChildDBAfterProjectionCleanup      shortcutLifecycleEffect = "purge_child_db_after_projection_cleanup"
	shortcutEffectRetryConflictUnavailableWithoutRunner   shortcutLifecycleEffect = "retry_conflict_unavailable_without_runner"
	shortcutEffectDeferReplacementUntilPendingFinalizes   shortcutLifecycleEffect = "defer_replacement_until_pending_finalizes"
)

type shortcutLifecyclePlan struct {
	Record       config.MountRecord
	Changed      bool
	RemoveRecord bool
	Effects      []shortcutLifecycleEffect
}

func planShortcutPendingRemoval(record *config.MountRecord) (shortcutLifecyclePlan, error) {
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	next := *record
	next.State = config.MountStatePendingRemoval
	next.StateReason = config.MountStateReasonShortcutRemoved
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectPersistInventoryBeforeProjectionCleanup,
			shortcutEffectKeepReservationsActive,
			shortcutEffectPurgeChildDBAfterProjectionCleanup,
		},
	}, nil
}

func planDeferredShortcutReplacement(record *config.MountRecord) (shortcutLifecyclePlan, error) {
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	next := *record
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectPersistInventoryBeforeRunnerRestart,
			shortcutEffectKeepReservationsActive,
			shortcutEffectDeferReplacementUntilPendingFinalizes,
		},
	}, nil
}

func planChildRootLifecycleActionSuccess(
	record *config.MountRecord,
	action *childRootLifecycleAction,
	identity *config.RootIdentity,
) (shortcutLifecyclePlan, error) {
	if action == nil {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires local root action")
	}
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	next := *record
	switch action.kind {
	case childRootLifecycleActionRename:
		next.LocalAlias = childRootActionAlias(action)
		next.RelativeLocalPath = action.toRelativeLocalPath
		next.ReservedLocalPaths = removeString(next.ReservedLocalPaths, action.toRelativeLocalPath)
		next.LocalRootMaterialized = true
		if identity != nil {
			next.LocalRootIdentity = cloneRootIdentity(identity)
		}
		next.State = config.MountStateActive
		next.StateReason = ""
	case childRootLifecycleActionDelete:
		next.State = config.MountStatePendingRemoval
		next.StateReason = config.MountStateReasonShortcutRemoved
	default:
		return shortcutLifecyclePlan{}, fmt.Errorf("unknown child root lifecycle action %q", action.kind)
	}

	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectStopChildBeforeGraphAliasMutation,
			shortcutEffectPersistInventoryBeforeRunnerRestart,
			shortcutEffectRecompileAfterLifecycleMutation,
			shortcutEffectKeepReservationsActive,
		},
	}, nil
}

func planChildRootLifecycleActionFailure(
	record *config.MountRecord,
	action *childRootLifecycleAction,
	state config.MountState,
	reason config.MountStateReason,
) (shortcutLifecyclePlan, error) {
	if action == nil {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires local root action")
	}
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	if reason == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires failure reason")
	}
	next := *record
	next.State = state
	next.StateReason = reason
	if action.toRelativeLocalPath != "" {
		next.ReservedLocalPaths = appendUniqueStrings(next.ReservedLocalPaths, action.toRelativeLocalPath)
	}
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectPersistInventoryBeforeRunnerRestart,
			shortcutEffectKeepReservationsActive,
			shortcutEffectRetryConflictUnavailableWithoutRunner,
		},
	}, nil
}

func planProjectionMoveSuccess(
	record *config.MountRecord,
	move *childProjectionMove,
	identity *config.RootIdentity,
) (shortcutLifecyclePlan, error) {
	if move == nil {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires projection move")
	}
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	next := *record
	next.ReservedLocalPaths = removeString(next.ReservedLocalPaths, move.fromRelativeLocalPath)
	next.LocalRootMaterialized = true
	if identity != nil {
		next.LocalRootIdentity = cloneRootIdentity(identity)
	}
	if next.StateReason == config.MountStateReasonLocalProjectionUnavailable ||
		next.StateReason == config.MountStateReasonLocalProjectionConflict {
		next.State = config.MountStateActive
		next.StateReason = ""
	}
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectStopChildBeforeProjectionMove,
			shortcutEffectPersistInventoryBeforeRunnerRestart,
			shortcutEffectRecompileAfterLifecycleMutation,
			shortcutEffectKeepReservationsActive,
		},
	}, nil
}

func planProjectionMoveFailure(
	record *config.MountRecord,
	move *childProjectionMove,
	state config.MountState,
	reason config.MountStateReason,
) (shortcutLifecyclePlan, error) {
	if move == nil {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires projection move")
	}
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	if reason == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires failure reason")
	}
	next := *record
	next.State = state
	next.StateReason = reason
	next.ReservedLocalPaths = appendUniqueStrings(next.ReservedLocalPaths, move.fromRelativeLocalPath)
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectPersistInventoryBeforeRunnerRestart,
			shortcutEffectKeepReservationsActive,
			shortcutEffectRetryConflictUnavailableWithoutRunner,
		},
	}, nil
}

func planPendingRemovalCleanup(
	record *config.MountRecord,
	cleaned bool,
	reason config.MountStateReason,
) (shortcutLifecyclePlan, error) {
	if record == nil || record.MountID == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("shortcut lifecycle transition requires mount ID")
	}
	if record.State != config.MountStatePendingRemoval {
		return shortcutLifecyclePlan{}, fmt.Errorf("pending-removal cleanup requires pending_removal state")
	}
	next := *record
	if cleaned {
		return shortcutLifecyclePlan{
			Record:       next,
			Changed:      true,
			RemoveRecord: true,
			Effects: []shortcutLifecycleEffect{
				shortcutEffectPurgeChildDBAfterProjectionCleanup,
				shortcutEffectRecompileAfterLifecycleMutation,
			},
		}, nil
	}
	if reason == "" {
		return shortcutLifecyclePlan{}, fmt.Errorf("pending-removal cleanup requires blocked reason")
	}
	next.StateReason = reason
	return shortcutLifecyclePlan{
		Record:  next,
		Changed: !reflect.DeepEqual(next, *record),
		Effects: []shortcutLifecycleEffect{
			shortcutEffectKeepReservationsActive,
			shortcutEffectRetryConflictUnavailableWithoutRunner,
		},
	}, nil
}
