// Baseline persistence of the sync store.
//
// Contents:
//   - CommitMutation:          atomically apply mutation to baseline + remote_state
//   - classifyBaselineMutation: map ActionType to one explicit baseline mutation kind
//   - applySingleMutation:     dispatch mutation to appropriate DB helper
//   - updateBaselineCache:     patch in-memory baseline after DB commit
//   - cachePatchForMutationTx: decide in the transaction what the cache must change
//   - mutationToEntry:         convert BaselineMutation to BaselineEntry
//   - commitUpsert:            INSERT/UPDATE baseline for download/upload/create
//   - commitDelete:            DELETE baseline for delete/cleanup
//   - commitMove:              UPDATE path for move outcomes
//
// Related files:
//   - store.go:             SyncStore type definition and lifecycle
//   - store_read_baseline.go: Load, row scanning and CheckCacheConsistency
//   - store_publication_mutation.go: PublicationMutationFromAction
//   - store_write_remote_state.go: updateRemoteStateOnOutcome, in the same transaction
//   - store_write_observation.go: CommitObservation advances the observation cursor

package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// SQL statements for baseline operations.
const (
	sqlLoadBaseline = `SELECT item_id, path, parent_id, item_type,
		local_hash, remote_hash, local_size, remote_size, local_mtime, remote_mtime,
		local_ctime, local_device, local_inode, local_birth_time, local_has_identity, etag
		FROM baseline`

	sqlUpsertBaseline = `INSERT INTO baseline
		(item_id, path, parent_id, item_type, local_hash, remote_hash,
		 local_size, remote_size, local_mtime, remote_mtime,
		 local_ctime, local_device, local_inode, local_birth_time, local_has_identity, etag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
		 local_ctime = excluded.local_ctime,
		 local_device = excluded.local_device,
		 local_inode = excluded.local_inode,
		 local_birth_time = excluded.local_birth_time,
		 local_has_identity = excluded.local_has_identity,
		 etag = excluded.etag`

	sqlDeleteBaseline = `DELETE FROM baseline WHERE path = ?`
)

// baselineRowArgs is the one mapping from a baseline entry to the bound
// values of sqlUpsertBaseline, in its column order. Every writer of a
// baseline row goes through it (upsertBaselineRowTx, or a prepared
// sqlUpsertBaseline in bulk test seeding), so a new column is added here and
// in the statement, never at each call site: positional arguments spread over
// callers broke silently whenever a column was added. DriveID is not a
// column; the store's content drive owns it.
func baselineRowArgs(e *syncdomain.BaselineEntry) []any {
	return []any{
		e.ItemID,
		e.Path,
		nullString(e.ParentID),
		e.ItemType,
		nullString(e.LocalHash),
		nullString(e.RemoteHash),
		nullKnownInt64(e.LocalSize, e.LocalSizeKnown),
		nullKnownInt64(e.RemoteSize, e.RemoteSizeKnown),
		nullOptionalInt64(e.LocalMtime),
		nullOptionalInt64(e.RemoteMtime),
		e.LocalCtime,
		int64(e.LocalDevice),
		int64(e.LocalInode),
		e.LocalBirthTime,
		boolInt(e.LocalHasIdentity),
		nullString(e.ETag),
	}
}

// upsertBaselineRowTx inserts e as the baseline row of its item, or replaces
// every column of the row the item already has. Callers wrap the error with
// what they were recording.
func upsertBaselineRowTx(ctx context.Context, tx sqlTxRunner, e *syncdomain.BaselineEntry) error {
	_, err := tx.ExecContext(ctx, sqlUpsertBaseline, baselineRowArgs(e)...)

	return err //nolint:wrapcheck // every caller names the path it was recording
}

// localBaselineRefresh is the explicit input for refreshing local baseline
// identity after convergence paths such as keep-both conflict handling.
type localBaselineRefresh struct {
	Path             string
	DriveID          driveid.ID
	ItemID           string
	ItemType         syncdomain.ItemType
	LocalHash        string
	LocalSize        int64
	LocalSizeKnown   bool
	LocalMtime       int64
	LocalCtime       int64
	LocalDevice      uint64
	LocalInode       uint64
	LocalBirthTime   int64 // entry birth time (Unix nanoseconds); zero means unknown
	LocalHasIdentity bool
}

