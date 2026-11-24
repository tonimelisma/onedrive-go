package syncstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// The planning read returns the planner-visible current truth: the content
// filter applied, the rows below a folder a side lost pruned, the comparison
// rows of exactly those rows (undecided: deciding is the planner's), the
// observation issues the filter keeps and the baseline, with the default
// drive standing in for a store that has not recorded its content drive.
//
// Validates: R-2.1.3, R-2.4.1
func TestReadPlanningSnapshot_ReturnsThePlannerVisibleCurrentTruth(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	driveID := driveid.New(syncfixture.EngineTestDriveID)
	_, err := store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type, local_hash, remote_hash)
		VALUES
			('folder-docs', 'docs', 'folder', '', ''),
			('file-docs', 'docs/file.txt', 'file', 'hash', 'hash')`)
	require.NoError(t, err)
	_, err = store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO local_state (path, item_type, hash, size, mtime)
		VALUES
			('docs/file.txt', 'file', 'hash', 1, 1),
			('new.txt', 'file', 'h', 1, 1),
			('skip/hidden.txt', 'file', 'h', 1, 1)`)
	require.NoError(t, err)
	_, err = store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO remote_state (drive_id, item_id, path, item_type, hash)
		VALUES
			('', 'folder-docs', 'docs', 'folder', ''),
			('', 'file-docs', 'docs/file.txt', 'file', 'hash'),
			('', 'remote-skip', 'skip/remote.txt', 'file', 'h')`)
	require.NoError(t, err)
	_, err = store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO observation_issues (path, issue_type, scope_key)
		VALUES ('CON', ?, ''), ('skip/CON', ?, '')`,
		string(syncdomain.IssueInvalidFilename), string(syncdomain.IssueInvalidFilename))
	require.NoError(t, err)

	snapshot, err := store.ReadPlanningSnapshot(ctx, driveID, syncdomain.ContentFilterConfig{IgnoredDirs: []string{"skip"}})
	require.NoError(t, err)

	assert.True(t, driveID.Equal(snapshot.ContentDriveID), "the default drive stands in for an unrecorded content drive")
	// The local docs folder is gone, so its file is pruned from the local
	// side; skip/ is filtered from both.
	assert.Equal(t, []string{"new.txt"}, localStatePaths(snapshot.Local))
	assert.Equal(t, []string{"docs", "docs/file.txt"}, remoteStatePaths(snapshot.Remote))
	for _, row := range snapshot.Remote {
		assert.True(t, driveID.Equal(row.DriveID), "a remote row without a drive is the content drive's: %s", row.Path)
	}
	comparisonPaths := make([]string, 0, len(snapshot.Comparisons))
	for i := range snapshot.Comparisons {
		comparisonPaths = append(comparisonPaths, snapshot.Comparisons[i].Path)
	}
	assert.Equal(t, []string{"docs", "docs/file.txt", "new.txt"}, comparisonPaths)
	require.Len(t, snapshot.ObservationIssues, 1)
	assert.Equal(t, "CON", snapshot.ObservationIssues[0].Path)
	require.NotNil(t, snapshot.Baseline)
	assert.Equal(t, 2, snapshot.Baseline.Len())
}

// The planning read is one measured transaction (R-6.6.15), and its baseline
// is read in it, not taken from the store's baseline cache: a row that reached
// SQLite without passing through the cache (as when a commit lands between a
// run's cached view and its planning read) is in the snapshot's baseline with
// its comparison row, so every comparison row with a baseline side has its
// entry (R-2.17.2). The planner-visible TEMP tables the read writes are
// rolled back with it. (A second connection cannot commit in the middle of
// the read to prove more: the read's observation-state step takes the write
// lock, so such a commit either waits for the read or fails it.)
//
// Validates: R-2.1.3, R-2.17.2, R-6.6.15
func TestReadPlanningSnapshot_ReadsEveryFactInOneTransaction(t *testing.T) {
	t.Parallel()

	store := newTestStore(t)
	ctx := t.Context()
	cached, err := store.Load(ctx)
	require.NoError(t, err)
	_, err = store.RawDBForTest().ExecContext(ctx, `
		INSERT INTO baseline (item_id, path, item_type) VALUES ('item-fm2', 'fm2', 'folder')`)
	require.NoError(t, err)
	_, found := cached.GetByPath("fm2")
	require.False(t, found, "the cache has not seen the row")

	collector := perf.NewCollector(nil)
	snapshot, err := store.ReadPlanningSnapshot(perf.WithCollector(ctx, collector),
		driveid.New(syncfixture.EngineTestDriveID), syncdomain.ContentFilterConfig{})
	require.NoError(t, err)

	assert.Equal(t, 1, collector.Snapshot().DBTransactionCount, "the whole read is one measured transaction")
	require.Len(t, snapshot.Comparisons, 1)
	assert.Equal(t, "fm2", snapshot.Comparisons[0].Path)
	_, found = snapshot.Baseline.GetByPath("fm2")
	assert.True(t, found, "the comparison row's baseline entry is in the snapshot's baseline")

	var tempTables int
	require.NoError(t, store.RawDBForTest().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM temp.sqlite_master WHERE name LIKE 'planner_visible_%'`).Scan(&tempTables))
	assert.Zero(t, tempTables, "the read's temp tables are rolled back")
}
