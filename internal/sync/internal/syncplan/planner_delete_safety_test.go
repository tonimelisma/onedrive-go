package syncplan

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func baselineWithItems(t *testing.T, files int, folders int) *syncdomain.BaselineSnapshot {
	t.Helper()

	entries := make([]*syncdomain.BaselineEntry, 0, files+folders)
	for i := range files {
		entries = append(entries, &syncdomain.BaselineEntry{Path: fmt.Sprintf("f%03d.txt", i), ItemID: fmt.Sprintf("file-%d", i), ItemType: syncdomain.ItemTypeFile})
	}
	for i := range folders {
		entries = append(entries, &syncdomain.BaselineEntry{Path: fmt.Sprintf("d%03d", i), ItemID: fmt.Sprintf("dir-%d", i), ItemType: syncdomain.ItemTypeFolder})
	}
	return syncfixture.BaselineWith(entries...)
}

func remoteDeleteAction(path string, itemType syncdomain.ItemType) syncdomain.Action {
	return syncdomain.Action{
		Type:   syncdomain.ActionRemoteDelete,
		Path:   path,
		ItemID: "id-" + path,
		View: &syncdomain.PathView{
			Path:     path,
			Baseline: &syncdomain.BaselineEntry{Path: path, ItemType: itemType},
			Remote:   &syncdomain.RemoteState{ItemType: itemType},
		},
	}
}

func planOf(actions ...syncdomain.Action) *syncdomain.ActionPlan {
	return &syncdomain.ActionPlan{Actions: actions, Deps: buildDependencies(actions)}
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_FolderDeletesCountTowardThreshold(t *testing.T) {
	t.Parallel()

	var actions []syncdomain.Action
	for i := range 30 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), syncdomain.ItemTypeFile))
	}
	for i := range 25 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("d%03d", i), syncdomain.ItemTypeFolder))
	}
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 70, 30),
		LocalRows: []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile}},
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

	actions := func() []syncdomain.Action {
		return []syncdomain.Action{
			remoteDeleteAction("nas/a.txt", syncdomain.ItemTypeFile),
			remoteDeleteAction("nas", syncdomain.ItemTypeFolder),
			remoteDeleteAction("usb/b.txt", syncdomain.ItemTypeFile),
			remoteDeleteAction("disk/c.txt", syncdomain.ItemTypeFile),
			remoteDeleteAction("disk", syncdomain.ItemTypeFolder),
			remoteDeleteAction("nasty.txt", syncdomain.ItemTypeFile),
			remoteDeleteAction("other.txt", syncdomain.ItemTypeFile),
		}
	}
	input := &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 100, 0),
		LocalRows: []syncdomain.LocalStateRow{
			{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile},
			{Path: "usb", ItemType: syncdomain.ItemTypeFolder},
			{Path: "disk", ItemType: syncdomain.ItemTypeFile},
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
		assert.Equal(t, syncdomain.DeleteSafetySideRemote, held.Side)
		assert.Equal(t, syncdomain.DeleteSafetyReasonMountPointGone, held.Reason)
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

	plan := planOf(remoteDeleteAction("f000.txt", syncdomain.ItemTypeFile))

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 1, 0),
		LocalRows: []syncdomain.LocalStateRow{
			{Path: "lost+found", ItemType: syncdomain.ItemTypeFolder},
			{Path: ".onedrive-go.abc.partial", ItemType: syncdomain.ItemTypeFile},
		},
	}))

	assert.Empty(t, plan.Actions, "an unmounted ext disk leaves only lost+found: the side vanished")
	require.Len(t, plan.HeldDeletes, 1)
	assert.Equal(t, syncdomain.DeleteSafetyReasonSideVanished, plan.HeldDeletes[0].Reason)
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo(t *testing.T) {
	t.Parallel()

	// A local file replaced by a folder: the remote file delete and the
	// remote folder create at the same path. Running the create without the
	// delete would collide with the item the held delete was to remove.
	deleteFile := remoteDeleteAction("a", syncdomain.ItemTypeFile)
	createFolder := syncdomain.Action{Type: syncdomain.ActionFolderCreate, Path: "a", CreateSide: syncdomain.CreateRemote, View: &syncdomain.PathView{Path: "a"}}
	upload := syncdomain.Action{Type: syncdomain.ActionUpload, Path: "other.txt", View: &syncdomain.PathView{Path: "other.txt"}}
	plan := planOf(deleteFile, createFolder, upload)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 1, 0),
	}))

	require.Len(t, plan.Actions, 1, "unrelated work continues")
	assert.Equal(t, syncdomain.ActionUpload, plan.Actions[0].Type)
	require.Len(t, plan.HeldDeletes, 1)
	assert.Equal(t, "a", plan.HeldDeletes[0].Path)
}