type baselineMutationKind int

const (
	baselineMutationNoop baselineMutationKind = iota
	baselineMutationUpsert
	baselineMutationDelete
	baselineMutationMove
)

// CommitMutation atomically applies a single mutation to the baseline in a
// SQLite transaction. What the cache must change is decided inside the
// transaction, a rebuild's read included, and applied after COMMIT, where
// nothing can fail: an error always means nothing was committed.
func (s *SyncStore) CommitMutation(ctx context.Context, outcome *syncdomain.BaselineMutation) (err error) {
	if outcome == nil {
		return errors.New("syncstore: commit mutation requires outcome")
	}

	if !outcome.Success {
		return nil
	}

	// Ensure baseline is loaded so we can update the in-memory cache.
	if s.cachedBaseline() == nil {
		if _, loadErr := s.Load(ctx); loadErr != nil {
			return fmt.Errorf("syncstore: loading baseline before commit outcome: %w", loadErr)
		}
	}

	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning commit outcome transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("syncstore: rollback outcome transaction for %s", outcome.Path))
	}()

	state, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, outcome.DriveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ContentDriveID.IsZero() {
		outcome.DriveID = state.ContentDriveID
	}

	if applyErr := s.applyOutcomeTx(ctx, tx, outcome); applyErr != nil {
		return applyErr
	}
	patch, err := s.cachePatchForMutationTx(ctx, tx, outcome)
	if err != nil {
		return err
	}
	if crashErr := s.reachCrashPoint(syncdomain.CrashPointCommitOutcome); crashErr != nil {
		err = crashErr
		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing outcome transaction: %w", err)
	}
	s.applyBaselineCachePatch(patch)

	return nil
}

// applyOutcomeTx writes everything outcome changes, in tx: its baseline and
// remote_state rows, its delete-ledger entry, and the end of the
// pending-create records it settles (store_pending_creates.go), which goes
// with the outcome, before the commit's crash point.
func (s *SyncStore) applyOutcomeTx(ctx context.Context, tx sqlTxRunner, outcome *syncdomain.BaselineMutation) error {
	before, err := readBaselineBeforeOutcomeTx(ctx, tx, outcome)
	if err != nil {
		return err
	}
	if applyErr := applySingleMutation(ctx, tx, outcome); applyErr != nil {
		return applyErr
	}
	if ledgerErr := s.recordDeleteLedgerTx(ctx, tx, outcome); ledgerErr != nil {
		return ledgerErr
	}

	return settlePendingCreatesTx(ctx, tx, outcome, before)
}

