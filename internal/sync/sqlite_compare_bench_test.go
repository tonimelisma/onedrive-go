package sync

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"
)

// Benchmarks of the comparison SQL at drive scale (D-098): every plan runs
// QueryComparisonState and QueryReconciliationState over the whole state, so
// their cost bounds how fast a pass over a large drive can be. Shapes:
//
//   - steady: everything synced, nothing changed (a quiet pass);
//   - remote-rename: OneDrive renamed a folder holding half the items, the
//     shape followed_ancestor_deletes joins on (every descendant's remote
//     path differs from its baseline path);
//   - local-rename: the same rename made locally, the shape
//     local_move_candidates and followed_local_ancestor_deletes join on.
//
// Run with: go test ./internal/sync -run '^$' -bench 'BenchmarkComparisonQueries/items=10000/' -benchtime 1x
// (the pattern is a regexp: items=10000/ also matches items=100000/).

const (
	benchFilesPerFolder = 100
	benchSlowEnv        = "ONEDRIVE_GO_BENCH_SLOW"
)

type comparisonBenchShape string

const (
	benchShapeSteady       comparisonBenchShape = "steady"
	benchShapeRemoteRename comparisonBenchShape = "remote-rename"
	benchShapeLocalRename  comparisonBenchShape = "local-rename"
)

func BenchmarkComparisonQueries(b *testing.B) {
	for _, items := range []int{10_000, 100_000} {
		for _, shape := range []comparisonBenchShape{benchShapeSteady, benchShapeRemoteRename, benchShapeLocalRename} {
			b.Run(fmt.Sprintf("items=%d/%s", items, shape), func(b *testing.B) {
				if items > 10_000 && shape == benchShapeLocalRename && os.Getenv(benchSlowEnv) == "" {
					// Measured above ten minutes per plan on a 2-core host
					// (10k items took 57 s): the local-move CTEs grow with
					// moved items times baseline rows.
					b.Skipf("set %s=1 to run (over ten minutes)", benchSlowEnv)
				}
				store := newTestStore(b)
				seedComparisonBench(b, store, items, shape)
				ctx := b.Context()

				b.ResetTimer()
				for b.Loop() {
					comparisons, err := store.QueryComparisonState(ctx)
					require.NoError(b, err)
					reconciliations, err := store.QueryReconciliationState(ctx)
					require.NoError(b, err)
					b.ReportMetric(float64(len(comparisons)), "comparison-rows")
					b.ReportMetric(float64(len(reconciliations)), "reconciliation-rows")
				}
			})
		}
	}
}

// seedComparisonBench writes items synced files in folders of
// benchFilesPerFolder under two top folders, "moved" and "still", then
// applies shape to "moved".
func seedComparisonBench(b *testing.B, store *SyncStore, items int, shape comparisonBenchShape) {
	b.Helper()

	ctx := context.Background()
	tx, err := store.rawDB().BeginTx(ctx, nil)
	require.NoError(b, err)

	insertBaseline, err := tx.PrepareContext(ctx, sqlUpsertBaseline)
	require.NoError(b, err)
	insertLocal, err := tx.PrepareContext(ctx, `INSERT INTO local_state
		(path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_has_identity)
		VALUES (?, ?, ?, ?, ?, ?, 1, ?, 1)`)
	require.NoError(b, err)
	insertRemote, err := tx.PrepareContext(ctx, `INSERT INTO remote_state
		(drive_id, item_id, path, item_type, hash, size, mtime, etag) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	require.NoError(b, err)

	const mtime = int64(1_700_000_000_000_000_000)
	inode := int64(0)
	add := func(path, parentID, itemID, itemType, hash string, size int64) {
		inode++
		localPath, remotePath := path, path
		if len(path) >= len("moved") && path[:len("moved")] == "moved" {
			switch shape {
			case benchShapeRemoteRename:
				remotePath = "renamed" + path[len("moved"):]
			case benchShapeLocalRename:
				localPath = "renamed" + path[len("moved"):]
			case benchShapeSteady:
			}
		}
		_, err := insertBaseline.ExecContext(ctx, itemID, path, parentID, itemType, nullString(hash), nullString(hash),
			size, size, mtime, mtime, mtime, 1, inode, 1, "etag-"+itemID)
		require.NoError(b, err)
		_, err = insertLocal.ExecContext(ctx, localPath, itemType, nullString(hash), size, mtime, mtime, inode)
		require.NoError(b, err)
		_, err = insertRemote.ExecContext(ctx, engineTestDriveID, itemID, remotePath, itemType, nullString(hash), size, mtime, "etag-"+itemID)
		require.NoError(b, err)
	}

	folders := items / benchFilesPerFolder
	for top, topName := range []string{"moved", "still"} {
		topID := "top-" + strconv.Itoa(top)
		add(topName, strRoot, topID, ItemTypeFolder.String(), "", 0)
		for f := range folders / 2 {
			folderPath := fmt.Sprintf("%s/folder-%05d", topName, f)
			folderID := fmt.Sprintf("%s-%d", topID, f)
			add(folderPath, topID, folderID, ItemTypeFolder.String(), "", 0)
			for i := range benchFilesPerFolder {
				id := fmt.Sprintf("%s-%d", folderID, i)
				add(fmt.Sprintf("%s/file-%03d.txt", folderPath, i), folderID, id, ItemTypeFile.String(), "hash-"+id, int64(100+i))
			}
		}
	}
	require.NoError(b, tx.Commit())
}
