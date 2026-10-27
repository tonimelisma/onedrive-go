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

	line := heldDeletesSummary(&syncengine.Report{HeldRemoteDeletes: 60, HeldLocalDeletes: 2})
	assert.Contains(t, line, "60 on OneDrive")
	assert.Contains(t, line, "2 on this computer")
	assert.Contains(t, line, "onedrive-go sync --confirm-deletes")
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

	conditions := buildStatusConditionJSON(snapshot, true, 5)
	require.Len(t, conditions, 1)
	assert.Equal(t, string(syncengine.ConditionDeletesHeld), conditions[0].ConditionKey)
	assert.Equal(t, "Deletes held for confirmation", conditions[0].Title)
	assert.Contains(t, conditions[0].Action, "sync --confirm-deletes")
	assert.Equal(t, 2, conditions[0].Count)
	assert.Equal(t, []string{"Docs/a.txt", "Docs/b.txt"}, conditions[0].Paths)
	assert.Equal(t, "deletes on OneDrive", conditions[0].Scope)
}
