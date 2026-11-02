package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// A new file is created on OneDrive in two steps: an empty item first, then
// its content (create by body, R-5.6.14; a path-routed upload session makes
// the same visible 0-byte item, L-34). Until the content commits, that empty
// item is a real OneDrive file. Seen by the next pass without memory of who
// made it, it was a create_conflict: the user's file was renamed to a
// conflict copy and the empty file downloaded in its place.
//
// pending_creates is that memory. The executor records the intent to create
// at the path before the request that creates the item, and the item itself
// (ID and creation eTag) as soon as it exists, before the first content byte
// is sent; observation then recognizes the empty item as this client's own
// placeholder:
//
//   - still empty and not yet synced: it is adopted as a baseline row whose
//     local side is the empty content, so the planner sees "the local file
//     changed" and uploads by ID with If-Match, or, when the local file is
//     gone, deletes the placeholder with If-Match;
//   - still empty after the upload committed (a delta page captured
//     mid-upload): stale, and not recorded as a remote change;
//   - anything else (content, a delete): no longer a placeholder, and the
//     record is dropped.
//
// The item ID and its emptiness identify the placeholder, not its creation
// eTag: opening and then canceling an upload session on the empty item moves
// its eTag although no content landed (live, e2e
// TestE2E_SyncWatch_FileModification). Requiring the creation eTag dropped
// the record there; the item read as someone else's file, every retry of the
// create got 409, and upload-only mode could never deliver the user's file.
// Any content written by anyone makes the item non-empty, and the writes that
// finish a placeholder are conditional on the eTag just read.

// A process can die while the creating request is in flight: OneDrive then
// makes the empty item after the client is gone, and no response ever reaches
// the hook that records it (seen live on 2026-11-01: the item was created two
// seconds after a kill, and the next pass made a conflict copy of the user's
// file). The intent row closes that window: it names the path before the
// request, and an empty file observed at that path within
// pendingCreateIntentTTL of the intent is the placeholder, whose ID is then
// recorded. The intent is cleared when the attempt ends without having
// created anything, and expires so a stale one cannot claim a stranger's
// empty file long after.

// pendingCreateIntentTTL is how long an intent without an item ID can claim
// an empty file observed at its path.
const pendingCreateIntentTTL = 72 * time.Hour

// PendingCreate is one recorded, not yet confirmed create. ItemID is empty
// while only the intent is recorded.
type PendingCreate struct {
	ItemID      string
	DriveID     driveid.ID
	Path        string
	ParentID    string
	CreatedETag string
	LocalSize   int64
	LocalHash   string
	IntendedAt  int64 // Unix nanoseconds; when the create was decided
}

// RecordPendingCreateIntent durably records that a create at pending.Path is
// about to be requested. A row already naming the item at that path keeps
// its item ID and eTag: a retry after a 409 must not forget the placeholder
// it is about to upload into.
func (s *SyncStore) RecordPendingCreateIntent(ctx context.Context, pending *PendingCreate) error {
	if pending == nil || pending.Path == "" {
		return fmt.Errorf("sync: recording a pending create intent: missing path")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_creates
		(path, item_id, drive_id, parent_id, created_etag, local_size, local_hash, intended_at)
		VALUES (?, '', ?, ?, '', ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			drive_id = excluded.drive_id, parent_id = excluded.parent_id,
			local_size = excluded.local_size, local_hash = excluded.local_hash,
			intended_at = excluded.intended_at`,
		pending.Path, pending.DriveID.String(), pending.ParentID,
		pending.LocalSize, pending.LocalHash, s.nowFunc().UnixNano(),
	); err != nil {
		return fmt.Errorf("sync: recording pending create intent at %s: %w", pending.Path, err)
	}

	return nil
}

// RecordPendingCreate durably records a create whose empty item exists.
func (s *SyncStore) RecordPendingCreate(ctx context.Context, pending *PendingCreate) error {
	if pending == nil || pending.ItemID == "" || pending.Path == "" {
		return fmt.Errorf("sync: recording a pending create: missing item ID or path")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_creates
		(path, item_id, drive_id, parent_id, created_etag, local_size, local_hash, intended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			item_id = excluded.item_id, drive_id = excluded.drive_id, parent_id = excluded.parent_id,
			created_etag = excluded.created_etag, local_size = excluded.local_size, local_hash = excluded.local_hash,
			intended_at = CASE WHEN pending_creates.intended_at > 0 THEN pending_creates.intended_at ELSE excluded.intended_at END`,
		pending.Path, pending.ItemID, pending.DriveID.String(), pending.ParentID,
		pending.CreatedETag, pending.LocalSize, pending.LocalHash, s.nowFunc().UnixNano(),
	); err != nil {
		return fmt.Errorf("sync: recording pending create of %s (item %s): %w", pending.Path, pending.ItemID, err)
	}

	return nil
}

// ClearPendingCreate drops the record of itemID.
func (s *SyncStore) ClearPendingCreate(ctx context.Context, itemID string) error {
	return deletePendingCreateTx(ctx, s.db, itemID)
}

// ClearPendingCreateIntent drops an intent at path that never became an
// item: the attempt ended, by success or failure, without the create hook
// firing, so nothing on OneDrive is ours to recognize there. A row that
// already names an item is kept.
func (s *SyncStore) ClearPendingCreateIntent(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, path); err != nil {
		return fmt.Errorf("sync: clearing pending create intent at %s: %w", path, err)
	}

	return nil
}

