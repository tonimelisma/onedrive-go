package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.2.16
//
// The refusal runs before any per-row policy: an unclassified row at a path
// whose local truth is unavailable, which the suppression would otherwise
// plan as nothing (the classified control below), still fails the plan, and
// the error names the first such path and counts the others.
func TestPlanCurrentState_UnclassifiedRowFailsEvenWhereTruthIsUnavailable(t *testing.T) {
	t.Parallel()

	plan := func(kind ReconciliationKind) (*ActionPlan, error) {
		return NewPlanner(testLogger(t)).PlanCurrentState(
			[]SQLiteComparisonRow{
				{Path: "docs/a.txt", LocalPresent: true, ComparisonKind: ComparisonLocalOnlyCreate},
				{Path: "docs/b.txt", LocalPresent: true, ComparisonKind: ComparisonLocalOnlyCreate},
			},
			[]SQLiteReconciliationRow{
				{Path: "docs/a.txt", ItemType: ItemTypeFile, ComparisonKind: ComparisonLocalOnlyCreate, ReconciliationKind: kind},
				{Path: "docs/b.txt", ItemType: ItemTypeFile, ComparisonKind: ComparisonLocalOnlyCreate, ReconciliationKind: kind},
			},
			[]LocalStateRow{
				{Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "local-a"},
				{Path: "docs/b.txt", ItemType: ItemTypeFile, Hash: "local-b"},
			},
			nil,
			[]ObservationIssueRow{
				{Path: "docs/a.txt", IssueType: IssueLocalReadDenied},
				{Path: "docs/b.txt", IssueType: IssueLocalReadDenied},
			},
			NewBaselineForTest(nil),
			PlannerMountContext{},
			SyncBidirectional,
		)
	}

	suppressed, err := plan(ReconcileUpload)
	require.NoError(t, err)
	assert.Empty(t, suppressed.Actions, "the control: unavailable truth suppresses the classified rows")

	_, err = plan(ReconcileUnclassified)
	require.ErrorIs(t, err, ErrUnclassifiedPlan)
	var unclassified *unclassifiedPlanError
	require.ErrorAs(t, err, &unclassified)
	assert.Equal(t, "docs/a.txt", unclassified.Path)
	assert.Equal(t, ComparisonLocalOnlyCreate, unclassified.Comparison)
	assert.Equal(t, 1, unclassified.Others)
	assert.EqualError(t, err, `sync: planner state is outside the decision table: "docs/a.txt" `+
		`(comparison local_only_create, reconciliation unclassified) and 1 more path`)
}

// Validates: R-6.2.16
//
// A comparison row the planner is handed without a matching classified
// reconciliation fails the plan too: move composition reads comparison kinds
// directly.
func TestPlanCurrentState_UnclassifiedComparisonRowFailsThePlan(t *testing.T) {
	t.Parallel()

	_, err := NewPlanner(testLogger(t)).PlanCurrentState(
		[]SQLiteComparisonRow{{Path: "docs/a.txt", LocalPresent: true}},
		nil,
		[]LocalStateRow{{Path: "docs/a.txt", ItemType: ItemTypeFile, Hash: "local-a"}},
		nil,
		nil,
		NewBaselineForTest(nil),
		PlannerMountContext{},
		SyncBidirectional,
	)
	require.ErrorIs(t, err, ErrUnclassifiedPlan)
	require.ErrorContains(t, err, `"docs/a.txt"`)
}

// Validates: R-2.2.6, R-6.2.16
//
// A folder planned as a content transfer is a comparison bug. It used to be
// logged and dropped, a path that silently stopped syncing; it now fails the
// plan, naming the path.
func TestPlanCurrentState_FolderTransferFailsThePlan(t *testing.T) {
	t.Parallel()

	_, err := NewPlanner(testLogger(t)).PlanCurrentState(
		[]SQLiteComparisonRow{{Path: "local-dir", LocalPresent: true, ComparisonKind: ComparisonLocalOnlyCreate}},
		[]SQLiteReconciliationRow{{
			Path:               "local-dir",
			ItemType:           ItemTypeFolder,
			ComparisonKind:     ComparisonLocalOnlyCreate,
			ReconciliationKind: ReconcileUpload,
		}},
		[]LocalStateRow{{Path: "local-dir", ItemType: ItemTypeFolder}},
		nil,
		nil,
		NewBaselineForTest(nil),
		PlannerMountContext{},
		SyncBidirectional,
	)
	require.ErrorIs(t, err, errFolderTransferPlanned)
	require.ErrorContains(t, err, "local-dir")
}

// Validates: R-6.2.16
//
// PlanCurrentState builds every action from a constant, so only a
// programming error makes one outside the enums; the test hands such actions
// to planBuiltActions, the stage PlanCurrentState plans every built action
// through. It used to plan them: mode admission refused an unlisted type and
// the deferral counts had no bucket for it, so the action vanished and its
// path silently stopped syncing. Now the plan fails before any step reads the
// action, naming its path, whether reconciliation or move composition built
// it, and for a folder create whose side is outside its enum too.
func TestPlanCurrentState_UnlistedActionTypeFailsThePlan(t *testing.T) {
	t.Parallel()

	const (
		unlistedAction = ActionCleanup + 1
		unlistedSide   = CreateRemote + 1
	)
	listed := Action{Type: ActionUpload, Path: "docs/listed.txt", View: &PathView{Path: "docs/listed.txt"}}
	plan := func(reconciled, composed []Action) (*ActionPlan, error) {
		return NewPlanner(testLogger(t)).planBuiltActions(reconciled, composed, nil,
			&moveComposition{}, &moveInputs{}, PlannerMountContext{}, SyncBidirectional)
	}

	control, err := plan([]Action{listed}, nil)
	require.NoError(t, err)
	require.Len(t, control.Actions, 1, "the control: a listed action is planned")

	for _, tc := range []struct {
		name                 string
		reconciled, composed []Action
		wantDetail           string
	}{
		{
			name:       "reconciled",
			reconciled: []Action{listed, {Type: unlistedAction, Path: "docs/a.txt"}},
			wantDetail: `ActionType(10) at "docs/a.txt"`,
		},
		{
			name:       "composed",
			reconciled: []Action{listed},
			composed:   []Action{{Type: unlistedAction, Path: "docs/b.txt", OldPath: "docs/old.txt"}},
			wantDetail: `ActionType(10) at "docs/b.txt"`,
		},
		{
			name:       "folder create side",
			reconciled: []Action{{Type: ActionFolderCreate, CreateSide: unlistedSide, Path: "docs/c"}},
			wantDetail: `folder_create on side FolderCreateSide(2) at "docs/c"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			refused, err := plan(tc.reconciled, tc.composed)
			require.ErrorIs(t, err, errUnlistedActionPlanned)
			require.ErrorContains(t, err, tc.wantDetail)
			assert.Nil(t, refused)
		})
	}
}
