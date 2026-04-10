// Package syncstore persists sync baseline, observation, conflict, failure, and scope state.
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
package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// SQL statements for conflict operations.
const (
	sqlListConflicts = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, state, requested_resolution, requested_at, resolving_at,
		resolution_error, resolved_at, resolved_by
		FROM conflicts WHERE state != 'resolved' AND resolution = 'unresolved'
		ORDER BY detected_at`

	sqlListAllConflicts = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, state, requested_resolution, requested_at, resolving_at,
		resolution_error, resolved_at, resolved_by
		FROM conflicts
		ORDER BY detected_at DESC`

	sqlGetConflictByID = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, state, requested_resolution, requested_at, resolving_at,
		resolution_error, resolved_at, resolved_by
		FROM conflicts WHERE id = ?`

	sqlGetConflictByPath = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, state, requested_resolution, requested_at, resolving_at,
		resolution_error, resolved_at, resolved_by
		FROM conflicts WHERE path = ? AND state != 'resolved' AND resolution = 'unresolved'
		ORDER BY detected_at DESC LIMIT 1`

	sqlResolveConflict = `UPDATE conflicts
		SET resolution = ?, state = 'resolved', resolved_at = ?, resolved_by = 'user',
		    requested_resolution = NULL, requested_at = NULL, resolving_at = NULL,
		    resolution_error = NULL
		WHERE id = ? AND state != 'resolved'`
)

type ConflictRequestStatus string

const (
	ConflictRequestQueued            ConflictRequestStatus = "queued"
	ConflictRequestAlreadyQueued     ConflictRequestStatus = "already_queued"
	ConflictRequestAlreadyResolving  ConflictRequestStatus = "already_resolving"
	ConflictRequestAlreadyResolved   ConflictRequestStatus = "already_resolved"
	ConflictRequestDifferentStrategy ConflictRequestStatus = "different_strategy"
)

type ConflictRequestResult struct {
	Status   ConflictRequestStatus
	Conflict synctypes.ConflictRecord
}

// ListConflicts returns all unresolved conflicts ordered by detection time.
func (m *SyncStore) ListConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListConflicts)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) ordered
// by detection time descending. Used by 'conflicts --history'.
func (m *SyncStore) ListAllConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return m.queryConflicts(ctx, sqlListAllConflicts)
}

