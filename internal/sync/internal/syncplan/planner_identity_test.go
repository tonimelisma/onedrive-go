package syncplan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const reconcileIdentityGolden = "testdata/reconcile/matrix.identity.golden"

// identityMatrixDifferences is every cell of the reconciliation matrix whose
// identity-keyed plan differs from the current planner's, with why. A cell
// that differs and is not here, or is here and no longer differs, fails
// TestReconcileMatrix_IdentityGolden: every difference is a reviewed one.
//
//nolint:gochecknoglobals // the reviewed table the golden test checks against.
var identityMatrixDifferences = map[string]string{
	"local unchanged        | remote moved+edit doc3  ": "a move and an edit are two attributes of one node, so the " +
		"plan moves the file and downloads the edit in one pass; the current planner moves it and leaves the edit " +
		"to the next pass",
	"local moved doc2       | remote edited           ": "the same composition the other way round: the local move " +
		"goes up and OneDrive's edit comes down at the moved path in one pass",
	"local moved doc2       | remote moved+edit doc3  ": "OneDrive's position wins, as in the current planner, and " +
		"its edit comes down at that position in the same pass",
	"local moved+edit doc2  | remote unchanged        ": "the local identity joins the moved and edited file to its " +
		"item, so OneDrive's item is moved and its content replaced; the current planner cannot corroborate an " +
		"edited file's identity by content and deletes the item and uploads a new one, losing its history",
	"local moved+edit doc2  | remote moved doc2       ": "both sides moved the item to doc2 and only the local side " +
		"edited it: the move is agreed and the edit goes up; the current planner sees a delete and a create/create " +
		"conflict at doc2 and keeps a conflict copy",
	"local moved+edit doc2  | remote moved doc3       ": "OneDrive's position wins and the local edit is uploaded " +
		"there; the current planner keeps both as separate items (download at doc3, upload at doc2)",
	"local moved+edit doc2  | remote edited           ": "both sides edited the one item, so it is one content " +
		"conflict at the item's position (the local move goes up, a conflict copy keeps the local edit, OneDrive's " +
		"edit comes down); the current planner keeps both as separate items (download at doc, upload at doc2)",
	"local moved+edit doc2  | remote moved+edit doc3  ": "one item both sides moved and edited: OneDrive's " +
		"position wins and the two edits are a content conflict there; the current planner keeps them as " +
		"separate items",
}

// identityInputsForStore is what the identity-keyed planner reads of store,
// from the same planner-visible rows the current planner reads.
func identityInputsForStore(t *testing.T, s *pmState) *IdentityInputs {
	t.Helper()

	in := plannerInputsForStore(t, s.store)

	return &IdentityInputs{
		Baseline: in.baseline,
		Local:    in.localRows,
		Remote:   in.remoteRows,
		DriveID:  driveid.New(syncfixture.EngineTestDriveID),

		ObservationIssues: in.observationIssues,
	}
}

func (s *pmState) identityPlan() *syncdomain.ActionPlan {
	s.writeStore()
	plan, err := PlanIdentityKeyed(identityInputsForStore(s.t, s))
	require.NoError(s.t, err)

	return plan
}

// The identity-keyed planner's answer for every cell of the reconciliation
// matrix, checked in as testdata/reconcile/matrix.identity.golden, and every
// cell where it differs from the current planner's matrix.golden listed and
// explained in identityMatrixDifferences. Refresh it with
// `go test ./internal/sync/internal/syncplan -run TestReconcileMatrix_IdentityGolden -update`.
//
// Validates: R-2.3.1
func TestReconcileMatrix_IdentityGolden(t *testing.T) {
	syncfixture.SkipInShort(t)
	t.Parallel()

	current, err := os.ReadFile(filepath.FromSlash(reconcileMatrixGolden))
	require.NoError(t, err)
	currentPlans := matrixLinesByCell(string(current))

	var out strings.Builder
	differing := map[string]bool{}
	emit := func(cell, plan string) {
		out.WriteString(cell + "-> " + plan + "\n")
		if currentPlans[cell] != plan {
			differing[cell] = true
		}
	}
	out.WriteString("# baseline | local | remote -> plan (one path; files A/B/C, dir = folder, - = absent)\n")
	sides := []matrixSide{matrixAbsent, matrixA, matrixB, matrixC, matrixFolder}
	for _, baseline := range []matrixSide{matrixAbsent, matrixA, matrixFolder} {
		for _, local := range sides {
			for _, remote := range sides {
				s := matrixCell(t, baseline, local, remote)
				emit(fmt.Sprintf("%-3s | %-3s | %-3s ", baseline, local, remote), renderMatrixPlan(s.identityPlan()))
			}
		}
	}
	out.WriteString("\n# moves: baseline file A at doc; where each side holds it now\n")
	for _, local := range moveMatrixLocal() {
		for _, remote := range moveMatrixRemote() {
			s := newPMState(t)
			s.addSynced(matrixPath, false, matrixA.content())
			local.apply(s, true)
			remote.apply(s, false)
			emit(fmt.Sprintf("local %-16s | remote %-16s ", local.name, remote.name), renderMatrixPlan(s.identityPlan()))
		}
	}

	requireGolden(t, reconcileIdentityGolden, out.String())

	for cell := range differing {
		assert.Contains(t, identityMatrixDifferences, cell,
			"the identity planner's plan for %q differs from matrix.golden (%q) without an explanation",
			cell, currentPlans[cell])
	}
	for cell := range identityMatrixDifferences {
		assert.True(t, differing[cell], "%q is explained as a difference but both planners agree", cell)
	}
}