// RefreshLocalBaseline updates the local-side comparison tuple for one path
// while preserving any existing remote-side metadata. If a matching
// remote_state row exists, it is marked synced in the same transaction.
func (s *SyncStore) RefreshLocalBaseline(ctx context.Context, refresh *localBaselineRefresh) (err error) {
	cache := s.cachedBaseline()
	if cache == nil {
		if _, loadErr := s.Load(ctx); loadErr != nil {
			return fmt.Errorf("syncstore: loading baseline before refresh local baseline: %w", loadErr)
		}
		cache = s.cachedBaseline()
	}

	existing, hasExisting := cache.GetByPath(refresh.Path)

	entry := &syncdomain.BaselineEntry{
		Path:             refresh.Path,
		DriveID:          refresh.DriveID,
		ItemID:           refresh.ItemID,
		ItemType:         refresh.ItemType,
		LocalHash:        refresh.LocalHash,
		LocalSize:        refresh.LocalSize,
		LocalSizeKnown:   refresh.LocalSizeKnown,
		LocalMtime:       refresh.LocalMtime,
		LocalCtime:       refresh.LocalCtime,
		LocalDevice:      refresh.LocalDevice,
		LocalInode:       refresh.LocalInode,
		LocalBirthTime:   refresh.LocalBirthTime,
		LocalHasIdentity: refresh.LocalHasIdentity,
	}

	if hasExisting {
		entry.ParentID = existing.ParentID
		entry.RemoteHash = existing.RemoteHash
		entry.RemoteSize = existing.RemoteSize
		entry.RemoteSizeKnown = existing.RemoteSizeKnown
		entry.RemoteMtime = existing.RemoteMtime
		entry.ETag = existing.ETag
	}

	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("syncstore: beginning refresh local baseline transaction: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, fmt.Sprintf("syncstore: rollback refresh local baseline transaction for %s", refresh.Path))
	}()

	state, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return err
	}
	if ensureErr := s.ensureContentDriveIDTx(ctx, tx, entry.DriveID, state); ensureErr != nil {
		return ensureErr
	}
	if !state.ContentDriveID.IsZero() {
		entry.DriveID = state.ContentDriveID
	}

	if err = upsertBaselineRowTx(ctx, tx, entry); err != nil {
		return fmt.Errorf("syncstore: refreshing baseline for %s: %w", refresh.Path, err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("syncstore: committing refresh local baseline transaction: %w", err)
	}

	s.applyBaselineCachePatch(baselineCachePatch{put: entry})

	return nil
}

