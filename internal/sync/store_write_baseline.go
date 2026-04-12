// Package sync persists sync baseline, observation, conflict, failure, and scope state.
//
// Contents:
//   - Load:                    read baseline table into memory
//   - scanBaselineRow:         scan single baseline row with nullable handling
//   - GetDeltaToken:           retrieve saved delta token for drive/scope
//   - CommitDeltaToken:        persist delta token in own transaction
//   - DeleteDeltaToken:        remove delta token for drive/scope
//   - saveDeltaToken:          persist delta token within existing transaction
//   - CommitMutation:          atomically apply mutation to baseline + remote_state
//   - classifyBaselineMutation: map ActionType to one explicit baseline mutation kind
//   - applySingleMutation:     dispatch mutation to appropriate DB helper
//   - updateBaselineCache:     patch in-memory baseline after DB commit
//   - reloadBaselineCache:     rebuild the cache from SQLite after impossible cache state
//   - mutationToEntry:         convert BaselineMutation to BaselineEntry
//   - commitUpsert:            INSERT/UPDATE baseline for download/upload/create
//   - commitDelete:            DELETE baseline for delete/cleanup
//   - commitMove:              UPDATE path for move outcomes
//   - commitConflict:          INSERT conflict record
//   - updateRemoteStateOnOutcome: update remote_state in same transaction
//   - CheckCacheConsistency:   verify in-memory cache matches DB
//
// Related files:
//   - store.go:             SyncStore type definition and lifecycle
//   - store_write_observation.go: CommitObservation calls saveDeltaToken
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
		local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime, synced_at, etag
		FROM baseline`

	sqlGetDeltaCursor = `SELECT cursor FROM delta_tokens WHERE drive_id = ? AND scope_id = ?`

	sqlUpsertBaseline = `INSERT INTO baseline
		(drive_id, item_id, path, parent_id, item_type, local_hash, remote_hash,
		 local_size, remote_size, local_mtime, remote_mtime, synced_at, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, item_id) DO UPDATE SET
		 path = excluded.path,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 local_size = excluded.local_size,
		 remote_size = excluded.remote_size,
		 local_mtime = excluded.local_mtime,
		 remote_mtime = excluded.remote_mtime,
		 synced_at = excluded.synced_at,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`

	sqlInsertConflict = `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at,
		 local_hash, remote_hash, local_mtime, remote_mtime,
		 resolution, resolved_at, resolved_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	sqlUpsertDeltaCursor = `INSERT INTO delta_tokens (drive_id, scope_id, scope_drive, cursor, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(drive_id, scope_id) DO UPDATE SET
		 scope_drive = excluded.scope_drive,
		 cursor = excluded.cursor,
		 updated_at = excluded.updated_at`
)

// LocalBaselineRefresh is the explicit reconciliation input used by convergence
// paths such as keep-both conflict resolution.
type LocalBaselineRefresh struct {
	Path           string
	DriveID        driveid.ID
	ItemID         string
	ItemType       ItemType
	LocalHash      string
	LocalSize      int64
	LocalSizeKnown bool
	LocalMtime     int64
}

type baselineMutationKind int

const (
	baselineMutationUpsert baselineMutationKind = iota
	baselineMutationDelete
	baselineMutationMove
	baselineMutationConflict
)

// Load reads the entire baseline table into memory, populating ByPath and
// ByID maps. The result is cached on the manager — subsequent calls return
// the cached baseline without querying the database. The cache is kept
// consistent by CommitMutation(), which incrementally patches the in-memory
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
		ByPath:     make(map[string]*BaselineEntry),
		ByID:       make(map[driveid.ItemKey]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows)
		if err != nil {
			return nil, err
		}

		b.ByPath[entry.Path] = entry
		b.ByID[driveid.NewItemKey(entry.DriveID, entry.ItemID)] = entry

		dlk := DirLowerKeyFromPath(entry.Path)
		b.ByDirLower[dlk] = append(b.ByDirLower[dlk], entry)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: iterating baseline rows: %w", err)
	}

	m.baseline = b
	m.logger.Debug("baseline loaded", slog.Int("entries", len(b.ByPath)))

	return b, nil
}

