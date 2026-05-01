package sync

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	sqlQueryComparisonState = `
WITH
all_paths AS (
	SELECT path FROM baseline
	UNION
	SELECT path FROM local_state
	UNION
	SELECT path FROM remote_state
),
local_move_candidates AS (
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = COALESCE(b.item_type, '')
		AND l.local_has_identity = 1
		AND b.local_has_identity = 1
		AND l.local_device = b.local_device
		AND l.local_inode = b.local_inode
	WHERE NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
	UNION ALL
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = 'file'
		AND COALESCE(b.item_type, '') = 'file'
		AND b.local_has_identity = 0
		AND NULLIF(l.hash, '') = NULLIF(b.local_hash, '')
	WHERE b.local_hash IS NOT NULL
		AND b.local_hash <> ''
		AND NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
),
local_move_sources AS (
	SELECT
		source_path,
		MIN(target_path) AS target_path,
		COUNT(*) AS candidate_count
	FROM local_move_candidates
	GROUP BY source_path
),
local_move_targets AS (
	SELECT
		target_path,
		source_path,
		candidate_count
	FROM local_move_sources
),
remote_move_sources AS (
	SELECT
		b.path AS source_path,
		r.path AS target_path
	FROM baseline b
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
),
remote_move_targets AS (
	SELECT
		target_path,
		source_path
	FROM remote_move_sources
),
comparison_state AS (
	SELECT
		p.path,
		COALESCE(b.item_id, '') AS baseline_item_id,
		COALESCE(r.item_id, '') AS remote_item_id,
		COALESCE(b.item_type, l.item_type, r.item_type, '') AS item_type,
		CASE WHEN b.path IS NOT NULL THEN 1 ELSE 0 END AS baseline_present,
		CASE WHEN l.path IS NOT NULL THEN 1 ELSE 0 END AS local_present,
		CASE WHEN r.path IS NOT NULL THEN 1 ELSE 0 END AS remote_present,
		CASE
			WHEN b.path IS NULL OR l.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(l.item_type, '') THEN 1
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND (
					COALESCE(b.local_device, 0) <> COALESCE(l.local_device, 0)
					OR COALESCE(b.local_inode, 0) <> COALESCE(l.local_inode, 0)
				) THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.local_hash, '') <> '' OR COALESCE(l.hash, '') <> '' THEN
				CASE WHEN COALESCE(b.local_hash, '') <> COALESCE(l.hash, '') THEN 1 ELSE 0 END
			WHEN COALESCE(b.local_size, 0) <> COALESCE(l.size, 0) THEN 1
			WHEN COALESCE(b.local_mtime, 0) <> COALESCE(l.mtime, 0) THEN 1
			ELSE 0
		END AS local_changed,
		CASE
			WHEN b.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(r.item_type, '') THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.remote_hash, '') <> '' OR COALESCE(r.hash, '') <> '' THEN
				CASE WHEN COALESCE(b.remote_hash, '') <> COALESCE(r.hash, '') THEN 1 ELSE 0 END
			WHEN COALESCE(b.remote_size, 0) <> COALESCE(r.size, 0) THEN 1
			WHEN COALESCE(b.remote_mtime, 0) <> COALESCE(r.mtime, 0) THEN 1
			WHEN COALESCE(b.etag, '') <> COALESCE(r.etag, '') THEN 1
			ELSE 0
		END AS remote_changed,
		CASE
			WHEN l.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(l.item_type, '') <> COALESCE(r.item_type, '') THEN 0
			WHEN COALESCE(l.item_type, '') = 'folder' THEN 1
			WHEN COALESCE(l.hash, '') <> COALESCE(r.hash, '') THEN 0
			WHEN COALESCE(l.size, 0) <> COALESCE(r.size, 0) THEN 0
			ELSE 1
		END AS current_equal,
		COALESCE(lms.target_path, '') AS local_move_target,
		COALESCE(lmt.source_path, '') AS local_move_source,
		COALESCE(lms.candidate_count, lmt.candidate_count, 0) AS local_move_candidate_count,
		COALESCE(rms.target_path, '') AS remote_move_target,
		COALESCE(rmt.source_path, '') AS remote_move_source,
		CASE
			WHEN b.path IS NOT NULL AND l.path IS NULL AND COALESCE(lms.candidate_count, 0) = 1 THEN 'local_move_source'
			WHEN b.path IS NOT NULL AND l.path IS NULL AND r.path IS NULL THEN 'both_missing'
			WHEN l.path IS NOT NULL AND b.path IS NULL AND lmt.source_path <> '' AND lmt.candidate_count = 1 THEN 'local_move_dest'
			WHEN b.path IS NOT NULL AND rms.target_path <> '' THEN 'remote_move_source'
			WHEN r.path IS NOT NULL AND b.path IS NULL AND rmt.source_path <> '' THEN 'remote_move_dest'
			WHEN b.path IS NULL AND l.path IS NOT NULL AND r.path IS NULL THEN 'local_only_create'
			WHEN b.path IS NULL AND l.path IS NULL AND r.path IS NOT NULL THEN 'remote_only_create'
			WHEN b.path IS NULL AND l.path IS NOT NULL AND r.path IS NOT NULL AND (
				COALESCE(l.item_type, '') = COALESCE(r.item_type, '')
				AND (
					COALESCE(l.item_type, '') = 'folder'
					OR (
						COALESCE(l.hash, '') = COALESCE(r.hash, '')
						AND COALESCE(l.size, 0) = COALESCE(r.size, 0)
					)
				)
			) THEN 'create_equal'
			WHEN b.path IS NULL AND l.path IS NOT NULL AND r.path IS NOT NULL THEN 'create_conflict'
			WHEN b.path IS NOT NULL AND l.path IS NOT NULL AND r.path IS NOT NULL AND
				(
					COALESCE(b.item_type, '') = COALESCE(l.item_type, '')
					AND (
						COALESCE(b.local_has_identity, 0) = 0
						OR COALESCE(l.local_has_identity, 0) = 0
						OR (
							COALESCE(b.local_device, 0) = COALESCE(l.local_device, 0)
							AND COALESCE(b.local_inode, 0) = COALESCE(l.local_inode, 0)
						)
					)
					AND (
						COALESCE(b.item_type, '') = 'folder'
						OR (
							COALESCE(b.local_hash, '') = COALESCE(l.hash, '')
							AND COALESCE(b.local_size, 0) = COALESCE(l.size, 0)
							AND COALESCE(b.local_mtime, 0) = COALESCE(l.mtime, 0)
						)
					)
				) AND (
					COALESCE(b.item_type, '') = COALESCE(r.item_type, '')
					AND (
						COALESCE(b.item_type, '') = 'folder'
						OR (
							COALESCE(b.remote_hash, '') = COALESCE(r.hash, '')
							AND COALESCE(b.remote_size, 0) = COALESCE(r.size, 0)
							AND COALESCE(b.remote_mtime, 0) = COALESCE(r.mtime, 0)
							AND COALESCE(b.etag, '') = COALESCE(r.etag, '')
						)
					)
				) THEN 'unchanged'
			WHEN b.path IS NOT NULL AND l.path IS NOT NULL AND r.path IS NOT NULL AND (
				COALESCE(l.item_type, '') = COALESCE(r.item_type, '')
				AND (
					COALESCE(l.item_type, '') = 'folder'
					OR (
						COALESCE(l.hash, '') = COALESCE(r.hash, '')
						AND COALESCE(l.size, 0) = COALESCE(r.size, 0)
					)
				)
				AND (
					COALESCE(b.item_type, '') <> COALESCE(r.item_type, '')
					OR (
						COALESCE(b.item_type, '') <> 'folder'
						AND (
							COALESCE(b.remote_hash, '') <> COALESCE(r.hash, '')
							OR COALESCE(b.remote_size, 0) <> COALESCE(r.size, 0)
							OR COALESCE(b.remote_mtime, 0) <> COALESCE(r.mtime, 0)
							OR COALESCE(b.etag, '') <> COALESCE(r.etag, '')
						)
					)
				)
			) THEN 'equal_again'
			WHEN b.path IS NOT NULL AND l.path IS NULL AND r.path IS NOT NULL THEN 'local_missing'
			WHEN b.path IS NOT NULL AND l.path IS NOT NULL AND r.path IS NULL THEN 'remote_missing'
			WHEN b.path IS NOT NULL AND l.path IS NOT NULL AND r.path IS NOT NULL THEN 'diverged'
			ELSE 'unknown'
		END AS comparison_kind
	FROM all_paths p
	LEFT JOIN baseline b ON b.path = p.path
	LEFT JOIN local_state l ON l.path = p.path
	LEFT JOIN remote_state r ON r.path = p.path
	LEFT JOIN local_move_sources lms ON lms.source_path = p.path
	LEFT JOIN local_move_targets lmt ON lmt.target_path = p.path
	LEFT JOIN remote_move_sources rms ON rms.source_path = p.path
	LEFT JOIN remote_move_targets rmt ON rmt.target_path = p.path
)
SELECT
	path,
	baseline_item_id,
	remote_item_id,
	item_type,
	baseline_present,
	local_present,
	remote_present,
	local_changed,
	remote_changed,
	current_equal,
	local_move_target,
	local_move_source,
	local_move_candidate_count,
	remote_move_target,
	remote_move_source,
	comparison_kind
FROM comparison_state
ORDER BY path`

	sqlQueryReconciliationState = `
WITH
all_paths AS (
	SELECT path FROM baseline
	UNION
	SELECT path FROM local_state
	UNION
	SELECT path FROM remote_state
),
local_move_candidates AS (
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = COALESCE(b.item_type, '')
		AND l.local_has_identity = 1
		AND b.local_has_identity = 1
		AND l.local_device = b.local_device
		AND l.local_inode = b.local_inode
	WHERE NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
	UNION ALL
	SELECT
		b.path AS source_path,
		l.path AS target_path
	FROM baseline b
	JOIN local_state l
		ON l.path <> b.path
		AND COALESCE(l.item_type, '') = 'file'
		AND COALESCE(b.item_type, '') = 'file'
		AND b.local_has_identity = 0
		AND NULLIF(l.hash, '') = NULLIF(b.local_hash, '')
	WHERE b.local_hash IS NOT NULL
		AND b.local_hash <> ''
		AND NOT EXISTS (SELECT 1 FROM baseline target WHERE target.path = l.path)
),
local_move_sources AS (
	SELECT
		source_path,
		MIN(target_path) AS target_path,
		COUNT(*) AS candidate_count
	FROM local_move_candidates
	GROUP BY source_path
),
local_move_targets AS (
	SELECT
		target_path,
		source_path,
		candidate_count
	FROM local_move_sources
),
remote_move_sources AS (
	SELECT
		b.path AS source_path,
		r.path AS target_path
	FROM baseline b
	JOIN remote_state r ON r.item_id = b.item_id AND r.path <> b.path
),
remote_move_targets AS (
	SELECT
		target_path,
		source_path
	FROM remote_move_sources
),
comparison_flags AS (
	SELECT
		p.path,
		COALESCE(b.item_type, l.item_type, r.item_type, '') AS item_type,
		CASE WHEN b.path IS NOT NULL THEN 1 ELSE 0 END AS baseline_present,
		CASE WHEN l.path IS NOT NULL THEN 1 ELSE 0 END AS local_present,
		CASE WHEN r.path IS NOT NULL THEN 1 ELSE 0 END AS remote_present,
		CASE
			WHEN b.path IS NULL OR l.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(l.item_type, '') THEN 1
			WHEN COALESCE(b.local_has_identity, 0) = 1 AND COALESCE(l.local_has_identity, 0) = 1
				AND (
					COALESCE(b.local_device, 0) <> COALESCE(l.local_device, 0)
					OR COALESCE(b.local_inode, 0) <> COALESCE(l.local_inode, 0)
				) THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.local_hash, '') <> '' OR COALESCE(l.hash, '') <> '' THEN
				CASE WHEN COALESCE(b.local_hash, '') <> COALESCE(l.hash, '') THEN 1 ELSE 0 END
			WHEN COALESCE(b.local_size, 0) <> COALESCE(l.size, 0) THEN 1
			WHEN COALESCE(b.local_mtime, 0) <> COALESCE(l.mtime, 0) THEN 1
			ELSE 0
		END AS local_changed,
		CASE
			WHEN b.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(b.item_type, '') <> COALESCE(r.item_type, '') THEN 1
			WHEN COALESCE(b.item_type, '') = 'folder' THEN 0
			WHEN COALESCE(b.remote_hash, '') <> '' OR COALESCE(r.hash, '') <> '' THEN
				CASE WHEN COALESCE(b.remote_hash, '') <> COALESCE(r.hash, '') THEN 1 ELSE 0 END
			WHEN COALESCE(b.remote_size, 0) <> COALESCE(r.size, 0) THEN 1
			WHEN COALESCE(b.remote_mtime, 0) <> COALESCE(r.mtime, 0) THEN 1
			WHEN COALESCE(b.etag, '') <> COALESCE(r.etag, '') THEN 1
			ELSE 0
		END AS remote_changed,
		CASE
			WHEN l.path IS NULL OR r.path IS NULL THEN 0
			WHEN COALESCE(l.item_type, '') <> COALESCE(r.item_type, '') THEN 0
			WHEN COALESCE(l.item_type, '') = 'folder' THEN 1
			WHEN COALESCE(l.hash, '') <> COALESCE(r.hash, '') THEN 0
			WHEN COALESCE(l.size, 0) <> COALESCE(r.size, 0) THEN 0
			ELSE 1
		END AS current_equal,
		COALESCE(lms.target_path, '') AS local_move_target,
		COALESCE(lms.candidate_count, lmt.candidate_count, 0) AS local_move_candidate_count,
		COALESCE(lmt.source_path, '') AS local_move_source,
		COALESCE(rms.target_path, '') AS remote_move_target,
		COALESCE(rmt.source_path, '') AS remote_move_source
	FROM all_paths p
	LEFT JOIN baseline b ON b.path = p.path
	LEFT JOIN local_state l ON l.path = p.path
	LEFT JOIN remote_state r ON r.path = p.path
	LEFT JOIN local_move_sources lms ON lms.source_path = p.path
	LEFT JOIN local_move_targets lmt ON lmt.target_path = p.path
	LEFT JOIN remote_move_sources rms ON rms.source_path = p.path
	LEFT JOIN remote_move_targets rmt ON rmt.target_path = p.path
),
comparison_state AS (
	SELECT
		path,
		item_type,
		baseline_present,
		local_present,
		remote_present,
		local_changed,
		remote_changed,
		current_equal,
		local_move_target,
		local_move_candidate_count,
		local_move_source,
		remote_move_target,
		remote_move_source,
		CASE
			WHEN baseline_present = 1 AND local_present = 0 AND local_move_candidate_count = 1 THEN 'local_move_source'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 0 THEN 'both_missing'
			WHEN local_present = 1 AND baseline_present = 0 AND local_move_source <> '' AND local_move_candidate_count = 1 THEN 'local_move_dest'
			WHEN baseline_present = 1 AND remote_move_target <> '' THEN 'remote_move_source'
			WHEN remote_present = 1 AND baseline_present = 0 AND remote_move_source <> '' THEN 'remote_move_dest'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 0 THEN 'local_only_create'
			WHEN baseline_present = 0 AND local_present = 0 AND remote_present = 1 THEN 'remote_only_create'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 1 AND current_equal = 1 THEN 'create_equal'
			WHEN baseline_present = 0 AND local_present = 1 AND remote_present = 1 THEN 'create_conflict'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 AND local_changed = 0 AND remote_changed = 0 THEN 'unchanged'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 AND current_equal = 1 AND remote_changed = 1 THEN 'equal_again'
			WHEN baseline_present = 1 AND local_present = 0 AND remote_present = 1 THEN 'local_missing'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 0 THEN 'remote_missing'
			WHEN baseline_present = 1 AND local_present = 1 AND remote_present = 1 THEN 'diverged'
			ELSE 'unknown'
		END AS comparison_kind
	FROM comparison_flags
),
reconciliation_state AS (
	SELECT
		path,
		item_type,
		comparison_kind,
		local_move_target,
		local_move_source,
		remote_move_target,
		remote_move_source,
		CASE
			WHEN comparison_kind = 'both_missing' THEN 'baseline_remove'
			WHEN comparison_kind = 'local_move_source' THEN 'local_move'
			WHEN comparison_kind = 'local_move_dest' THEN 'local_move'
			WHEN comparison_kind = 'remote_move_source' THEN 'remote_move'
			WHEN comparison_kind = 'remote_move_dest' THEN 'remote_move'
			WHEN comparison_kind = 'local_only_create' AND item_type = 'folder' THEN 'folder_create_remote'
			WHEN comparison_kind = 'local_only_create' THEN 'upload'
			WHEN comparison_kind = 'remote_only_create' AND item_type = 'folder' THEN 'folder_create_local'
			WHEN comparison_kind = 'remote_only_create' THEN 'download'
			WHEN comparison_kind = 'create_equal' THEN 'baseline_update'
			WHEN comparison_kind = 'create_conflict' THEN 'conflict_create_create'
			WHEN comparison_kind = 'unchanged' THEN 'noop'
			WHEN comparison_kind = 'equal_again' THEN 'baseline_update'
			WHEN comparison_kind = 'local_missing' AND item_type = 'folder' AND remote_changed = 0 THEN 'folder_create_local'
			WHEN comparison_kind = 'local_missing' AND remote_changed = 0 THEN 'remote_delete'
			WHEN comparison_kind = 'local_missing' AND remote_changed = 1 THEN 'download'
			WHEN comparison_kind = 'remote_missing' AND local_changed = 0 THEN 'local_delete'
			WHEN comparison_kind = 'remote_missing' AND local_changed = 1 THEN 'conflict_edit_delete'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 0 THEN 'upload'
			WHEN comparison_kind = 'diverged' AND local_changed = 0 AND remote_changed = 1 THEN 'download'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 1 AND current_equal = 1 THEN 'baseline_update'
			WHEN comparison_kind = 'diverged' AND local_changed = 1 AND remote_changed = 1 THEN 'conflict_edit_edit'
			ELSE 'noop'
		END AS reconciliation_kind
	FROM comparison_state
)
SELECT
	path,
	item_type,
	comparison_kind,
	reconciliation_kind,
	local_move_target,
	local_move_source,
	remote_move_target,
	remote_move_source
FROM reconciliation_state
ORDER BY path`
)

