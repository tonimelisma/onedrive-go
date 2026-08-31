package sync

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
			store.baseline.Put(&BaselineEntry{
				Path:      "file.txt",
				ItemID:    "item",
				DriveID:   driveid.New(testDriveID),
				ItemType:  ItemTypeFile,
				LocalSize: int64(i),
			})
			store.baseline.Delete("file.txt")
		}
	}()

	for range rounds {
		if _, checkErr := store.CheckCacheConsistency(ctx); checkErr != nil {
			require.NoError(t, checkErr)
		}
	}

	wg.Wait()
}
