package syncplan

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.2.16
//
// The refusal runs before any per-row policy: an unclassified row at a path
// whose local truth is unavailable, which the suppression would otherwise
// plan as nothing (the classified control below), still fails the plan, and
// the error names the first such path and counts the others.
func TestPlanCurrentState_UnclassifiedRowFailsEvenWhereTruthIsUnavailable(t *testing.T) {
	t.Parallel()

	plan := func(kind syncdomain.ReconciliationKind) (*syncdomain.ActionPlan, error) {
		return NewPlanner(syncfixture.TestLogger(t)).PlanCurrentState(
			[]DecidedComparison{
				{ComparisonRow: syncdomain.ComparisonRow{Path: "docs/a.txt", LocalType: syncdomain.SideFile}, ComparisonKind: syncdomain.ComparisonLocalOnlyCreate},
				{ComparisonRow: syncdomain.ComparisonRow{Path: "docs/b.txt", LocalType: syncdomain.SideFile}, ComparisonKind: syncdomain.ComparisonLocalOnlyCreate},
			},
			[]syncdomain.SQLiteReconciliationRow{
				{Path: "docs/a.txt", ItemType: syncdomain.ItemTypeFile, ComparisonKind: syncdomain.ComparisonLocalOnlyCreate, ReconciliationKind: kind},
				{Path: "docs/b.txt", ItemType: syncdomain.ItemTypeFile, ComparisonKind: syncdomain.ComparisonLocalOnlyCreate, ReconciliationKind: kind},
			},
			[]syncdomain.LocalStateRow{
				{Path: "docs/a.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-a"},
				{Path: "docs/b.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-b"},
			},
			nil,
			[]syncdomain.ObservationIssueRow{
				{Path: "docs/a.txt", IssueType: syncdomain.IssueLocalReadDenied},
				{Path: "docs/b.txt", IssueType: syncdomain.IssueLocalReadDenied},
			},
			syncdomain.NewBaselineSnapshot(nil),
			PlannerMountContext{},
			syncdomain.SyncBidirectional,
		)
	}

	suppressed, err := plan(syncdomain.ReconcileUpload)
	require.NoError(t, err)
	assert.Empty(t, suppressed.Actions, "the control: unavailable truth suppresses the classified rows")

	_, err = plan(syncdomain.ReconcileUnclassified)
	require.ErrorIs(t, err, ErrUnclassifiedPlan)
	var unclassified *unclassifiedPlanError
	require.ErrorAs(t, err, &unclassified)
	assert.Equal(t, "docs/a.txt", unclassified.Path)
	assert.Equal(t, syncdomain.ComparisonLocalOnlyCreate, unclassified.Comparison)
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

	_, err := NewPlanner(syncfixture.TestLogger(t)).PlanCurrentState(
		[]DecidedComparison{{ComparisonRow: syncdomain.ComparisonRow{Path: "docs/a.txt", LocalType: syncdomain.SideFile}}},
		nil,
		[]syncdomain.LocalStateRow{{Path: "docs/a.txt", ItemType: syncdomain.ItemTypeFile, Hash: "local-a"}},
		nil,
		nil,
		syncdomain.NewBaselineSnapshot(nil),
		PlannerMountContext{},
		syncdomain.SyncBidirectional,
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

	_, err := NewPlanner(syncfixture.TestLogger(t)).PlanCurrentState(
		[]DecidedComparison{{ComparisonRow: syncdomain.ComparisonRow{Path: "local-dir", LocalType: syncdomain.SideFolder}, ComparisonKind: syncdomain.ComparisonLocalOnlyCreate}},
		[]syncdomain.SQLiteReconciliationRow{{
			Path:               "local-dir",
			ItemType:           syncdomain.ItemTypeFolder,
			ComparisonKind:     syncdomain.ComparisonLocalOnlyCreate,
			ReconciliationKind: syncdomain.ReconcileUpload,
		}},
		[]syncdomain.LocalStateRow{{Path: "local-dir", ItemType: syncdomain.ItemTypeFolder}},
		nil,
		nil,
		syncdomain.NewBaselineSnapshot(nil),
		PlannerMountContext{},
		syncdomain.SyncBidirectional,
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
		unlistedAction = syncdomain.ActionCleanup + 1
		unlistedSide   = syncdomain.CreateRemote + 1
	)
	listed := syncdomain.Action{Type: syncdomain.ActionUpload, Path: "docs/listed.txt", View: &syncdomain.PathView{Path: "docs/listed.txt"}}
	plan := func(reconciled, composed []syncdomain.Action) (*syncdomain.ActionPlan, error) {
		return NewPlanner(syncfixture.TestLogger(t)).planBuiltActions(reconciled, composed, nil,
			&moveComposition{}, &moveInputs{}, PlannerMountContext{}, syncdomain.SyncBidirectional)
	}

	control, err := plan([]syncdomain.Action{listed}, nil)
	require.NoError(t, err)
	require.Len(t, control.Actions, 1, "the control: a listed action is planned")

	for _, tc := range []struct {
		name                 string
		reconciled, composed []syncdomain.Action
		wantDetail           string
	}{
		{
			name:       "reconciled",
			reconciled: []syncdomain.Action{listed, {Type: unlistedAction, Path: "docs/a.txt"}},
			wantDetail: `ActionType(10) at "docs/a.txt"`,
		},
		{
			name:       "composed",
			reconciled: []syncdomain.Action{listed},
			composed:   []syncdomain.Action{{Type: unlistedAction, Path: "docs/b.txt", OldPath: "docs/old.txt"}},
			wantDetail: `ActionType(10) at "docs/b.txt"`,
		},
		{
			name:       "folder create side",
			reconciled: []syncdomain.Action{{Type: syncdomain.ActionFolderCreate, CreateSide: unlistedSide, Path: "docs/c"}},
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
