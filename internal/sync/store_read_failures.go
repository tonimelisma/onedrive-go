// Package sync persists sync baseline, observation, conflict, failure, and scope state.
//
// sync_failures read paths stay separate from mutation paths so query-heavy
// status/retry helpers do not hide behind write-prefixed filenames.
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ListSyncFailures returns all sync_failures rows ordered by last_seen_at DESC.
func (m *SyncStore) ListSyncFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ListActionableFailures returns sync_failures rows where category is actionable.
// Used by the issues command to show user-actionable file issues.
func (m *SyncStore) ListActionableFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'actionable' ORDER BY last_seen_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing actionable sync failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// ListRemoteBlockedFailures returns the held rows that define the derived
// remote read-only scopes. These rows remain transient because they represent
// blocked work, not independent actionable failures.
func (m *SyncStore) ListRemoteBlockedFailures(ctx context.Context) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE failure_role = ?
			AND (scope_key LIKE 'perm:remote-write:%' OR scope_key LIKE 'perm:remote:%')
		ORDER BY last_seen_at DESC`,
		FailureRoleHeld,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: listing remote blocked failures: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// PendingRetrySummary returns aggregated counts of transient failures
// grouped by scope_key, with the earliest next_retry_at per group.
func (m *SyncStore) PendingRetrySummary(ctx context.Context) ([]PendingRetryGroup, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT COALESCE(scope_key, ''), COUNT(*), MIN(next_retry_at)
		 FROM sync_failures
		 WHERE category = 'transient' AND next_retry_at > 0
		 GROUP BY scope_key
		 ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, fmt.Errorf("sync: querying pending retry summary: %w", err)
	}
	defer rows.Close()

	var result []PendingRetryGroup

	for rows.Next() {
		var g PendingRetryGroup
		var minNano int64
		var wireScopeKey string

		if scanErr := rows.Scan(&wireScopeKey, &g.Count, &minNano); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning pending retry group: %w", scanErr)
		}

		g.ScopeKey = ParseScopeKey(wireScopeKey)
		if minNano > 0 {
			g.EarliestNext = time.Unix(0, minNano)
		}

		result = append(result, g)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating pending retry groups: %w", err)
	}

	return result, nil
}

// ListSyncFailuresForRetry returns transient sync_failures rows whose
// next_retry_at has expired (ready for retry).
func (m *SyncStore) ListSyncFailuresForRetry(ctx context.Context, now time.Time) ([]SyncFailureRow, error) {
	nowNano := now.UnixNano()
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at <= ?`,
		nowNano)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures for retry: %w", err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// EarliestSyncFailureRetryAt returns the minimum future next_retry_at across
// transient sync_failures. Returns zero time if none exist.
func (m *SyncStore) EarliestSyncFailureRetryAt(ctx context.Context, now time.Time) (time.Time, error) {
	nowNano := now.UnixNano()

	var minNano *int64
	err := m.db.QueryRowContext(ctx,
		`SELECT MIN(next_retry_at) FROM sync_failures
		WHERE category = 'transient'
			AND next_retry_at IS NOT NULL
			AND next_retry_at > ?`,
		nowNano,
	).Scan(&minNano)
	if err != nil {
		return time.Time{}, fmt.Errorf("sync: querying earliest sync failure retry: %w", err)
	}

	if minNano == nil {
		return time.Time{}, nil
	}

	return time.Unix(0, *minNano), nil
}

// SyncFailureCount returns the number of transient sync_failures rows.
func (m *SyncStore) SyncFailureCount(ctx context.Context) (int, error) {
	var count int

	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sync_failures WHERE category = 'transient'`,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("sync: counting sync failures: %w", err)
	}

	return count, nil
}

// ListSyncFailuresByIssueType returns all sync_failures rows with the given issue_type.
func (m *SyncStore) ListSyncFailuresByIssueType(ctx context.Context, issueType string) ([]SyncFailureRow, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE issue_type = ? ORDER BY last_seen_at DESC`, issueType)
	if err != nil {
		return nil, fmt.Errorf("sync: listing sync failures by type %s: %w", issueType, err)
	}
	defer rows.Close()

	return scanSyncFailureRows(rows)
}

// PickTrialCandidate returns the oldest scope-blocked failure for the given scope key.
func (m *SyncStore) PickTrialCandidate(
	ctx context.Context,
	scopeKey ScopeKey,
) (*SyncFailureRow, bool, error) {
	wire := scopeKey.String()

	row := m.db.QueryRowContext(ctx,
		`SELECT `+sqlSelectSyncFailureCols+` FROM sync_failures
		WHERE scope_key = ? AND failure_role = ? AND next_retry_at IS NULL
		ORDER BY first_seen_at ASC
		LIMIT 1`,
		wire, FailureRoleHeld,
	)

	var r SyncFailureRow
	err := scanSyncFailureRow(row, &r)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}

		return nil, false, fmt.Errorf("sync: picking trial candidate for scope %s: %w", wire, err)
	}

	return &r, true, nil
}

type syncFailureScanner interface {
	Scan(dest ...any) error
}

func scanSyncFailureRow(scanner syncFailureScanner, row *SyncFailureRow) error {
	if row == nil {
		return fmt.Errorf("sync: scanning sync failure row: nil destination")
	}

	var wireScopeKey string
	if err := scanner.Scan(
		&row.Path, &row.DriveID, &row.Direction, &row.ActionType, &row.Role, &row.Category,
		&row.IssueType, &row.ItemID,
		&row.FailureCount, &row.NextRetryAt,
		&row.LastError, &row.HTTPStatus,
		&row.FirstSeenAt, &row.LastSeenAt,
		&row.FileSize, &row.LocalHash,
		&wireScopeKey,
	); err != nil {
		return fmt.Errorf("sync: scanning sync failure row: %w", err)
	}

	row.ScopeKey = ParseScopeKey(wireScopeKey)
	return nil
}

// scanSyncFailureRows scans multiple sync_failures rows from a query result.
func scanSyncFailureRows(rows *sql.Rows) ([]SyncFailureRow, error) {
	var result []SyncFailureRow

	for rows.Next() {
		var r SyncFailureRow
		if scanErr := scanSyncFailureRow(rows, &r); scanErr != nil {
			return nil, fmt.Errorf("sync: scanning sync failure row: %w", scanErr)
		}
		result = append(result, r)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating sync failure rows: %w", err)
	}

	return result, nil
}