// matrixLinesByCell indexes a rendered matrix by its cell (everything before
// "-> ").
func matrixLinesByCell(golden string) map[string]string {
	byCell := map[string]string{}
	for _, line := range strings.Split(golden, "\n") {
		cell, plan, ok := strings.Cut(line, "-> ")
		if !ok || strings.HasPrefix(line, "#") {
			continue
		}
		byCell[cell] = plan
	}

	return byCell
}

// Validates: R-2.3.1
func TestPlanIdentityKeyed_LocalJoinConfidence(t *testing.T) {
	t.Parallel()

	base := &syncdomain.BaselineEntry{
		Path: "a", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}
	row := func(birth int64, itemType syncdomain.ItemType) *syncdomain.LocalStateRow {
		return &syncdomain.LocalStateRow{
			Path: "b", ItemType: itemType, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true, LocalBirthTime: birth,
		}
	}

	born := *base
	born.LocalBirthTime = 5
	assert.Equal(t, identityStrong, localJoinConfidence(&born, row(5, syncdomain.ItemTypeFile)))
	assert.Equal(t, identityByPath, localJoinConfidence(&born, row(6, syncdomain.ItemTypeFile)), "a reused inode")
	assert.Equal(t, identityWeak, localJoinConfidence(base, row(0, syncdomain.ItemTypeFile)))
	assert.Equal(t, identityWeak, localJoinConfidence(&born, row(0, syncdomain.ItemTypeFile)))
	assert.Equal(t, identityByPath, localJoinConfidence(base, row(0, syncdomain.ItemTypeFolder)), "another type")
	noIdentity := *base
	noIdentity.LocalHasIdentity = false
	assert.Equal(t, identityByPath, localJoinConfidence(&noIdentity, row(0, syncdomain.ItemTypeFile)))
}

// A hard link is one identity at two local paths: neither path can claim
// the baseline item by identity, so both fall back to path pairing.
//
// Validates: R-2.3.1
func TestPlanIdentityKeyed_HardLinkFallsBackToPaths(t *testing.T) {
	t.Parallel()

	entry := syncdomain.BaselineEntry{
		Path: "a", ItemID: "item-1", ItemType: syncdomain.ItemTypeFile, LocalHash: "h", RemoteHash: "h",
		LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&entry}),
		Local: []syncdomain.LocalStateRow{
			{Path: "a", ItemType: syncdomain.ItemTypeFile, Hash: "h", LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "b", ItemType: syncdomain.ItemTypeFile, Hash: "h", LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
		},
		Remote: []syncdomain.RemoteStateRow{{ItemID: "item-1", Path: "a", ItemType: syncdomain.ItemTypeFile, Hash: "h"}},
	}

	plan, err := PlanIdentityKeyed(in)
	require.NoError(t, err)
	assert.Equal(t, "upload b", renderMatrixPlan(plan), "a stays the synced item by path, b is new")
}

