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
		View: &pathView{
			Path:     path,
			Baseline: &BaselineEntry{Path: path, ItemType: itemType},
			Remote:   &remoteState{ItemType: itemType},
		},
	}
}

func planOf(actions ...Action) *actionPlan {
	return &actionPlan{Actions: actions, Deps: buildDependencies(actions)}
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

	require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
		Baseline:  baselineWithItems(t, 70, 30),
		LocalRows: []localStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
	}))

	assert.Empty(t, plan.Actions, "30 file deletes plus 25 folder deletes is above max(50, 30 of 100)")
	assert.Len(t, plan.HeldDeletes, 55)
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_LostAndFoundDoesNotMakeASideNonEmpty(t *testing.T) {
	t.Parallel()

	plan := planOf(remoteDeleteAction("f000.txt", ItemTypeFile))

	require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
		Baseline: baselineWithItems(t, 1, 0),
		LocalRows: []localStateRow{
			{Path: "lost+found", ItemType: ItemTypeFolder},
			{Path: ".onedrive-go.abc.partial", ItemType: ItemTypeFile},
		},
	}))

	assert.Empty(t, plan.Actions, "an unmounted ext disk leaves only lost+found: the side vanished")
	require.Len(t, plan.HeldDeletes, 1)
	assert.Equal(t, deleteSafetyReasonSideVanished, plan.HeldDeletes[0].Reason)
}

// Validates: R-6.2.5
func TestDeleteSafetyHold_DependentOfHeldDeleteIsHeldToo(t *testing.T) {
	t.Parallel()

	// A local file replaced by a folder: the remote file delete and the
	// remote folder create at the same path. Running the create without the
	// delete would collide with the item the held delete was to remove.
	deleteFile := remoteDeleteAction("a", ItemTypeFile)
	createFolder := Action{Type: ActionFolderCreate, Path: "a", CreateSide: CreateRemote, View: &pathView{Path: "a"}}
	upload := Action{Type: ActionUpload, Path: "other.txt", View: &pathView{Path: "other.txt"}}
	plan := planOf(deleteFile, createFolder, upload)

	require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
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
	var approved []heldDelete
	for i := range 120 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile)
		actions = append(actions, action)
		if i < 60 {
			approved = append(approved, heldDelete{Path: action.Path, Side: deleteSafetySideRemote, ItemID: action.ItemID})
		}
	}
	// The user confirmed the first 60; one of them now names another item.
	approved[0].ItemID = "a-different-item"
	plan := planOf(actions...)

	require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
		Baseline:  baselineWithItems(t, 200, 0),
		LocalRows: []localStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
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
	var approved []heldDelete
	for i := range 60 {
		action := remoteDeleteAction(fmt.Sprintf("f%03d.txt", i), ItemTypeFile)
		actions = append(actions, action)
		approved = append(approved, heldDelete{Path: action.Path, Side: deleteSafetySideRemote})
	}
	plan := planOf(actions...)

	require.NoError(t, applyDeleteSafetyHold(plan, &deleteSafetyInput{
		Baseline:  baselineWithItems(t, 100, 0),
		LocalRows: []localStateRow{{Path: "keep.txt", ItemType: ItemTypeFile}},
		Approved:  approved,
	}))

	assert.Empty(t, plan.Actions, "a held row that names no item cannot vouch for a delete that does")
	require.Len(t, plan.HeldDeletes, 60)
	assert.Equal(t, "id-f000.txt", plan.HeldDeletes[0].ItemID, "held rows record the item ID")
}
