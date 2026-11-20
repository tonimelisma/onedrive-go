package syncdomain

import (
	"fmt"
)

// kindUnclassified is the name both kind enums give their zero member, and the
// literal both SQL CASE expressions return from their ELSE.
const kindUnclassified = "unclassified"

// ComparisonKind is the per-path comparison the SQL decision table computes
// (sqlComparisonKindCase): which of baseline, local and remote hold the path,
// and how the sides relate to the baseline. The enum is the vocabulary; the
// SQL carries its names as literals, which
// TestPlannerKindCases_LiteralsAreExactlyTheKinds holds to the enum.
//
// The zero value is ComparisonUnclassified, the CASE's ELSE: a state no rule
// covers, which planning refuses (validatePlannerKinds). Members are never
// persisted, so their numbers may change; ComparisonDiverged stays last,
// because AllComparisonKinds and ParseComparisonKind range up to it.
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
		return "unchanged" //nolint:goconst // an enum name; the other use in the package is a test file's content
	case ComparisonEqualAgain:
		return "equal_again"
	case comparisonLocalMissing:
		return "local_missing"
	case comparisonRemoteMissing:
		return "remote_missing"
	case ComparisonDiverged:
		return "diverged"
	default:
		return fmt.Sprintf("comparisonKind(%d)", uint8(k))
	}
}

// AllComparisonKinds lists every member in declaration order, the
// unclassified one first.
func AllComparisonKinds() []ComparisonKind {
	kinds := make([]ComparisonKind, 0, int(ComparisonDiverged)+1)
	for k := ComparisonUnclassified; k <= ComparisonDiverged; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}

// ParseComparisonKind reads the comparison kind SQL returned. "unclassified"
// parses to the zero member: it is a state outside the decision table, which
// planning refuses with the row's inputs. Any other unknown name is the SQL
// and the enum drifting apart.
func ParseComparisonKind(s string) (ComparisonKind, error) {
	for k := ComparisonUnclassified; k <= ComparisonDiverged; k++ {
		if k.String() == s {
			return k, nil
		}
	}

	return ComparisonUnclassified, fmt.Errorf("sync: unknown comparison kind %q", s)
}

// ReconciliationKind is the decision the SQL decision table takes for a path
// (sqlReconciliationKindCase), which the planner maps to actions
// (buildActionsForReconciliation). It owns its names: some equal action-type
// names (local_delete, upload, ...), but those are a persisted wire format
// (retry_work.action_type) and coincide by choice, not by definition.
//
// The zero value is ReconcileUnclassified, the CASE's ELSE; ReconcileNoop is
// the explicit "nothing to do" that the decision table states for a path.
// ReconcileReplaceLocal stays last (AllReconciliationKinds,
// ParseReconciliationKind).
type ReconciliationKind uint8

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
		return kindUnclassified
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
		return fmt.Sprintf("reconciliationKind(%d)", uint8(k))
	}
}

// AllReconciliationKinds lists every member in declaration order, the
// unclassified one first.
func AllReconciliationKinds() []ReconciliationKind {
	kinds := make([]ReconciliationKind, 0, int(ReconcileReplaceLocal)+1)
	for k := ReconcileUnclassified; k <= ReconcileReplaceLocal; k++ {
		kinds = append(kinds, k)
	}

	return kinds
}

// ParseReconciliationKind reads the reconciliation kind SQL returned, with
// the same contract as ParseComparisonKind.
func ParseReconciliationKind(s string) (ReconciliationKind, error) {
	for k := ReconcileUnclassified; k <= ReconcileReplaceLocal; k++ {
		if k.String() == s {
			return k, nil
		}
	}

	return ReconcileUnclassified, fmt.Errorf("sync: unknown reconciliation kind %q", s)
}

// PlannerDecisionInput is one input column of the SQL decision table as the
// planning transaction's diagnostic read found it (sqlQueryPlannerDecisionInputs).
type PlannerDecisionInput struct {
	Column string
	// Value is the column's value as text, or "NULL": no rule tests for a
	// NULL, so a flag expression that yields one leaves the row unclassified.
	Value string
}