// A folder OneDrive deleted, whose inode a new local folder at another path
// now has, with no birth time to tell a rename from a reused inode: the weak
// identity must not carry OneDrive's delete onto the new folder. The new
// folder goes up and the deleted folder's row is cleaned up, as the current
// planner plans it.
//
// Validates: R-2.3.1
func TestPlanIdentityKeyed_WeakIdentityAtANewPathDoesNotTakeADelete(t *testing.T) {
	t.Parallel()

	entry := syncdomain.BaselineEntry{
		Path: "old", ItemID: "item-1", ItemType: syncdomain.ItemTypeFolder,
		LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&entry}),
		Local: []syncdomain.LocalStateRow{
			{Path: "new", ItemType: syncdomain.ItemTypeFolder, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
		},
	}

	plan, err := PlanIdentityKeyed(in)
	require.NoError(t, err)
	assert.Equal(t, "cleanup old; folder_create new (remote)", renderMatrixPlan(plan))
}

// Every node's actions depend on the one before them, in order, and nodes
// do not depend on each other.
//
// Validates: R-2.3.1
func TestPlanIdentityKeyed_ActionsOfOneNodeAreOrdered(t *testing.T) {
	t.Parallel()

	s := newPMState(t)
	s.addSynced(matrixPath, false, matrixA.content())
	moveTo("doc2", matrixB)(s, true)
	plan := s.identityPlan()

	require.Len(t, plan.Actions, 2)
	lines := []string{actionLine(&plan.Actions[0]), actionLine(&plan.Actions[1])}
	assert.Equal(t, []string{"remote_move doc2 <- doc", "upload doc2"}, lines)
	assert.Empty(t, plan.Deps[0])
	assert.Equal(t, []int{0}, plan.Deps[1])
}

// DiffPlans compares plans path by path, ignoring order and dependencies; a
// held delete counts as the delete it holds.
//
// Validates: R-2.3.1
func TestDiffPlans_ComparesActionsPerPath(t *testing.T) {
	t.Parallel()

	current := &syncdomain.ActionPlan{
		Actions: []syncdomain.Action{
			{Type: syncdomain.ActionUpload, Path: "b"},
			{Type: syncdomain.ActionRemoteDelete, Path: "doc"},
			{Type: syncdomain.ActionUpload, Path: "doc2"},
		},
		HeldDeletes: []syncdomain.HeldDelete{{Path: "gone", Side: syncdomain.DeleteSafetySideRemote}},
	}
	shadow := &syncdomain.ActionPlan{
		Actions: []syncdomain.Action{
			{Type: syncdomain.ActionUpload, Path: "doc2"},
			{Type: syncdomain.ActionRemoteMove, Path: "doc2", OldPath: "doc"},
			{Type: syncdomain.ActionRemoteDelete, Path: "gone"},
			{Type: syncdomain.ActionUpload, Path: "b"},
		},
	}

	diffs := DiffPlans(current, shadow)
	require.Len(t, diffs, 2)
	assert.Equal(t, PlanDifference{Path: "doc", Current: []string{"remote_delete doc"}}, diffs[0])
	assert.Equal(t, PlanDifference{
		Path:    "doc2",
		Current: []string{"upload doc2"},
		Shadow:  []string{"remote_move doc2 <- doc", "upload doc2"},
	}, diffs[1])
	assert.Empty(t, DiffPlans(shadow, shadow))
}

// A difference at a moved item is explained; one at an item nobody moved,
// replaced or saw an issue at is not.
//
// Validates: R-2.3.1
func TestExplainPlanDifferences_KnownReasonsOnly(t *testing.T) {
	t.Parallel()

	s := newPMState(t)
	s.addSynced(matrixPath, false, matrixA.content())
	s.addSynced("other", false, matrixA.content())
	moveTo("doc2", matrixB)(s, true)
	s.writeStore()
	in := identityInputsForStore(t, s)

	diffs := []PlanDifference{
		{Path: "doc2", Current: []string{"upload doc2"}},
		{Path: "other", Shadow: []string{"upload other"}},
	}
	unexplained := ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, diffs)
	assert.Equal(t, reasonMovedItem, diffs[0].Reason)
	require.Len(t, unexplained, 1)
	assert.Equal(t, "other", unexplained[0].Path)
	assert.Contains(t, unexplained[0].String(), "unexplained")
}

// Baseline updates alone are bookkeeping; a difference that also moves
// content is not.
//
// Validates: R-2.3.1
func TestExplainPlanDifferences_BaselineUpdatesAloneAreBookkeeping(t *testing.T) {
	t.Parallel()

	in := &IdentityInputs{Baseline: syncdomain.NewBaselineSnapshot(nil)}
	diffs := []PlanDifference{
		{Path: "a", Current: []string{"baseline_update a"}},
		{Path: "b", Current: []string{"baseline_update b"}, Shadow: []string{"download b"}},
	}
	unexplained := ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, diffs)
	assert.Equal(t, reasonBookkeeping, diffs[0].Reason)
	require.Len(t, unexplained, 1)
	assert.Equal(t, "b", unexplained[0].Path)
}

