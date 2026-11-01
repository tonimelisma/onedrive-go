package sync

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// BenchmarkReplacePlannerVisibleState times the planner-visible TEMP-table
// build every plan pays inside its read transaction: inserting the filtered
// local and remote rows, then pruning the descendants of folders the baseline
// knows but a side no longer has. About 10k rows a side (ten top folders of
// 90 folders of 10 files) against a baseline of the same tree:
//
//   - steady: every baseline folder is present on both sides, nothing is
//     pruned (a quiet watch replan);
//   - missing-folder: one top folder, a tenth of the tree, is gone from both
//     sides while its descendants are still listed, so they are pruned.
//
// BenchmarkReplacePlannerVisibleStateCorrelatedPrune builds the same tables
// and prunes them with the correlated-scan statements this increment
// replaced (the oracle in store_planner_visible_test.go), steady shape only.
//
// Run with: go test ./internal/sync -run '^$' -bench 'BenchmarkReplacePlannerVisibleState$' -benchtime 1x

const (
	benchPlannerVisibleTopFolders       = 10
	benchPlannerVisibleFoldersPerTop    = 90
	benchPlannerVisibleFilesPerFolder   = 10
	benchPlannerVisibleMissingTopFolder = "top-00"
)

type plannerVisibleBenchShape string

const (
	benchPlannerVisibleSteady        plannerVisibleBenchShape = "steady"
	benchPlannerVisibleMissingFolder plannerVisibleBenchShape = "missing-folder"
)

// Validates: R-6.1.3
func BenchmarkReplacePlannerVisibleState(b *testing.B) {
	for _, shape := range []plannerVisibleBenchShape{benchPlannerVisibleSteady, benchPlannerVisibleMissingFolder} {
		b.Run(fmt.Sprintf("rows=%d/%s", benchPlannerVisibleRowCount(), shape), func(b *testing.B) {
			store := newTestStore(b)
			localRows, remoteRows := seedPlannerVisibleBench(b, store, shape)
			ctx := b.Context()
			contentDriveID := driveid.New(engineTestDriveID)

			b.ResetTimer()
			for b.Loop() {
				tx, err := beginPerfTx(ctx, store.rawDB())
				require.NoError(b, err)
				visible, err := replacePlannerVisibleStateTx(ctx, tx, localRows, remoteRows, contentDriveID)
				require.NoError(b, err)
				require.NoError(b, tx.Rollback())
				b.ReportMetric(float64(len(visible.Local)), "local-rows")
				b.ReportMetric(float64(len(visible.Remote)), "remote-rows")
			}
		})
	}
}

func BenchmarkReplacePlannerVisibleStateCorrelatedPrune(b *testing.B) {
	b.Run(fmt.Sprintf("rows=%d/%s", benchPlannerVisibleRowCount(), benchPlannerVisibleSteady), func(b *testing.B) {
		store := newTestStore(b)
		localRows, remoteRows := seedPlannerVisibleBench(b, store, benchPlannerVisibleSteady)

		b.ResetTimer()
		for b.Loop() {
			local, remote := plannerVisiblePathsAfterPrune(b, store, localRows, remoteRows,
				oraclePrunePlannerVisibleLocalDescendants, oraclePrunePlannerVisibleRemoteDescendants)
			b.ReportMetric(float64(len(local)), "local-rows")
			b.ReportMetric(float64(len(remote)), "remote-rows")
		}
	})
}

func benchPlannerVisibleRowCount() int {
	return benchPlannerVisibleTopFolders * (1 + benchPlannerVisibleFoldersPerTop*(1+benchPlannerVisibleFilesPerFolder))
}

// seedPlannerVisibleBench writes the synced tree into baseline and returns
// the local and remote rows of the same tree, minus the missing top folder's
// own row when shape asks for it.
func seedPlannerVisibleBench(b *testing.B, store *SyncStore, shape plannerVisibleBenchShape) ([]localStateRow, []remoteStateRow) {
	b.Helper()

	ctx := context.Background()
	tx, err := store.rawDB().BeginTx(ctx, nil)
	require.NoError(b, err)
	insertBaseline, err := tx.PrepareContext(ctx, sqlUpsertBaseline)
	require.NoError(b, err)

	const mtime = int64(1_700_000_000_000_000_000)
	var localRows []localStateRow
	var remoteRows []remoteStateRow
	inode := uint64(0)
	add := func(path, parentID, itemID string, itemType ItemType, hash string, size int64) {
		inode++
		_, err := insertBaseline.ExecContext(ctx, baselineRowArgs(&BaselineEntry{
			Path: path, ItemID: itemID, ParentID: parentID, ItemType: itemType, LocalHash: hash, RemoteHash: hash,
			LocalSize: size, LocalSizeKnown: true, RemoteSize: size, RemoteSizeKnown: true,
			LocalMtime: mtime, RemoteMtime: mtime, LocalCtime: mtime, LocalDevice: 1, LocalInode: inode,
			LocalHasIdentity: true, ETag: "etag-" + itemID,
		})...)
		require.NoError(b, err)
		if shape == benchPlannerVisibleMissingFolder && path == benchPlannerVisibleMissingTopFolder {
			return
		}
		localRows = append(localRows, localStateRow{
			Path: path, ItemType: itemType, Hash: hash, Size: size, Mtime: mtime, Ctime: mtime,
			LocalDevice: 1, LocalInode: inode, LocalHasIdentity: true,
		})
		remoteRows = append(remoteRows, remoteStateRow{
			DriveID: driveid.New(engineTestDriveID), ItemID: itemID, Path: path, ItemType: itemType,
			Hash: hash, Size: size, SizeKnown: true, Mtime: mtime, ETag: "etag-" + itemID,
		})
	}

	for top := range benchPlannerVisibleTopFolders {
		topPath := fmt.Sprintf("top-%02d", top)
		topID := "top-" + fmt.Sprint(top)
		add(topPath, strRoot, topID, ItemTypeFolder, "", 0)
		for f := range benchPlannerVisibleFoldersPerTop {
			folderPath := fmt.Sprintf("%s/folder-%03d", topPath, f)
			folderID := fmt.Sprintf("%s-%d", topID, f)
			add(folderPath, topID, folderID, ItemTypeFolder, "", 0)
			for i := range benchPlannerVisibleFilesPerFolder {
				id := fmt.Sprintf("%s-%d", folderID, i)
				add(fmt.Sprintf("%s/file-%02d.txt", folderPath, i), folderID, id, ItemTypeFile, "hash-"+id, int64(100+i))
			}
		}
	}
	require.NoError(b, insertBaseline.Close())
	require.NoError(b, tx.Commit())

	return localRows, remoteRows
}
