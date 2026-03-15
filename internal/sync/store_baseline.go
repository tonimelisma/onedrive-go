// store_baseline.go — Baseline CRUD, delta tokens, and outcome commits for SyncStore.
//
// Contents:
//   - Load:                    read baseline table into memory
//   - scanBaselineRow:         scan single baseline row with nullable handling
//   - GetDeltaToken:           retrieve saved delta token for drive/scope
//   - CommitDeltaToken:        persist delta token in own transaction
//   - DeleteDeltaToken:        remove delta token for drive/scope
//   - saveDeltaToken:          persist delta token within existing transaction
//   - CommitOutcome:           atomically apply outcome to baseline + remote_state
//   - applySingleOutcome:      dispatch outcome to appropriate DB helper
//   - updateBaselineCache:     patch in-memory baseline after DB commit
//   - outcomeToEntry:          convert Outcome to BaselineEntry
//   - commitUpsert:            INSERT/UPDATE baseline for download/upload/create
//   - commitDelete:            DELETE baseline for delete/cleanup
//   - commitMove:              UPDATE path for move outcomes
//   - commitConflict:          INSERT conflict record
//   - updateRemoteStateOnOutcome: update remote_state in same transaction
//   - CheckCacheConsistency:   verify in-memory cache matches DB
//
// Related files:
//   - store.go:             SyncStore type definition and lifecycle
//   - store_observation.go: CommitObservation calls saveDeltaToken
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT drive_id, item_id, path, parent_id, item_type,
		local_hash, remote_hash, size, mtime, synced_at, etag
		FROM baseline`

	//nolint:gosec // G101: "token" is a delta cursor, not credentials.
	sqlGetDeltaToken = `SELECT token FROM delta_tokens WHERE drive_id = ? AND scope_id = ?`

	sqlUpsertBaseline = `INSERT INTO baseline
		(drive_id, item_id, path, parent_id, item_type, local_hash, remote_hash,
		 size, mtime, synced_at, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, item_id) DO UPDATE SET
		 path = excluded.path,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 size = excluded.size,
		 mtime = excluded.mtime,
		 synced_at = excluded.synced_at,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`

	sqlInsertConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime,
		 resolution, resolved_at, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpsertDeltaToken = `INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, token, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, scope_id) DO UPDATE SET
		 scope_drive = excluded.scope_drive,
		 token = excluded.token,
		 updated_at = excluded.updated_at`
)

// Load reads the entire baseline table into memory, populating ByPath and
// ByID maps. The result is cached on the manager — subsequent calls return
// the cached baseline without querying the database. The cache is kept
// consistent by CommitOutcome(), which incrementally patches the in-memory
// maps via updateBaselineCache() after each transaction. This is safe
// because SyncStore exclusively owns the database (sole-writer
// pattern with SetMaxOpenConns(1)).
func (m *SyncStore) Load(ctx context.Context) (*Baseline, error) {
	m.baselineMu.Lock()
	defer m.baselineMu.Unlock()

	if m.baseline != nil {
		return m.baseline, nil
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}
	defer rows.Close()

	b := &Baseline{
		byPath:     make(map[string]*BaselineEntry),
		byID:       make(map[driveid.ItemKey]*BaselineEntry),
		byDirLower: make(map[dirLowerKey][]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows)
		if err != nil {
			return nil, err
		}

		b.byPath[entry.Path] = entry
		b.byID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry

		dlk := dirLowerKeyFromPath(entry.Path)
		b.byDirLower[dlk] = append(b.byDirLower[dlk], entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating baseline rows: %w", err)
	}

	m.baseline = b
	m.logger.Debug("baseline loaded", slog.Int("entries", len(b.byPath)))

	return b, nil
}

// scanBaselineRow scans a single row from the baseline table, handling
// nullable columns with sql.Null* types.
func scanBaselineRow(rows *sql.Rows) (*BaselineEntry, error) {
	var (
		e          BaselineEntry
		itemType   string
		parentID   sql.NullString
		localHash  sql.NullString
		remoteHash sql.NullString
		size       sql.NullInt64
		mtime      sql.NullInt64
		etag       sql.NullString
	)

	err := rows.Scan(
		&e.DriveID, &e.ItemID, &e.Path, &parentID, &itemType,
		&localHash, &remoteHash, &size, &mtime, &e.SyncedAt, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning baseline row: %w", err)
	}

	parsed, err := ParseItemType(itemType)
	if err != nil {
		return nil, err
	}

	e.ItemType = parsed
	e.ParentID = parentID.String
	e.LocalHash = localHash.String
	e.RemoteHash = remoteHash.String
	e.ETag = etag.String

	if size.Valid {
		e.Size = size.Int64
	}

	if mtime.Valid {
		e.Mtime = mtime.Int64
	}

	return &e, nil
}

// GetDeltaToken returns the saved delta token for a drive and scope, or empty
// string if no token has been saved yet. Use scopeID="" for the primary
// drive-level delta; use a remoteItem.id for shortcut-scoped deltas.
func (m *SyncStore) GetDeltaToken(ctx context.Context, driveID, scopeID string) (string, error) {
	var token string

	err := m.db.QueryRowContext(ctx, sqlGetDeltaToken, driveID, scopeID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: getting delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return token, nil
}

// CommitOutcome atomically applies a single outcome to the baseline in a
// SQLite transaction. After the DB write, the in-memory baseline cache is
// updated incrementally (Put or Delete).
func (m *SyncStore) CommitOutcome(ctx context.Context, outcome *Outcome) error {
	if !outcome.Success {
		return nil
	}

	// Ensure baseline is loaded so we can update the in-memory cache.
	if m.baseline == nil {
		if _, loadErr := m.Load(ctx); loadErr != nil {
			return fmt.Errorf("sync: loading baseline before commit outcome: %w", loadErr)
		}
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning commit outcome transaction: %w", err)
	}
	defer tx.Rollback()

	syncedAt := m.nowFunc().UnixNano()

	if applyErr := applySingleOutcome(ctx, tx, outcome, syncedAt); applyErr != nil {
		return applyErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("sync: committing outcome transaction: %w", commitErr)
	}

	// Update in-memory baseline cache incrementally.
	m.updateBaselineCache(outcome, syncedAt)

	return nil
}

// applySingleOutcome dispatches a single outcome to the appropriate DB helper.
func applySingleOutcome(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	var err error

	switch o.Action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		err = commitUpsert(ctx, tx, o, syncedAt)
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		err = commitDelete(ctx, tx, o.Path)
	case ActionLocalMove, ActionRemoteMove:
		err = commitMove(ctx, tx, o, syncedAt)
	case ActionConflict:
		err = commitConflict(ctx, tx, o, syncedAt)
	}

	if err != nil {
		return err
	}

	// Update remote_state in the same transaction.
	return updateRemoteStateOnOutcome(ctx, tx, o)
}

// updateBaselineCache applies a single outcome to the in-memory baseline,
// keeping the cache consistent without a full DB reload.
func (m *SyncStore) updateBaselineCache(o *Outcome, syncedAt int64) {
	switch o.Action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		m.baseline.Put(outcomeToEntry(o, syncedAt))
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		m.baseline.Delete(o.Path)
	case ActionLocalMove, ActionRemoteMove:
		m.baseline.Delete(o.OldPath)
		m.baseline.Put(outcomeToEntry(o, syncedAt))
	case ActionConflict:
		if o.ResolvedBy == ResolvedByAuto {
			m.baseline.Put(outcomeToEntry(o, syncedAt))
		} else if o.ConflictType == ConflictEditDelete {
			// Unresolved edit-delete conflict from local delete: the original file
			// is gone (renamed to conflict copy), so remove the baseline entry.
			m.baseline.Delete(o.Path)
		}
	}
}

// outcomeToEntry converts an Outcome into a BaselineEntry for cache update.
func outcomeToEntry(o *Outcome, syncedAt int64) *BaselineEntry {
	return &BaselineEntry{
		Path:       o.Path,
		DriveID:    o.DriveID,
		ItemID:     o.ItemID,
		ParentID:   o.ParentID,
		ItemType:   o.ItemType,
		LocalHash:  o.LocalHash,
		RemoteHash: o.RemoteHash,
		Size:       o.Size,
		Mtime:      o.Mtime,
		SyncedAt:   syncedAt,
		ETag:       o.ETag,
	}
}

// CommitDeltaToken persists a delta token in its own transaction, separate
// from baseline updates. Used after all actions in a pass complete.
// Use scopeID="" and scopeDrive=driveID for the primary drive-level delta.
// For shortcut-scoped deltas, scopeID=remoteItem.id and scopeDrive=remoteItem.driveId.
func (m *SyncStore) CommitDeltaToken(ctx context.Context, token, driveID, scopeID, scopeDrive string) error {
	if token == "" {
		return nil
	}

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync: beginning delta token transaction: %w", err)
	}
	defer tx.Rollback()

	updatedAt := m.nowFunc().UnixNano()
	if saveErr := m.saveDeltaToken(ctx, tx, driveID, scopeID, scopeDrive, token, updatedAt); saveErr != nil {
		return saveErr
	}

	if commitErr := tx.Commit(); commitErr != nil {
		return fmt.Errorf("sync: committing delta token transaction: %w", commitErr)
	}

	m.logger.Debug("delta token committed",
		slog.String("drive_id", driveID),
		slog.String("scope_id", scopeID),
	)

	return nil
}

// DeleteDeltaToken removes a delta token for a specific drive and scope.
// Used when a shortcut is removed to clean up its scoped delta token.
func (m *SyncStore) DeleteDeltaToken(ctx context.Context, driveID, scopeID string) error {
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM delta_tokens WHERE drive_id = ? AND scope_id = ?`,
		driveID, scopeID)
	if err != nil {
		return fmt.Errorf("sync: deleting delta token for drive %s scope %s: %w", driveID, scopeID, err)
	}

	return nil
}