// scanBaselineRow scans a single row from the baseline table, handling
// nullable columns with sql.Null* types.
func scanBaselineRow(rows *sql.Rows) (*BaselineEntry, error) {
	var (
		e           BaselineEntry
		parentID    sql.NullString
		localHash   sql.NullString
		remoteHash  sql.NullString
		localSize   sql.NullInt64
		remoteSize  sql.NullInt64
		localMtime  sql.NullInt64
		remoteMtime sql.NullInt64
		etag        sql.NullString
	)

	err := rows.Scan(
		&e.DriveID, &e.ItemID, &e.Path, &parentID, &e.ItemType,
		&localHash, &remoteHash, &localSize, &remoteSize, &localMtime, &remoteMtime, &e.SyncedAt, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning baseline row: %w", err)
	}

	e.ParentID = parentID.String
	e.LocalHash = localHash.String
	e.RemoteHash = remoteHash.String
	e.ETag = etag.String

	if localSize.Valid {
		e.LocalSize = localSize.Int64
		e.LocalSizeKnown = true
	}

	if remoteSize.Valid {
		e.RemoteSize = remoteSize.Int64
		e.RemoteSizeKnown = true
	}

	if localMtime.Valid {
		e.LocalMtime = localMtime.Int64
	}

	if remoteMtime.Valid {
		e.RemoteMtime = remoteMtime.Int64
	}

	return &e, nil
}

