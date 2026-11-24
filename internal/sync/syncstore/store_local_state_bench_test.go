package syncstore

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// BenchmarkDeleteLocalStateSubtree times the subtree delete every watch delete
// runs (deleteLocalStatePrefixTx) on a local_state table of about 100,000
// rows (100 top folders of 10 folders of 99 files), each inside a transaction
// that is rolled back:
//
//   - folder: one top folder and its 1,000 rows below it;
//   - files=100: one hundred single files, one statement each, as a bulk
//     delete of files observed in one batch costs.
//
// The substr form is the correlated prefix test the range replaced
// (sqlSubstrLocalStateSubtreeDelete): it reads every row of the table per
// statement, where the range seeks the primary key.
//
// Run with: go test ./internal/sync/syncstore -run '^$' -bench BenchmarkDeleteLocalStateSubtree -benchtime 5x

const (
	benchLocalStateTopFolders     = 100
	benchLocalStateFoldersPerTop  = 10
	benchLocalStateFilesPerFolder = 99
	benchLocalStateSingleDeletes  = 100
)

// sqlSubstrLocalStateSubtreeDelete is the substr form of the subtree delete,
// kept as the benchmark's baseline and the range form's oracle.
const sqlSubstrLocalStateSubtreeDelete = `DELETE FROM local_state
	WHERE path = ? COLLATE BINARY
	   OR substr(path, 1, length(?)) = ? COLLATE BINARY`

func substrDeleteLocalStateSubtreeTx(ctx context.Context, tx sqlTxRunner, prefix string) error {
	descendantPrefix := prefix + "/"
	if _, err := tx.ExecContext(ctx, sqlSubstrLocalStateSubtreeDelete, prefix, descendantPrefix, descendantPrefix); err != nil {
		return fmt.Errorf("substr subtree delete %s: %w", prefix, err)
	}

	return nil
}

// Validates: R-2.8.8, R-6.1.3
func BenchmarkDeleteLocalStateSubtree(b *testing.B) {
	forms := []struct {
		name   string
		delete func(context.Context, sqlTxRunner, string) error
	}{
		{name: "substr", delete: substrDeleteLocalStateSubtreeTx},
		{name: "range", delete: deleteLocalStatePrefixTx},
	}
	singleFiles := make([]string, 0, benchLocalStateSingleDeletes)
	for i := range benchLocalStateSingleDeletes {
		singleFiles = append(singleFiles, benchLocalStateFilePath(i, 0, i%benchLocalStateFilesPerFolder))
	}
	shapes := []struct {
		name  string
		paths []string
	}{
		{name: "folder", paths: []string{benchLocalStateTopPath(benchLocalStateTopFolders / 2)}},
		{name: fmt.Sprintf("files=%d", benchLocalStateSingleDeletes), paths: singleFiles},
	}

	store := newTestStore(b)
	rows := seedLocalStateBench(b, store)
	for _, form := range forms {
		for _, shape := range shapes {
			b.Run(fmt.Sprintf("rows=%d/%s/%s", rows, form.name, shape.name), func(b *testing.B) {
				ctx := b.Context()
				for b.Loop() {
					tx, err := beginPerfTx(ctx, store.RawDBForTest())
					require.NoError(b, err)
					for _, path := range shape.paths {
						require.NoError(b, form.delete(ctx, tx, path))
					}
					require.NoError(b, tx.Rollback())
				}
			})
		}
	}
}

func benchLocalStateTopPath(top int) string {
	return fmt.Sprintf("top-%03d", top)
}

func benchLocalStateFilePath(top, folder, file int) string {
	return fmt.Sprintf("%s/folder-%02d/file-%02d.txt", benchLocalStateTopPath(top), folder, file)
}

// seedLocalStateBench writes the benchmark's tree into local_state and
// returns its row count.
func seedLocalStateBench(b *testing.B, store *SyncStore) int {
	b.Helper()

	rows := make([]syncdomain.LocalStateRow, 0,
		benchLocalStateTopFolders*(1+benchLocalStateFoldersPerTop*(1+benchLocalStateFilesPerFolder)))
	for top := range benchLocalStateTopFolders {
		rows = append(rows, syncdomain.LocalStateRow{Path: benchLocalStateTopPath(top), ItemType: syncdomain.ItemTypeFolder})
		for folder := range benchLocalStateFoldersPerTop {
			rows = append(rows, syncdomain.LocalStateRow{
				Path: fmt.Sprintf("%s/folder-%02d", benchLocalStateTopPath(top), folder), ItemType: syncdomain.ItemTypeFolder,
			})
			for file := range benchLocalStateFilesPerFolder {
				rows = append(rows, syncdomain.LocalStateRow{
					Path: benchLocalStateFilePath(top, folder, file), ItemType: syncdomain.ItemTypeFile, Hash: "hash",
				})
			}
		}
	}
	require.NoError(b, store.ReplaceLocalState(b.Context(), rows))

	return len(rows)
}
