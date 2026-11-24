package syncstoretest

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// ReadPlanningSnapshot reads what store holds as the engine reads it at the
// start of a pass (SyncStore.ReadPlanningSnapshot), for the test drive with
// no content filter, and holds every comparison row to the states the query
// can produce (syncdomain.DecisionState.Valid).
func ReadPlanningSnapshot(t *testing.T, store *syncstore.SyncStore) *syncdomain.PlanningSnapshot {
	t.Helper()

	snapshot, err := store.ReadPlanningSnapshot(t.Context(),
		driveid.New(syncfixture.EngineTestDriveID), syncdomain.ContentFilterConfig{})
	require.NoError(t, err)
	for i := range snapshot.Comparisons {
		require.NoError(t, snapshot.Comparisons[i].DecisionState().Valid(),
			"the decision state of %q", snapshot.Comparisons[i].Path)
	}

	return snapshot
}