// saveDeltaToken persists the delta token in the same transaction as
// baseline updates.
func (m *SyncStore) saveDeltaToken(
	ctx context.Context, tx *sql.Tx, driveID, scopeID, scopeDrive, token string, updatedAt int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpsertDeltaToken, driveID, scopeID, scopeDrive, token, updatedAt)
	if err != nil {
		return fmt.Errorf("sync: saving delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return nil
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes. Handles the case where a
// server-side delete+recreate assigns a new item_id for an existing path
// by removing the stale row first (prevents UNIQUE constraint violation on path).
func commitUpsert(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	// Remove any stale baseline row at the same path but different identity.
	// This happens when the server assigns a new item_id for a path that
	// was previously tracked under a different ID (delete+recreate, or
	// re-upload after server-side deletion).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM baseline WHERE path = ? AND NOT (drive_id = ? AND item_id = ?)`,
		o.Path, o.DriveID, o.ItemID,
	); err != nil {
		return fmt.Errorf("sync: clearing stale baseline for %s: %w", o.Path, err)
	}

	_, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		o.DriveID, o.ItemID, o.Path,
		nullString(o.ParentID),
		o.ItemType.String(),
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Size),
		nullInt64(o.Mtime),
		syncedAt,
		nullString(o.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: upserting baseline for %s: %w", o.Path, err)
	}

	return nil
}

