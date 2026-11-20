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
	ComparisonKind          ComparisonKind
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
	// read by the engine for the plan error (QueryPlannerDecisionInputs);
	// nil for every classified row.
	DecisionInputs []PlannerDecisionInput
}

// IsUnclassified reports a reconciliation row outside the decision table: the
// comparison has no rule, or no reconciliation rule matched it.
func (r *SQLiteReconciliationRow) IsUnclassified() bool {
	return r.ComparisonKind == ComparisonUnclassified || r.ReconciliationKind == ReconcileUnclassified
}
