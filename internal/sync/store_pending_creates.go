package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
//   - still empty and already adopted: the row follows the item's eTag and
//     modification time, which move without content, so a drive that gives
//     empty files no hash never reads them as a remote edit;
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
// file). A process that lives can lose the response as well: the request
// reaches OneDrive and its answer is lost on the way back (live, 2026-11-11,
// during a network outage). The intent row closes that window: it names the
// path before the request, and an empty file observed at that path within
// pendingCreateIntentTTL of the intent, and not already synced, is the
// placeholder, whose ID is then recorded.
//
// The intent is cleared when the attempt ends with nothing left to recognize:
// OneDrive refused the create, the attempt failed before it asked, or the
// file was complete as created (a zero-byte file, whose create hook never
// runs). It is kept when the attempt was canceled, and when its create ended
// without an answer (graph.ErrCreateOutcomeUnknown: no response, a 5xx, a
// 408): the create may have landed, or may still land.
//
// A kept intent ends in one of three ways. Observation claims the empty file
// at its path, or meets something else there and drops it. A retry of the
// create claims it on a 409. Or no create at that path is ever retried and
// no item appears there: the create never reached OneDrive and the user then
// renamed or deleted the file. Such an intent stayed for good, and while
// fresh it could claim an empty file a stranger made at that path, which the
// next pass then deleted since the local file was gone (model test, chaos
// seeds 9 and 11). The step after planning therefore prunes intents
// (pruneAbandonedCreateIntents). One past pendingCreateIntentTTL claims
// nothing and goes after any plan. One whose path a complete local scan
// finds empty goes only after a pass that observed OneDrive before the local
// tree (a one-shot sync, a watch bootstrap): that observation ran first, so
// an intent whose create landed has been claimed by then and names its item.
// A watch replan observes the local tree only, so a long-running watch keeps
// such an intent: the change batch that shows a landed create claims it, and
// one that never landed goes at the next start or once it expires. A create
// that lands after its intent was dropped shows up as an empty remote file
// and is downloaded, the lesser harm.
//
// Observation is one of two places that claim the file of a kept intent. The
// executor is the other: a retry of the create gets 409 from the file the
// first attempt made, and, given an intent that was there before the retry,
// it records that empty file as the pending create and uploads into it by ID
// with If-Match (resolveCreateConflict in executor_transfer.go). Both ask the
// same of the file: at the intent's path, live, a file, empty, within the
// TTL (PendingCreate.intentIsFresh). Observation also passes over a file the
// baseline already holds (claimIntendedPlaceholderTx). A 409 without such an
// intent never claims anything.

// pendingCreateIntentTTL is how long an intent without an item ID can claim
// an empty file observed at its path. Past it the intent is pruned.
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
	IntendedAt  int64 // when the create was decided, as storedTimeNano encodes it
}