// PendingCreateAt returns the recorded create at path; found is false when
// there is none.
func (s *SyncStore) PendingCreateAt(ctx context.Context, path string) (pending PendingCreate, found bool, err error) {
	return scanPendingCreate(s.db.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE path = ?`, path))
}

const sqlSelectPendingCreate = `SELECT item_id, drive_id, path, parent_id, created_etag, local_size, local_hash, intended_at
	FROM pending_creates`

type pendingCreateExecer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func deletePendingCreateTx(ctx context.Context, tx pendingCreateExecer, itemID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("sync: clearing pending create of item %s: %w", itemID, err)
	}

	return nil
}

func scanPendingCreate(row *sql.Row) (PendingCreate, bool, error) {
	var (
		pending PendingCreate
		driveID string
	)
	err := row.Scan(&pending.ItemID, &driveID, &pending.Path, &pending.ParentID,
		&pending.CreatedETag, &pending.LocalSize, &pending.LocalHash, &pending.IntendedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return PendingCreate{}, false, nil
	case err != nil:
		return PendingCreate{}, false, fmt.Errorf("sync: reading pending create: %w", err)
	}
	pending.DriveID = driveid.New(driveID)

	return pending, true, nil
}

// freeBaselinePathForPlaceholderTx reports whether our placeholder can take
// path in the baseline. A baseline row there whose item OneDrive still holds
// at that path keeps it. One whose item OneDrive deleted or moved away (often
// in this very window: a folder replaced by a file, or a file renamed online
// while edited here, then the new file's create) is synced agreement about a
// name OneDrive had already freed, or the create could not have succeeded:
// that row is dropped so the placeholder is adopted. The item itself, if it
// still exists elsewhere, reads as a new remote item and is downloaded, so
// nothing is lost. Kept, the row turned our own empty item into a create
// conflict: the user's file renamed to a conflict copy and the empty item
// downloaded in its place (found by the chaos Graph layer, D-092).
func freeBaselinePathForPlaceholderTx(ctx context.Context, tx sqlTxRunner, path string) (bool, error) {
	var (
		occupantID string
		stillThere int
	)
	switch err := tx.QueryRowContext(ctx, `SELECT b.item_id,
			(SELECT count(*) FROM remote_state r WHERE r.item_id = b.item_id AND r.path = b.path)
		FROM baseline b WHERE b.path = ?`, path).Scan(&occupantID, &stillThere); {
	case errors.Is(err, sql.ErrNoRows):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("sync: checking baseline at %s: %w", path, err)
	case stillThere > 0:
		return false, nil
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM baseline WHERE item_id = ?`, occupantID); err != nil {
		return false, fmt.Errorf("sync: dropping the baseline of %s, which OneDrive no longer has at %s: %w", occupantID, path, err)
	}

	return true, nil
}

// placeholderDecision is what observation does with an observed item that has
// a pending-create record.
type placeholderDecision int

const (
	placeholderNotPending placeholderDecision = iota // no record: observe normally
	placeholderAdopt                                 // our untouched placeholder, not yet synced
	placeholderStale                                 // our placeholder state, after the upload committed
	placeholderResolved                              // no longer a placeholder: drop the record
)

