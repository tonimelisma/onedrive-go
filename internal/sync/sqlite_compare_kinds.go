package sync

import (
	"fmt"
)

// kindUnclassified is the name both kind enums give their zero member, and the
// literal both SQL CASE expressions return from their ELSE.
const kindUnclassified = "unclassified"

// comparisonKind is the per-path comparison the SQL decision table computes
// (sqlComparisonKindCase): which of baseline, local and remote hold the path,
// and how the sides relate to the baseline. The enum is the vocabulary; the
// SQL carries its names as literals, which
// TestPlannerKindCases_LiteralsAreExactlyTheKinds holds to the enum.
//
// The zero value is comparisonUnclassified, the CASE's ELSE: a state no rule
// covers, which planning refuses (validatePlannerKinds). Members are never
// persisted, so their numbers may change; comparisonDiverged stays last,
// because allComparisonKinds and parseComparisonKind range up to it.
type comparisonKind uint8

const (
	comparisonUnclassified comparisonKind = iota
	comparisonLocalMoveSource
	comparisonDeleteAwaitsAncestorMove
	comparisonBothMissing
	comparisonLocalMoveDest
	comparisonRemoteMoveSource
	comparisonRemoteMoveDest
	comparisonKeptLocalFolder
	comparisonLocalOnlyCreate
	comparisonRemoteMoveShadowed
	comparisonRemoteOnlyCreate
	comparisonCreateEqual
	comparisonCreateConflict
	comparisonUnchanged
	comparisonEqualAgain
	comparisonLocalMissing
	comparisonRemoteMissing
	comparisonDiverged
)

//nolint:gocyclo // one case per member: the name table of the enum
func (k comparisonKind) String() string {
	switch k {
	case comparisonUnclassified:
		return kindUnclassified
	case comparisonLocalMoveSource:
		return "local_move_source"
	case comparisonDeleteAwaitsAncestorMove:
		return "delete_awaits_ancestor_move"
	case comparisonBothMissing:
		return "both_missing"
	case comparisonLocalMoveDest:
		return "local_move_dest"
	case comparisonRemoteMoveSource:
		return "remote_move_source"
	case comparisonRemoteMoveDest:
		return "remote_move_dest"
	case comparisonKeptLocalFolder:
		return "kept_local_folder"
	case comparisonLocalOnlyCreate:
		return "local_only_create"
	case comparisonRemoteMoveShadowed:
		return "remote_move_shadowed"
	case comparisonRemoteOnlyCreate:
		return "remote_only_create"
	case comparisonCreateEqual:
		return "create_equal"
	case comparisonCreateConflict:
		return "create_conflict"
	case comparisonUnchanged:
		return "unchanged" //nolint:goconst // an enum name; the other use in the package is a test file's content
	case comparisonEqualAgain:
		return "equal_again"
	case comparisonLocalMissing:
		return "local_missing"
	case comparisonRemoteMissing:
		return "remote_missing"
	case comparisonDiverged:
		return "diverged"
	default:
		return fmt.Sprintf("comparisonKind(%d)", uint8(k))
	}
}

// allComparisonKinds lists every member in declaration order, the
// unclassified one first.
func allComparisonKinds() []comparisonKind {
	kinds := make([]comparisonKind, 0, int(comparisonDiverged)+1)
	for k := comparisonUnclassified; k <= comparisonDiverged; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}

// parseComparisonKind reads the comparison kind SQL returned. "unclassified"
// parses to the zero member: it is a state outside the decision table, which
// planning refuses with the row's inputs. Any other unknown name is the SQL
// and the enum drifting apart.
func parseComparisonKind(s string) (comparisonKind, error) {
	for k := comparisonUnclassified; k <= comparisonDiverged; k++ {
		if k.String() == s {
			return k, nil
		}
	}

	return comparisonUnclassified, fmt.Errorf("sync: unknown comparison kind %q", s)
}

// reconciliationKind is the decision the SQL decision table takes for a path
// (sqlReconciliationKindCase), which the planner maps to actions
// (buildActionsForReconciliation). It owns its names: some equal action-type
// names (local_delete, upload, ...), but those are a persisted wire format
// (retry_work.action_type) and coincide by choice, not by definition.
//
// The zero value is reconcileUnclassified, the CASE's ELSE; reconcileNoop is
// the explicit "nothing to do" that the decision table states for a path.
// reconcileReplaceLocal stays last (allReconciliationKinds,
// parseReconciliationKind).
type reconciliationKind uint8

const (
	reconcileUnclassified reconciliationKind = iota
	reconcileNoop
	reconcileBaselineRemove
	reconcileBaselineUpdate
	reconcileFolderCreateLocal
	reconcileFolderCreateRemote
	reconcileUpload
	reconcileDownload
	reconcileLocalDelete
	reconcileRemoteDelete
	reconcileLocalMove
	reconcileRemoteMove
	reconcileConflictEditEdit
	reconcileConflictEditDelete
	reconcileConflictCreateCreate
	reconcileConflictKeepBoth
	reconcileReplaceRemote
	reconcileReplaceLocal
)

//nolint:gocyclo // one case per member: the name table of the enum
func (k reconciliationKind) String() string {
	switch k {
	case reconcileUnclassified:
		return kindUnclassified
	case reconcileNoop:
		return "noop"
	case reconcileBaselineRemove:
		return "baseline_remove"
	case reconcileBaselineUpdate:
		return "baseline_update"
	case reconcileFolderCreateLocal:
		return "folder_create_local"
	case reconcileFolderCreateRemote:
		return "folder_create_remote"
	case reconcileUpload:
		return "upload"
	case reconcileDownload:
		return "download"
	case reconcileLocalDelete:
		return "local_delete"
	case reconcileRemoteDelete:
		return "remote_delete"
	case reconcileLocalMove:
		return "local_move"
	case reconcileRemoteMove:
		return "remote_move"
	case reconcileConflictEditEdit:
		return "conflict_edit_edit"
	case reconcileConflictEditDelete:
		return "conflict_edit_delete"
	case reconcileConflictCreateCreate:
		return "conflict_create_create"
	case reconcileConflictKeepBoth:
		return "conflict_keep_both"
	case reconcileReplaceRemote:
		return "replace_remote"
	case reconcileReplaceLocal:
		return "replace_local"
	default:
		return fmt.Sprintf("reconciliationKind(%d)", uint8(k))
	}
}

// allReconciliationKinds lists every member in declaration order, the
// unclassified one first.
func allReconciliationKinds() []reconciliationKind {
	kinds := make([]reconciliationKind, 0, int(reconcileReplaceLocal)+1)
	for k := reconcileUnclassified; k <= reconcileReplaceLocal; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}

// parseReconciliationKind reads the reconciliation kind SQL returned, with
// the same contract as parseComparisonKind.
func parseReconciliationKind(s string) (reconciliationKind, error) {
	for k := reconcileUnclassified; k <= reconcileReplaceLocal; k++ {
		if k.String() == s {
			return k, nil
		}
	}

	return reconcileUnclassified, fmt.Errorf("sync: unknown reconciliation kind %q", s)
}

// plannerDecisionInput is one input column of the SQL decision table as the
// planning transaction's diagnostic read found it (sqlQueryPlannerDecisionInputs).
type plannerDecisionInput struct {
	Column string
	// Value is the column's value as text, or "NULL": no rule tests for a
	// NULL, so a flag expression that yields one leaves the row unclassified.
	Value string
}
