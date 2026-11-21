package syncdomain

type SQLiteComparisonRow struct {
	Path                    string
	BaselineItemID          string
	RemoteItemID            string
	ItemType                ItemType
	BaselinePresent         bool
	LocalPresent            bool
	RemotePresent           bool
	LocalChanged            bool
	RemoteChanged           bool
	CurrentEqual            bool
	LocalMoveTarget         string
	LocalMoveSource         string
	LocalMoveCandidateCount int
	RemoteMoveTarget        string
	RemoteMoveSource        string
	// BaselineType, LocalType and RemoteType are what each side holds at the
	// path; the flags below are the rest of the decision table's inputs.
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
	ComparisonKind                      ComparisonKind
}

// DecisionState is the decision table's input this row carries. The move
// booleans take the candidate count as the table reads it: one candidate
// pairs, more than one pairs nothing.
func (r *SQLiteComparisonRow) DecisionState() DecisionState {
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
