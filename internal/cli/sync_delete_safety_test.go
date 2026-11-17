package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Validates: R-6.2.5
func TestNewSyncCmd_ConfirmDeletesIsOneShotOnly(t *testing.T) {
	t.Parallel()

	cmd := newSyncCmd()
	require.NotNil(t, cmd.Flags().Lookup("confirm-deletes"))
	require.NoError(t, cmd.Flags().Set("confirm-deletes", "true"))
	require.NoError(t, cmd.Flags().Set("watch", "true"))

	err := cmd.ValidateFlagGroups()
	require.Error(t, err, "a watch session must not keep confirming deletes it has not shown")
	assert.Contains(t, err.Error(), "confirm-deletes")
}

// Validates: R-6.2.5
func TestHeldDeletesSummary_NamesSidesAndTheReleaseCommand(t *testing.T) {
	t.Parallel()

	assert.Empty(t, heldDeletesSummary(&syncengine.Report{}))

	const advice = " for confirmation: more was deleted than looks intended. " +
		"Check the sync folder, then run 'onedrive-go sync --confirm-deletes' to apply them."
	assert.Equal(t, "Held 60 deletes on OneDrive and 2 deletes on this computer"+advice,
		heldDeletesSummary(&syncengine.Report{HeldRemoteDeletes: 60, HeldLocalDeletes: 2}))
	assert.Equal(t, "Held 1 delete on OneDrive"+advice, heldDeletesSummary(&syncengine.Report{HeldRemoteDeletes: 1}))
	assert.Equal(t, "Held 1 delete on this computer"+advice, heldDeletesSummary(&syncengine.Report{HeldLocalDeletes: 1}))
}

// Validates: R-6.2.5
func TestStatusConditions_HeldDeletesAreDescribed(t *testing.T) {
	t.Parallel()

	snapshot := &syncengine.DriveStatusSnapshot{
		HeldDeletes: []syncengine.HeldDeleteRow{
			{Path: "Docs/a.txt", Side: "remote", Reason: "too_many", ItemType: syncengine.ItemTypeFile},
			{Path: "Docs/b.txt", Side: "remote", Reason: "too_many", ItemType: syncengine.ItemTypeFile},
		},
	}

	conditions := buildStatusConditionJSON(snapshot, statusStoreRootDrive, true, 5)
	require.Len(t, conditions, 1)
	assert.Equal(t, string(syncengine.ConditionDeletesHeld), conditions[0].ConditionKey)
	assert.Equal(t, "Deletes held for confirmation", conditions[0].Title)
	assert.Contains(t, conditions[0].Action, "sync --confirm-deletes")
	assert.Equal(t, 2, conditions[0].Count)
	assert.Equal(t, []string{"Docs/a.txt", "Docs/b.txt"}, conditions[0].Paths)
	assert.Equal(t, "deletes on OneDrive", conditions[0].Scope)
}
