package syncdomain

import (
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// PlanningSnapshot is the current truth one planning read returns
// (syncstore's SyncStore.ReadPlanningSnapshot): every fact below was read in
// one store transaction, so a commit that lands during the read is in all of
// them or in none. The facts are raw: the comparison rows carry the decision
// table's inputs but no decided kind, since deciding is the planner's
// (syncplan.DecideComparisons).
type PlanningSnapshot struct {
	// ContentDriveID is the drive whose content the store mirrors, or the
	// default drive the read was given while the store has recorded none; a
	// remote row without a drive of its own is this drive's.
	ContentDriveID driveid.ID
	// Comparisons are the planner-visible paths' comparison rows in path
	// order.
	Comparisons []ComparisonRow
	// Local and Remote are the planner-visible current rows: the content
	// filter applied, and the rows below a folder the baseline knows but the
	// side no longer has pruned.
	Local  []LocalStateRow
	Remote []RemoteStateRow
	// ObservationIssues are the observation issues the content filter keeps.
	ObservationIssues []ObservationIssueRow
	// Baseline is read in the same transaction as the comparison rows, so a
	// row with a baseline side always has its entry. It is a snapshot, a type
	// a run's live baseline view is not: the planner cannot be handed the
	// cache, which can lag or lead the comparison rows when a commit lands
	// between them.
	Baseline *BaselineSnapshot
}

// ComparisonRow is one path's comparison facts as the planning read returns
// them: the decision table's inputs and the move pairings, never a decided
// kind (the planner's DecidedComparison carries that).
type ComparisonRow struct {
	Path                    string
	BaselineItemID          string
	RemoteItemID            string
	LocalChanged            bool
	RemoteChanged           bool
	CurrentEqual            bool
	LocalMoveTarget         string
	LocalMoveSource         string
	LocalMoveCandidateCount int
	RemoteMoveTarget        string
	RemoteMoveSource        string
	// BaselineType, LocalType and RemoteType are what each side holds at the
	// path, SideAbsent where it holds nothing: whether a side is present and
	// the path's item type are read from them (ItemType). The schema makes
	// every item_type of the three tables, and of the planner-visible copies,
	// one of the enum's names, so a side is present exactly when its type is
	// not SideAbsent. The flags below are the rest of the decision table's
	// inputs.
	BaselineType, LocalType, RemoteType SideType
	LocalItemReplaced                   bool
	RemoteItemReplaced                  bool
	DeleteAwaitsAncestorMove            bool
	LocalCopyAwaitsAncestorMove         bool
	RemoteCreateAwaitsAncestorMove      bool
	KeptLocalFolder                     bool
	RemoteMoveShadowed                  bool
	LocalSubtreeChanged                 bool
	RemoteSubtreeChanged                bool
	MetadataDrift                       bool
}

// ItemType is the path's item type: that of the first side present of the
// baseline, local and remote ones. Every row of the comparison query has a
// side present, as its path comes from one; a row with none, which only a
// test can build, is a file, the enum's zero value.
func (r *ComparisonRow) ItemType() ItemType {
	for _, side := range []SideType{r.BaselineType, r.LocalType, r.RemoteType} {
		switch side {
		case SideFile:
			return ItemTypeFile
		case SideFolder:
			return ItemTypeFolder
		case SideRoot:
			return ItemTypeRoot
		case SideAbsent:
		default:
			panic(fmt.Sprintf("sync: item type of %v", side))
		}
	}

	return ItemTypeFile
}

// DecisionState is the decision table's input this row carries. The move
// booleans take the candidate count as the table reads it: one candidate
// pairs, more than one pairs nothing.
func (r *ComparisonRow) DecisionState() DecisionState {
	single := r.LocalMoveCandidateCount == 1

	return DecisionState{
		Baseline:                       r.BaselineType,
		Local:                          r.LocalType,
		Remote:                         r.RemoteType,
		LocalMoveSource:                single && r.LocalMoveTarget != "",
		LocalMoveDest:                  single && r.LocalMoveSource != "",
		RemoteMoveSource:               r.RemoteMoveTarget != "",
		RemoteMoveDest:                 r.RemoteMoveSource != "",
		LocalItemReplaced:              r.LocalItemReplaced,
		RemoteItemReplaced:             r.RemoteItemReplaced,
		DeleteAwaitsAncestorMove:       r.DeleteAwaitsAncestorMove,
		LocalCopyAwaitsAncestorMove:    r.LocalCopyAwaitsAncestorMove,
		RemoteCreateAwaitsAncestorMove: r.RemoteCreateAwaitsAncestorMove,
		KeptLocalFolder:                r.KeptLocalFolder,
		RemoteMoveShadowed:             r.RemoteMoveShadowed,
		CurrentEqual:                   r.CurrentEqual,
		LocalChanged:                   r.LocalChanged,
		RemoteChanged:                  r.RemoteChanged,
		LocalSubtreeChanged:            r.LocalSubtreeChanged,
		RemoteSubtreeChanged:           r.RemoteSubtreeChanged,
		MetadataDrift:                  r.MetadataDrift,
	}
}

// ReconciliationRowOf is the reconciliation row decision makes of the
// comparison row r. An unclassified row carries the decision table's inputs,
// so the plan error names them (R-6.2.16).
func ReconciliationRowOf(r *ComparisonRow, decision Decision) SQLiteReconciliationRow {
	rec := SQLiteReconciliationRow{
		Path:               r.Path,
		ItemType:           r.ItemType(),
		ComparisonKind:     decision.Comparison,
		ReconciliationKind: decision.Reconciliation,
		LocalMoveTarget:    r.LocalMoveTarget,
		LocalMoveSource:    r.LocalMoveSource,
		RemoteMoveTarget:   r.RemoteMoveTarget,
		RemoteMoveSource:   r.RemoteMoveSource,
	}
	if rec.IsUnclassified() {
		rec.DecisionInputs = r.DecisionState().Inputs()
	}

	return rec
}

type SQLiteReconciliationRow struct {
	Path               string
	ItemType           ItemType
	ComparisonKind     ComparisonKind
	ReconciliationKind ReconciliationKind
	LocalMoveTarget    string
	LocalMoveSource    string
	RemoteMoveTarget   string
	RemoteMoveSource   string
	// DecisionInputs are the decision-table inputs of an unclassified row,
	// for the plan error (DecisionState.Inputs); nil for every classified
	// row.
	DecisionInputs []PlannerDecisionInput
}

// IsUnclassified reports a reconciliation row outside the decision table: the
// comparison has no rule, or no reconciliation rule matched it.
func (r *SQLiteReconciliationRow) IsUnclassified() bool {
	return r.ComparisonKind == ComparisonUnclassified || r.ReconciliationKind == ReconcileUnclassified
}