// commitDelete removes a baseline entry for delete and cleanup outcomes.
func commitDelete(ctx context.Context, tx *sql.Tx, path string) error {
	_, err := tx.ExecContext(ctx, sqlDeleteBaseline, path)
	if err != nil {
		return fmt.Errorf("sync: deleting baseline for %s: %w", path, err)
	}

	return nil
}

// commitMove atomically updates the path for move outcomes. With the ID-based
// PK, a move is a single UPDATE (not DELETE+INSERT) — the row identity
// (drive_id, item_id) doesn't change, only the path does.
func commitMove(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	// Upsert handles both the path update and all other field updates.
	// The ON CONFLICT(drive_id, item_id) clause matches the existing row
	// and updates path + all mutable fields atomically.
	return commitUpsert(ctx, tx, o, syncedAt)
}

// commitConflict inserts a conflict record. Auto-resolved conflicts
// (Outcome.ResolvedBy == ResolvedByAuto) are inserted as already resolved, and
// the baseline is updated (the upload created a new remote item).
func commitConflict(ctx context.Context, tx *sql.Tx, o *Outcome, syncedAt int64) error {
	conflictID := uuid.New().String()

	resolution := ResolutionUnresolved
	var resolvedAt sql.NullInt64
	var resolvedBy sql.NullString

	if o.ResolvedBy == ResolvedByAuto {
		resolution = ResolutionKeepLocal
		resolvedAt = sql.NullInt64{Int64: syncedAt, Valid: true}
		resolvedBy = sql.NullString{String: ResolvedByAuto, Valid: true}
	}

	_, err := tx.ExecContext(ctx, sqlInsertConflict,
		conflictID, o.DriveID,
		nullString(o.ItemID),
		o.Path, o.ConflictType, syncedAt,
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullInt64(o.Mtime),
		nullInt64(o.RemoteMtime),
		resolution, resolvedAt, resolvedBy,
	)
	if err != nil {
		return fmt.Errorf("sync: inserting conflict for %s: %w", o.Path, err)
	}

	// Auto-resolved conflicts also update the baseline (the upload created
	// a new remote item that should be tracked).
	if o.ResolvedBy == ResolvedByAuto {
		if upsertErr := commitUpsert(ctx, tx, o, syncedAt); upsertErr != nil {
			return upsertErr
		}
	}

	// Unresolved edit-delete conflict from local delete: the original file is
	// gone (renamed to conflict copy), so remove the baseline entry (B-133).
	if o.ResolvedBy == "" && o.ConflictType == ConflictEditDelete {
		if delErr := commitDelete(ctx, tx, o.Path); delErr != nil {
			return delErr
		}
	}

	return nil
}