// GetDeltaToken returns the saved delta token for a drive and scope, or empty
// string if no token has been saved yet. Use scopeID="" for the primary
// drive-level delta; use a remoteItem.id for shortcut-scoped deltas.
func (m *SyncStore) GetDeltaToken(ctx context.Context, driveID, scopeID string) (string, error) {
	var token string

	err := m.db.QueryRowContext(ctx, sqlGetDeltaCursor, driveID, scopeID).Scan(&token)
	if err == sql.ErrNoRows {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("sync: getting delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return token, nil
}

// CommitMutation atomically applies a single mutation to the baseline in a
// SQLite transaction. After the DB write, the in-memory baseline cache is
// updated incrementally (Put or Delete).
func (m *SyncStore) CommitMutation(ctx context.Context, outcome *BaselineMutation) (err error) {
	if outcome == nil {
		return fmt.Errorf("sync: commit mutation requires outcome")
	}

	if !outcome.Success {
		return nil
	}

	// Ensure baseline is loaded so we can update the in-memory cache.
	if m.baseline == nil {
		if _, loadErr := m.Load(ctx); loadErr != nil {
			return fmt.Errorf("sync: loading baseline before commit outcome: %w", loadErr)
		}
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning commit outcome transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback outcome transaction for %s", outcome.Path))
	}()

	syncedAt := m.nowFunc().UnixNano()

	if applyErr := applySingleMutation(ctx, tx, outcome, syncedAt); applyErr != nil {
		return applyErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing outcome transaction: %w", err)
	}

	// Update in-memory baseline cache incrementally.
	if cacheErr := m.updateBaselineCache(ctx, outcome, syncedAt); cacheErr != nil {
		return cacheErr
	}

	return nil
}

// RefreshLocalBaseline updates the local-side comparison tuple for one path
// while preserving any existing remote-side metadata. If a matching
// remote_state row exists, it is marked synced in the same transaction.
func (m *SyncStore) RefreshLocalBaseline(ctx context.Context, refresh LocalBaselineRefresh) (err error) {
	if m.baseline == nil {
		if _, loadErr := m.Load(ctx); loadErr != nil {
			return fmt.Errorf("sync: loading baseline before refresh local baseline: %w", loadErr)
		}
	}

	var existing *BaselineEntry
	if entry, ok := m.baseline.GetByPath(refresh.Path); ok {
		existing = entry
	}

	syncedAt := m.nowFunc().UnixNano()
	entry := &BaselineEntry{
		Path:           refresh.Path,
		DriveID:        refresh.DriveID,
		ItemID:         refresh.ItemID,
		ItemType:       refresh.ItemType,
		LocalHash:      refresh.LocalHash,
		LocalSize:      refresh.LocalSize,
		LocalSizeKnown: refresh.LocalSizeKnown,
		LocalMtime:     refresh.LocalMtime,
		SyncedAt:       syncedAt,
	}

	if existing != nil {
		entry.ParentID = existing.ParentID
		entry.RemoteHash = existing.RemoteHash
		entry.RemoteSize = existing.RemoteSize
		entry.RemoteSizeKnown = existing.RemoteSizeKnown
		entry.RemoteMtime = existing.RemoteMtime
		entry.ETag = existing.ETag
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning refresh local baseline transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback refresh local baseline transaction for %s", refresh.Path))
	}()

	_, err = tx.ExecContext(ctx, sqlUpsertBaseline,
		entry.DriveID.String(),
		entry.ItemID,
		entry.Path,
		nullString(entry.ParentID),
		entry.ItemType,
		nullString(entry.LocalHash),
		nullString(entry.RemoteHash),
		nullKnownInt64(entry.LocalSize, entry.LocalSizeKnown),
		nullKnownInt64(entry.RemoteSize, entry.RemoteSizeKnown),
		nullOptionalInt64(entry.LocalMtime),
		nullOptionalInt64(entry.RemoteMtime),
		entry.SyncedAt,
		nullString(entry.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: refreshing baseline for %s: %w", refresh.Path, err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing refresh local baseline transaction: %w", err)
	}

	m.baseline.Put(entry)

	return nil
}

func classifyBaselineMutation(action ActionType) (baselineMutationKind, error) {
	switch action {
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		return baselineMutationUpsert, nil
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		return baselineMutationDelete, nil
	case ActionLocalMove, ActionRemoteMove:
		return baselineMutationMove, nil
	case ActionConflict:
		return baselineMutationConflict, nil
	default:
		return 0, fmt.Errorf("sync: classifying baseline mutation for %s: unknown action type", action.String())
	}
}

// applySingleMutation dispatches a single mutation to the appropriate DB helper.
func applySingleMutation(ctx context.Context, tx sqlTxRunner, o *BaselineMutation, syncedAt int64) error {
	mutation, err := classifyBaselineMutation(o.Action)
	if err != nil {
		return err
	}

	switch mutation {
	case baselineMutationUpsert:
		err = commitUpsert(ctx, tx, o, syncedAt)
	case baselineMutationDelete:
		err = commitDelete(ctx, tx, o.Path)
	case baselineMutationMove:
		err = commitMove(ctx, tx, o, syncedAt)
	case baselineMutationConflict:
		err = commitConflict(ctx, tx, o, syncedAt)
	}

	if err != nil {
		return err
	}

	// Update remote_state in the same transaction.
	return updateRemoteStateOnOutcome(ctx, tx, o, syncedAt)
}

// updateBaselineCache applies a single outcome to the in-memory baseline,
// keeping the cache consistent without a full DB reload.
func (m *SyncStore) updateBaselineCache(ctx context.Context, o *BaselineMutation, syncedAt int64) error {
	mutation, err := classifyBaselineMutation(o.Action)
	if err != nil {
		m.logger.Warn("baseline cache classification failed, reloading cache from database",
			slog.String("path", o.Path),
			slog.String("action", o.Action.String()),
			slog.String("error", err.Error()),
		)

		if reloadErr := m.reloadBaselineCache(ctx); reloadErr != nil {
			return fmt.Errorf("sync: reloading baseline cache after impossible mutation state: %w", reloadErr)
		}

		return nil
	}

	switch mutation {
	case baselineMutationUpsert:
		m.baseline.Put(mutationToEntry(o, syncedAt))
	case baselineMutationDelete:
		m.baseline.Delete(o.Path)
	case baselineMutationMove:
		m.baseline.Delete(o.OldPath)
		m.baseline.Put(mutationToEntry(o, syncedAt))
	case baselineMutationConflict:
		if o.ResolvedBy == ResolvedByAuto {
			m.baseline.Put(mutationToEntry(o, syncedAt))
		} else if o.ConflictType == ConflictEditDelete {
			// Unresolved edit-delete conflict from local delete: the original file
			// is gone (renamed to conflict copy), so remove the baseline entry.
			m.baseline.Delete(o.Path)
		}
	}

	return nil
}

func (m *SyncStore) reloadBaselineCache(ctx context.Context) error {
	m.baselineMu.Lock()
	m.baseline = nil
	m.baselineMu.Unlock()

	if _, err := m.Load(ctx); err != nil {
		return fmt.Errorf("sync: loading baseline after cache invalidation: %w", err)
	}

	return nil
}

// mutationToEntry converts a BaselineMutation into a BaselineEntry for cache update.
func mutationToEntry(o *BaselineMutation, syncedAt int64) *BaselineEntry {
	return &BaselineEntry{
		Path:            o.Path,
		DriveID:         o.DriveID,
		ItemID:          o.ItemID,
		ParentID:        o.ParentID,
		ItemType:        o.ItemType,
		LocalHash:       o.LocalHash,
		RemoteHash:      o.RemoteHash,
		LocalSize:       o.LocalSize,
		LocalSizeKnown:  o.LocalSizeKnown,
		RemoteSize:      o.RemoteSize,
		RemoteSizeKnown: o.RemoteSizeKnown,
		LocalMtime:      o.LocalMtime,
		RemoteMtime:     o.RemoteMtime,
		SyncedAt:        syncedAt,
		ETag:            o.ETag,
	}
}

func mutationFromActionOutcome(o *ActionOutcome) *BaselineMutation {
	return &BaselineMutation{
		Action:          o.Action,
		Success:         o.Success,
		Path:            o.Path,
		OldPath:         o.OldPath,
		DriveID:         o.DriveID,
		ItemID:          o.ItemID,
		ParentID:        o.ParentID,
		ItemType:        o.ItemType,
		LocalHash:       o.LocalHash,
		RemoteHash:      o.RemoteHash,
		LocalSize:       o.LocalSize,
		LocalSizeKnown:  o.LocalSizeKnown,
		RemoteSize:      o.RemoteSize,
		RemoteSizeKnown: o.RemoteSizeKnown,
		LocalMtime:      o.LocalMtime,
		RemoteMtime:     o.RemoteMtime,
		ETag:            o.ETag,
		ConflictType:    o.ConflictType,
		ResolvedBy:      o.ResolvedBy,
	}
}

// CommitDeltaToken persists a delta token in its own transaction, separate
// from baseline updates. Used after all actions in a pass complete.
// Use scopeID="" and scopeDrive=driveID for the primary drive-level delta.
// For shortcut-scoped deltas, scopeID=remoteItem.id and scopeDrive=remoteItem.driveId.
func (m *SyncStore) CommitDeltaToken(ctx context.Context, token, driveID, scopeID, scopeDrive string) (err error) {
	if token == "" {
		return nil
	}

	tx, err := beginPerfTx(ctx, m.db)
	if err != nil {
		return fmt.Errorf("sync: beginning delta token transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("sync: rollback delta token transaction for drive %s scope %q", driveID, scopeID))
	}()

	updatedAt := m.nowFunc().UnixNano()
	if saveErr := m.saveDeltaToken(ctx, tx, driveID, scopeID, scopeDrive, token, updatedAt); saveErr != nil {
		return saveErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing delta token transaction: %w", err)
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
	ctx context.Context, tx sqlTxRunner, driveID, scopeID, scopeDrive, token string, updatedAt int64,
) error {
	_, err := tx.ExecContext(ctx, sqlUpsertDeltaCursor, driveID, scopeID, scopeDrive, token, updatedAt)
	if err != nil {
		return fmt.Errorf("sync: saving delta token for drive %s scope %q: %w", driveID, scopeID, err)
	}

	return nil
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes. Handles the case where a
// server-side delete+recreate assigns a new item_id for an existing path
// by removing the stale row first (prevents UNIQUE constraint violation on path).
func commitUpsert(ctx context.Context, tx sqlTxRunner, o *BaselineMutation, syncedAt int64) error {
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
		nullKnownInt64(o.LocalSize, o.LocalSizeKnown),
		nullKnownInt64(o.RemoteSize, o.RemoteSizeKnown),
		nullOptionalInt64(o.LocalMtime),
		nullOptionalInt64(o.RemoteMtime),
		syncedAt,
		nullString(o.ETag),
	)
	if err != nil {
		return fmt.Errorf("sync: upserting baseline for %s: %w", o.Path, err)
	}

	return nil
}

// commitDelete removes a baseline entry for delete and cleanup outcomes.
func commitDelete(ctx context.Context, tx sqlTxRunner, path string) error {
	_, err := tx.ExecContext(ctx, sqlDeleteBaseline, path)
	if err != nil {
		return fmt.Errorf("sync: deleting baseline for %s: %w", path, err)
	}

	return nil
}

// commitMove atomically updates the path for move outcomes. With the ID-based
// PK, a move is a single UPDATE (not DELETE+INSERT) — the row identity
// (drive_id, item_id) doesn't change, only the path does.
func commitMove(ctx context.Context, tx sqlTxRunner, o *BaselineMutation, syncedAt int64) error {
	// Upsert handles both the path update and all other field updates.
	// The ON CONFLICT(drive_id, item_id) clause matches the existing row
	// and updates path + all mutable fields atomically.
	return commitUpsert(ctx, tx, o, syncedAt)
}

// commitConflict inserts a conflict record. Auto-resolved conflicts
// (ActionOutcome.ResolvedBy == ResolvedByAuto) are inserted as already resolved, and
// the baseline is updated (the upload created a new remote item).
func commitConflict(ctx context.Context, tx sqlTxRunner, o *BaselineMutation, syncedAt int64) error {
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
		nullOptionalInt64(o.LocalMtime),
		nullOptionalInt64(o.RemoteMtime),
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

// updateRemoteStateOnOutcome updates the remote mirror when execution produces
// authoritative new remote truth.
func updateRemoteStateOnOutcome(ctx context.Context, tx sqlTxRunner, o *BaselineMutation, syncedAt int64) error {
	if o.ItemID == "" || o.DriveID.IsZero() {
		return nil
	}

	switch o.Action {
	case ActionUpload, ActionFolderCreate:
		_, err := tx.ExecContext(ctx,
			`INSERT INTO remote_state (
				drive_id, item_id, path, parent_id, item_type, hash, size, mtime, etag,
				previous_path, is_filtered, observed_at, filter_generation, filter_reason
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, 0, '')
			ON CONFLICT(drive_id, item_id) DO UPDATE SET
				path = excluded.path,
				parent_id = excluded.parent_id,
				item_type = excluded.item_type,
				hash = excluded.hash,
				size = excluded.size,
				mtime = excluded.mtime,
				etag = excluded.etag,
				previous_path = excluded.previous_path,
				is_filtered = 0,
				observed_at = excluded.observed_at,
				filter_generation = 0,
				filter_reason = ''`,
			o.DriveID.String(), o.ItemID, o.Path, nullString(o.ParentID), o.ItemType,
			nullString(o.RemoteHash), nullKnownInt64(o.RemoteSize, o.RemoteSizeKnown), nullOptionalInt64(o.RemoteMtime),
			nullString(o.ETag), sql.NullString{}, syncedAt,
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for upload %s: %w", o.Path, err)
		}
	case ActionRemoteDelete:
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM remote_state WHERE drive_id = ? AND item_id = ?`,
			o.DriveID.String(), o.ItemID,
		); err != nil {
			return fmt.Errorf("sync: deleting remote_state for remote delete %s: %w", o.Path, err)
		}
	case ActionRemoteMove:
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_state
			 SET path = ?, previous_path = ?, observed_at = ?, is_filtered = 0, filter_generation = 0, filter_reason = ''
			 WHERE drive_id = ? AND item_id = ?`,
			o.Path, nullString(o.OldPath), syncedAt, o.DriveID.String(), o.ItemID,
		); err != nil {
			return fmt.Errorf("sync: updating remote_state for remote move %s: %w", o.Path, err)
		}
	case ActionDownload,
		ActionLocalDelete,
		ActionLocalMove,
		ActionConflict,
		ActionUpdateSynced,
		ActionCleanup:
		return nil
	}

	return nil
}

// CheckCacheConsistency reloads baseline entries from the database and compares
// them with the in-memory cache. Returns the number of mismatches found (report-only,
// no auto-fix). Intended for periodic verification in watch mode (B-198).
//
// Thread-safety: called from the engine-owned result loop (single-goroutine context)
// after all workers complete. The Baseline cache is stable at this point —
// no concurrent mutation is possible.
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
	for p, cached := range m.baseline.ByPath {
		dbEntry, ok := dbEntries[p]
		if !ok {
			m.logger.Warn("cache consistency: entry in cache not in DB",
				slog.String("path", p),
			)

			mismatches++

			continue
		}

		if !baselineEntriesEqual(cached, dbEntry) {
			m.logger.Warn("cache consistency: field mismatch",
				slog.String("path", p),
			)

			mismatches++
		}
	}

	// Check for entries in DB not in cache.
	for p := range dbEntries {
		if _, ok := m.baseline.ByPath[p]; !ok {
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

func baselineEntriesEqual(a, b *BaselineEntry) bool {
	return a.LocalHash == b.LocalHash &&
		a.RemoteHash == b.RemoteHash &&
		a.LocalSize == b.LocalSize &&
		a.LocalSizeKnown == b.LocalSizeKnown &&
		a.RemoteSize == b.RemoteSize &&
		a.RemoteSizeKnown == b.RemoteSizeKnown &&
		a.LocalMtime == b.LocalMtime &&
		a.RemoteMtime == b.RemoteMtime &&
		a.ItemID == b.ItemID
}