type SQLiteComparisonRow struct {
	Path                    string
	BaselineItemID          string
	RemoteItemID            string
	ItemType                ItemType
	BaselinePresent         bool
	LocalPresent            bool
	RemotePresent           bool
	LocalChanged            bool
	RemoteChanged           bool
	CurrentEqual            bool
	LocalMoveTarget         string
	LocalMoveSource         string
	LocalMoveCandidateCount int
	RemoteMoveTarget        string
	RemoteMoveSource        string
	ComparisonKind          string
}

type SQLiteReconciliationRow struct {
	Path               string
	ItemType           ItemType
	ComparisonKind     string
	ReconciliationKind string
	LocalMoveTarget    string
	LocalMoveSource    string
	RemoteMoveTarget   string
	RemoteMoveSource   string
}

func (m *SyncStore) QueryComparisonState(ctx context.Context) ([]SQLiteComparisonRow, error) {
	return queryComparisonStateWithRunner(ctx, m.db)
}

func queryComparisonStateWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
) ([]SQLiteComparisonRow, error) {
	return queryComparisonStateWithRunnerForTables(ctx, runner, "local_state", "remote_state")
}

func queryComparisonStateWithRunnerForTables(
	ctx context.Context,
	runner sqlTxRunner,
	localStateTable string,
	remoteStateTable string,
) ([]SQLiteComparisonRow, error) {
	rows, err := runner.QueryContext(ctx, sqlQueryForStateTables(sqlQueryComparisonState, localStateTable, remoteStateTable))
	if err != nil {
		return nil, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	defer rows.Close()

	var results []SQLiteComparisonRow
	for rows.Next() {
		var (
			row                                        SQLiteComparisonRow
			itemType                                   sql.NullString
			baselinePresent, localPresent              int
			remotePresent, localChanged, remoteChanged int
			currentEqual                               int
		)
		if err := rows.Scan(
			&row.Path,
			&row.BaselineItemID,
			&row.RemoteItemID,
			&itemType,
			&baselinePresent,
			&localPresent,
			&remotePresent,
			&localChanged,
			&remoteChanged,
			&currentEqual,
			&row.LocalMoveTarget,
			&row.LocalMoveSource,
			&row.LocalMoveCandidateCount,
			&row.RemoteMoveTarget,
			&row.RemoteMoveSource,
			&row.ComparisonKind,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning comparison state row: %w", err)
		}
		if itemType.Valid {
			parsed, err := ParseItemType(itemType.String)
			if err != nil {
				return nil, fmt.Errorf("sync: parsing comparison state item type %q: %w", itemType.String, err)
			}
			row.ItemType = parsed
		}
		row.BaselinePresent = baselinePresent != 0
		row.LocalPresent = localPresent != 0
		row.RemotePresent = remotePresent != 0
		row.LocalChanged = localChanged != 0
		row.RemoteChanged = remoteChanged != 0
		row.CurrentEqual = currentEqual != 0
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating comparison state rows: %w", err)
	}

	return results, nil
}

func (m *SyncStore) QueryReconciliationState(ctx context.Context) ([]SQLiteReconciliationRow, error) {
	return queryReconciliationStateWithRunner(ctx, m.db)
}

func queryReconciliationStateWithRunner(
	ctx context.Context,
	runner sqlTxRunner,
) ([]SQLiteReconciliationRow, error) {
	return queryReconciliationStateWithRunnerForTables(ctx, runner, "local_state", "remote_state")
}

func queryReconciliationStateWithRunnerForTables(
	ctx context.Context,
	runner sqlTxRunner,
	localStateTable string,
	remoteStateTable string,
) ([]SQLiteReconciliationRow, error) {
	rows, err := runner.QueryContext(ctx, sqlQueryForStateTables(sqlQueryReconciliationState, localStateTable, remoteStateTable))
	if err != nil {
		return nil, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	defer rows.Close()

	var results []SQLiteReconciliationRow
	for rows.Next() {
		var (
			row      SQLiteReconciliationRow
			itemType sql.NullString
		)
		if err := rows.Scan(
			&row.Path,
			&itemType,
			&row.ComparisonKind,
			&row.ReconciliationKind,
			&row.LocalMoveTarget,
			&row.LocalMoveSource,
			&row.RemoteMoveTarget,
			&row.RemoteMoveSource,
		); err != nil {
			return nil, fmt.Errorf("sync: scanning reconciliation state row: %w", err)
		}
		if itemType.Valid {
			parsed, err := ParseItemType(itemType.String)
			if err != nil {
				return nil, fmt.Errorf("sync: parsing reconciliation state item type %q: %w", itemType.String, err)
			}
			row.ItemType = parsed
		}
		results = append(results, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating reconciliation state rows: %w", err)
	}

	return results, nil
}

func sqlQueryForStateTables(query string, localStateTable string, remoteStateTable string) string {
	return strings.NewReplacer(
		"local_state", localStateTable,
		"remote_state", remoteStateTable,
	).Replace(query)
}
