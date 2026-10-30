package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// A new file is created on OneDrive in two steps: an empty item first, then
// its content (create by body, R-5.6.14; a path-routed upload session makes
// the same visible 0-byte item, L-34). Until the content commits, that empty
// item is a real OneDrive file. Seen by the next pass without memory of who
// made it, it was a create_conflict: the user's file was renamed to a
// conflict copy and the empty file downloaded in its place.
//
// pending_creates is that memory. The executor records the item before the
// first content byte is sent; observation then recognizes the empty item as
// this client's own placeholder:
//
//   - still empty with its creation eTag and not yet synced: it is adopted as
//     a baseline row whose local side is the empty content, so the planner
//     sees "the local file changed" and uploads by ID with If-Match, or, when
//     the local file is gone, deletes the placeholder with If-Match;
//   - still empty with its creation eTag after the upload committed (a delta
//     page captured mid-upload): stale, and not recorded as a remote change;
//   - anything else (new content, another eTag, a delete): no longer a
//     placeholder, and the record is dropped.

// PendingCreate is one recorded, not yet confirmed create.
type PendingCreate struct {
	ItemID      string
	DriveID     driveid.ID
	Path        string
	ParentID    string
	CreatedETag string
	LocalSize   int64
	LocalHash   string
}

// RecordPendingCreate durably records a create whose empty item exists.
func (s *SyncStore) RecordPendingCreate(ctx context.Context, pending *PendingCreate) error {
	if pending == nil || pending.ItemID == "" || pending.Path == "" {
		return fmt.Errorf("sync: recording a pending create: missing item ID or path")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_creates
		(item_id, drive_id, path, parent_id, created_etag, local_size, local_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(item_id) DO UPDATE SET
			drive_id = excluded.drive_id, path = excluded.path, parent_id = excluded.parent_id,
			created_etag = excluded.created_etag, local_size = excluded.local_size, local_hash = excluded.local_hash`,
		pending.ItemID, pending.DriveID.String(), pending.Path, pending.ParentID,
		pending.CreatedETag, pending.LocalSize, pending.LocalHash,
	); err != nil {
		return fmt.Errorf("sync: recording pending create of %s (item %s): %w", pending.Path, pending.ItemID, err)
	}

	return nil
}

// ClearPendingCreate drops the record of itemID.
func (s *SyncStore) ClearPendingCreate(ctx context.Context, itemID string) error {
	return deletePendingCreateTx(ctx, s.db, itemID)
}

// PendingCreateAt returns the recorded create at path; found is false when
// there is none.
func (s *SyncStore) PendingCreateAt(ctx context.Context, path string) (pending PendingCreate, found bool, err error) {
	return scanPendingCreate(s.db.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE path = ?`, path))
}

const sqlSelectPendingCreate = `SELECT item_id, drive_id, path, parent_id, created_etag, local_size, local_hash
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
		&pending.CreatedETag, &pending.LocalSize, &pending.LocalHash)
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
func classifyObservedPlaceholderTx(ctx context.Context, tx sqlTxRunner, item *observedItem) (*PendingCreate, placeholderDecision, error) {
	pending, found, err := scanPendingCreate(tx.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE item_id = ?`, item.ItemID))
	if err != nil || !found {
		return nil, placeholderNotPending, err
	}
	if item.IsDeleted || item.Size != 0 || item.ETag != pending.CreatedETag {
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
	if _, err := tx.ExecContext(ctx, sqlUpsertBaseline,
		entry.ItemID, entry.Path, nullString(entry.ParentID), entry.ItemType,
		nullString(entry.LocalHash), nullString(entry.RemoteHash),
		int64(0), int64(0), nil, nullOptionalInt64(entry.RemoteMtime),
		0, 0, 0, 0, nullString(entry.ETag),
	); err != nil {
		return fmt.Errorf("sync: adopting placeholder %s at %s: %w", item.ItemID, item.Path, err)
	}
	*adopted = append(*adopted, entry)

	return nil
}