// ForgetBaselineLocalCtime clears the stored inode change time of the
// baseline row at path, and nothing else, in the store and in the cached
// baseline. A local delete's content check proved the file no longer holds
// what the row records while its stat facts may all still match, and a row
// without a change time is one the next scan reads instead of trusting
// (CanReuseBaselineHash). A path with no row is left alone.
func (s *SyncStore) ForgetBaselineLocalCtime(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE baseline SET local_ctime = 0 WHERE path = ?`, path); err != nil {
		return fmt.Errorf("syncstore: forgetting the change time of %s: %w", path, err)
	}
	cache := s.cachedBaseline()
	if cache == nil {
		return nil
	}
	if entry, ok := cache.GetByPath(path); ok {
		entry.LocalCtime = 0
		s.applyBaselineCachePatch(baselineCachePatch{put: &entry})
	}

	return nil
}

// classifyMutation is classifyBaselineMutation for one outcome: a
// publication-only baseline update that carries an old path re-keys the row
// where both sides already hold the item (D-050), which is a move of the
// baseline row and its subtree. A delete of an entry no synced item is
// (Action.DeletesUnsyncedEntry) removed nothing on record: the row at its
// path, if any, is the item that lands there, so it changes nothing.
func classifyMutation(o *syncdomain.BaselineMutation) (baselineMutationKind, error) {
	if o.Action == syncdomain.ActionBaselineUpdate && o.OldPath != "" && o.OldPath != o.Path {
		return baselineMutationMove, nil
	}
	if o.DeletesUnsyncedEntry && o.Action == syncdomain.ActionLocalDelete {
		return baselineMutationNoop, nil
	}

	return classifyBaselineMutation(o.Action)
}

func classifyBaselineMutation(action syncdomain.ActionType) (baselineMutationKind, error) {
	switch action {
	case syncdomain.ActionConflictCopy:
		return baselineMutationNoop, nil
	case syncdomain.ActionDownload, syncdomain.ActionUpload, syncdomain.ActionFolderCreate, syncdomain.ActionBaselineUpdate:
		return baselineMutationUpsert, nil
	case syncdomain.ActionLocalDelete, syncdomain.ActionRemoteDelete, syncdomain.ActionCleanup:
		return baselineMutationDelete, nil
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove:
		return baselineMutationMove, nil
	default:
		// The plan and the retry_work rows cannot carry one; refused before
		// any write of the mutation's transaction.
		return 0, fmt.Errorf("syncstore: classifying baseline mutation: %s", syncdomain.OutsideTheEnum(action))
	}
}

// errMutationWithoutItemID reports a successful mutation outcome that has no
// remote item identity to key its baseline row by.
var errMutationWithoutItemID = errors.New("mutation outcome has no item ID")

// applySingleMutation dispatches a single mutation to the appropriate DB helper.
func applySingleMutation(ctx context.Context, tx sqlTxRunner, o *syncdomain.BaselineMutation) error {
	mutation, err := classifyMutation(o)
	if err != nil {
		return err
	}

	// item_id is the baseline's primary key. An upload or move whose Graph
	// response carried no id was written with an empty key, and the next one
	// rewrote that row's path (D-035): refuse it instead of corrupting the
	// baseline.
	if (mutation == baselineMutationUpsert || mutation == baselineMutationMove) && o.ItemID == "" {
		return fmt.Errorf("syncstore: committing %s for %s: %w", o.Action.String(), o.Path, errMutationWithoutItemID)
	}

	switch mutation {
	case baselineMutationNoop:
		err = nil
	case baselineMutationUpsert:
		err = commitUpsert(ctx, tx, o)
	case baselineMutationDelete:
		err = commitDelete(ctx, tx, o.Path)
		if err == nil && o.KeptLocally {
			err = recordKeptLocalFolderTx(ctx, tx, o)
		}
	case baselineMutationMove:
		err = commitMove(ctx, tx, o)
	}

	if err != nil {
		return err
	}

	// Update remote_state in the same transaction.
	return updateRemoteStateOnOutcome(ctx, tx, o)
}

// baselineCachePatch is how one committed transaction's baseline writes
// reach the cache. It is decided inside the transaction, so a fallible step
// (a rebuild's read above all) fails the transaction, and applied after
// COMMIT by applyBaselineCachePatch, which cannot fail. The zero patch
// changes nothing.
type baselineCachePatch struct {
	// reload, when set, replaces every entry: the transaction's own read of
	// the table after its writes.
	reload *baselineIndexes
	put    *syncdomain.BaselineEntry
	remove string
}

// applyBaselineCachePatch applies p to the cache. Without a cache there is
// nothing to patch, except that a rebuild becomes the cache.
func (s *SyncStore) applyBaselineCachePatch(p baselineCachePatch) {
	s.baselineMu.Lock()
	defer s.baselineMu.Unlock()

	switch {
	case p.reload != nil && s.baseline == nil:
		s.baseline = newBaselineCache(*p.reload)
	case p.reload != nil:
		s.baseline.swap(*p.reload)
	case s.baseline == nil:
	case p.put != nil:
		s.baseline.put(p.put)
	case p.remove != "":
		s.baseline.delete(p.remove)
	}
}

// cachePatchForMutationTx decides, inside the outcome's transaction and
// after its writes, what the cache must change for outcome o.
func (s *SyncStore) cachePatchForMutationTx(
	ctx context.Context,
	tx sqlTxRunner,
	o *syncdomain.BaselineMutation,
) (baselineCachePatch, error) {
	mutation, err := classifyMutation(o)
	if err != nil {
		return baselineCachePatch{}, err
	}

	entry := mutationToEntry(o)
	switch mutation {
	case baselineMutationNoop:
		return baselineCachePatch{}, nil
	case baselineMutationUpsert:
		if cache := s.cachedBaseline(); cache != nil && o.ItemType == syncdomain.ItemTypeFolder {
			if prev, ok := cache.GetByPath(o.Path); ok && prev.ItemID != o.ItemID {
				// A folder rebound to a new item (R-2.10.49): commitUpsert also
				// moved its children's parent_id, which a single put cannot
				// mirror. Rebinds are rare, so the cache is rebuilt.
				return s.reloadPatchTx(ctx, tx)
			}
		}
		return baselineCachePatch{put: &entry}, nil
	case baselineMutationDelete:
		return baselineCachePatch{remove: o.Path}, nil
	case baselineMutationMove:
		if isFolderPathMove(o) {
			// The subtree moved too; rebuilding beats re-deriving every
			// descendant path in the cache.
			return s.reloadPatchTx(ctx, tx)
		}
		// put drops the item's previous path itself. OldPath is a path in the
		// moving side's coordinates, which can name another item's baseline
		// row (a divergent rename moves the local copy from a path the
		// baseline never recorded for this item).
		return baselineCachePatch{put: &entry}, nil
	default:
		return baselineCachePatch{}, fmt.Errorf("syncstore: patching the baseline cache for %s: mutation kind %d", o.Path, mutation)
	}
}

// reloadPatchTx reads the whole baseline table as tx sees it, its own writes
// included, as a patch that rebuilds the cache.
func (s *SyncStore) reloadPatchTx(ctx context.Context, tx sqlTxRunner) (baselineCachePatch, error) {
	contentDriveID := s.contentDriveID()
	if contentDriveID.IsZero() {
		var err error
		contentDriveID, err = contentDriveIDForDB(ctx, tx)
		if err != nil {
			return baselineCachePatch{}, fmt.Errorf("syncstore: loading content drive for baseline reload: %w", err)
		}
	}
	entries, err := loadBaselineEntries(ctx, tx, contentDriveID)
	if err != nil {
		return baselineCachePatch{}, fmt.Errorf("syncstore: reading the baseline to rebuild its cache: %w", err)
	}
	idx := indexBaselineEntries(entries)

	return baselineCachePatch{reload: &idx}, nil
}

// mutationToEntry converts a BaselineMutation into a BaselineEntry for cache update.
func mutationToEntry(o *syncdomain.BaselineMutation) syncdomain.BaselineEntry {
	return syncdomain.BaselineEntry{
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
		LocalCtime:       o.LocalCtime,
		LocalDevice:      o.LocalDevice,
		LocalInode:       o.LocalInode,
		LocalBirthTime:   o.LocalBirthTime,
		LocalHasIdentity: o.LocalHasIdentity,
		ETag:             o.ETag,
	}
}

// commitUpsert inserts or updates a baseline entry for download, upload,
// folder create, and update-synced outcomes. Handles the case where a
// server-side delete+recreate assigns a new item_id for an existing path
// by removing the stale row first (prevents UNIQUE constraint violation on path).
func commitUpsert(ctx context.Context, tx sqlTxRunner, o *syncdomain.BaselineMutation) error {
	// A folder rebound to a new item at the same path (R-2.10.49) takes its
	// children with it: their parent_id named the dead item.
	if o.ItemType == syncdomain.ItemTypeFolder {
		if _, err := tx.ExecContext(ctx,
			`UPDATE baseline SET parent_id = ?
			WHERE parent_id IN (SELECT item_id FROM baseline WHERE path = ? AND item_id <> ?)`,
			o.ItemID, o.Path, o.ItemID,
		); err != nil {
			return fmt.Errorf("syncstore: rebinding children of %s: %w", o.Path, err)
		}
	}

	// Remove any stale baseline row at the same path but different identity.
	// This happens when the server assigns a new item_id for a path that
	// was previously tracked under a different ID (delete+recreate, or
	// re-upload after server-side deletion).
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM baseline WHERE path = ? AND item_id <> ?`,
		o.Path, o.ItemID,
	); err != nil {
		return fmt.Errorf("syncstore: clearing stale baseline for %s: %w", o.Path, err)
	}

	entry := mutationToEntry(o)
	if err := upsertBaselineRowTx(ctx, tx, &entry); err != nil {
		return fmt.Errorf("syncstore: upserting baseline for %s: %w", o.Path, err)
	}
	// A synced item at the path supersedes a folder kept there after its
	// online deletion.
	if _, err := tx.ExecContext(ctx, `DELETE FROM kept_local_folders WHERE path = ?`, o.Path); err != nil {
		return fmt.Errorf("syncstore: clearing kept local folder %s: %w", o.Path, err)
	}

	return nil
}

