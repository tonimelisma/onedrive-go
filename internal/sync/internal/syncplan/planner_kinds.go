package syncplan

import (
	"errors"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ErrUnclassifiedPlan is the class of every unclassifiedPlanError.
var ErrUnclassifiedPlan = errors.New("sync: planner state is outside the decision table")

// unclassifiedPlanError is a plan that met a row the decision table does not
// classify (R-6.2.16). A path planned as nothing to do silently stops
// syncing, so the plan fails instead, and the error carries what a fix to the
// table needs: the path, both kinds and, when the engine read them, the
// inputs no rule matched. It stops the drive's sync pass; a watch runner that
// ends on it is restarted after its backoff (R-2.8.11).
type unclassifiedPlanError struct {
	Path           string
	Comparison     syncdomain.ComparisonKind
	Reconciliation syncdomain.ReconciliationKind
	// Inputs are nil for rows handed to the planner without the engine's
	// diagnostic read (tests, the planner model).
	Inputs []syncdomain.PlannerDecisionInput
	// Others counts the plan's further unclassified rows.
	Others int
}

func (e *unclassifiedPlanError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %q (comparison %s, reconciliation %s", ErrUnclassifiedPlan.Error(), e.Path, e.Comparison, e.Reconciliation)
	for i, input := range e.Inputs {
		separator := " "
		if i == 0 {
			separator = "; "
		}
		fmt.Fprintf(&b, "%s%s=%s", separator, input.Column, input.Value)
	}
	b.WriteString(")")
	switch {
	case e.Others == 1:
		b.WriteString(" and 1 more path")
	case e.Others > 1:
		fmt.Fprintf(&b, " and %d more paths", e.Others)
	}

	return b.String()
}

func (e *unclassifiedPlanError) Unwrap() error {
	return ErrUnclassifiedPlan
}

func unclassifiedRowError(rec *syncdomain.SQLiteReconciliationRow) *unclassifiedPlanError {
	return &unclassifiedPlanError{
		Path:           rec.Path,
		Comparison:     rec.ComparisonKind,
		Reconciliation: rec.ReconciliationKind,
		Inputs:         rec.DecisionInputs,
	}
}

// FirstUnclassifiedRow returns the index of the first unclassified row, or -1.
func FirstUnclassifiedRow(rows []syncdomain.SQLiteReconciliationRow) int {
	for i := range rows {
		if rows[i].IsUnclassified() {
			return i
		}
	}

	return -1
}

// validatePlannerKinds refuses a plan with any row outside the decision table.
// PlanCurrentState runs it before any per-row policy, the unavailable-truth
// suppression included, so no early return can plan such a row as nothing.
// The error names the first unclassified path (rows come ordered by path) and
// counts the rest. Comparison rows are checked too: move composition reads
// their kinds directly.
func validatePlannerKinds(comparisons []DecidedComparison, reconciliations []syncdomain.SQLiteReconciliationRow) error {
	if first := FirstUnclassifiedRow(reconciliations); first >= 0 {
		unclassified := unclassifiedRowError(&reconciliations[first])
		for i := first + 1; i < len(reconciliations); i++ {
			if reconciliations[i].IsUnclassified() {
				unclassified.Others++
			}
		}

		return unclassified
	}
	for i := range comparisons {
		if comparisons[i].ComparisonKind == syncdomain.ComparisonUnclassified {
			// No reconciliation rule matches an unclassified comparison
			// (the SQL tests for named kinds only), so its reconciliation
			// is unclassified too.
			return &unclassifiedPlanError{
				Path:           comparisons[i].Path,
				Comparison:     syncdomain.ComparisonUnclassified,
				Reconciliation: syncdomain.ReconcileUnclassified,
			}
		}
	}

	return nil
}

// errUnlistedActionPlanned is the class of a plan holding an action whose type,
// or whose folder create side, is outside its enum (R-6.2.16). The planner
// builds every action from a constant, so only a programming error makes one.
// Every rule after the build decides by action type, and no rule has a verdict
// for such a value: let through, its path would silently stop syncing. Like a
// row outside the decision table, it fails the plan instead.
var errUnlistedActionPlanned = errors.New("sync: planned action is outside the action enums")

// validatePlannedActions refuses built actions holding a value outside the
// action enums, naming the first. planBuiltActions runs it before any rule
// reads the actions, so each rule's own default, which panics (OutsideTheEnum),
// stays unreachable.
func validatePlannedActions(actions []syncdomain.Action) error {
	for i := range actions {
		action := &actions[i]
		if !action.Type.IsMember() {
			return fmt.Errorf("%w: %s at %q", errUnlistedActionPlanned, action.Type, action.Path)
		}
		if action.Type == syncdomain.ActionFolderCreate && !action.CreateSide.IsMember() {
			return fmt.Errorf("%w: %s on side %s at %q", errUnlistedActionPlanned, action.Type, action.CreateSide, action.Path)
		}
	}

	return nil
}