// queryConflicts executes a conflict query and scans the results.
func (m *SyncStore) queryConflicts(ctx context.Context, query string) ([]synctypes.ConflictRecord, error) {
	rows, err := m.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("sync: querying conflicts: %w", err)
	}
	defer rows.Close()

	var conflicts []synctypes.ConflictRecord

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
func (m *SyncStore) GetConflict(ctx context.Context, idOrPath string) (*synctypes.ConflictRecord, error) {
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

// ResolveConflict marks a conflict as resolved with the given resolution
// strategy. Only updates unresolved conflicts (idempotent-safe).
func (m *SyncStore) ResolveConflict(ctx context.Context, id, resolution string) error {
	resolvedAt := m.nowFunc().UnixNano()

	result, err := m.db.ExecContext(ctx, sqlResolveConflict, resolution, resolvedAt, id)
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

	tx, err := m.db.BeginTx(ctx, nil)
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

	switch conflict.State {
	case synctypes.ConflictStateUnresolved, synctypes.ConflictStateResolveFailed:
		if _, err := tx.ExecContext(ctx,
			`UPDATE conflicts
			SET state = ?, requested_resolution = ?, requested_at = ?,
			    resolving_at = NULL, resolution_error = NULL
			WHERE id = ? AND state IN (?, ?)`,
			synctypes.ConflictStateResolutionRequested,
			resolution,
			now,
			id,
			synctypes.ConflictStateUnresolved,
			synctypes.ConflictStateResolveFailed,
		); err != nil {
			return ConflictRequestResult{}, fmt.Errorf("sync: queue conflict resolution %s: %w", id, err)
		}
		conflict.State = synctypes.ConflictStateResolutionRequested
		conflict.RequestedResolution = resolution
		conflict.RequestedAt = now
		conflict.ResolvingAt = 0
		conflict.ResolutionError = ""
		if err := tx.Commit(); err != nil {
			return ConflictRequestResult{}, fmt.Errorf("sync: commit conflict resolution request %s: %w", id, err)
		}
		return ConflictRequestResult{Status: ConflictRequestQueued, Conflict: *conflict}, nil

	case synctypes.ConflictStateResolutionRequested:
		status := ConflictRequestAlreadyQueued
		if conflict.RequestedResolution != resolution {
			status = ConflictRequestDifferentStrategy
		}
		if err := tx.Commit(); err != nil {
			return ConflictRequestResult{}, fmt.Errorf("sync: commit conflict resolution request read %s: %w", id, err)
		}
		return ConflictRequestResult{Status: status, Conflict: *conflict}, nil

	case synctypes.ConflictStateResolving:
		if err := tx.Commit(); err != nil {
			return ConflictRequestResult{}, fmt.Errorf("sync: commit conflict resolving read %s: %w", id, err)
		}
		return ConflictRequestResult{Status: ConflictRequestAlreadyResolving, Conflict: *conflict}, nil

	case synctypes.ConflictStateResolved:
		if err := tx.Commit(); err != nil {
			return ConflictRequestResult{}, fmt.Errorf("sync: commit resolved conflict read %s: %w", id, err)
		}
		return ConflictRequestResult{Status: ConflictRequestAlreadyResolved, Conflict: *conflict}, nil

	default:
		return ConflictRequestResult{}, fmt.Errorf("sync: conflict %s is not queueable in state %q", id, conflict.State)
	}
}

func isQueueableConflictResolution(resolution string) bool {
	switch resolution {
	case synctypes.ResolutionKeepBoth, synctypes.ResolutionKeepLocal, synctypes.ResolutionKeepRemote:
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
) ([]synctypes.ConflictRecord, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := m.db.QueryContext(ctx,
		`SELECT id, drive_id, item_id, path, conflict_type,
			detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
			resolution, state, requested_resolution, requested_at, resolving_at,
			resolution_error, resolved_at, resolved_by
		FROM conflicts
		WHERE state = ?
		ORDER BY requested_at, detected_at
		LIMIT ?`,
		synctypes.ConflictStateResolutionRequested,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: listing requested conflict resolutions: %w", err)
	}
	defer rows.Close()

	var conflicts []synctypes.ConflictRecord
	for rows.Next() {
		c, err := scanConflictRow(rows)
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

// ClaimConflictResolution moves a queued request into resolving state so only
// one engine instance may execute its side effects.
func (m *SyncStore) ClaimConflictResolution(
	ctx context.Context,
	id string,
) (*synctypes.ConflictRecord, bool, error) {
	now := m.nowFunc().UnixNano()
	result, err := m.db.ExecContext(ctx,
		`UPDATE conflicts
		SET state = ?, resolving_at = ?, resolution_error = NULL
		WHERE id = ? AND state = ?`,
		synctypes.ConflictStateResolving,
		now,
		id,
		synctypes.ConflictStateResolutionRequested,
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

	conflict, err := m.GetConflict(ctx, id)
	if err != nil {
		return nil, false, err
	}
	conflict.State = synctypes.ConflictStateResolving
	conflict.ResolvingAt = now

	return conflict, true, nil
}

func (m *SyncStore) MarkConflictResolutionFailed(ctx context.Context, id string, cause error) error {
	message := ""
	if cause != nil {
		message = cause.Error()
	}

	_, err := m.db.ExecContext(ctx,
		`UPDATE conflicts
		SET state = ?, resolution_error = ?, resolving_at = NULL
		WHERE id = ? AND state = ?`,
		synctypes.ConflictStateResolveFailed,
		message,
		id,
		synctypes.ConflictStateResolving,
	)
	if err != nil {
		return fmt.Errorf("sync: marking conflict resolution %s failed: %w", id, err)
	}

	return nil
}

func (m *SyncStore) ResetStaleResolvingConflicts(ctx context.Context, olderThan time.Time) (int, error) {
	result, err := m.db.ExecContext(ctx,
		`UPDATE conflicts
		SET state = ?, resolving_at = NULL
		WHERE state = ? AND resolving_at IS NOT NULL AND resolving_at < ?`,
		synctypes.ConflictStateResolutionRequested,
		synctypes.ConflictStateResolving,
		olderThan.UnixNano(),
	)
	if err != nil {
		return 0, fmt.Errorf("sync: resetting stale resolving conflicts: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sync: checking stale resolving conflict count: %w", err)
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
		`SELECT COUNT(*) FROM conflicts WHERE state != 'resolved' AND resolution = 'unresolved'`).Scan(&count)
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

// scanConflict scans a single conflict row from any scanner (*sql.Rows or
// *sql.Row), handling nullable columns. The `history` column is intentionally
// excluded — it is dormant/unused (B-160).
func scanConflict(s conflictScanner) (*synctypes.ConflictRecord, error) {
	var (
		c           synctypes.ConflictRecord
		itemID      sql.NullString
		localHash   sql.NullString
		remoteHash  sql.NullString
		localMtime  sql.NullInt64
		remoteMtime sql.NullInt64
		requested   sql.NullString
		requestedAt sql.NullInt64
		resolvingAt sql.NullInt64
		resolveErr  sql.NullString
		resolvedAt  sql.NullInt64
		resolvedBy  sql.NullString
	)

	err := s.Scan(
		&c.ID, &c.DriveID, &itemID, &c.Path, &c.ConflictType,
		&c.DetectedAt, &localHash, &remoteHash, &localMtime, &remoteMtime,
		&c.Resolution, &c.State, &requested, &requestedAt, &resolvingAt,
		&resolveErr, &resolvedAt, &resolvedBy,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict row: %w", err)
	}

	c.ItemID = itemID.String
	c.Name = path.Base(c.Path) // derived for display convenience (B-071)
	c.LocalHash = localHash.String
	c.RemoteHash = remoteHash.String
	c.RequestedResolution = requested.String
	c.ResolutionError = resolveErr.String
	c.ResolvedBy = resolvedBy.String

	if localMtime.Valid {
		c.LocalMtime = localMtime.Int64
	}

	if remoteMtime.Valid {
		c.RemoteMtime = remoteMtime.Int64
	}

	if requestedAt.Valid {
		c.RequestedAt = requestedAt.Int64
	}

	if resolvingAt.Valid {
		c.ResolvingAt = resolvingAt.Int64
	}

	if resolvedAt.Valid {
		c.ResolvedAt = resolvedAt.Int64
	}

	return &c, nil
}

// scanConflictRow scans a conflict from a multi-row result set. Delegates
// to scanConflict via the conflictScanner interface (B-149).
func scanConflictRow(rows *sql.Rows) (*synctypes.ConflictRecord, error) {
	c, err := scanConflict(rows)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning conflict row: %w", err)
	}

	return c, nil
}

// scanConflictRowSingle scans a conflict from a single-row result.
// Returns sql.ErrNoRows transparently for callers that need it (B-149).
func scanConflictRowSingle(row *sql.Row) (*synctypes.ConflictRecord, error) {
	return scanConflict(row)
}