// Validates: R-6.2.5, R-6.4.2
func TestDeleteSafetyHold_ConfirmReleasesExactlyTheHeldSet(t *testing.T) {
	t.Parallel()

	var actions []syncdomain.Action
	var approved []syncdomain.HeldDelete
	for i := range 120 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), syncdomain.ItemTypeFile)
		actions = append(actions, action)
		if i < 60 {
			approved = append(approved, syncdomain.HeldDelete{Path: action.Path, Side: syncdomain.DeleteSafetySideRemote, ItemID: action.ItemID})
		}
	}
	// The user confirmed the first 60; one of them now names another item.
	approved[0].ItemID = "a-different-item"
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 200, 0),
		LocalRows: []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile}},
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

	var actions []syncdomain.Action
	var approved []syncdomain.HeldDelete
	for i := range 60 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), syncdomain.ItemTypeFile)
		actions = append(actions, action)
		approved = append(approved, syncdomain.HeldDelete{Path: action.Path, Side: syncdomain.DeleteSafetySideRemote})
	}
	plan := planOf(actions...)

	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 100, 0),
		LocalRows: []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile}},
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

	var actions []syncdomain.Action
	for i := range 40 {
		actions = append(actions, remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), syncdomain.ItemTypeFile))
	}
	local := []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile}}

	alone := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(alone, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
	}))
	assert.Len(t, alone.Actions, 40, "40 of 160 is below max(50, 48): runs")
	assert.Empty(t, alone.HeldDeletes)

	afterForty := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(afterForty, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
		RecentDeletes: map[syncdomain.DeleteSafetySide]int{syncdomain.DeleteSafetySideRemote: 40},
	}))
	assert.Empty(t, afterForty.Actions, "40 now plus 40 within the window is 80 of the 200 there were: held")
	assert.Len(t, afterForty.HeldDeletes, 40)
	assert.Equal(t, syncdomain.DeleteSafetyReasonTooMany, afterForty.HeldDeletes[0].Reason)

	otherSide := planOf(actions...)
	require.NoError(t, ApplyDeleteSafetyHold(otherSide, &DeleteSafetyInput{
		Baseline: baselineWithItems(t, 160, 0), LocalRows: local,
		RecentDeletes: map[syncdomain.DeleteSafetySide]int{syncdomain.DeleteSafetySideLocal: 40},
	}))
	assert.Len(t, otherSide.Actions, 40, "deletes toward the other side do not count")
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_ConfirmedDeletesAreMarkedForTheLedger(t *testing.T) {
	t.Parallel()

	plan := planOf(
		remoteDeleteAction("f000.txt", syncdomain.ItemTypeFile),
		remoteDeleteAction("f001.txt", syncdomain.ItemTypeFile),
	)
	require.NoError(t, ApplyDeleteSafetyHold(plan, &DeleteSafetyInput{
		Baseline:  baselineWithItems(t, 2, 0),
		LocalRows: []syncdomain.LocalStateRow{{Path: "keep.txt", ItemType: syncdomain.ItemTypeFile}},
		Approved:  []syncdomain.HeldDelete{{Path: "f000.txt", Side: syncdomain.DeleteSafetySideRemote, ItemID: "id-f000.txt"}},
	}))

	require.Len(t, plan.Actions, 2)
	assert.True(t, plan.Actions[0].DeleteConfirmed, "the approved delete is confirmed")
	assert.False(t, plan.Actions[1].DeleteConfirmed, "the other one counts toward the next hold")
}
