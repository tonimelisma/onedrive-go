package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncstoretest"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// interruptedRemovalStatusPaths returns the paths status lists under the
// interrupted-removal condition, read the way `status` reads them: the drive
// snapshot projected into condition groups.
func interruptedRemovalStatusPaths(t *testing.T, store *syncstore.SyncStore) []string {
	t.Helper()

	snapshot, err := store.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	groups := syncstore.ProjectStoredConditionGroups(&snapshot)
	for i := range groups {
		if groups[i].ConditionKey == syncdomain.ConditionLocalInterrupted {
			return groups[i].Paths
		}
	}

	return nil
}

// Every full scan that finds the leftover of an interrupted removal reports
// it, so the scan that no longer finds it has proven it gone: the local
// findings batch clears the condition and status stops listing it. A scan
// that still finds it keeps it, and that re-report is not a change that
// replans (R-2.8.21). The condition used to be left out of the batch's
// managed types and outlived the leftover for good.
//
// Validates: R-6.4.9, R-2.8.21
func TestSyncStore_ReconcileObservationFindings_InterruptedRemovalLastsAsLongAsTheLeftover(t *testing.T) {
	t.Parallel()

	store := syncstoretest.NewTestStore(t)
	ctx := t.Context()
	const leftoverPath = "Docs/.onedrive-go.0123456789abcdef.detach"
	leftover := syncdomain.SkippedItem{
		Path:   leftoverPath,
		Reason: syncdomain.IssueLocalInterruptedRemoval,
		Detail: "sync set this file aside while removing it and was interrupted",
	}
	reconcileScan := func(skipped ...syncdomain.SkippedItem) bool {
		batch := localObservationFindingsBatchFromSkippedItems(driveid.New(syncfixture.TestDriveID), skipped)
		changed, err := store.ReconcileObservationFindingsReportingChange(ctx, &batch)
		require.NoError(t, err)

		return changed
	}

	assert.True(t, reconcileScan(leftover), "the first scan that finds the leftover records it")
	assert.False(t, reconcileScan(leftover), "a scan that still finds it keeps it, and that is no change")
	assert.Equal(t, []string{leftoverPath}, interruptedRemovalStatusPaths(t, store))

	assert.True(t, reconcileScan(), "the scan that no longer finds the leftover clears its condition")
	assert.Empty(t, syncstoretest.ObservationIssuePathsForTest(t, store))
	assert.Empty(t, interruptedRemovalStatusPaths(t, store), "status no longer lists it")
}
