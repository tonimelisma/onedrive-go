package sync

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func baselineWithItems(t *testing.T, files int, folders int) *Baseline {
	t.Helper()

	entries := make([]*BaselineEntry, 0, files+folders)
	for i := range files {
		entries = append(entries, &BaselineEntry{Path: fmt.Sprintf("f%03d.txt", i), ItemID: fmt.Sprintf("file-%d", i), ItemType: ItemTypeFile})
	}
	for i := range folders {
		entries = append(entries, &BaselineEntry{Path: fmt.Sprintf("d%03d", i), ItemID: fmt.Sprintf("dir-%d", i), ItemType: ItemTypeFolder})
	}
	return baselineWith(entries...)
}

func remoteDeleteAction(path string, itemType ItemType) Action {
	return Action{
		Type:   ActionRemoteDelete,
		Path:   path,
		ItemID: "id-" + path,
		View: &PathView{
			Path:     path,
			Baseline: &BaselineEntry{Path: path, ItemType: itemType},
			Remote:   &RemoteState{ItemType: itemType},
		},
	}
}

func planOf(actions ...Action) *ActionPlan {
	return &ActionPlan{Actions: actions, Deps: buildDependencies(actions)}
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_FolderDeletesCountTowardThreshold(t *testing.T) {
	t.Parallel()

	var actions []Action
	for i := range 30 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile))
	}
	for i := range 25 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("d%03d", i), ItemTypeFolder))
	}
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 70, 30),
		LocalRows: []LocalStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
	}))

	assert.Empty(t, plan.Actions, "30 file deletes plus 25 folder deletes is above max(50, 30 of 100)")
	assert.Len(t, plan.HeldDeletes, 55)
}

// A delete toward OneDrive of a recorded mount point whose folder is gone
// from the sync folder, or of anything below it, is held whatever the count:
// letting go of a disk sync crossed into takes one confirmation. The plan's
// other deletes are judged as usual, and a mount point whose folder is still
// there (a mounted disk the user emptied) is an ordinary delete. A file put
// in place of the mount point's folder does not count as the folder.
//
// Validates: R-6.2.5, R-6.2.15
func TestDeleteSafetyHold_DeletesOfAMissingMountPointAreHeldWhateverTheirNumber(t *testing.T) {
	t.Parallel()

	actions := func() []Action {
		return []Action{
			remoteDeleteAction("nas/a.txt", ItemTypeFile),
			remoteDeleteAction("nas", ItemTypeFolder),
			remoteDeleteAction("usb/b.txt", ItemTypeFile),
			remoteDeleteAction("disk/c.txt", ItemTypeFile),
			remoteDeleteAction("disk", ItemTypeFolder),
			remoteDeleteAction("nasty.txt", ItemTypeFile),
			remoteDeleteAction("other.txt", ItemTypeFile),
		}
	}
	input := &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 100, 0),
		LocalRows: []LocalStateRow{
			{Path: "keep.txt", ItemType: ItemTypeFile},
			{Path: "usb", ItemType: ItemTypeFolder},
			{Path: "disk", ItemType: ItemTypeFile},
		},
		MountBoundaries: []string{"nas", "usb", "disk"},
	}
	plan := planOf(actions()...)
	require.NoError(t, ApplyDeleteSafetyHold(plan, input))

	var kept []string
	for i := range plan.Actions {
		kept = append(kept, plan.Actions[i].Path)
	}
	assert.ElementsMatch(t, []string{"usb/b.txt", "nasty.txt", "other.txt"}, kept,
		"a mounted disk's delete and deletes outside the mount point run")
	require.Len(t, plan.HeldDeletes, 4)
	for _, held := range plan.HeldDeletes {
		assert.Contains(t, []string{"nas", "nas/a.txt", "disk", "disk/c.txt"}, held.Path)
		assert.Equal(t, DeleteSafetySideRemote, held.Side)
		assert.Equal(t, DeleteSafetyReasonMountPointGone, held.Reason)
	}

	input.Approved = plan.HeldDeletes
	confirmed := planOf(actions()...)
	require.NoError(t, ApplyDeleteSafetyHold(confirmed, input))
	assert.Len(t, confirmed.Actions, 7, "confirmed, the held deletes run")
	assert.Empty(t, confirmed.HeldDeletes)
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_LostAndFoundDoesNotMakeASideNonEmpty(t *testing.T) {
	t.Parallel()

	plan := planOf(remoteDeleteAction("f000.txt", ItemTypeFile))

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 1, 0),
		LocalRows: []LocalStateRow{
			{Path: "lost+found", ItemType: ItemTypeFolder},
			{Path: ".onedrive-go.abc.partial", ItemType: ItemTypeFile},
		},
	}))

	assert.Empty(t, plan.Actions, "an unmounted ext disk leaves only lost+found: the side vanished")
	require.Len(t, plan.HeldDeletes, 1)
	assert.Equal(t, DeleteSafetyReasonSideVanished, plan.HeldDeletes[0].Reason)
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo(t *testing.T) {
	t.Parallel()

	// A local file replaced by a folder: the remote file delete and the
	// remote folder create at the same path. Running the create without the
	// delete would collide with the item the held delete was to remove.
	deleteFile := remoteDeleteAction("a", ItemTypeFile)
	createFolder := Action{Type: ActionFolderCreate, Path: "a", CreateSide: CreateRemote, View: &PathView{Path: "a"}}
	upload := Action{Type: ActionUpload, Path: "other.txt", View: &PathView{Path: "other.txt"}}
	plan := planOf(deleteFile, createFolder, upload)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 1, 0),
	}))

	require.Len(t, plan.Actions, 1, "unrelated work continues")
	assert.Equal(t, ActionUpload, plan.Actions[0].Type)
	require.Len(t, plan.HeldDeletes, 1)
	assert.Equal(t, "a", plan.HeldDeletes[0].Path)
}

