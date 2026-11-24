package syncstore

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

const (
	plannerVisibleLocalStateTable  = "planner_visible_local_state"
	plannerVisibleRemoteStateTable = "planner_visible_remote_state"

	sqlCreatePlannerVisibleLocalState = `CREATE TEMP TABLE IF NOT EXISTS planner_visible_local_state (
		path       TEXT    NOT NULL PRIMARY KEY,
		item_type  TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
		hash       TEXT,
		size       INTEGER,
		mtime      INTEGER,
		ctime      INTEGER NOT NULL DEFAULT 0,
		local_device INTEGER NOT NULL DEFAULT 0,
		local_inode  INTEGER NOT NULL DEFAULT 0,
		local_birth_time INTEGER NOT NULL DEFAULT 0,
		local_has_identity INTEGER NOT NULL DEFAULT 0
	)`
	sqlCreatePlannerVisibleRemoteState = `CREATE TEMP TABLE IF NOT EXISTS planner_visible_remote_state (
		drive_id      TEXT    NOT NULL DEFAULT '',
		item_id       TEXT    NOT NULL PRIMARY KEY,
		path          TEXT    NOT NULL UNIQUE,
		item_type     TEXT    NOT NULL CHECK(item_type IN ('file', 'folder', 'root')),
		hash          TEXT,
		size          INTEGER,
		mtime         INTEGER,
		etag          TEXT
	)`
	sqlDeletePlannerVisibleLocalState  = `DELETE FROM planner_visible_local_state`
	sqlDeletePlannerVisibleRemoteState = `DELETE FROM planner_visible_remote_state`
	// The row inserts go in chunks of plannerVisibleInsertChunkRows rows per
	// INSERT ... VALUES (multiRowInsertSQL appends the row tuples): one
	// statement per row cost 170 µs compiled each time and 48 µs prepared
	// once, a 200-row statement 16 µs a row (BenchmarkReplacePlannerVisibleState,
	// spec/design/performance-benchmarking.md).
	sqlInsertPlannerVisibleLocalStatePrefix = `INSERT INTO planner_visible_local_state
		(path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_birth_time, local_has_identity)
		VALUES `
	sqlInsertPlannerVisibleRemoteStatePrefix = `INSERT INTO planner_visible_remote_state
		(drive_id, item_id, path, item_type, hash, size, mtime, etag)
		VALUES `
	sqlListPlannerVisibleLocalState = `SELECT
		path, item_type, hash, size, mtime, ctime, local_device, local_inode, local_birth_time, local_has_identity
		FROM planner_visible_local_state
		ORDER BY path`
	sqlListPlannerVisibleRemoteState = `SELECT ` + sqlSelectRemoteStateCols + `
		FROM planner_visible_remote_state
		ORDER BY path`
	// The prunes remove the rows below folders the baseline knows but the
	// side no longer has. The missing folders are computed once (one pass
	// over baseline with a primary-key probe each) and each one removes its
	// subtree by a primary-key range on the TEMP table: every path below p
	// sorts between p || '/' and p || '0' under BINARY collation, the idiom
	// the comparison CTEs use. A correlated EXISTS over baseline with a
	// substr() prefix test scanned baseline once per visible row instead
	// (rows x baseline rows, 10 minutes per replan at 11k rows).
	// sqlHideLocalMoveLeftovers hides from the planner the old name a local
	// move left behind (local_move_intents) while it has no baseline row:
	// the moved file's baseline is at its new name, so the planner would read
	// the old one as a new file and upload it a second time. A path that
	// still has a baseline row (the move had not renamed when the process
	// died) is never hidden, since its absence would read as a local delete.
	sqlHideLocalMoveLeftovers = `DELETE FROM planner_visible_local_state
		WHERE path IN (SELECT old_path FROM local_move_intents)
			AND path NOT IN (SELECT path FROM baseline)`
	sqlPrunePlannerVisibleLocalDescendants = `WITH missing AS MATERIALIZED (
			SELECT parent.path AS path
			FROM baseline parent
			LEFT JOIN planner_visible_local_state parent_local ON parent_local.path = parent.path
			WHERE parent.item_type = 'folder'
				AND parent.path <> ''
				AND parent_local.path IS NULL
		)
		DELETE FROM planner_visible_local_state
		WHERE path IN (
			SELECT below.path
			FROM missing
			JOIN planner_visible_local_state below
				ON below.path >= missing.path || '/' AND below.path < missing.path || '0'
		)`
	sqlPrunePlannerVisibleRemoteDescendants = `WITH missing AS MATERIALIZED (
			SELECT parent.path AS path
			FROM baseline parent
			LEFT JOIN planner_visible_remote_state parent_remote ON parent_remote.path = parent.path
			WHERE parent.item_type = 'folder'
				AND parent.path <> ''
				AND parent_remote.path IS NULL
		)
		DELETE FROM planner_visible_remote_state
		WHERE path IN (
			SELECT below.path
			FROM missing
			JOIN planner_visible_remote_state below
				ON below.path >= missing.path || '/' AND below.path < missing.path || '0'
		)`
)