// intentIsFresh reports whether the intent was recorded within
// pendingCreateIntentTTL of now, so that an empty file at its path can still
// be the file its create made. Observation's claim, the executor's claim on a
// 409 and the prune all ask this one question.
func (p *PendingCreate) intentIsFresh(now time.Time) bool {
	return storedTimeIsSet(p.IntendedAt) && now.Sub(time.Unix(0, p.IntendedAt)) <= pendingCreateIntentTTL
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
		pending.LocalSize, pending.LocalHash, storedTimeNano(s.nowFunc()),
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
			intended_at = CASE WHEN pending_creates.intended_at <> 0 THEN pending_creates.intended_at ELSE excluded.intended_at END`,
		pending.Path, pending.ItemID, pending.DriveID.String(), pending.ParentID,
		pending.CreatedETag, pending.LocalSize, pending.LocalHash, storedTimeNano(s.nowFunc()),
	); err != nil {
		return fmt.Errorf("sync: recording pending create of %s (item %s): %w", pending.Path, pending.ItemID, err)
	}

	return nil
}

// ClearPendingCreateIntent drops an intent at path that never became an
// item: the attempt ended, by success or by a failure that says nothing was
// created, without the create hook firing (it fires for a failure that names
// a created item too), without being canceled and without leaving the outcome
// of its create unknown, so nothing on OneDrive is ours to recognize there. A
// row that already names an item is kept.
func (s *SyncStore) ClearPendingCreateIntent(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, path); err != nil {
		return fmt.Errorf("sync: clearing pending create intent at %s: %w", path, err)
	}

	return nil
}

// passObservation is what a pass observed before it planned. It bounds what
// the prune of create intents may conclude from that plan's truth.
type passObservation int

const (
	// passRefreshedLocalOnly is a watch replan: it refreshes local truth and
	// plans on the OneDrive changes the observers committed before it. The
	// empty file of a create whose answer was lost may not have been in a
	// change batch yet, so nothing is concluded from its absence.
	passRefreshedLocalOnly passObservation = iota
	// passObservedRemoteThenLocal is a one-shot sync or a watch bootstrap:
	// OneDrive is observed, then the local tree, then the pass plans. Every
	// empty file of a create that landed before the pass has been claimed by
	// the time the prune runs.
	passObservedRemoteThenLocal
)

// claimedLandedCreates reports whether the pass's own remote observation ran
// before its plan, and so claimed the empty file of every create that landed.
func (o passObservation) claimedLandedCreates() bool {
	switch o {
	case passObservedRemoteThenLocal:
		return true
	case passRefreshedLocalOnly:
		return false
	default:
		return false
	}
}

// pruneAbandonedCreateIntents deletes the intents without an item ID that no
// create will claim any more. It runs once a plan has been built, after the
// observations that plan was built from were committed, so a row that names
// an item is never touched here (observation drops it once the item holds
// content or is gone).
//
//   - Past pendingCreateIntentTTL an intent claims nothing, so it is deleted
//     whatever the pass observed.
//   - After a pass that observed OneDrive before the local tree, an intent
//     whose file is gone is deleted: local truth is complete, the scan visits
//     the path, local and remote truth are both available there, and
//     local_state holds no entry at it. That pass's remote observation
//     claimed the file of a create that landed, so no create at that path
//     will be retried or claimed, and the intent could only claim a
//     stranger's empty file.
//
// A watch replan observes the local tree only, so a long-running watch keeps
// a fresh intent whose file is gone: the change batch that shows a landed
// create claims it and the next replan deletes the placeholder, and an intent
// whose create never landed goes at the next start or once it expires.
//
// scanned reports whether a full local scan visits path at all: a path the
// content filter hides or a shortcut root reserves is never in local_state,
// so its absence there proves nothing. Read boundaries on either side, case
// collisions and other observation issues that block truth keep an intent the
// same way, as suspect local truth keeps every one.
func (s *SyncStore) pruneAbandonedCreateIntents(
	ctx context.Context, observed passObservation, scanned func(path string) bool,
) (err error) {
	tx, err := beginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning the prune of abandoned create intents: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback the prune of abandoned create intents")
	}()

	intents, err := listCreateIntentsTx(ctx, tx)
	if err != nil || len(intents) == 0 {
		return err
	}
	fileGone, err := createIntentFileGoneTx(ctx, tx, observed, scanned)
	if err != nil {
		return err
	}

	now := s.nowFunc()
	for i := range intents {
		intent := &intents[i]
		expired := !intent.intentIsFresh(now)
		abandoned := expired
		if !expired {
			if abandoned, err = fileGone(intent.Path); err != nil {
				return err
			}
		}
		if !abandoned {
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, intent.Path); err != nil {
			return fmt.Errorf("sync: dropping the abandoned create intent at %s: %w", intent.Path, err)
		}
		s.logger.Debug("dropped a create intent no create will claim",
			slog.String("path", intent.Path), slog.Bool("expired", expired))
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing the prune of abandoned create intents: %w", err)
	}

	return nil
}

// createIntentFileGoneTx returns the test of whether the committed truth of
// a pass proves that the file of an intent at a path is gone. It proves
// nothing anywhere when the pass did not observe OneDrive before it planned
// or local truth is not complete, and nothing at a path the scan does not
// visit or whose local or remote truth is unavailable.
func createIntentFileGoneTx(
	ctx context.Context, tx sqlTxRunner, observed passObservation, scanned func(path string) bool,
) (func(path string) (bool, error), error) {
	provesNothing := func(string) (bool, error) { return false, nil }
	if !observed.claimedLandedCreates() {
		return provesNothing, nil
	}
	state, err := readObservationStateFromTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !state.LocalTruthComplete {
		return provesNothing, nil
	}
	issues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("sync: reading observation issues for the create intents: %w", err)
	}
	availability := newTruthAvailabilityIndex(issues)

	return func(path string) (bool, error) {
		status := availability.StatusForPath(path)
		if !scanned(path) || !status.Local.IsAvailable() || !status.Remote.IsAvailable() {
			return false, nil
		}

		return localStateIsEmptyAtTx(ctx, tx, path)
	}, nil
}

// listCreateIntentsTx returns the rows that record only an intent.
func listCreateIntentsTx(ctx context.Context, tx sqlTxRunner) ([]PendingCreate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path, intended_at FROM pending_creates WHERE item_id = '' ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("sync: listing create intents: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var intents []PendingCreate
	for rows.Next() {
		var intent PendingCreate
		if err := rows.Scan(&intent.Path, &intent.IntendedAt); err != nil {
			return nil, fmt.Errorf("sync: reading a create intent: %w", err)
		}
		intents = append(intents, intent)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sync: listing create intents: %w", err)
	}

	return intents, nil
}

// localStateIsEmptyAtTx reports whether local_state holds no entry at path.
func localStateIsEmptyAtTx(ctx context.Context, tx sqlTxRunner, path string) (bool, error) {
	var entries int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM local_state WHERE path = ?`, path).Scan(&entries); err != nil {
		return false, fmt.Errorf("sync: reading local_state at %s: %w", path, err)
	}

	return entries == 0, nil
}

