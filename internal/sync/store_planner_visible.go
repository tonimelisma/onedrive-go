package sync

import (
	"context"
	"fmt"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

const (
	plannerVisibleLocalStateTable  = "planner_visible_local_state"
	plannerVisibleRemoteStateTable = "planner_visible_remote_state"

	sqlCreatePlannerVisibleLocalState = `CREATE TEMP TABLE IF NOT EXISTS planner_visible_local_state (
		path       TEXT    NOT NULL PRIMARY KEY,
		item_type  TEXT    NOT NULL,
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
		item_type     TEXT    NOT NULL,
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
	Local  []localStateRow
	Remote []remoteStateRow
}

func filterLocalStateRowsForPlanning(rows []localStateRow, filter ContentFilterConfig) []localStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := newContentFilter(filter)
	filtered := make([]localStateRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, rows[i].ItemType) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func filterRemoteStateRowsForPlanning(rows []remoteStateRow, filter ContentFilterConfig) []remoteStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := newContentFilter(filter)
	filtered := make([]remoteStateRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, rows[i].ItemType) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func filterObservationIssueRowsForPlanning(rows []ObservationIssueRow, filter ContentFilterConfig) []ObservationIssueRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := newContentFilter(filter)
	filtered := make([]ObservationIssueRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, ItemTypeFile) || visibility.Visible(rows[i].Path, ItemTypeFolder) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func replacePlannerVisibleStateTx(
	ctx context.Context,
	tx sqlTxRunner,
	localRows []localStateRow,
	remoteRows []remoteStateRow,
	contentDriveID driveid.ID,
) (plannerVisibleRows, error) {
	for _, query := range []string{
		sqlCreatePlannerVisibleLocalState,
		sqlCreatePlannerVisibleRemoteState,
		sqlDeletePlannerVisibleLocalState,
		sqlDeletePlannerVisibleRemoteState,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return plannerVisibleRows{}, fmt.Errorf("sync: preparing planner-visible state: %w", err)
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
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return plannerVisibleRows{}, fmt.Errorf("sync: pruning planner-visible state: %w", err)
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
func insertPlannerVisibleLocalRowsTx(ctx context.Context, tx sqlTxRunner, rows []localStateRow) error {
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
			return fmt.Errorf("sync: inserting planner-visible local_state rows from %s: %w", chunk[0].Path, err)
		}
	}

	return nil
}

func insertPlannerVisibleRemoteRowsTx(ctx context.Context, tx sqlTxRunner, rows []remoteStateRow) error {
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
			return fmt.Errorf("sync: inserting planner-visible remote_state rows from %s: %w", chunk[0].Path, err)
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