// Validates: R-6.2.5, R-6.4.2
func TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet(t *testing.T) {
	t.Parallel()

	var actions []Action
	var approved []HeldDelete
	for i := range 120 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile)
		actions = append(actions, action)
		if i < 60 {
			approved = append(approved, HeldDelete{Path: action.Path, Side: DeleteSafetySideRemote, ItemID: action.ItemID})
		}
	}
	// The user confirmed the first 60; one of them now names another item.
	approved[0].ItemID = "a-different-item"
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 200, 0),
		LocalRows: []LocalStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
		Approved:  approved,
	}))

	assert.Len(t, plan.Actions, 59, "exactly the confirmed deletes run")
	assert.Len(t, plan.HeldDeletes, 61, "deletes that appeared after the user looked are judged again and held")
	for i := range plan.Actions {
		assert.NotEqual(t, "f000.txt", plan.Actions[i].Path, "a confirmation is bound to the item it held")
	}
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_HeldRowWithoutItemIDDoesNotApproveAKnownItem(t *testing.T) {
	t.Parallel()

	var actions []Action
	var approved []HeldDelete
	for i := range 60 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile)
		actions = append(actions, action)
		approved = append(approved, HeldDelete{Path: action.Path, Side: DeleteSafetySideRemote})
	}
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 100, 0),
		LocalRows: []LocalStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
		Approved:  approved,
	}))

	assert.Empty(t, plan.Actions, "a held row that names no item cannot vouch for a delete that does")
	require.Len(t, plan.HeldDeletes, 60)
	assert.Equal(t, "id-f000.txt", plan.HeldDeletes[0].ItemID, "held rows record the item ID")
}

// Validates: R-6.2.5
//
// The hold used to judge each plan alone: 40 deletes against 200 items ran,
// then 40 more ran, and so on until the drive was empty. The unconfirmed
// deletes already run within the window count with this plan's, against the
// baseline as it stood before them.
func TestDeleteSafetyHold_RecentUnconfirmedDeletesCountTowardTheThreshold(t *testing.T) {
	t.Parallel()

	var actions []Action
	for i := range 40 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile))
	}
	local := []LocalStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}}

	alone := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(alone, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
	}))
	assert.Len(t, alone.Actions, 40, "40 of 160 is below max(50, 48): runs")
	assert.Empty(t, alone.HeldDeletes)

	afterForty := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(afterForty, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
		RecentDeletes: map[DeleteSafetySide]int{DeleteSafetySideRemote: 40},
	}))
	assert.Empty(t, afterForty.Actions, "40 now plus 40 within the window is 80 of the 200 there were: held")
	assert.Len(t, afterForty.HeldDeletes, 40)
	assert.Equal(t, DeleteSafetyReasonTooMany, afterForty.HeldDeletes[0].Reason)

	otherSide := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(otherSide, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
		RecentDeletes: map[DeleteSafetySide]int{DeleteSafetySideLocal: 40},
	}))
	assert.Len(t, otherSide.Actions, 40, "deletes toward the other side do not count")
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_ConfirmedDeletesAreMarkedForTheLedger(t *testing.T) {
	t.Parallel()

	plan := planOf(
		remoteDeleteAction("f000.txt", ItemTypeFile),
		remoteDeleteAction("f001.txt", ItemTypeFile),
	)
	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 2, 0),
		LocalRows: []LocalStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
		Approved:  []HeldDelete{{Path: "f000.txt", Side: DeleteSafetySideRemote, ItemID: "id-f000.txt"}},
	}))

	require.Len(t, plan.Actions, 2)
	assert.True(t, plan.Actions[0].DeleteConfirmed, "the approved delete is confirmed")
	assert.False(t, plan.Actions[1].DeleteConfirmed, "the other one counts toward the next hold")
}
