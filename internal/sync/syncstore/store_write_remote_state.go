// The remote_state update an outcome commits in the same transaction as its
// baseline mutation.

package syncstore

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// updateRemoteStateOnOutcome updates the remote mirror when execution produces
// authoritative new remote truth.
func updateRemoteStateOnOutcome(ctx context.Context, tx sqlTxRunner, o *syncdomain.BaselineMutation) error {
	if o.ItemID == "" || o.DriveID.IsZero() {
		return nil
	}

	switch o.Action {
	case syncdomain.ActionFolderCreate:
		// A folder created online is new remote truth. A folder created
		// locally mirrors an item remote_state already holds, at the path
		// OneDrive reports; the local path the create used can differ (a
		// parent that sits elsewhere locally until its own move runs), and
		// writing it here moved the item in remote_state to a path OneDrive
		// never had.
		_, err := tx.ExecContext(ctx,
			`INSERT INTO remote_state (
				drive_id, item_id, path, item_type, hash, size, mtime, etag
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(item_id) DO NOTHING`,
			o.DriveID.String(), o.ItemID, o.Path, o.ItemType,
			nullString(o.RemoteHash), nullKnownInt64(o.RemoteSize, o.RemoteSizeKnown), nullOptionalInt64(o.RemoteMtime),
			nullString(o.ETag),
		)
		if err != nil {
			return fmt.Errorf("syncstore: recording remote_state for folder create %s: %w", o.Path, err)
		}
	case syncdomain.ActionUpload:
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
			return fmt.Errorf("syncstore: updating remote_state for upload %s: %w", o.Path, err)
		}
	case syncdomain.ActionConflictCopy:
		return nil
	case syncdomain.ActionRemoteDelete:
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM remote_state WHERE item_id = ?`,
			o.ItemID,
		); err != nil {
			return fmt.Errorf("syncstore: deleting remote_state for remote delete %s: %w", o.Path, err)
		}
	case syncdomain.ActionRemoteMove:
		if _, err := tx.ExecContext(ctx,
			`UPDATE remote_state
			 SET drive_id = ?, path = ?
			 WHERE item_id = ?`,
			o.DriveID.String(), o.Path, o.ItemID,
		); err != nil {
			return fmt.Errorf("syncstore: updating remote_state for remote move %s: %w", o.Path, err)
		}
	case syncdomain.ActionDownload,
		syncdomain.ActionLocalDelete,
		syncdomain.ActionLocalMove,
		syncdomain.ActionBaselineUpdate,
		syncdomain.ActionCleanup:
		return nil
	default:
		// classifyMutation refuses this first, inside the same transaction.
		return fmt.Errorf("syncstore: updating remote_state for %s: %s", o.Path, syncdomain.OutsideTheEnum(o.Action))
	}

	return nil
}
