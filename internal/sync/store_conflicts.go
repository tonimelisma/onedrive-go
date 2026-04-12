// Package sync persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - ListConflicts:           unresolved conflicts only
//   - ListAllConflicts:        all conflicts (resolved + unresolved)
//   - GetConflict:             lookup by UUID or path
//   - ResolveConflict:         mark conflict as resolved
//   - PruneResolvedConflicts:  delete old resolved conflicts
//   - UnresolvedConflictCount: count unresolved conflicts
//
// Related files:
//   - store.go:           SyncStore type definition and lifecycle
//   - store_baseline.go:  commitConflict (inserts conflict records within outcome commits)
package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"
)

// SQL statements for conflict operations.
const (
	sqlSelectConflictCols = `id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by`

	sqlSelectConflictRequestCols = `c.id, c.drive_id, c.item_id, c.path, c.conflict_type,
		c.detected_at, c.local_hash, c.remote_hash, c.local_mtime, c.remote_mtime,
		c.resolution, c.resolved_at, c.resolved_by,
		r.state, r.requested_resolution, r.requested_at, r.applying_at, r.last_error`

	sqlListConflicts = `SELECT ` + sqlSelectConflictCols + `
		FROM conflicts WHERE resolution = 'unresolved'
		ORDER BY detected_at`

	sqlListAllConflicts = `SELECT ` + sqlSelectConflictCols + `
		FROM conflicts
		ORDER BY detected_at DESC`

	sqlGetConflictByID = `SELECT ` + sqlSelectConflictCols + `
		FROM conflicts WHERE id = ?`

	sqlGetConflictByPath = `SELECT ` + sqlSelectConflictCols + `
		FROM conflicts WHERE path = ? AND resolution = 'unresolved'
		ORDER BY detected_at DESC LIMIT 1`

	sqlGetConflictRequestByID = `SELECT ` + sqlSelectConflictRequestCols + `
		FROM conflict_requests r
		JOIN conflicts c ON c.id = r.conflict_id
		WHERE c.id = ?`
)

type ConflictRequestStatus string

const (
	ConflictRequestQueued          ConflictRequestStatus = "queued"
	ConflictRequestAlreadyQueued   ConflictRequestStatus = "already_queued"
	ConflictRequestAlreadyApplying ConflictRequestStatus = "already_applying"
	ConflictRequestAlreadyResolved ConflictRequestStatus = "already_resolved"
)

type ConflictRequestResult struct {
	Status   ConflictRequestStatus
	Conflict ConflictRecord
}

// ListConflicts returns all unresolved conflicts ordered by detection time.
func (m *SyncStore) ListConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListConflicts)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) ordered
// by detection time descending. The product CLI consumes this through
// per-drive `status --history`.
func (m *SyncStore) ListAllConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListAllConflicts)
}

// queryConflicts executes a conflict query and scans the results.
func (m *SyncStore) queryConflicts(ctx context.Context, query string) ([]ConflictRecord, error) {
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []ConflictRecord

	for rows.Next() {
		c, err := scanConflictRow(rows)
		if err != nil {
			return nil, err
		}

		conflicts = append(conflicts, *c)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating conflict rows: %w", err)
	}

	return conflicts, nil
}

// GetConflict looks up a conflict by UUID or path. Tries ID first, falls
// back to path (most recent unresolved conflict for that path).
func (m *SyncStore) GetConflict(ctx context.Context, idOrPath string) (*ConflictRecord, error) {
	// Try by ID first.
	row := m.db.QueryRowContext(ctx, sqlGetConflictByID, idOrPath)

	c, err := scanConflictRowSingle(row)
	if err == nil {
		return c, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sync: getting conflict by ID %q: %w", idOrPath, err)
	}

	// Fall back to path lookup.
	row = m.db.QueryRowContext(ctx, sqlGetConflictByPath, idOrPath)

	c, err = scanConflictRowSingle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sync: conflict not found for %q", idOrPath)
	}

	if err != nil {
		return nil, fmt.Errorf("sync: getting conflict by path %q: %w", idOrPath, err)
	}

	return c, nil
}