// plannerVisibleInsertChunkRows is the number of rows one INSERT carries.
// Ten columns a row keeps a chunk far below SQLite's 32,766 bound variables,
// and chunks of 1,000 measured no faster than 200.
const plannerVisibleInsertChunkRows = 200

const (
	plannerVisibleLocalStateColumns  = 10
	plannerVisibleRemoteStateColumns = 8
)

type plannerVisibleRows struct {
	Local  []syncdomain.LocalStateRow
	Remote []syncdomain.RemoteStateRow
}

// ReadPlanningSnapshot reads the current truth a plan is built from, in one
// measured transaction, so a commit that lands during the read is in every
// fact it returns or in none: the planner-visible local and remote rows
// (filter applied, the rows below a folder a side lost pruned), their
// comparison rows, the observation issues filter keeps and the baseline.
// defaultDriveID stands in for the content drive while the store has
// recorded none. The read writes only the planner-visible TEMP tables the
// comparison query joins, and rolls them back with the transaction.
func (s *SyncStore) ReadPlanningSnapshot(
	ctx context.Context,
	defaultDriveID driveid.ID,
	filter syncdomain.ContentFilterConfig,
) (snapshot *syncdomain.PlanningSnapshot, err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("syncstore: beginning the planning read: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "syncstore: rolling back the planning read")
	}()

	return readPlanningSnapshotTx(ctx, tx, defaultDriveID, filter)
}

func readPlanningSnapshotTx(
	ctx context.Context,
	tx sqlTxRunner,
	defaultDriveID driveid.ID,
	filter syncdomain.ContentFilterConfig,
) (*syncdomain.PlanningSnapshot, error) {
	rawLocalRows, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("syncstore: listing local_state rows: %w", err)
	}
	observationState, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("syncstore: reading observation state for remote_state: %w", err)
	}
	contentDriveID := observationState.ContentDriveID
	if contentDriveID.IsZero() {
		contentDriveID = defaultDriveID
	}
	rawRemoteRows, err := queryRemoteStateRowsWithRunner(ctx, tx, sqlListRemoteState, contentDriveID)
	if err != nil {
		return nil, fmt.Errorf("syncstore: listing remote_state rows: %w", err)
	}
	visible, err := replacePlannerVisibleStateTx(ctx, tx,
		filterLocalStateRowsForPlanning(rawLocalRows, filter),
		filterRemoteStateRowsForPlanning(rawRemoteRows, filter),
		contentDriveID,
	)
	if err != nil {
		return nil, err
	}
	comparisons, err := queryComparisonRowsForTables(ctx, tx, plannerVisibleLocalStateTable, plannerVisibleRemoteStateTable)
	if err != nil {
		return nil, fmt.Errorf("syncstore: querying comparison rows: %w", err)
	}
	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("syncstore: listing observation issues: %w", err)
	}
	baseline, err := loadBaselineWithRunner(ctx, tx, contentDriveID)
	if err != nil {
		return nil, fmt.Errorf("syncstore: loading planning baseline snapshot: %w", err)
	}

	return &syncdomain.PlanningSnapshot{
		ContentDriveID:    contentDriveID,
		Comparisons:       comparisons,
		Local:             visible.Local,
		Remote:            visible.Remote,
		ObservationIssues: filterObservationIssueRowsForPlanning(observationIssues, filter),
		Baseline:          baseline,
	}, nil
}