// A synced file the current plan moves aside on OneDrive, where the shadow
// deletes it, explains the differences at both ends of the move, and only
// there.
//
// Validates: R-2.3.1, R-2.17.12
func TestExplainPlanDifferences_ADeleteSetAsideExplainsBothEnds(t *testing.T) {
	t.Parallel()

	aside := "x/c" + MoveSwapInfix + "1.md"
	current := &syncdomain.ActionPlan{Actions: []syncdomain.Action{
		{Type: syncdomain.ActionRemoteMove, Path: aside, OldPath: "x/c.md"},
	}}
	shadow := &syncdomain.ActionPlan{Actions: []syncdomain.Action{
		{Type: syncdomain.ActionRemoteDelete, Path: "x/c.md"},
	}}
	in := &IdentityInputs{Baseline: syncdomain.NewBaselineSnapshot(nil)}
	diffs := []PlanDifference{
		{Path: aside, Current: []string{"remote_move " + aside + " <- x/c.md"}},
		{Path: "x/c.md", Shadow: []string{"remote_delete x/c.md"}},
		{Path: "y", Shadow: []string{"remote_delete y"}},
	}
	unexplained := ExplainPlanDifferences(in, current, shadow, diffs)
	assert.Equal(t, reasonSetAside, diffs[0].Reason)
	assert.Equal(t, reasonSetAside, diffs[1].Reason)
	require.Len(t, unexplained, 1)
	assert.Equal(t, "y", unexplained[0].Path)
}

// A local identity at two local paths, and an item ID or local identity at
// two baseline paths, explain the differences at those paths only.
//
// Validates: R-2.3.1
func TestExplainPlanDifferences_SharedIdentityAtTwoPaths(t *testing.T) {
	t.Parallel()

	old := syncdomain.BaselineEntry{Path: "old", ItemID: "item-1", ItemType: syncdomain.ItemTypeFile}
	renamed := syncdomain.BaselineEntry{Path: "renamed", ItemID: "item-1", ItemType: syncdomain.ItemTypeFile}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&old, &renamed}),
		Local: []syncdomain.LocalStateRow{
			{Path: "a", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "b", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "c", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 8, LocalHasIdentity: true},
		},
	}
	diffs := []PlanDifference{
		{Path: "a", Shadow: []string{"upload a"}},
		{Path: "old", Shadow: []string{"remote_delete old"}},
		{Path: "c", Shadow: []string{"upload c"}},
	}
	unexplained := ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, diffs)
	assert.Equal(t, reasonSharedLocalIdentity, diffs[0].Reason)
	assert.Equal(t, reasonSharedLocalIdentity, diffs[1].Reason)
	require.Len(t, unexplained, 1)
	assert.Equal(t, "c", unexplained[0].Path)
}