// GetConflictRequest returns the durable request workflow for one conflict.
func (m *SyncStore) GetConflictRequest(ctx context.Context, id string) (*ConflictRequestRecord, error) {
	row := m.db.QueryRowContext(ctx, sqlGetConflictRequestByID, id)

	request, err := scanConflictRequestRowSingle(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("sync: conflict request %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("sync: getting conflict request %s: %w", id, err)
	}

	return request, nil
}

// ResolveConflict marks a conflict as resolved with the given resolution
// strategy. Only updates unresolved conflicts (idempotent-safe).
func (m *SyncStore) ResolveConflict(ctx context.Context, id, resolution string) (retErr error) {
	resolvedAt := m.nowFunc().UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: begin resolve conflict tx: %w", err)
	}
	defer func() {
		retErr = finalizeTxRollback(retErr, tx, "sync: rollback resolve conflict tx")
	}()

	result, err := tx.ExecContext(ctx,
		`UPDATE conflicts
		SET resolution = ?, resolved_at = ?, resolved_by = ?
		WHERE id = ? AND resolution = ?`,
		resolution,
		resolvedAt,
		ResolvedByUser,
		id,
		ResolutionUnresolved,
	)
	if err != nil {
		return fmt.Errorf("sync: resolving conflict %s: %w", id, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("sync: checking rows affected for conflict %s: %w", id, err)
	}
	if rows == 0 {
		return fmt.Errorf("sync: conflict %s not found or already resolved", id)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM conflict_requests WHERE conflict_id = ?`, id); err != nil {
		return fmt.Errorf("sync: clearing conflict request %s: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sync: commit resolve conflict %s: %w", id, err)
	}

	m.logger.Info("conflict resolved",
		slog.String("id", id),
		slog.String("resolution", resolution),
	)

	return nil
}

// RequestConflictResolution durably records the user's desired resolution.
// The engine, not the CLI, later claims and executes the request.
func (m *SyncStore) RequestConflictResolution(
	ctx context.Context,
	id string,
	resolution string,
) (result ConflictRequestResult, retErr error) {
	if !isQueueableConflictResolution(resolution) {
		return ConflictRequestResult{}, fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}

	now := m.nowFunc().UnixNano()

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return ConflictRequestResult{}, fmt.Errorf("sync: begin conflict request tx: %w", err)
	}
	defer func() {
		retErr = finalizeTxRollback(retErr, tx, "sync: rollback conflict request tx")
	}()

	row := tx.QueryRowContext(ctx, sqlGetConflictByID, id)
	conflict, err := scanConflictRowSingle(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConflictRequestResult{}, fmt.Errorf("sync: conflict %s not found", id)
		}
		return ConflictRequestResult{}, fmt.Errorf("sync: loading conflict %s: %w", id, err)
	}
	if conflict.Resolution != ResolutionUnresolved {
		return commitConflictRequestResult(
			tx,
			id,
			"sync: commit resolved conflict read",
			&ConflictRequestResult{
				Status:   ConflictRequestAlreadyResolved,
				Conflict: *conflict,
			},
		)
	}

	insertResult, err := tx.ExecContext(ctx,
		`INSERT INTO conflict_requests (
			conflict_id, requested_resolution, state, requested_at, applying_at, last_error
		) VALUES (?, ?, ?, ?, NULL, NULL)
		ON CONFLICT(conflict_id) DO NOTHING`,
		id,
		resolution,
		ConflictStateQueued,
		now,
	)
	if err != nil {
		return ConflictRequestResult{}, fmt.Errorf("sync: insert conflict request %s: %w", id, err)
	}
	inserted, err := insertResult.RowsAffected()
	if err != nil {
		return ConflictRequestResult{}, fmt.Errorf("sync: checking inserted conflict request %s: %w", id, err)
	}
	if inserted == 1 {
		return commitConflictRequestResult(
			tx,
			id,
			"sync: commit conflict resolution request",
			&ConflictRequestResult{
				Status:   ConflictRequestQueued,
				Conflict: *conflict,
			},
		)
	}

	request, err := getConflictRequestTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConflictRequestResult{}, fmt.Errorf("sync: conflict request %s disappeared during queueing", id)
		}
		return ConflictRequestResult{}, fmt.Errorf("sync: loading conflict request %s: %w", id, err)
	}

	return handleExistingConflictRequestTx(ctx, tx, conflict, request, id, resolution, now)
}

func handleExistingConflictRequestTx(
	ctx context.Context,
	tx sqlCommitTx,
	conflict *ConflictRecord,
	request *ConflictRequestRecord,
	id string,
	resolution string,
	now int64,
) (ConflictRequestResult, error) {
	switch request.State {
	case ConflictStateQueued:
		if request.RequestedResolution != resolution {
			return overwriteQueuedConflictRequestTx(ctx, tx, conflict, id, resolution, now)
		}
		return commitConflictRequestResult(
			tx,
			id,
			"sync: commit conflict resolution request read",
			&ConflictRequestResult{
				Status:   ConflictRequestAlreadyQueued,
				Conflict: *conflict,
			},
		)
	case ConflictStateApplying:
		return commitConflictRequestResult(
			tx,
			id,
			"sync: commit conflict applying read",
			&ConflictRequestResult{
				Status:   ConflictRequestAlreadyApplying,
				Conflict: *conflict,
			},
		)
	default:
		return ConflictRequestResult{}, fmt.Errorf("sync: conflict request %s has invalid workflow state %q", id, request.State)
	}
}

func overwriteQueuedConflictRequestTx(
	ctx context.Context,
	tx sqlCommitTx,
	conflict *ConflictRecord,
	id string,
	resolution string,
	now int64,
) (ConflictRequestResult, error) {
	updateResult, err := tx.ExecContext(ctx,
		`UPDATE conflict_requests
		SET requested_resolution = ?, requested_at = ?,
		    applying_at = NULL, last_error = NULL
		WHERE conflict_id = ? AND state = ?`,
		resolution,
		now,
		id,
		ConflictStateQueued,
	)
	if err != nil {
		return ConflictRequestResult{}, fmt.Errorf("sync: overwrite queued conflict resolution %s: %w", id, err)
	}
	updated, err := updateResult.RowsAffected()
	if err != nil {
		return ConflictRequestResult{}, fmt.Errorf("sync: checking overwritten conflict resolution %s: %w", id, err)
	}
	if updated != 1 {
		return ConflictRequestResult{}, fmt.Errorf("sync: conflict request %s changed while overwriting", id)
	}

	return commitConflictRequestResult(
		tx,
		id,
		"sync: commit overwritten conflict resolution",
		&ConflictRequestResult{
			Status:   ConflictRequestQueued,
			Conflict: *conflict,
		},
	)
}

func commitConflictRequestResult(
	tx sqlCommitTx,
	id string,
	contextMessage string,
	result *ConflictRequestResult,
) (ConflictRequestResult, error) {
	commitErr := tx.Commit()
	if commitErr != nil {
		return ConflictRequestResult{}, fmt.Errorf("%s %s: %w", contextMessage, id, commitErr)
	}

	return *result, nil
}

func isQueueableConflictResolution(resolution string) bool {
	switch resolution {
	case ResolutionKeepBoth, ResolutionKeepLocal, ResolutionKeepRemote:
		return true
	default:
		return false
	}
}

// ListRequestedConflictResolutions returns conflict resolution requests ready
// for engine-owned execution.
func (m *SyncStore) ListRequestedConflictResolutions(
	ctx context.Context,
	limit int,
) ([]ConflictRequestRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT `+sqlSelectConflictRequestCols+`
		FROM conflict_requests r
		JOIN conflicts c ON c.id = r.conflict_id
		WHERE r.state = ?
		ORDER BY r.requested_at, c.detected_at
		LIMIT ?`,
		ConflictStateQueued,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: listing requested conflict resolutions: %w", err)
	}
	defer rows.Close()

	var conflicts []ConflictRequestRecord
	for rows.Next() {
		c, err := scanConflictRequestRow(rows)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating requested conflict resolutions: %w", err)
	}

	return conflicts, nil
}

// ClaimConflictResolution moves a queued request into applying state so only
// one engine instance may execute its side effects.
func (m *SyncStore) ClaimConflictResolution(
	ctx context.Context,
	id string,
) (*ConflictRequestRecord, bool, error) {
	now := m.nowFunc().UnixNano()
	result, err := m.db.ExecContext(ctx,
		`UPDATE conflict_requests
		SET state = ?, applying_at = ?, last_error = NULL
		WHERE conflict_id = ? AND state = ?`,
		ConflictStateApplying,
		now,
		id,
		ConflictStateQueued,
	)
	if err != nil {
		return nil, false, fmt.Errorf("sync: claiming conflict resolution %s: %w", id, err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("sync: checking claimed conflict resolution %s: %w", id, err)
	}
	if rows == 0 {
		return nil, false, nil
	}

	conflict, err := m.GetConflictRequest(ctx, id)
	if err != nil {
		return nil, false, err
	}
	conflict.State = ConflictStateApplying
	conflict.ApplyingAt = now

	return conflict, true, nil
}

func (m *SyncStore) MarkConflictResolutionFailed(ctx context.Context, id string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	_, err := m.db.ExecContext(ctx,
		`UPDATE conflict_requests
		SET state = ?, last_error = ?, applying_at = NULL
		WHERE conflict_id = ? AND state = ?`,
		ConflictStateQueued,
		message,
		id,
		ConflictStateApplying,
	)
	if err != nil {
		return fmt.Errorf("sync: marking conflict resolution %s failed: %w", id, err)
	}

	return nil
}

func (m *SyncStore) ResetStaleResolvingConflicts(ctx context.Context, olderThan time.Time) (int, error) {
	result, err := m.db.ExecContext(ctx,
		`UPDATE conflict_requests
		SET state = ?, applying_at = NULL
		WHERE state = ? AND applying_at IS NOT NULL AND applying_at < ?`,
		ConflictStateQueued,
		ConflictStateApplying,
		olderThan.UnixNano(),
	)
	if err != nil {
		return 0, fmt.Errorf("sync: resetting stale applying conflicts: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: checking stale applying conflict count: %w", err)
	}

	return int(rows), nil
}

// PruneResolvedConflicts deletes resolved conflicts whose detection time is
// older than the given retention duration. Unresolved conflicts are never
// pruned. Returns the number of deleted rows (B-087).
func (m *SyncStore) PruneResolvedConflicts(ctx context.Context, retention time.Duration) (int, error) {
	cutoff := m.nowFunc().Add(-retention).UnixNano()

	result, err := m.db.ExecContext(ctx,
		`DELETE FROM conflicts WHERE resolution != 'unresolved' AND detected_at < ?`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("sync: pruning resolved conflicts: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: checking pruned conflict count: %w", err)
	}

	if rows > 0 {
		m.logger.Info("pruned resolved conflicts",
			slog.Int64("pruned", rows),
			slog.Duration("retention", retention),
		)
	}

	return int(rows), nil
}

// UnresolvedConflictCount returns the number of unresolved conflicts.
func (m *SyncStore) UnresolvedConflictCount(ctx context.Context) (int, error) {
	var count int

	err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM conflicts WHERE resolution = 'unresolved'`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("unresolved conflict count: %w", err)
	}

	return count, nil
}

// ---------------------------------------------------------------------------
// Conflict row scanning
// ---------------------------------------------------------------------------

// conflictScanner abstracts the Scan method shared by *sql.Rows and *sql.Row,
// allowing a single scan implementation for both multi-row and single-row
// conflict queries (B-149).
type conflictScanner interface {
	Scan(dest ...any) error
}

// scanConflict scans a single conflict fact row from any scanner (*sql.Rows or
// *sql.Row), handling nullable columns.
func scanConflict(s conflictScanner) (*ConflictRecord, error) {
	var (
		c           ConflictRecord
		itemID      sql.NullString
		localHash   sql.NullString
		remoteHash  sql.NullString
		localMtime  sql.NullInt64
		remoteMtime sql.NullInt64
		resolvedAt  sql.NullInt64
		resolvedBy  sql.NullString
	)

	err := s.Scan(
		&c.ID, &c.DriveID, &itemID, &c.Path, &c.ConflictType,
		&c.DetectedAt, &localHash, &remoteHash, &localMtime, &remoteMtime,
		&c.Resolution, &resolvedAt, &resolvedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict row: %w", err)
	}

	c.ItemID = itemID.String
	c.Name = path.Base(c.Path) // derived for display convenience (B-071)
	c.LocalHash = localHash.String
	c.RemoteHash = remoteHash.String
	c.ResolvedBy = resolvedBy.String

	if localMtime.Valid {
		c.LocalMtime = localMtime.Int64
	}

	if remoteMtime.Valid {
		c.RemoteMtime = remoteMtime.Int64
	}

	if resolvedAt.Valid {
		c.ResolvedAt = resolvedAt.Int64
	}

	return &c, nil
}

func scanConflictRequest(s conflictScanner) (*ConflictRequestRecord, error) {
	var (
		record      ConflictRequestRecord
		itemID      sql.NullString
		localHash   sql.NullString
		remoteHash  sql.NullString
		localMtime  sql.NullInt64
		remoteMtime sql.NullInt64
		resolvedAt  sql.NullInt64
		resolvedBy  sql.NullString
		requestedAt sql.NullInt64
		applyingAt  sql.NullInt64
		lastErr     sql.NullString
	)

	err := s.Scan(
		&record.ID, &record.DriveID, &itemID, &record.Path, &record.ConflictType,
		&record.DetectedAt, &localHash, &remoteHash, &localMtime, &remoteMtime,
		&record.Resolution, &resolvedAt, &resolvedBy,
		&record.State, &record.RequestedResolution, &requestedAt, &applyingAt, &lastErr,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict request row: %w", err)
	}

	record.ItemID = itemID.String
	record.Name = path.Base(record.Path)
	record.LocalHash = localHash.String
	record.RemoteHash = remoteHash.String
	record.LastError = lastErr.String
	record.ResolvedBy = resolvedBy.String

	if localMtime.Valid {
		record.LocalMtime = localMtime.Int64
	}
	if remoteMtime.Valid {
		record.RemoteMtime = remoteMtime.Int64
	}
	if resolvedAt.Valid {
		record.ResolvedAt = resolvedAt.Int64
	}
	if requestedAt.Valid {
		record.RequestedAt = requestedAt.Int64
	}
	if applyingAt.Valid {
		record.ApplyingAt = applyingAt.Int64
	}

	return &record, nil
}

// scanConflictRow scans a conflict from a multi-row result set. Delegates
// to scanConflict via the conflictScanner interface (B-149).
func scanConflictRow(rows *sql.Rows) (*ConflictRecord, error) {
	c, err := scanConflict(rows)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict row: %w", err)
	}

	return c, nil
}

// scanConflictRowSingle scans a conflict from a single-row result.
// Returns sql.ErrNoRows transparently for callers that need it (B-149).
func scanConflictRowSingle(row *sql.Row) (*ConflictRecord, error) {
	return scanConflict(row)
}

func scanConflictRequestRow(rows *sql.Rows) (*ConflictRequestRecord, error) {
	record, err := scanConflictRequest(rows)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict request row: %w", err)
	}

	return record, nil
}

func scanConflictRequestRowSingle(row *sql.Row) (*ConflictRequestRecord, error) {
	return scanConflictRequest(row)
}

func getConflictRequestTx(
	ctx context.Context,
	tx sqlTxRunner,
	id string,
) (*ConflictRequestRecord, error) {
	row := tx.QueryRowContext(ctx, sqlGetConflictRequestByID, id)
	return scanConflictRequestRowSingle(row)
}
