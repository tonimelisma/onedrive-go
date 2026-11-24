package syncplan

import (
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Decider is the decision table: Decide, or a test's stand-in
// that removes one rule to prove a pass fails closed on the row it
// classified (R-6.2.16).
type Decider func(syncdomain.DecisionState) Decision

// DecidedComparison is a comparison row of one planning read with the
// comparison kind the decision table gave it (DecideComparisons). The kind is
// the planner's: the store's row cannot carry one.
type DecidedComparison struct {
	syncdomain.ComparisonRow
	ComparisonKind ComparisonKind
}

// DecideComparisons decides every comparison row of one planning read with
// decide: it returns the rows with their comparison kinds, and
// each row's reconciliation row, in the same order. The store reads the rows
// and decides nothing (syncstore's ReadPlanningSnapshot); the planner owns
// the decision. An unclassified row carries its inputs for the plan error
// PlanCurrentState returns (R-6.2.16).
func DecideComparisons(
	comparisons []syncdomain.ComparisonRow,
	decide Decider,
) ([]DecidedComparison, []ReconciliationRow) {
	decided := make([]DecidedComparison, len(comparisons))
	reconciliations := make([]ReconciliationRow, len(comparisons))
	for i := range comparisons {
		decision := decide(comparisons[i].DecisionState())
		decided[i] = DecidedComparison{ComparisonRow: comparisons[i], ComparisonKind: decision.Comparison}
		reconciliations[i] = reconciliationRowOf(&comparisons[i], decision)
	}

	return decided, reconciliations
}

// PlanSnapshot decides the comparison rows of snapshot with decide
// (DecideComparisons) and plans the current actionable set from them and
// the snapshot's other facts (PlanCurrentState).
func (p *Planner) PlanSnapshot(
	snapshot *syncdomain.PlanningSnapshot,
	decide Decider,
	mount PlannerMountContext,
	mode syncdomain.SyncMode,
) (*syncdomain.ActionPlan, error) {
	comparisons, reconciliations := DecideComparisons(snapshot.Comparisons, decide)

	return p.PlanCurrentState(
		comparisons,
		reconciliations,
		snapshot.Local,
		snapshot.Remote,
		snapshot.ObservationIssues,
		snapshot.Baseline,
		mount,
		mode,
	)
}