// updateRemoteStateOnOutcome updates remote_state based on a completed action
// outcome, called from within the same transaction as the baseline update.
// Silently skips if no matching remote_state row exists (e.g., upload-only mode).
func updateRemoteStateOnOutcome(ctx context.Context, tx *sql.Tx, o *Outcome) error {
	if o.ItemID == "" || o.DriveID.IsZero() {
		return nil
	}

	// Note: sync_failures cleanup is handled exclusively by the engine's
	// clearFailureOnSuccess method (D-6). The store owns only baseline and
	// remote_state commits.
	switch o.Action { //nolint:exhaustive // only action types that touch remote_state
	case ActionDownload:
		// Hash guard: only transition if the remote_state hash matches the
		// downloaded hash. Prevents stale overwrite when a new observation
		// arrived while the download was in progress.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status = ? AND hash IS ?`,
			statusSynced,
			o.DriveID.String(), o.ItemID, statusDownloading, nullString(o.RemoteHash),
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for download %s: %w", o.Path, err)
		}

	case ActionLocalDelete:
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?
			WHERE drive_id = ? AND item_id = ? AND sync_status = ?`,
			statusDeleted,
			o.DriveID.String(), o.ItemID, statusDeleting,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for local delete %s: %w", o.Path, err)
		}

	case ActionUpload, ActionFolderCreate:
		// Unconditional: upload resolves any state.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET sync_status = ?, hash = ?, size = ?, mtime = ?
			WHERE drive_id = ? AND item_id = ?`,
			statusSynced, nullString(o.RemoteHash), nullInt64(o.Size), nullInt64(o.Mtime),
			o.DriveID.String(), o.ItemID,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for upload %s: %w", o.Path, err)
		}

	case ActionLocalMove, ActionRemoteMove:
		// Move success: update path and mark synced.
		_, err := tx.ExecContext(ctx,
			`UPDATE remote_state SET path = ?, sync_status = ?
			WHERE drive_id = ? AND item_id = ?`,
			o.Path, statusSynced,
			o.DriveID.String(), o.ItemID,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for move %s: %w", o.Path, err)
		}
	}

	return nil
}

// CheckCacheConsistency reloads baseline entries from the database and compares
// them with the in-memory cache. Returns the number of mismatches found (report-only,
// no auto-fix). Intended for periodic verification in watch mode (B-198).
func (m *SyncStore) CheckCacheConsistency(ctx context.Context) (int, error) {
	if m.baseline == nil {
		return 0, nil
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return 0, fmt.Errorf("sync: querying baseline for consistency check: %w", err)
	}
	defer rows.Close()

	dbEntries := make(map[string]*BaselineEntry)

	for rows.Next() {
		entry, scanErr := scanBaselineRow(rows)
		if scanErr != nil {
			return 0, scanErr
		}

		dbEntries[entry.Path] = entry
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("sync: iterating baseline rows for consistency check: %w", err)
	}

	mismatches := 0

	// Check for entries in cache not in DB, or with different values.
	for p, cached := range m.baseline.byPath {
		dbEntry, ok := dbEntries[p]
		if !ok {
			m.logger.Warn("cache consistency: entry in cache not in DB",
				slog.String("path", p),
			)

			mismatches++

			continue
		}

		if cached.LocalHash != dbEntry.LocalHash || cached.RemoteHash != dbEntry.RemoteHash ||
			cached.Size != dbEntry.Size || cached.ItemID != dbEntry.ItemID {
			m.logger.Warn("cache consistency: field mismatch",
				slog.String("path", p),
			)

			mismatches++
		}
	}

	// Check for entries in DB not in cache.
	for p := range dbEntries {
		if _, ok := m.baseline.byPath[p]; !ok {
			m.logger.Warn("cache consistency: entry in DB not in cache",
				slog.String("path", p),
			)

			mismatches++
		}
	}

	if mismatches > 0 {
		m.logger.Warn("cache consistency check complete",
			slog.Int("mismatches", mismatches),
		)
	}

	return mismatches, nil
}