// classifyObservedPlaceholderTx decides how observation treats item when it
// has a pending-create record.
// now decides whether an intent without an item ID is still fresh enough to
// claim an empty file observed at its path.
func classifyObservedPlaceholderTx(
	ctx context.Context, tx sqlTxRunner, item *observedItem, now time.Time,
) (*PendingCreate, placeholderDecision, error) {
	pending, found, err := scanPendingCreate(tx.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE item_id = ?`, item.ItemID))
	if err != nil {
		return nil, placeholderNotPending, err
	}
	if !found {
		pending, found, err = claimIntendedPlaceholderTx(ctx, tx, item, now)
		if err != nil || !found {
			return nil, placeholderNotPending, err
		}
	}
	if item.IsDeleted || item.Size != 0 {
		return &pending, placeholderResolved, nil
	}

	var remoteSize sql.NullInt64
	scanErr := tx.QueryRowContext(ctx, `SELECT remote_size FROM baseline WHERE item_id = ?`, item.ItemID).Scan(&remoteSize)
	switch {
	case errors.Is(scanErr, sql.ErrNoRows):
		return &pending, placeholderAdopt, nil
	case scanErr != nil:
		return nil, placeholderNotPending, fmt.Errorf("sync: reading baseline of pending create %s: %w", item.ItemID, scanErr)
	case remoteSize.Valid && remoteSize.Int64 > 0:
		return &pending, placeholderStale, nil
	default:
		// Already adopted: the baseline holds the placeholder itself.
		return &pending, placeholderNotPending, nil
	}
}

// adoptPlaceholderTx records our placeholder as synced empty content, so the
// local file reads as changed (upload by ID) or deleted (delete by ID). The
// adopted entry is appended to adopted for the in-memory baseline; nothing is
// adopted when the path is already taken.
func adoptPlaceholderTx(
	ctx context.Context, tx sqlTxRunner, item *observedItem, pending *PendingCreate, adopted *[]*BaselineEntry,
) error {
	if item.Path != pending.Path {
		// Moved or renamed meanwhile: not the create this record describes.
		return deletePendingCreateTx(ctx, tx, item.ItemID)
	}
	if free, err := freeBaselinePathForPlaceholderTx(ctx, tx, item.Path); err != nil || !free {
		return err
	}

	entry := &BaselineEntry{
		Path: item.Path, DriveID: item.DriveID, ItemID: item.ItemID, ParentID: pending.ParentID,
		ItemType: ItemTypeFile, LocalHash: item.Hash, RemoteHash: item.Hash,
		LocalSize: 0, LocalSizeKnown: true, RemoteSize: 0, RemoteSizeKnown: true,
		RemoteMtime: item.Mtime, ETag: item.ETag,
	}
	if err := upsertBaselineRowTx(ctx, tx, entry); err != nil {
		return fmt.Errorf("sync: adopting placeholder %s at %s: %w", item.ItemID, item.Path, err)
	}
	*adopted = append(*adopted, entry)

	return nil
}

// claimIntendedPlaceholderTx matches an observed item against an intent at
// its path that has no item ID yet. An empty, live file observed there
// within pendingCreateIntentTTL of the intent is the placeholder the dying
// process created: the row takes its ID and eTag. Anything else at the path
// (content, a folder, a deletion) or an expired intent means no placeholder
// of ours is there, and the intent is dropped.
func claimIntendedPlaceholderTx(
	ctx context.Context, tx sqlTxRunner, item *observedItem, now time.Time,
) (PendingCreate, bool, error) {
	pending, found, err := scanPendingCreate(tx.QueryRowContext(ctx,
		sqlSelectPendingCreate+` WHERE path = ? AND item_id = ''`, item.Path))
	if err != nil || !found {
		return PendingCreate{}, false, err
	}
	fresh := pending.IntendedAt > 0 && now.Sub(time.Unix(0, pending.IntendedAt)) <= pendingCreateIntentTTL
	isEmptyFile := !item.IsDeleted && item.ItemType == ItemTypeFile && !item.SizeUnknown && item.Size == 0
	if !fresh || !isEmptyFile {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, item.Path); err != nil {
			return PendingCreate{}, false, fmt.Errorf("sync: dropping pending create intent at %s: %w", item.Path, err)
		}
		return PendingCreate{}, false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pending_creates SET item_id = ?, created_etag = ? WHERE path = ? AND item_id = ''`,
		item.ItemID, item.ETag, item.Path); err != nil {
		return PendingCreate{}, false, fmt.Errorf("sync: claiming placeholder %s for the intent at %s: %w", item.ItemID, item.Path, err)
	}
	pending.ItemID = item.ItemID
	pending.CreatedETag = item.ETag

	return pending, true, nil
}
