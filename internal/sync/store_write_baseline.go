// Package sync persists sync baseline, observation, failure, and scope state.
//
// Contents:
//   - Load:                    read baseline table into memory
//   - scanBaselineRow:         scan single baseline row with nullable handling
//   - CommitMutation:          atomically apply mutation to baseline + remote_state
//   - classifyBaselineMutation: map ActionType to one explicit baseline mutation kind
//   - applySingleMutation:     dispatch mutation to appropriate DB helper
//   - updateBaselineCache:     patch in-memory baseline after DB commit
//   - reloadBaselineCache:     rebuild the cache from SQLite after impossible cache state
//   - mutationToEntry:         convert BaselineMutation to BaselineEntry
//   - commitUpsert:            INSERT/UPDATE baseline for download/upload/create
//   - commitDelete:            DELETE baseline for delete/cleanup
//   - commitMove:              UPDATE path for move outcomes
//   - updateRemoteStateOnOutcome: update remote_state in same transaction
//   - CheckCacheConsistency:   verify in-memory cache matches DB
//
// Related files:
//   - store.go:             SyncStore type definition and lifecycle
//   - store_write_observation.go: CommitObservation advances the observation cursor
package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT item_id, path, parent_id, item_type,
		local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime,
		local_device, local_inode, local_has_identity, etag
		FROM baseline`

	sqlUpsertBaseline = `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash,
		 local_size, remote_size, local_mtime, remote_mtime,
		 local_device, local_inode, local_has_identity, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
		 path = excluded.path,
		 parent_id = excluded.parent_id,
		 item_type = excluded.item_type,
		 local_hash = excluded.local_hash,
		 remote_hash = excluded.remote_hash,
		 local_size = excluded.local_size,
		 remote_size = excluded.remote_size,
		 local_mtime = excluded.local_mtime,
		 remote_mtime = excluded.remote_mtime,
		 local_device = excluded.local_device,
		 local_inode = excluded.local_inode,
		 local_has_identity = excluded.local_has_identity,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`
)

// LocalBaselineRefresh is the explicit input for refreshing local baseline
// identity after convergence paths such as keep-both conflict handling.
type LocalBaselineRefresh struct {
	Path             string
	DriveID          driveid.ID
	ItemID           string
	ItemType         ItemType
	LocalHash        string
	LocalSize        int64
	LocalSizeKnown   bool
	LocalMtime       int64
	LocalDevice      uint64
	LocalInode       uint64
	LocalHasIdentity bool
}

type baselineMutationKind int

const (
	baselineMutationNoop baselineMutationKind = iota
	baselineMutationUpsert
	baselineMutationDelete
	baselineMutationMove
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

	contentDriveID, err := m.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return nil, fmt.Errorf("sync: loading content drive for baseline: %w", err)
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}
	defer rows.Close()

	b := &Baseline{
		ByPath:     make(map[string]*BaselineEntry),
		ByID:       make(map[string]*BaselineEntry),
		ByDirLower: make(map[DirLowerKey][]*BaselineEntry),
	}

	for rows.Next() {
		entry, err := scanBaselineRow(rows, contentDriveID)
		if err != nil {
			return nil, err
		}

		b.ByPath[entry.Path] = entry
		b.ByID[entry.ItemID] = entry

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
func scanBaselineRow(rows *sql.Rows, contentDriveID driveid.ID) (*BaselineEntry, error) {
	var (
		e                BaselineEntry
		parentID         sql.NullString
		localHash        sql.NullString
		remoteHash       sql.NullString
		localSize        sql.NullInt64
		remoteSize       sql.NullInt64
		localMtime       sql.NullInt64
		remoteMtime      sql.NullInt64
		localDevice      int64
		localInode       int64
		localHasIdentity int
		etag             sql.NullString
	)

	err := rows.Scan(
		&e.ItemID, &e.Path, &parentID, &e.ItemType,
		&localHash, &remoteHash, &localSize, &remoteSize, &localMtime, &remoteMtime,
		&localDevice, &localInode, &localHasIdentity, &etag,
	)
	if err != nil {
		return nil, fmt.Errorf("sync: scanning baseline row: %w", err)
	}

	e.DriveID = contentDriveID
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
	e.LocalDevice = uint64(localDevice)
	e.LocalInode = uint64(localInode)
	e.LocalHasIdentity = localHasIdentity != 0

	return &e, nil
}

func publicationDriveIDForAction(action *Action, defaultDriveID driveid.ID) driveid.ID {
	if action == nil {
		return defaultDriveID
	}
	if !action.DriveID.IsZero() {
		return action.DriveID
	}
	if action.View != nil {
		if action.View.Remote != nil && !action.View.Remote.DriveID.IsZero() {
			return action.View.Remote.DriveID
		}
		if action.View.Baseline != nil && !action.View.Baseline.DriveID.IsZero() {
			return action.View.Baseline.DriveID
		}
	}

	return defaultDriveID
}

func actionItemType(action *Action) ItemType {
	if action == nil {
		return ItemTypeFile
	}
	if action.View != nil {
		if action.View.Remote != nil && action.View.Remote.ItemType != ItemTypeFile {
			return action.View.Remote.ItemType
		}
		if action.View.Baseline != nil && action.View.Baseline.ItemType != ItemTypeFile {
			return action.View.Baseline.ItemType
		}
		if action.View.Local != nil && action.View.Local.ItemType != ItemTypeFile {
			return action.View.Local.ItemType
		}
	}

	return ItemTypeFile
}

func fillMutationFromBaselineDefaults(mutation *BaselineMutation, baseline *BaselineEntry) {
	if mutation == nil || baseline == nil {
		return
	}

	if mutation.ItemID == "" {
		mutation.ItemID = baseline.ItemID
	}
	if mutation.ParentID == "" {
		mutation.ParentID = baseline.ParentID
	}
	if mutation.LocalHash == "" {
		mutation.LocalHash = baseline.LocalHash
	}
	if !mutation.LocalSizeKnown {
		mutation.LocalSize = baseline.LocalSize
		mutation.LocalSizeKnown = baseline.LocalSizeKnown
	}
	if mutation.LocalMtime == 0 {
		mutation.LocalMtime = baseline.LocalMtime
	}
	if !mutation.LocalIdentityObserved {
		mutation.LocalDevice = baseline.LocalDevice
		mutation.LocalInode = baseline.LocalInode
		mutation.LocalHasIdentity = baseline.LocalHasIdentity
	}
	if mutation.RemoteHash == "" {
		mutation.RemoteHash = baseline.RemoteHash
	}
	if !mutation.RemoteSizeKnown {
		mutation.RemoteSize = baseline.RemoteSize
		mutation.RemoteSizeKnown = baseline.RemoteSizeKnown
	}
	if mutation.RemoteMtime == 0 {
		mutation.RemoteMtime = baseline.RemoteMtime
	}
	if mutation.ETag == "" {
		mutation.ETag = baseline.ETag
	}
}

func publicationMutationFromAction(action *Action, defaultDriveID driveid.ID) (*BaselineMutation, error) {
	if action == nil {
		return nil, fmt.Errorf("sync: building publication mutation: nil action")
	}
	if action.Type != ActionUpdateSynced && action.Type != ActionCleanup {
		return nil, fmt.Errorf("sync: building publication mutation: %s is not publication-only", action.Type.String())
	}

	mutation := &BaselineMutation{
		Action:   action.Type,
		Success:  true,
		Path:     action.Path,
		OldPath:  action.OldPath,
		DriveID:  publicationDriveIDForAction(action, defaultDriveID),
		ItemID:   action.ItemID,
		ItemType: actionItemType(action),
	}

	if action.View != nil {
		if action.View.Remote != nil {
			mutation.ItemID = action.View.Remote.ItemID
			mutation.RemoteHash = action.View.Remote.Hash
			mutation.RemoteSize = action.View.Remote.Size
			mutation.RemoteSizeKnown = true
			mutation.RemoteMtime = action.View.Remote.Mtime
			mutation.ETag = action.View.Remote.ETag
			mutation.ItemType = action.View.Remote.ItemType
			if !action.View.Remote.DriveID.IsZero() {
				mutation.DriveID = action.View.Remote.DriveID
			}
		}

		if action.View.Local != nil {
			mutation.LocalHash = action.View.Local.Hash
			mutation.LocalSize = action.View.Local.Size
			mutation.LocalSizeKnown = true
			mutation.LocalMtime = action.View.Local.Mtime
			mutation.LocalDevice = action.View.Local.LocalDevice
			mutation.LocalInode = action.View.Local.LocalInode
			mutation.LocalHasIdentity = action.View.Local.LocalHasIdentity
			mutation.LocalIdentityObserved = true
		}

		fillMutationFromBaselineDefaults(mutation, action.View.Baseline)

		if mutation.ItemType == ItemTypeFile && action.View.Baseline != nil && action.View.Baseline.ItemType != ItemTypeFile {
			mutation.ItemType = action.View.Baseline.ItemType
		}
	}

	return mutation, nil
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

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureContentDriveIDTx(ctx, tx, outcome.DriveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ContentDriveID.IsZero() {
		outcome.DriveID = state.ContentDriveID
	}

	if applyErr := applySingleMutation(ctx, tx, outcome); applyErr != nil {
		return applyErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing outcome transaction: %w", err)
	}

	// Update in-memory baseline cache incrementally.
	if cacheErr := m.updateBaselineCache(ctx, outcome); cacheErr != nil {
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

	entry := &BaselineEntry{
		Path:             refresh.Path,
		DriveID:          refresh.DriveID,
		ItemID:           refresh.ItemID,
		ItemType:         refresh.ItemType,
		LocalHash:        refresh.LocalHash,
		LocalSize:        refresh.LocalSize,
		LocalSizeKnown:   refresh.LocalSizeKnown,
		LocalMtime:       refresh.LocalMtime,
		LocalDevice:      refresh.LocalDevice,
		LocalInode:       refresh.LocalInode,
		LocalHasIdentity: refresh.LocalHasIdentity,
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

	state, err := m.readObservationStateTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := m.ensureContentDriveIDTx(ctx, tx, entry.DriveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ContentDriveID.IsZero() {
		entry.DriveID = state.ContentDriveID
	}

	_, err = tx.ExecContext(ctx, sqlUpsertBaseline,
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
		int64(entry.LocalDevice),
		int64(entry.LocalInode),
		boolInt(entry.LocalHasIdentity),
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
	case ActionConflictCopy:
		return baselineMutationNoop, nil
	case ActionDownload, ActionUpload, ActionFolderCreate, ActionUpdateSynced:
		return baselineMutationUpsert, nil
	case ActionLocalDelete, ActionRemoteDelete, ActionCleanup:
		return baselineMutationDelete, nil
	case ActionLocalMove, ActionRemoteMove:
		return baselineMutationMove, nil
	default:
		return 0, fmt.Errorf("sync: classifying baseline mutation for %s: unknown action type", action.String())
	}
}

// applySingleMutation dispatches a single mutation to the appropriate DB helper.
func applySingleMutation(ctx context.Context, tx sqlTxRunner, o *BaselineMutation) error {
	mutation, err := classifyBaselineMutation(o.Action)
	if err != nil {
		return err
	}

	switch mutation {
	case baselineMutationNoop:
		err = nil
	case baselineMutationUpsert:
		err = commitUpsert(ctx, tx, o)
	case baselineMutationDelete:
		err = commitDelete(ctx, tx, o.Path)
	case baselineMutationMove:
		err = commitMove(ctx, tx, o)
	}

	if err != nil {
		return err
	}

	// Update remote_state in the same transaction.
	return updateRemoteStateOnOutcome(ctx, tx, o)
}

// updateBaselineCache applies a single outcome to the in-memory baseline,
// keeping the cache consistent without a full DB reload.
func (m *SyncStore) updateBaselineCache(ctx context.Context, o *BaselineMutation) error {
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
	case baselineMutationNoop:
		return nil
	case baselineMutationUpsert:
		m.baseline.Put(mutationToEntry(o))
	case baselineMutationDelete:
		m.baseline.Delete(o.Path)
	case baselineMutationMove:
		m.baseline.Delete(o.OldPath)
		m.baseline.Put(mutationToEntry(o))
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
func mutationToEntry(o *BaselineMutation) *BaselineEntry {
	return &BaselineEntry{
		Path:             o.Path,
		DriveID:          o.DriveID,
		ItemID:           o.ItemID,
		ParentID:         o.ParentID,
		ItemType:         o.ItemType,
		LocalHash:        o.LocalHash,
		RemoteHash:       o.RemoteHash,
		LocalSize:        o.LocalSize,
		LocalSizeKnown:   o.LocalSizeKnown,
		RemoteSize:       o.RemoteSize,
		RemoteSizeKnown:  o.RemoteSizeKnown,
		LocalMtime:       o.LocalMtime,
		RemoteMtime:      o.RemoteMtime,
		LocalDevice:      o.LocalDevice,
		LocalInode:       o.LocalInode,
		LocalHasIdentity: o.LocalHasIdentity,
		ETag:             o.ETag,
	}
}

func mutationFromActionOutcome(o *ActionOutcome) *BaselineMutation {
	if o == nil {
		return nil
	}
	if o.Action == ActionConflictCopy {
		return nil
	}

	return &BaselineMutation{
		Action:                o.Action,
		Success:               o.Success,
		Path:                  o.Path,
		OldPath:               o.OldPath,
		DriveID:               o.DriveID,
		ItemID:                o.ItemID,
		ParentID:              o.ParentID,
		ItemType:              o.ItemType,
		LocalHash:             o.LocalHash,
		RemoteHash:            o.RemoteHash,
		LocalSize:             o.LocalSize,
		LocalSizeKnown:        o.LocalSizeKnown,
		RemoteSize:            o.RemoteSize,
		RemoteSizeKnown:       o.RemoteSizeKnown,
		LocalMtime:            o.LocalMtime,
		RemoteMtime:           o.RemoteMtime,
		LocalDevice:           o.LocalDevice,
		LocalInode:            o.LocalInode,
		LocalHasIdentity:      o.LocalHasIdentity,
		LocalIdentityObserved: o.LocalIdentityObserved,
		ETag:                  o.ETag,
	}
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes. Handles the case where a
// server-side delete+recreate assigns a new item_id for an existing path
// by removing the stale row first (prevents UNIQUE constraint violation on path).
func commitUpsert(ctx context.Context, tx sqlTxRunner, o *BaselineMutation) error {
	// Remove any stale baseline row at the same path but different identity.
	// This happens when the server assigns a new item_id for a path that
	// was previously tracked under a different ID (delete+recreate, or
	// re-upload after server-side deletion).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM baseline WHERE path = ? AND item_id <> ?`,
		o.Path, o.ItemID,
	); err != nil {
		return fmt.Errorf("sync: clearing stale baseline for %s: %w", o.Path, err)
	}

	_, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		o.ItemID, o.Path,
		nullString(o.ParentID),
		o.ItemType.String(),
		nullString(o.LocalHash),
		nullString(o.RemoteHash),
		nullKnownInt64(o.LocalSize, o.LocalSizeKnown),
		nullKnownInt64(o.RemoteSize, o.RemoteSizeKnown),
		nullOptionalInt64(o.LocalMtime),
		nullOptionalInt64(o.RemoteMtime),
		int64(o.LocalDevice),
		int64(o.LocalInode),
		boolInt(o.LocalHasIdentity),
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
// (item_id) doesn't change, only the path does.
func commitMove(ctx context.Context, tx sqlTxRunner, o *BaselineMutation) error {
	// Upsert handles both the path update and all other field updates.
	// The ON CONFLICT(item_id) clause matches the existing row
	// and updates path + all mutable fields atomically.
	return commitUpsert(ctx, tx, o)
}

