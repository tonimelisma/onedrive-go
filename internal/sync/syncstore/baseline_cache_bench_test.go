package syncstore

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// benchBaselineItems is a drive-scale baseline (D-098).
const benchBaselineItems = 100_000

// BenchmarkBaselineLookups times what the scanner and the planner ask the
// baseline view per pass at drive scale: a lookup by path and by item ID for
// every entry, one full iteration, and a case-variant lookup per folder. The
// view returns entries by value, so this is the price of that copy.
//
// Run with: go test ./internal/sync/syncstore -run '^$' -bench BenchmarkBaselineLookups
func BenchmarkBaselineLookups(b *testing.B) {
	entries := make([]*syncdomain.BaselineEntry, 0, benchBaselineItems)
	paths := make([]string, 0, benchBaselineItems)
	for i := range benchBaselineItems {
		path := fmt.Sprintf("folder-%04d/file-%03d.txt", i/benchFilesPerFolder, i%benchFilesPerFolder)
		paths = append(paths, path)
		entries = append(entries, &syncdomain.BaselineEntry{
			Path: path, ItemID: fmt.Sprintf("id-%d", i), ItemType: syncdomain.ItemTypeFile,
			LocalSize: int64(i), LocalHash: "h", RemoteHash: "h", ETag: "e",
		})
	}
	view := newBaselineCache(indexBaselineEntries(entries))

	b.ResetTimer()
	for b.Loop() {
		var sum int64
		for i, path := range paths {
			byPath, _ := view.GetByPath(path)
			sum += byPath.LocalSize
			byID, _ := view.GetByID(entries[i].ItemID)
			sum += byID.LocalSize
		}
		view.ForEachPath(func(_ string, entry syncdomain.BaselineEntry) { sum += entry.LocalSize })
		require.NotZero(b, sum, "the lookups found nothing")
	}
}
