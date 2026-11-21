package syncstore

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-6.3.4
//
// CheckCacheConsistency exists to audit a live in-memory cache against the
// database, so it runs while the engine is mutating that cache. The baseline's
// maps are mutex-guarded; ranging them directly races the very mutations the
// check is meant to audit. Run under -race, this fails when the check reads
// them without the lock.
func TestCheckCacheConsistency_IsSafeAgainstConcurrentMutation(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()

	_, err := store.Load(ctx)
	require.NoError(t, err)

	const rounds = 200

	var wg sync.WaitGroup

	wg.Add(1)

	go func() {
		defer wg.Done()

		for i := range rounds {
			store.baseline.put(&syncdomain.BaselineEntry{
				Path:      "file.txt",
				ItemID:    "item",
				DriveID:   driveid.New(syncfixture.TestDriveID),
				ItemType:  syncdomain.ItemTypeFile,
				LocalSize: int64(i),
			})
			store.baseline.delete("file.txt")
		}
	}()

	for range rounds {
		// The writer changes the cache behind the database's back, so a
		// reported divergence is expected; the race detector is the check.
		if checkErr := store.CheckCacheConsistency(ctx); checkErr != nil {
			require.ErrorIs(t, checkErr, ErrBaselineCacheDiverged)
		}
	}

	wg.Wait()
}