// updateRemoteStateOnOutcome updates the remote mirror when execution produces
// authoritative new remote truth.
func updateRemoteStateOnOutcome(ctx context.Context, tx sqlTxRunner, o *BaselineMutation) error {
	if o.ItemID == "" || o.DriveID.IsZero() {
		return nil
	}

	switch o.Action {
	case ActionUpload, ActionFolderCreate:
		_, err := tx.ExecContext(ctx,
			`INSERT INTO remote_state (
				drive_id, item_id, path, item_type, hash, size, mtime, etag
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(item_id) DO UPDATE SET
				drive_id = excluded.drive_id,
				path = excluded.path,
				item_type = excluded.item_type,
				hash = excluded.hash,
				size = excluded.size,
				mtime = excluded.mtime,
				etag = excluded.etag`,
			o.DriveID.String(), o.ItemID, o.Path, o.ItemType,
			nullString(o.RemoteHash), nullKnownInt64(o.RemoteSize, o.RemoteSizeKnown), nullOptionalInt64(o.RemoteMtime),
			nullString(o.ETag),
		)
		if err != nil {
			return fmt.Errorf("sync: updating remote_state for upload %s: %w", o.Path, err)
		}
	case ActionConflictCopy:
		return nil
	case ActionRemoteDelete:
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM remote_state WHERE item_id = ?`,
			o.ItemID,
		); err != nil {
			return fmt.Errorf("sync: deleting remote_state for remote delete %s: %w", o.Path, err)
		}
	case ActionRemoteMove:
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_state
			 SET drive_id = ?, path = ?
			 WHERE item_id = ?`,
			o.DriveID.String(), o.Path, o.ItemID,
		); err != nil {
			return fmt.Errorf("sync: updating remote_state for remote move %s: %w", o.Path, err)
		}
	case ActionDownload,
		ActionLocalDelete,
		ActionLocalMove,
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

	contentDriveID, err := m.contentDriveIDForRead(ctx, driveid.ID{})
	if err != nil {
		return 0, fmt.Errorf("sync: loading content drive for consistency check: %w", err)
	}

	rows, err := m.db.QueryContext(ctx, sqlLoadBaseline)
	if err != nil {
		return 0, fmt.Errorf("sync: querying baseline for consistency check: %w", err)
	}
	defer rows.Close()

	dbEntries := make(map[string]*BaselineEntry)

	for rows.Next() {
		entry, scanErr := scanBaselineRow(rows, contentDriveID)
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
		a.LocalDevice == b.LocalDevice &&
		a.LocalInode == b.LocalInode &&
		a.LocalHasIdentity == b.LocalHasIdentity &&
		a.ItemID == b.ItemID
}