// A difference that follows from a move another difference's known reason
// covers is explained by it: the move's source, where the other planner
// plans the item in place (chaos seed 13: the current planner moved a file
// with a shared local identity, the identity planner downloaded it back),
// and a folder above the source that one planner deletes once the move took
// its child out (chaos seed 33: the identity planner moved a file out of a
// folder OneDrive deleted and deleted the folder, the current planner kept
// both). A delete above a source that also plans other work, and a
// difference no explained move touches, stay unexplained.
//
// Validates: R-2.3.1
func TestExplainPlanDifferences_TheConsequencesOfAnExplainedMove(t *testing.T) {
	t.Parallel()

	old := syncdomain.BaselineEntry{Path: "docs/b.txt", ItemID: "item-1", ItemType: syncdomain.ItemTypeFile}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&old}),
		Local: []syncdomain.LocalStateRow{
			{Path: "renamed-b.txt", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "hard-link.txt", ItemType: syncdomain.ItemTypeFile, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
		},
	}
	diffs := []PlanDifference{
		{Path: "docs/b.txt", Shadow: []string{"download docs/b.txt"}},
		{Path: "kept", Shadow: []string{"local_delete kept"}},
		{Path: "kept/q", Shadow: []string{"local_delete kept/q"}},
		{Path: "other", Shadow: []string{"upload other"}},
		{Path: "renamed-b.txt", Current: []string{"remote_move renamed-b.txt <- docs/b.txt"}, Shadow: []string{"upload renamed-b.txt"}},
		{Path: "hard-link.txt", Shadow: []string{"local_move hard-link.txt <- kept/q/b.txt"}},
	}
	unexplained := ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, diffs)

	assert.Equal(t, reasonSharedLocalIdentity, diffs[4].Reason)
	assert.Equal(t, reasonMoveSource, diffs[0].Reason, "the source of the explained move")
	assert.Equal(t, reasonEmptiedFolder, diffs[1].Reason, "a folder the explained move empties")
	assert.Equal(t, reasonEmptiedFolder, diffs[2].Reason)
	require.Len(t, unexplained, 1)
	assert.Equal(t, "other", unexplained[0].Path)

	// Chaos seed 121: OneDrive deleted a folder with new work below it; the
	// current planner re-creates it there, the identity planner deletes it
	// here with what is below, a delete a shared identity explains.
	moved := []PlanDifference{
		{Path: "x", Current: []string{"folder_create x (remote)"}, Shadow: []string{"local_delete x"}},
		{Path: "x/hard-link.txt", Current: []string{"folder_create x/hard-link.txt (remote)"}, Shadow: []string{"local_delete x/hard-link.txt"}},
	}
	in.Local = append(in.Local,
		syncdomain.LocalStateRow{Path: "x/hard-link.txt", ItemType: syncdomain.ItemTypeFolder, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true})
	unexplained = ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, moved)
	assert.Empty(t, unexplained)
	assert.Equal(t, reasonEmptiedFolder, moved[0].Reason, "the folder above an explained delete")

	busy := []PlanDifference{
		{Path: "hard-link.txt", Shadow: []string{"local_move hard-link.txt <- kept/b.txt"}},
		{Path: "kept", Shadow: []string{"local_delete kept", "upload kept"}},
	}
	unexplained = ExplainPlanDifferences(in, &syncdomain.ActionPlan{}, &syncdomain.ActionPlan{}, busy)
	require.Len(t, unexplained, 1, "a folder with other work above the source stays unexplained")
	assert.Equal(t, "kept", unexplained[0].Path)
}

// OneDrive deleted a synced folder and has a new one at its path (a new item
// ID): the baseline rebinds to the new folder and the local folder stays, as
// the current planner plans it (R-2.10.49), instead of deleting the local
// folder and creating OneDrive's anew.
//
// Validates: R-2.3.1, R-2.10.49
func TestPlanIdentityKeyed_ReplacedRemoteFolderRebindsAtItsPath(t *testing.T) {
	t.Parallel()

	entry := syncdomain.BaselineEntry{
		Path: "docs", ItemID: "item-old", ItemType: syncdomain.ItemTypeFolder,
		LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&entry}),
		Local: []syncdomain.LocalStateRow{
			{Path: "docs", ItemType: syncdomain.ItemTypeFolder, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
		},
		Remote: []syncdomain.RemoteStateRow{{ItemID: "item-new", Path: "docs", ItemType: syncdomain.ItemTypeFolder}},
	}

	plan, err := PlanIdentityKeyed(in)
	require.NoError(t, err)
	assert.NotContains(t, renderMatrixPlan(plan), "local_delete")
	assert.NotContains(t, renderMatrixPlan(plan), "folder_create")
}

// A folder OneDrive deleted, with a new local file below it: the folder is
// kept and goes back up, so the upload has a parent, as the current planner
// keeps it.
//
// Validates: R-2.3.1
func TestPlanIdentityKeyed_DeletedFolderWithNewWorkBelowIsKept(t *testing.T) {
	t.Parallel()

	entry := syncdomain.BaselineEntry{
		Path: "docs", ItemID: "item-1", ItemType: syncdomain.ItemTypeFolder,
		LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true,
	}
	in := &IdentityInputs{
		Baseline: syncdomain.NewBaselineSnapshot([]*syncdomain.BaselineEntry{&entry}),
		Local: []syncdomain.LocalStateRow{
			{Path: "docs", ItemType: syncdomain.ItemTypeFolder, LocalDevice: 1, LocalInode: 7, LocalHasIdentity: true},
			{Path: "docs/new.txt", ItemType: syncdomain.ItemTypeFile, Hash: "h", LocalDevice: 1, LocalInode: 8, LocalHasIdentity: true},
		},
	}

	plan, err := PlanIdentityKeyed(in)
	require.NoError(t, err)
	assert.Equal(t, "folder_create docs (remote); upload docs/new.txt", renderMatrixPlan(plan))
}