// PendingCreateAt returns the recorded create at path; found is false when
// there is none.
func (s *SyncStore) PendingCreateAt(ctx context.Context, path string) (pending PendingCreate, found bool, err error) {
	return scanPendingCreate(s.db.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE path = ?`, path))
}

const sqlSelectPendingCreate = `SELECT item_id, drive_id, path, parent_id, created_etag, local_size, local_hash, intended_at
	FROM pending_creates`

func deletePendingCreateTx(ctx context.Context, tx sqlTxRunner, itemID string) error {
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
	placeholderAdopted                               // our placeholder, adopted and still empty
	placeholderStale                                 // our placeholder state, after the upload committed
	placeholderResolved                              // no longer a placeholder: drop the record
)

// classifyObservedPlaceholderTx decides how observation treats item when it
// has a pending-create record. now decides whether an intent without an item
// ID is still fresh enough to claim an empty file observed at its path.
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
		return &pending, placeholderAdopted, nil
	}
}

// followAdoptedPlaceholderTx keeps the baseline row of an adopted placeholder
// in step with the item while it stays empty at its path. Its eTag moves
// without content (an upload session opened and canceled on the empty item
// does that, live, e2e TestE2E_SyncWatch_FileModification), and its
// modification time may move with it. Where neither side carries a hash (a
// zero-byte file on Business), the comparison reads those as a remote edit,
// and with the local file changed that was an edit-edit conflict: the user's
// file renamed to a conflict copy and the empty file downloaded in its place.
// Only the row's eTag and remote modification time follow; its content facts
// stay "synced empty content", so the planner still uploads into the item by
// ID. The updated row is appended to adopted for the in-memory baseline.
//
// An observation whose size Graph did not report (R-2.4.16) is not followed:
// the item may hold content by now, and a row that took its eTag would let
// the next upload by ID overwrite it.
func followAdoptedPlaceholderTx(
	ctx context.Context, tx sqlTxRunner, item *observedItem, pending *PendingCreate, adopted *[]*BaselineEntry,
) error {
	if item.Path != pending.Path || item.SizeUnknown {
		// Moved or renamed meanwhile, or not known to be empty: observed like
		// any item.
		return nil
	}

	entry, found, err := baselineEntryByItemIDTx(ctx, tx, item.ItemID, item.DriveID)
	if err != nil || !found {
		return err
	}
	if entry.Path != item.Path || !entry.RemoteSizeKnown || entry.RemoteSize != 0 ||
		(entry.ETag == item.ETag && entry.RemoteMtime == item.Mtime) {
		return nil
	}

	entry.ETag, entry.RemoteMtime = item.ETag, item.Mtime
	if upsertErr := upsertBaselineRowTx(ctx, tx, entry); upsertErr != nil {
		return fmt.Errorf("sync: following placeholder %s at %s: %w", item.ItemID, item.Path, upsertErr)
	}
	*adopted = append(*adopted, entry)

	return nil
}

// baselineEntryByItemIDTx reads the baseline row of itemID; found is false
// when it has none.
func baselineEntryByItemIDTx(
	ctx context.Context, tx sqlTxRunner, itemID string, driveID driveid.ID,
) (entry *BaselineEntry, found bool, err error) {
	rows, err := tx.QueryContext(ctx, sqlLoadBaseline+` WHERE item_id = ?`, itemID)
	if err != nil {
		return nil, false, fmt.Errorf("sync: reading baseline of %s: %w", itemID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	for rows.Next() {
		entry, err = scanBaselineRow(rows, driveID)
		if err != nil {
			return nil, false, err
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		return nil, false, fmt.Errorf("sync: reading baseline of %s: %w", itemID, iterErr)
	}

	return entry, entry != nil, nil
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
// within pendingCreateIntentTTL of the intent, and not already synced, is the
// placeholder the dying process created: the row takes its ID and eTag.
// Anything else at the path (content, a folder, a deletion) or an expired
// intent means no placeholder of ours is there, and the intent is dropped.
//
// So does an empty file the baseline already holds: the client knows it, so
// it is no create whose answer was lost. It is a zero-byte file whose create
// completed (such a create never runs the hook) while its intent outlived it,
// because the attempt was canceled as it succeeded or the clear failed.
// Claimed, the record named a file that stays empty, and nothing dropped it.
func claimIntendedPlaceholderTx(
	ctx context.Context, tx sqlTxRunner, item *observedItem, now time.Time,
) (PendingCreate, bool, error) {
	pending, found, err := scanPendingCreate(tx.QueryRowContext(ctx,
		sqlSelectPendingCreate+` WHERE path = ? AND item_id = ''`, item.Path))
	if err != nil || !found {
		return PendingCreate{}, false, err
	}
	isEmptyFile := !item.IsDeleted && item.ItemType == ItemTypeFile && !item.SizeUnknown && item.Size == 0
	claimable := pending.intentIsFresh(now) && isEmptyFile
	if claimable {
		synced, syncedErr := baselineHoldsItemTx(ctx, tx, item.ItemID)
		if syncedErr != nil {
			return PendingCreate{}, false, syncedErr
		}
		claimable = !synced
	}
	if !claimable {
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

// baselineHoldsItemTx reports whether the baseline has a row for itemID.
func baselineHoldsItemTx(ctx context.Context, tx sqlTxRunner, itemID string) (bool, error) {
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM baseline WHERE item_id = ?`, itemID).Scan(&rows); err != nil {
		return false, fmt.Errorf("sync: reading the baseline of %s: %w", itemID, err)
	}

	return rows > 0, nil
}
