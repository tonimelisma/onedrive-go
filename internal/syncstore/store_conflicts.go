// store_conflicts.go — Conflict management for SyncStore.
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
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE resolution = 'unresolved'
		ORDER BY detected_at`

	sqlListAllConflicts = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts
		ORDER BY detected_at DESC`

	sqlGetConflictByID = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE id = ?`

	sqlGetConflictByPath = `SELECT id, drive_id, item_id, path, conflict_type,
		detected_at, local_hash, remote_hash, local_mtime, remote_mtime,
		resolution, resolved_at, resolved_by
		FROM conflicts WHERE path = ? AND resolution = 'unresolved'
		ORDER BY detected_at DESC LIMIT 1`

	sqlResolveConflict = `UPDATE conflicts
		SET resolution = ?, resolved_at = ?, resolved_by = 'user'
		WHERE id = ? AND resolution = 'unresolved'`
)

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

	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("sync: getting conflict by ID %q: %w", idOrPath, err)
	}

	// Fall back to path lookup.
	row = m.db.QueryRowContext(ctx, sqlGetConflictByPath, idOrPath)

	c, err = scanConflictRowSingle(row)
	if err == sql.ErrNoRows {
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
		resolvedAt  sql.NullInt64
		resolvedBy  sql.NullString
	)

	err := s.Scan(
		&c.ID, &c.DriveID, &itemID, &c.Path, &c.ConflictType,
		&c.DetectedAt, &localHash, &remoteHash, &localMtime, &remoteMtime,
		&c.Resolution, &resolvedAt, &resolvedBy,
	)
	if err != nil {
		return nil, err //nolint:wrapcheck // callers wrap with context
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