// recordKeptLocalFolderTx remembers a folder deleted on OneDrive that the
// local delete kept because it holds only content sync ignores (D-012). The
// planner does not create a kept folder online again while it is the same
// folder and holds nothing sync sees (R-2.17.2). A folder without filesystem
// identity cannot be told from a new one at the path, so it is not kept.
func recordKeptLocalFolderTx(ctx context.Context, tx sqlTxRunner, o *syncdomain.BaselineMutation) error {
	if !o.LocalHasIdentity {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO kept_local_folders (path, local_device, local_inode, local_birth_time)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			local_device = excluded.local_device,
			local_inode = excluded.local_inode,
			local_birth_time = excluded.local_birth_time`,
		o.Path, int64(o.LocalDevice), int64(o.LocalInode), o.LocalBirthTime,
	); err != nil {
		return fmt.Errorf("syncstore: recording kept local folder %s: %w", o.Path, err)
	}

	return nil
}

// commitDelete removes a baseline entry for delete and cleanup outcomes.
func commitDelete(ctx context.Context, tx sqlTxRunner, path string) error {
	_, err := tx.ExecContext(ctx, sqlDeleteBaseline, path)
	if err != nil {
		return fmt.Errorf("syncstore: deleting baseline for %s: %w", path, err)
	}

	return nil
}

// commitMove atomically updates the path for move outcomes. With the ID-based
// PK, a move is a single UPDATE (not DELETE+INSERT) — the row identity
// (item_id) doesn't change, only the path does.
func commitMove(ctx context.Context, tx sqlTxRunner, o *syncdomain.BaselineMutation) error {
	// The baseline row is the item, keyed by item_id; where it sat before is
	// read from it, not from OldPath. OldPath is the moving side's path, and
	// for a local copy moved to OneDrive's name after a divergent rename it is
	// a path the baseline never recorded for this item.
	var previousPath string
	err := tx.QueryRowContext(ctx, `SELECT path FROM baseline WHERE item_id = ?`, o.ItemID).Scan(&previousPath)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("syncstore: reading baseline path of moved item %s: %w", o.ItemID, err)
	}

	// Upsert handles both the path update and all other field updates.
	// The ON CONFLICT(item_id) clause matches the existing row
	// and updates path + all mutable fields atomically.
	if err := commitUpsert(ctx, tx, o); err != nil {
		return err
	}
	if !isFolderPathMove(o) {
		return nil
	}

	// A folder move moves its whole subtree, on OneDrive and on disk alike,
	// and the planner emits no move for descendants that only follow it. The
	// descendants' baseline rows must follow in the same transaction; left at
	// the old paths, every child looked like a fresh move whose source had
	// vanished and was superseded on every replan.
	if previousPath != "" && previousPath != o.Path {
		if err := repathSubtreeTx(ctx, tx, "baseline", previousPath, o.Path); err != nil {
			return err
		}
	}
	if o.Action == syncdomain.ActionRemoteMove {
		// The local-origin move just moved the remote subtree; remote
		// observation would report the same paths later.
		return repathSubtreeTx(ctx, tx, "remote_state", o.OldPath, o.Path)
	}

	return nil
}

func isFolderPathMove(o *syncdomain.BaselineMutation) bool {
	return o.ItemType == syncdomain.ItemTypeFolder && o.OldPath != "" && o.OldPath != o.Path
}

// repathSubtreeTx rewrites every path strictly below oldPath to sit below
// newPath. Prefix matching uses SQLite's own length() on the bound prefix so
// multi-byte names cut at the same character boundary they are matched on.
// table is always a fixed internal table name, never input.
func repathSubtreeTx(ctx context.Context, tx sqlTxRunner, table, oldPath, newPath string) error {
	oldPrefix := oldPath + "/"
	newPrefix := newPath + "/"
	query := `UPDATE ` + table + `
		SET path = ? || substr(path, length(?) + 1)
		WHERE substr(path, 1, length(?)) = ? COLLATE BINARY`
	if _, err := tx.ExecContext(ctx, query, newPrefix, oldPrefix, oldPrefix, oldPrefix); err != nil {
		return fmt.Errorf("syncstore: moving %s subtree %s -> %s: %w", table, oldPath, newPath, err)
	}

	return nil
}
