package sync

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-2.1.3, R-2.4.1
func TestReplacePlannerVisibleStateTx_PrunesRemoteDescendantsWhenBaselineFolderMissingRemotely(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash)
		VALUES
			('folder-docs', 'docs', 'folder', '', ''),
			('file-docs', 'docs/file.txt', 'file', 'hash', 'hash')`)
	require.NoError(t, err)

	visible := plannerVisibleRowsForTest(t, store, nil, []remoteStateRow{
		{
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "file-docs",
			Path:     "docs/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
		{
			DriveID:  driveid.New(engineTestDriveID),
			ItemID:   "file-docs2",
			Path:     "docs2/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
	})

	assert.Equal(t, []string{"docs2/file.txt"}, remoteStatePaths(visible.Remote))
}

// Validates: R-2.1.3, R-2.4.1
func TestReplacePlannerVisibleStateTx_PrunesLocalDescendantsWhenBaselineFolderMissingLocally(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	_, err := store.rawDB().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash)
		VALUES
			('folder-docs', 'docs', 'folder', '', ''),
			('file-docs', 'docs/file.txt', 'file', 'hash', 'hash')`)
	require.NoError(t, err)

	visible := plannerVisibleRowsForTest(t, store, []localStateRow{
		{
			Path:     "docs/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
		{
			Path:     "docs2/file.txt",
			ItemType: ItemTypeFile,
			Hash:     "hash",
		},
	}, nil)

	assert.Equal(t, []string{"docs2/file.txt"}, localStatePaths(visible.Local))
}

func plannerVisibleRowsForTest(
	t *testing.T,
	store *SyncStore,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
) plannerVisibleRows {
	t.Helper()

	ctx := t.Context()
	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	visible, err := replacePlannerVisibleStateTx(
		ctx,
		tx,
		localRows,
		remoteRows,
		driveid.New(engineTestDriveID),
	)
	require.NoError(t, err)

	return visible
}

func localStatePaths(rows []localStateRow) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}
	return paths
}

func remoteStatePaths(rows []remoteStateRow) []string {
	paths := make([]string, 0, len(rows))
	for i := range rows {
		paths = append(paths, rows[i].Path)
	}
	return paths
}

// The prune statements used to be a correlated EXISTS over baseline with a
// substr() prefix test per visible row (rows x baseline rows). They are kept
// here as the oracle for the range-delete form that replaced them.
const (
	oraclePrunePlannerVisibleLocalDescendants = `DELETE FROM planner_visible_local_state
		WHERE EXISTS (
			SELECT 1
			FROM baseline parent
			LEFT JOIN planner_visible_local_state parent_local ON parent_local.path = parent.path
			WHERE parent.item_type = 'folder'
				AND parent.path <> ''
				AND parent_local.path IS NULL
				AND substr(planner_visible_local_state.path, 1, length(parent.path) + 1) = parent.path || '/'
		)`
	oraclePrunePlannerVisibleRemoteDescendants = `DELETE FROM planner_visible_remote_state
		WHERE EXISTS (
			SELECT 1
			FROM baseline parent
			LEFT JOIN planner_visible_remote_state parent_remote ON parent_remote.path = parent.path
			WHERE parent.item_type = 'folder'
				AND parent.path <> ''
				AND parent_remote.path IS NULL
				AND substr(planner_visible_remote_state.path, 1, length(parent.path) + 1) = parent.path || '/'
		)`
)

type plannerVisiblePruneCase struct {
	name string
	// baseline holds path -> item type of the synced tree.
	baseline map[string]ItemType
	// visible holds the paths a side still has (path -> item type); paths
	// absent from baseline are new on that side.
	visible map[string]ItemType
	// want lists the visible paths that survive the prune.
	want []string
}

