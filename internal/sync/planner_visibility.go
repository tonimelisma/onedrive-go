package sync

import (
	"context"
	"fmt"
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
		local_device INTEGER NOT NULL DEFAULT 0,
		local_inode  INTEGER NOT NULL DEFAULT 0,
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
	sqlInsertPlannerVisibleLocalState  = `INSERT INTO planner_visible_local_state
		(path, item_type, hash, size, mtime, local_device, local_inode, local_has_identity)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	sqlInsertPlannerVisibleRemoteState = `INSERT INTO planner_visible_remote_state
		(drive_id, item_id, path, item_type, hash, size, mtime, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
)

func filterLocalStateRowsForPlanning(rows []LocalStateRow, filter ContentFilterConfig) []LocalStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := NewContentFilter(filter)
	filtered := make([]LocalStateRow, 0, len(rows))
	for i := range rows {
		if visibility.Visible(rows[i].Path, rows[i].ItemType) {
			filtered = append(filtered, rows[i])
		}
	}
	return filtered
}

func filterRemoteStateRowsForPlanning(rows []RemoteStateRow, filter ContentFilterConfig) []RemoteStateRow {
	if len(rows) == 0 {
		return nil
	}
	visibility := NewContentFilter(filter)
	filtered := make([]RemoteStateRow, 0, len(rows))
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
	visibility := NewContentFilter(filter)
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
	localRows []LocalStateRow,
	remoteRows []RemoteStateRow,
) error {
	for _, query := range []string{
		sqlCreatePlannerVisibleLocalState,
		sqlCreatePlannerVisibleRemoteState,
		sqlDeletePlannerVisibleLocalState,
		sqlDeletePlannerVisibleRemoteState,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return fmt.Errorf("sync: preparing planner-visible state: %w", err)
		}
	}

	for i := range localRows {
		row := localRows[i]
		if _, err := tx.ExecContext(ctx, sqlInsertPlannerVisibleLocalState,
			row.Path,
			row.ItemType,
			nullString(row.Hash),
			nullKnownInt64(row.Size, true),
			nullOptionalInt64(row.Mtime),
			int64(row.LocalDevice),
			int64(row.LocalInode),
			boolInt(row.LocalHasIdentity),
		); err != nil {
			return fmt.Errorf("sync: inserting planner-visible local_state row for %s: %w", row.Path, err)
		}
	}

	for i := range remoteRows {
		row := remoteRows[i]
		if _, err := tx.ExecContext(ctx, sqlInsertPlannerVisibleRemoteState,
			row.DriveID.String(),
			row.ItemID,
			row.Path,
			row.ItemType,
			nullString(row.Hash),
			nullKnownInt64(row.Size, true),
			nullOptionalInt64(row.Mtime),
			nullString(row.ETag),
		); err != nil {
			return fmt.Errorf("sync: inserting planner-visible remote_state row for %s: %w", row.Path, err)
		}
	}

	return nil
}