func filterLocalStateRowsForPlanning(rows []syncdomain.LocalStateRow, filter syncdomain.ContentFilterConfig) []syncdomain.LocalStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := syncdomain.NewContentFilter(filter)
	filtered := make([]syncdomain.LocalStateRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, rows[i].ItemType) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func filterRemoteStateRowsForPlanning(rows []syncdomain.RemoteStateRow, filter syncdomain.ContentFilterConfig) []syncdomain.RemoteStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := syncdomain.NewContentFilter(filter)
	filtered := make([]syncdomain.RemoteStateRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, rows[i].ItemType) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func filterObservationIssueRowsForPlanning(
	rows []syncdomain.ObservationIssueRow, filter syncdomain.ContentFilterConfig,
) []syncdomain.ObservationIssueRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := syncdomain.NewContentFilter(filter)
	filtered := make([]syncdomain.ObservationIssueRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, syncdomain.ItemTypeFile) || visibility.Visible(rows[i].Path, syncdomain.ItemTypeFolder) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func replacePlannerVisibleStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	localRows []syncdomain.LocalStateRow,
	remoteRows []syncdomain.RemoteStateRow,
	contentDriveID driveid.ID,
) (plannerVisibleRows, error) {
	for _, query := range []string{
		sqlCreatePlannerVisibleLocalState,
		sqlCreatePlannerVisibleRemoteState,
		sqlDeletePlannerVisibleLocalState,
		sqlDeletePlannerVisibleRemoteState,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return plannerVisibleRows{}, fmt.Errorf("syncstore: preparing planner-visible state: %w", err)
		}
	}

	if err := insertPlannerVisibleLocalRowsTx(ctx, tx, localRows); err != nil {
		return plannerVisibleRows{}, err
	}
	if err := insertPlannerVisibleRemoteRowsTx(ctx, tx, remoteRows); err != nil {
		return plannerVisibleRows{}, err
	}

	for _, query := range []string{
		sqlPrunePlannerVisibleLocalDescendants,
		sqlPrunePlannerVisibleRemoteDescendants,
		sqlHideLocalMoveLeftovers,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return plannerVisibleRows{}, fmt.Errorf("syncstore: pruning planner-visible state: %w", err)
		}
	}

	prunedLocalRows, err := listLocalStateRowsWithQuery(ctx, tx, sqlListPlannerVisibleLocalState)
	if err != nil {
		return plannerVisibleRows{}, err
	}
	prunedRemoteRows, err := queryRemoteStateRowsWithRunner(ctx, tx, sqlListPlannerVisibleRemoteState, contentDriveID)
	if err != nil {
		return plannerVisibleRows{}, err
	}

	return plannerVisibleRows{
		Local:  prunedLocalRows,
		Remote: prunedRemoteRows,
	}, nil
}

// insertPlannerVisibleLocalRowsTx inserts the rows in multi-row statements
// of plannerVisibleInsertChunkRows rows each.
func insertPlannerVisibleLocalRowsTx(ctx context.Context, tx sqlTxRunner, rows []syncdomain.LocalStateRow) error {
	for start := 0; start < len(rows); start += plannerVisibleInsertChunkRows {
		chunk := rows[start:min(start+plannerVisibleInsertChunkRows, len(rows))]
		args := make([]any, 0, len(chunk)*plannerVisibleLocalStateColumns)
		for i := range chunk {
			row := &chunk[i]
			args = append(args,
				row.Path,
				row.ItemType,
				nullString(row.Hash),
				nullKnownInt64(row.Size, true),
				nullOptionalInt64(row.Mtime),
				row.Ctime,
				int64(row.LocalDevice),
				int64(row.LocalInode),
				row.LocalBirthTime,
				boolInt(row.LocalHasIdentity),
			)
		}
		query := multiRowInsertSQL(sqlInsertPlannerVisibleLocalStatePrefix, plannerVisibleLocalStateColumns, len(chunk))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("syncstore: inserting planner-visible local_state rows from %s: %w", chunk[0].Path, err)
		}
	}

	return nil
}

func insertPlannerVisibleRemoteRowsTx(ctx context.Context, tx sqlTxRunner, rows []syncdomain.RemoteStateRow) error {
	for start := 0; start < len(rows); start += plannerVisibleInsertChunkRows {
		chunk := rows[start:min(start+plannerVisibleInsertChunkRows, len(rows))]
		args := make([]any, 0, len(chunk)*plannerVisibleRemoteStateColumns)
		for i := range chunk {
			row := &chunk[i]
			args = append(args,
				row.DriveID.String(),
				row.ItemID,
				row.Path,
				row.ItemType,
				nullString(row.Hash),
				nullKnownInt64(row.Size, true),
				nullOptionalInt64(row.Mtime),
				nullString(row.ETag),
			)
		}
		query := multiRowInsertSQL(sqlInsertPlannerVisibleRemoteStatePrefix, plannerVisibleRemoteStateColumns, len(chunk))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("syncstore: inserting planner-visible remote_state rows from %s: %w", chunk[0].Path, err)
		}
	}

	return nil
}

// multiRowInsertSQL appends rowCount tuples of columnCount placeholders to an
// INSERT ... VALUES prefix.
func multiRowInsertSQL(prefix string, columnCount, rowCount int) string {
	tuple := "(?" + strings.Repeat(", ?", columnCount-1) + ")"
	var query strings.Builder
	query.Grow(len(prefix) + rowCount*(len(tuple)+2))
	query.WriteString(prefix)
	for row := range rowCount {
		if row > 0 {
			query.WriteString(", ")
		}
		query.WriteString(tuple)
	}

	return query.String()
}