func plannerVisiblePruneCases() []plannerVisiblePruneCase {
	return []plannerVisiblePruneCase{
		{
			// Validates: R-2.4.1
			name: "nothing missing prunes nothing",
			baseline: map[string]ItemType{
				"a": ItemTypeFolder, "a/b": ItemTypeFolder, "a/b/c.txt": ItemTypeFile, "d.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"a": ItemTypeFolder, "a/b": ItemTypeFolder, "a/b/c.txt": ItemTypeFile, "d.txt": ItemTypeFile, "new.txt": ItemTypeFile,
			},
			want: []string{"a", "a/b", "a/b/c.txt", "d.txt", "new.txt"},
		},
		{
			// Validates: R-2.4.1
			name: "missing top folder prunes its subtree but not a sibling prefix",
			baseline: map[string]ItemType{
				"a": ItemTypeFolder, "a/b": ItemTypeFolder, "a/b/c.txt": ItemTypeFile, "a/x.txt": ItemTypeFile,
				"ab": ItemTypeFolder, "ab/z.txt": ItemTypeFile, "d.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"a/b": ItemTypeFolder, "a/b/c.txt": ItemTypeFile, "a/x.txt": ItemTypeFile, "a/new.txt": ItemTypeFile,
				"ab": ItemTypeFolder, "ab/z.txt": ItemTypeFile, "d.txt": ItemTypeFile,
			},
			want: []string{"ab", "ab/z.txt", "d.txt"},
		},
		{
			// Validates: R-2.4.1
			name: "missing folder three levels down prunes only below it",
			baseline: map[string]ItemType{
				"p": ItemTypeFolder, "p/q": ItemTypeFolder, "p/q/r": ItemTypeFolder,
				"p/q/r/f.txt": ItemTypeFile, "p/q/r/s": ItemTypeFolder, "p/q/r/s/g.txt": ItemTypeFile, "p/q/g.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"p": ItemTypeFolder, "p/q": ItemTypeFolder,
				"p/q/r/f.txt": ItemTypeFile, "p/q/r/s": ItemTypeFolder, "p/q/r/s/g.txt": ItemTypeFile, "p/q/g.txt": ItemTypeFile,
			},
			want: []string{"p", "p/q", "p/q/g.txt"},
		},
		{
			// Validates: R-2.4.1
			name: "nested missing folders prune once",
			baseline: map[string]ItemType{
				"a": ItemTypeFolder, "a/b": ItemTypeFolder, "a/b/c": ItemTypeFolder, "a/b/c/d.txt": ItemTypeFile, "a/e.txt": ItemTypeFile,
				"a-b": ItemTypeFolder, "a-b/f.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"a/b/c": ItemTypeFolder, "a/b/c/d.txt": ItemTypeFile, "a/e.txt": ItemTypeFile,
				"a-b": ItemTypeFolder, "a-b/f.txt": ItemTypeFile,
			},
			want: []string{"a-b", "a-b/f.txt"},
		},
		{
			// Validates: R-2.4.1
			name: "missing file or root row never prunes",
			baseline: map[string]ItemType{
				"a": ItemTypeFolder, "a/f.txt": ItemTypeFile, "a/f.txt/odd.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"a": ItemTypeFolder, "a/f.txt/odd.txt": ItemTypeFile, "b.txt": ItemTypeFile,
			},
			want: []string{"a", "a/f.txt/odd.txt", "b.txt"},
		},
		{
			// Validates: R-2.4.1
			name: "folder with slash-adjacent names",
			baseline: map[string]ItemType{
				"a": ItemTypeFolder, "a0": ItemTypeFolder, "a0/x.txt": ItemTypeFile, "a.": ItemTypeFolder, "a./y.txt": ItemTypeFile,
			},
			visible: map[string]ItemType{
				"a/under.txt": ItemTypeFile, "a0": ItemTypeFolder, "a0/x.txt": ItemTypeFile, "a.": ItemTypeFolder, "a./y.txt": ItemTypeFile,
			},
			want: []string{"a.", "a./y.txt", "a0", "a0/x.txt"},
		},
	}
}

// Validates: R-2.1.3, R-2.4.1
//
// The range-delete prune removes exactly the rows the correlated scan removed
// on trees with missing folders at several depths, nested, and next to
// sibling prefixes, and its query plan is a primary-key range per missing
// folder, not a scan of the visible rows per baseline row.
func TestReplacePlannerVisibleStateTx_PruneMatchesCorrelatedScan(t *testing.T) {
	t.Parallel()

	for _, tc := range plannerVisiblePruneCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := newTestStore(t)
			seedPlannerVisibleBaseline(t, store, tc.baseline)
			localRows, remoteRows := plannerVisibleRowsFromPaths(tc.visible)

			visible := plannerVisibleRowsForTest(t, store, localRows, remoteRows)
			plan := explainPlannerVisiblePrunes(t, store)

			assert.Equal(t, tc.want, localStatePaths(visible.Local), "local rows; query plan:\n%s", plan)
			assert.Equal(t, tc.want, remoteStatePaths(visible.Remote), "remote rows; query plan:\n%s", plan)

			oracleLocal, oracleRemote := plannerVisiblePathsAfterPrune(t, store, localRows, remoteRows,
				oraclePrunePlannerVisibleLocalDescendants, oraclePrunePlannerVisibleRemoteDescendants)
			assert.Equal(t, oracleLocal, localStatePaths(visible.Local), "local rows differ from the correlated scan; query plan:\n%s", plan)
			assert.Equal(t, oracleRemote, remoteStatePaths(visible.Remote), "remote rows differ from the correlated scan; query plan:\n%s", plan)
		})
	}
}

