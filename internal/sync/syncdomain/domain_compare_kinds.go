package syncdomain

import (
	"fmt"
)

// kindUnclassified is ComparisonUnclassified's name.
const kindUnclassified = "unclassified"

// ComparisonKind is the per-path comparison the decision table computes
// (Decide, its first stage): which of baseline, local and remote hold the
// path, and how the sides relate to the baseline.
//
// The zero value is ComparisonUnclassified: a state no rule
// covers, which planning refuses (validatePlannerKinds). Members are never
// persisted, so their numbers may change; ComparisonDiverged stays last,
// because the kind tests range up to it (allComparisonKinds).
type ComparisonKind uint8

const (
	ComparisonUnclassified ComparisonKind = iota
	ComparisonLocalMoveSource
	comparisonDeleteAwaitsAncestorMove
	comparisonBothMissing
	ComparisonLocalMoveDest
	ComparisonRemoteMoveSource
	ComparisonRemoteMoveDest
	comparisonKeptLocalFolder
	ComparisonLocalOnlyCreate
	ComparisonRemoteMoveShadowed
	ComparisonRemoteOnlyCreate
	comparisonCreateEqual
	comparisonCreateConflict
	comparisonUnchanged
	ComparisonEqualAgain
	comparisonLocalMissing
	comparisonRemoteMissing
	ComparisonDiverged
)

//nolint:gocyclo // one case per member: the name table of the enum
func (k ComparisonKind) String() string {
	switch k {
	case ComparisonUnclassified:
		return kindUnclassified
	case ComparisonLocalMoveSource:
		return "local_move_source"
	case comparisonDeleteAwaitsAncestorMove:
		return "delete_awaits_ancestor_move"
	case comparisonBothMissing:
		return "both_missing"
	case ComparisonLocalMoveDest:
		return "local_move_dest"
	case ComparisonRemoteMoveSource:
		return "remote_move_source"
	case ComparisonRemoteMoveDest:
		return "remote_move_dest"
	case comparisonKeptLocalFolder:
		return "kept_local_folder"
	case ComparisonLocalOnlyCreate:
		return "local_only_create"
	case ComparisonRemoteMoveShadowed:
		return "remote_move_shadowed"
	case ComparisonRemoteOnlyCreate:
		return "remote_only_create"
	case comparisonCreateEqual:
		return "create_equal"
	case comparisonCreateConflict:
		return "create_conflict"
	case comparisonUnchanged:
		return "unchanged"
	case ComparisonEqualAgain:
		return "equal_again"
	case comparisonLocalMissing:
		return "local_missing"
	case comparisonRemoteMissing:
		return "remote_missing"
	case ComparisonDiverged:
		return "diverged"
	default:
		return fmt.Sprintf("ComparisonKind(%d)", uint8(k))
	}
}

// ReconciliationKind is the decision the decision table takes for a path
// (Reconcile, its second stage), which the planner maps to actions
// (buildActionsForReconciliation). It owns its names: some equal action-type
// names (local_delete, upload, ...), but those are a persisted wire format
// (retry_work.action_type) and coincide by choice, not by definition.
//
// The zero value is ReconcileUnclassified, the CASE's ELSE; ReconcileNoop is
// the explicit "nothing to do" that the decision table states for a path.
// ReconcileReplaceLocal stays last (allReconciliationKinds).
type ReconciliationKind uint8

// reconciliationKindUnclassified is ReconcileUnclassified's name, the name
// the comparison kinds give their zero member too.
const reconciliationKindUnclassified = "unclassified"

const (
	ReconcileUnclassified ReconciliationKind = iota
	ReconcileNoop
	ReconcileBaselineRemove
	ReconcileBaselineUpdate
	ReconcileFolderCreateLocal
	ReconcileFolderCreateRemote
	ReconcileUpload
	ReconcileDownload
	ReconcileLocalDelete
	ReconcileRemoteDelete
	ReconcileLocalMove
	ReconcileRemoteMove
	ReconcileConflictEditEdit
	ReconcileConflictEditDelete
	ReconcileConflictCreateCreate
	ReconcileConflictKeepBoth
	ReconcileReplaceRemote
	ReconcileReplaceLocal
)

//nolint:gocyclo // one case per member: the name table of the enum
func (k ReconciliationKind) String() string {
	switch k {
	case ReconcileUnclassified:
		return reconciliationKindUnclassified
	case ReconcileNoop:
		return "noop"
	case ReconcileBaselineRemove:
		return "baseline_remove"
	case ReconcileBaselineUpdate:
		return "baseline_update"
	case ReconcileFolderCreateLocal:
		return "folder_create_local"
	case ReconcileFolderCreateRemote:
		return "folder_create_remote"
	case ReconcileUpload:
		return "upload"
	case ReconcileDownload:
		return "download"
	case ReconcileLocalDelete:
		return "local_delete"
	case ReconcileRemoteDelete:
		return "remote_delete"
	case ReconcileLocalMove:
		return "local_move"
	case ReconcileRemoteMove:
		return "remote_move"
	case ReconcileConflictEditEdit:
		return "conflict_edit_edit"
	case ReconcileConflictEditDelete:
		return "conflict_edit_delete"
	case ReconcileConflictCreateCreate:
		return "conflict_create_create"
	case ReconcileConflictKeepBoth:
		return "conflict_keep_both"
	case ReconcileReplaceRemote:
		return "replace_remote"
	case ReconcileReplaceLocal:
		return "replace_local"
	default:
		return fmt.Sprintf("ReconciliationKind(%d)", uint8(k))
	}
}

// PlannerDecisionInput is one input of the decision table as the plan error
// prints it (DecisionState.Inputs).
type PlannerDecisionInput struct {
	Column string
	// Value is a side's item type (empty when absent) or a flag as 0 or 1.
	Value string
}