// Validates: R-2.4.1
func TestReplacePlannerVisibleStateTx_PruneUsesAPathRangePerMissingFolder(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	plan := explainPlannerVisiblePrunes(t, store)

	for _, table := range []string{plannerVisibleLocalStateTable, plannerVisibleRemoteStateTable} {
		assert.Regexp(t, `SEARCH below USING COVERING INDEX sqlite_autoindex_`+table+`_\d+ \(path>\? AND path<\?\)`, plan,
			"the prune of %s must range-scan the visible rows below each missing folder; query plan:\n%s", table, plan)
		assert.NotContains(t, plan, "SCAN "+table,
			"the prune must not scan the visible rows per baseline row; query plan:\n%s", table, plan)
	}
}

func seedPlannerVisibleBaseline(t *testing.T, store *SyncStore, baseline map[string]ItemType) {
	t.Helper()

	ctx := t.Context()
	_, err := store.rawDB().ExecContext(ctx,
		`INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash) VALUES ('root', '', 'root', '', '')`)
	require.NoError(t, err)
	for path, itemType := range baseline {
		_, err := store.rawDB().ExecContext(ctx,
			`INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash) VALUES (?, ?, ?, '', '')`,
			"id-"+path, path, itemType.String())
		require.NoError(t, err)
	}
}

func plannerVisibleRowsFromPaths(visible map[string]ItemType) ([]localStateRow, []remoteStateRow) {
	localRows := make([]localStateRow, 0, len(visible))
	remoteRows := make([]remoteStateRow, 0, len(visible))
	for path, itemType := range visible {
		localRows = append(localRows, localStateRow{Path: path, ItemType: itemType, Hash: "hash"})
		remoteRows = append(remoteRows, remoteStateRow{
			DriveID: driveid.New(engineTestDriveID), ItemID: "id-" + path, Path: path, ItemType: itemType, Hash: "hash",
		})
	}

	return localRows, remoteRows
}

// plannerVisiblePathsAfterPrune materializes the TEMP tables the way
// replacePlannerVisibleStateTx does and prunes them with the given
// statements, returning the surviving paths in order.
func plannerVisiblePathsAfterPrune(
	tb testing.TB,
	store *SyncStore,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	pruneLocal, pruneRemote string,
) ([]string, []string) {
	tb.Helper()

	ctx := tb.Context()
	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(tb, err)
	defer func() { _ = tx.Rollback() }()

	for _, query := range []string{
		sqlCreatePlannerVisibleLocalState, sqlCreatePlannerVisibleRemoteState,
		sqlDeletePlannerVisibleLocalState, sqlDeletePlannerVisibleRemoteState,
	} {
		_, err = tx.ExecContext(ctx, query)
		require.NoError(tb, err)
	}
	require.NoError(tb, insertPlannerVisibleLocalRowsTx(ctx, tx, localRows))
	require.NoError(tb, insertPlannerVisibleRemoteRowsTx(ctx, tx, remoteRows))
	_, err = tx.ExecContext(ctx, pruneLocal)
	require.NoError(tb, err)
	_, err = tx.ExecContext(ctx, pruneRemote)
	require.NoError(tb, err)

	local, err := listLocalStateRowsWithQuery(ctx, tx, sqlListPlannerVisibleLocalState)
	require.NoError(tb, err)
	remote, err := queryRemoteStateRowsWithRunner(ctx, tx, sqlListPlannerVisibleRemoteState, driveid.New(engineTestDriveID))
	require.NoError(tb, err)

	return localStatePaths(local), remoteStatePaths(remote)
}

// explainPlannerVisiblePrunes returns SQLite's query plans for both prune
// statements, so a regression to a per-row scan is visible in test output.
func explainPlannerVisiblePrunes(t *testing.T, store *SyncStore) string {
	t.Helper()

	ctx := t.Context()
	tx, err := beginPerfTx(ctx, store.rawDB())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	for _, query := range []string{
		sqlCreatePlannerVisibleLocalState, sqlCreatePlannerVisibleRemoteState,
	} {
		_, err = tx.ExecContext(ctx, query)
		require.NoError(t, err)
	}

	return queryPlanText(t, tx, sqlPrunePlannerVisibleLocalDescendants) + "\n" +
		queryPlanText(t, tx, sqlPrunePlannerVisibleRemoteDescendants)
}

func queryPlanText(t *testing.T, tx sqlTxRunner, query string) string {
	t.Helper()

	rows, err := tx.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query)
	require.NoError(t, err)
	defer rows.Close()

	var plan strings.Builder
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &notUsed, &detail))
		fmt.Fprintf(&plan, "%d/%d %s\n", id, parent, detail)
	}
	require.NoError(t, rows.Err())

	return plan.String()
}
