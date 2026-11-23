package syncstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A new file is created on OneDrive in two steps: an empty item first, then
// its content (create by body, R-5.6.14; a path-routed upload session makes
// the same visible 0-byte item, L-34). Until the content commits, that empty
// item is a real OneDrive file. Seen by the next pass without memory of who
// made it, it was a create_conflict: the user's file was renamed to a
// conflict copy and the empty file downloaded in its place.
//
// pending_creates is that memory, one row per path. One rule holds for the
// whole life of a row: it ends at the first moment its file can no longer
// receive this client's upload, and no remote change is ever swallowed, and
// no item adopted, because a row exists.
//
// A row is written in five places: the intent, before the request that
// creates the item (uploadNewFile); the item itself, ID and creation eTag,
// once the create made it and before the first content byte is sent (the
// create hook, also for a session request that failed after the create, and
// a retry's claim of the empty file of a create whose answer was lost,
// claimLostCreate); observation's claim of the empty file at an intent's path
// (claimIntendedPlaceholderTx); observation moving the row of an adopted
// placeholder renamed online while still empty (followAdoptedPlaceholderTx);
// and the commit of a move outcome, which moves the row of the item it moves
// and of everything below a folder it moves (movePendingCreatesTx). A row
// moved onto a path that already holds one ends instead: two rows never name
// one item, and none stays at a path its item left.
// Observation recognizes the empty item as this client's own placeholder
// while it is known to be empty:
//
//   - not yet synced: it is adopted as a baseline row whose local side is the
//     empty content, so the planner sees "the local file changed" and uploads
//     by ID with If-Match, or, when the local file is gone, deletes the
//     placeholder with If-Match;
//   - already adopted: the row follows the item's eTag and modification time,
//     which move without content, so a drive that gives empty files no hash
//     never reads them as a remote edit;
//   - after an upload with content committed (a delta page captured
//     mid-upload): stale, and not recorded as a remote change.
//
// An observation whose size Graph did not report (R-2.4.16) says nothing
// about emptiness: the item is observed like any item and keeps its row.
//
// A row ends, at the moment its file can no longer receive this client's
// upload, in one of these ways:
//
//   - observation sees the item hold content or be deleted, or meets it moved
//     before it was adopted (adoptPlaceholderTx), or refuses an intent's
//     claim (claimIntendedPlaceholderTx), or drops the baseline row of an item
//     OneDrive no longer holds at the path a new placeholder takes, and that
//     item's row with it (freeBaselinePathForPlaceholderTx);
//   - the commit of an outcome, in its own transaction
//     (settlePendingCreatesTx): a file outcome whose committed remote content
//     is empty drops the row at its path and the row naming its item (nothing
//     is left to upload, and an echo of an empty file is no change), and an
//     outcome that removes an item from the baseline drops the row naming
//     that item. An upload with content keeps its row for the stale echo above
//     until observation sees the content;
//   - the attempt: it clears its intent when it is known to have created
//     nothing or succeeded without the hook (a zero-byte file), and drops the
//     row of an item the cleanup removed (ForgetRemovedPlaceholder);
//   - the step after planning (PrunePendingCreates): an intent past
//     PendingCreateIntentTTL, an intent whose file is gone after a pass that
//     observed OneDrive first, the row of an adopted placeholder whose local
//     file is empty too, and the row naming an item at a path the local scan
//     does not visit (the content filter hides it, a shortcut root reserves
//     it), where this engine plans nothing;
//   - in watch mode, an intent whose file is gone once a remote poll that
//     began after the last create attempt at its path ended has been applied
//     (DropGoneCreateIntentsAt, watchCreateAttempts).
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
// PendingCreateIntentTTL of the intent, and not already synced, is the
// placeholder, whose ID is then recorded.
//
// The intent is cleared when the attempt ends with nothing left to recognize:
// OneDrive refused the create, the attempt failed before it asked, or the
// file was complete as created (a zero-byte file, whose create hook never
// runs; that clear is not skipped when a shutdown lands as the create
// succeeds). It is kept when an attempt that did not succeed was canceled,
// and when its create ended without an answer (graph.ErrCreateOutcomeUnknown:
// no response, a 5xx, a 408): the create may have landed, or may still land.
// A later attempt that fails short of a 409 (a 429, a request that never
// left) keeps such an earlier intent too: it created nothing itself, but it
// cannot tell whether the earlier create did.
//
// A kept intent ends in one of these ways. Observation claims the empty file
// at its path, or meets something else there and drops it. A retry of the
// create claims it on a 409. Or no create at that path is ever retried and
// no item appears there: the create never reached OneDrive and the user then
// renamed or deleted the file. Such an intent stayed for good, and while
// fresh it could claim an empty file a stranger made at that path, which the
// next pass then deleted since the local file was gone (model test, chaos
// seeds 9 and 11). The step after planning therefore prunes intents. One
// past PendingCreateIntentTTL claims nothing and goes after any plan. One
// whose path a complete local scan finds empty goes after a pass that
// observed OneDrive before the local tree (a one-shot sync, a watch
// bootstrap): that observation ran first, so an intent whose create landed
// has been claimed by then and names its item. A watch replan observes the
// local tree only and keeps such an intent; the watch loop drops it once a
// remote poll that began after the last create attempt at the path ended has
// been applied without claiming it, a poll that returned no items included.
// A create that lands after its intent was dropped (its change shows only in
// a later poll) shows up as an empty remote file and is downloaded, the
// lesser harm.
//
// Observation is one of two places that claim the file of a kept intent. The
// executor is the other: a retry of the create gets 409 from the file the
// first attempt made, and, given an intent that was there before the retry,
// it records that empty file as the pending create and uploads into it by ID
// with If-Match (resolveCreateConflict in executor_transfer.go). Both ask the
// same of the file: at the intent's path, live, a file, empty, within the
// TTL (PendingCreate.IntentIsFresh). Observation also passes over a file the
// baseline already holds, and waits on a file whose size is unknown
// (claimIntendedPlaceholderTx). A 409 without such an intent never claims
// anything.

// RecordPendingCreateIntent durably records that a create at pending.Path is
// about to be requested. A row already naming the item at that path keeps
// its item ID and eTag: a retry after a 409 must not forget the placeholder
// it is about to upload into.
func (s *SyncStore) RecordPendingCreateIntent(ctx context.Context, pending *syncdomain.PendingCreate) error {
	if pending == nil || pending.Path == "" {
		return errors.New("sync: recording a pending create intent: missing path")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_creates
		(path, item_id, drive_id, parent_id, created_etag, local_size, local_hash, intended_at)
		VALUES (?, '', ?, ?, '', ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			drive_id = excluded.drive_id, parent_id = excluded.parent_id,
			local_size = excluded.local_size, local_hash = excluded.local_hash,
			intended_at = excluded.intended_at`,
		pending.Path, pending.DriveID.String(), pending.ParentID,
		pending.LocalSize, pending.LocalHash, syncdomain.StoredTimeNano(s.nowFunc()),
	); err != nil {
		return fmt.Errorf("sync: recording pending create intent at %s: %w", pending.Path, err)
	}

	return nil
}

// RecordPendingCreate durably records a create whose empty item exists.
func (s *SyncStore) RecordPendingCreate(ctx context.Context, pending *syncdomain.PendingCreate) error {
	if pending == nil || pending.ItemID == "" || pending.Path == "" {
		return errors.New("sync: recording a pending create: missing item ID or path")
	}

	if _, err := s.db.ExecContext(ctx, `INSERT INTO pending_creates
		(path, item_id, drive_id, parent_id, created_etag, local_size, local_hash, intended_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			item_id = excluded.item_id, drive_id = excluded.drive_id, parent_id = excluded.parent_id,
			created_etag = excluded.created_etag, local_size = excluded.local_size, local_hash = excluded.local_hash,
			intended_at = CASE WHEN pending_creates.intended_at <> 0 THEN pending_creates.intended_at ELSE excluded.intended_at END`,
		pending.Path, pending.ItemID, pending.DriveID.String(), pending.ParentID,
		pending.CreatedETag, pending.LocalSize, pending.LocalHash, syncdomain.StoredTimeNano(s.nowFunc()),
	); err != nil {
		return fmt.Errorf("sync: recording pending create of %s (item %s): %w", pending.Path, pending.ItemID, err)
	}

	return nil
}

// ClearPendingCreateIntent drops an intent at path that never became an
// item: the attempt ended, by success or by a failure that says nothing was
// created, without the create hook firing (it fires for a failure that names
// a created item too), and without leaving the outcome of its create unknown,
// so nothing on OneDrive is ours to recognize there. A row that already names
// an item is kept.
func (s *SyncStore) ClearPendingCreateIntent(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, path); err != nil {
		return fmt.Errorf("sync: clearing pending create intent at %s: %w", path, err)
	}

	return nil
}

// ForgetRemovedPlaceholder drops the record of the create at path whose item
// itemID the cleanup after a failed upload removed: the row naming that item,
// and the intent at path if the item was removed before it was recorded. The
// item is gone, so it can never receive the content, and the create that
// made it answered, so no other item of that attempt can appear. A row naming
// another item is kept.
func (s *SyncStore) ForgetRemovedPlaceholder(ctx context.Context, path, itemID string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM pending_creates WHERE (item_id = ? AND item_id <> '') OR (path = ? AND item_id = '')`,
		itemID, path,
	); err != nil {
		return fmt.Errorf("sync: forgetting the removed placeholder %s at %s: %w", itemID, path, err)
	}

	return nil
}

// PassObservation is what a pass observed before it planned. It bounds what
// the prune of create intents may conclude from that plan's truth.
type PassObservation int

const (
	// PassRefreshedLocalOnly is a watch replan: it refreshes local truth and
	// plans on the OneDrive changes the observers committed before it. The
	// empty file of a create whose answer was lost may not have been in a
	// change batch yet, so nothing is concluded from its absence.
	PassRefreshedLocalOnly PassObservation = iota
	// PassObservedRemoteThenLocal is a one-shot sync or a watch bootstrap:
	// OneDrive is observed, then the local tree, then the pass plans. Every
	// empty file of a create that landed before the pass has been claimed by
	// the time the prune runs.
	PassObservedRemoteThenLocal
)

// claimedLandedCreates reports whether the pass's own remote observation ran
// before its plan, and so claimed the empty file of every create that landed.
func (o PassObservation) claimedLandedCreates() bool {
	switch o {
	case PassObservedRemoteThenLocal:
		return true
	case PassRefreshedLocalOnly:
		return false
	default:
		return false
	}
}

// PrunePendingCreates deletes, in one transaction once a plan has been
// built, the records no create, no upload and no observation needs any more.
// It runs after the observations that plan was built from were committed.
//
//   - Past PendingCreateIntentTTL an intent claims nothing, so it is deleted
//     whatever the pass observed.
//   - After a pass that observed OneDrive before the local tree, an intent
//     whose file is gone is deleted (createIntentFileGoneTx): that pass's
//     remote observation claimed the file of a create that landed, so no
//     create at that path will be retried or claimed, and the intent could
//     only claim a stranger's empty file.
//   - The row naming an adopted placeholder whose local file is empty too is
//     deleted (dropRecordsOfSettledEmptyPlaceholdersTx).
//   - A row naming an item at a path the scan does not visit is deleted
//     (dropRecordsAtUnscannedPathsTx).
//
// A watch replan observes the local tree only, so it keeps a fresh intent
// whose file is gone; the watch loop drops that one after a remote poll that
// began once the last create attempt at the path had ended
// (DropGoneCreateIntentsAt). Any other row naming an item ends through
// observation or an outcome's commit.
//
// It never runs while an upload of this engine is in flight: a one-shot pass
// prunes before it executes its plan, and a watch replan waits until no
// action is running (watchRuntime.canPrepareNow).
//
// scanned reports whether a full local scan visits path at all: a path the
// content filter hides or a shortcut root reserves is never in local_state,
// so its absence there proves nothing. Read boundaries on either side, case
// collisions and other observation issues that block truth keep an intent the
// same way, as suspect local truth keeps every one.
func (s *SyncStore) PrunePendingCreates(
	ctx context.Context, observed PassObservation, scanned func(path string) bool,
) (err error) {
	tx, err := BeginPerfTx(ctx, s.db)
	if err != nil {
		return fmt.Errorf("sync: beginning the prune of pending creates: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback the prune of pending creates")
	}()

	intents, err := listCreateIntentsTx(ctx, tx, "")
	if err != nil {
		return err
	}
	if _, dropErr := s.dropAbandonedCreateIntentsTx(ctx, tx, intents, observed.claimedLandedCreates(), scanned); dropErr != nil {
		return dropErr
	}
	if dropErr := s.dropRecordsOfSettledEmptyPlaceholdersTx(ctx, tx); dropErr != nil {
		return dropErr
	}
	if dropErr := s.dropRecordsAtUnscannedPathsTx(ctx, tx, scanned); dropErr != nil {
		return dropErr
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sync: committing the prune of pending creates: %w", err)
	}

	return nil
}

// DropGoneCreateIntentsAt is the watch loop's prune after a remote poll that
// began once the last create attempt at each of paths had ended, and that
// has been applied: every create of those attempts that landed before the
// poll has been claimed by then, so an intent still without an item there is
// judged as after a pass that observed OneDrive first. It returns the paths
// whose intent stays (its file is still there, or absence proves nothing
// there yet); a path with no intent any more, or whose row names an item, is
// not returned.
func (s *SyncStore) DropGoneCreateIntentsAt(
	ctx context.Context, paths []string, scanned func(path string) bool,
) (kept []string, err error) {
	tx, err := BeginPerfTx(ctx, s.db)
	if err != nil {
		return nil, fmt.Errorf("sync: beginning the prune of create intents after a poll: %w", err)
	}
	defer func() {
		err = finalizeTxRollback(err, tx, "sync: rollback the prune of create intents after a poll")
	}()

	var intents []syncdomain.PendingCreate
	for _, path := range paths {
		atPath, listErr := listCreateIntentsTx(ctx, tx, path)
		if listErr != nil {
			return nil, listErr
		}
		intents = append(intents, atPath...)
	}
	dropped, err := s.dropAbandonedCreateIntentsTx(ctx, tx, intents, true, scanned)
	if err != nil {
		return nil, err
	}

	if err = tx.Commit(); err != nil {
		return nil, fmt.Errorf("sync: committing the prune of create intents after a poll: %w", err)
	}
	for i := range intents {
		if !dropped[intents[i].Path] {
			kept = append(kept, intents[i].Path)
		}
	}

	return kept, nil
}

// dropAbandonedCreateIntentsTx deletes each of intents that is past its TTL
// or, when claimedLandedCreates, whose file the committed truth proves gone
// (createIntentFileGoneTx), and reports the paths it dropped.
func (s *SyncStore) dropAbandonedCreateIntentsTx(
	ctx context.Context, tx SqlTxRunner, intents []syncdomain.PendingCreate, claimedLandedCreates bool, scanned func(path string) bool,
) (map[string]bool, error) {
	dropped := make(map[string]bool, len(intents))
	if len(intents) == 0 {
		return dropped, nil
	}
	fileGone, err := createIntentFileGoneTx(ctx, tx, claimedLandedCreates, scanned)
	if err != nil {
		return nil, err
	}

	now := s.nowFunc()
	for i := range intents {
		intent := &intents[i]
		expired := !intent.IntentIsFresh(now)
		abandoned := expired
		if !expired {
			if abandoned, err = fileGone(intent.Path); err != nil {
				return nil, err
			}
		}
		if !abandoned {
			continue
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, intent.Path); err != nil {
			return nil, fmt.Errorf("sync: dropping the abandoned create intent at %s: %w", intent.Path, err)
		}
		dropped[intent.Path] = true
		s.logger.Debug("dropped a create intent no create will claim",
			slog.String("path", intent.Path), slog.Bool("expired", expired))
	}

	return dropped, nil
}

// createIntentFileGoneTx returns the test of whether committed truth proves
// that the file of an intent at a path is gone: local truth is complete, the
// scan visits the path, local and remote truth are both available there, and
// local_state holds no entry at it. It proves nothing anywhere unless the
// remote observation committed before it claimed the empty file of every
// create that landed (claimedLandedCreates) and local truth is complete.
func createIntentFileGoneTx(
	ctx context.Context, tx SqlTxRunner, claimedLandedCreates bool, scanned func(path string) bool,
) (func(path string) (bool, error), error) {
	provesNothing := func(string) (bool, error) { return false, nil }
	if !claimedLandedCreates {
		return provesNothing, nil
	}
	state, err := ReadObservationStateFromTx(ctx, tx)
	if err != nil {
		return nil, err
	}
	if !state.LocalTruthComplete {
		return provesNothing, nil
	}
	issues, err := QueryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return nil, fmt.Errorf("sync: reading observation issues for the create intents: %w", err)
	}
	availability := syncdomain.NewTruthAvailabilityIndex(issues)

	return func(path string) (bool, error) {
		status := availability.StatusForPath(path)
		if !scanned(path) || !status.Local.IsAvailable() || !status.Remote.IsAvailable() {
			return false, nil
		}

		return localStateIsEmptyAtTx(ctx, tx, path)
	}, nil
}

// dropRecordsOfSettledEmptyPlaceholdersTx deletes the rows naming an item
// whose baseline row records empty content on both sides and whose path the
// local tree holds as an empty file. The placeholder was adopted and the
// local file is empty too, so no plan uploads into the item and no outcome
// commits anything for it: the row stayed for good, and every later move of
// the item's eTag or modification time was absorbed as the placeholder
// following it. Nothing is left to upload, and an echo of an empty file is no
// change. A row whose baseline holds content (an upload committed) keeps its
// row for the stale echo of the empty state.
func (s *SyncStore) dropRecordsOfSettledEmptyPlaceholdersTx(ctx context.Context, tx SqlTxRunner) error {
	result, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE item_id <> '' AND item_id IN (
		SELECT b.item_id FROM baseline b JOIN local_state l ON l.path = b.path
		WHERE b.item_type = 'file' AND b.local_size = 0 AND b.remote_size = 0
			AND l.item_type = 'file' AND l.size = 0)`)
	if err != nil {
		return fmt.Errorf("sync: dropping the records of settled empty placeholders: %w", err)
	}
	if dropped, countErr := result.RowsAffected(); countErr == nil && dropped > 0 {
		s.logger.Debug("dropped the records of adopted placeholders whose local file is empty too",
			slog.Int64("records", dropped))
	}

	return nil
}

// dropRecordsAtUnscannedPathsTx deletes the rows naming an item at a path the
// full local scan does not visit (scanned is false: the content filter hides
// it, or a shortcut root reserves it). This engine plans nothing there, so no
// upload goes into the item and no outcome commits anything for it. Every
// such row ends here, whether or not observation adopted its item. The commit
// that removes an adopted item's hidden baseline row would end that row too,
// but this step runs before the plan executes. An intent at such a path is
// kept: it names no item, and its own TTL ends it.
func (s *SyncStore) dropRecordsAtUnscannedPathsTx(ctx context.Context, tx SqlTxRunner, scanned func(path string) bool) error {
	paths, err := listRecordedItemPathsTx(ctx, tx)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if scanned(path) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id <> ''`, path); err != nil {
			return fmt.Errorf("sync: dropping the record at %s, which the scan does not visit: %w", path, err)
		}
		s.logger.Debug("dropped the record of a created item at a path the scan does not visit", slog.String("path", path))
	}

	return nil
}

// listRecordedItemPathsTx returns the paths of the rows that name an item.
func listRecordedItemPathsTx(ctx context.Context, tx SqlTxRunner) ([]string, error) {
	return queryPathsTx(ctx, tx,
		`SELECT path FROM pending_creates WHERE item_id <> '' ORDER BY path`, "the records of created items")
}

// baselineBeforeOutcome is what the baseline held before an outcome was
// applied: the item at the outcome's path, and, for a move, the path the
// moved item's row had. Either is "" when there was none.
type baselineBeforeOutcome struct {
	occupant     string
	previousPath string
}

// readBaselineBeforeOutcomeTx reads baselineBeforeOutcome for outcome, in the
// transaction that applies it.
func readBaselineBeforeOutcomeTx(ctx context.Context, tx SqlTxRunner, outcome *syncdomain.BaselineMutation) (baselineBeforeOutcome, error) {
	kind, err := classifyMutation(outcome)
	if err != nil {
		return baselineBeforeOutcome{}, err
	}
	var before baselineBeforeOutcome
	if before.occupant, err = baselineOccupantAtTx(ctx, tx, outcome.Path); err != nil {
		return baselineBeforeOutcome{}, err
	}
	if kind != baselineMutationMove || outcome.ItemID == "" {
		return before, nil
	}
	switch scanErr := tx.QueryRowContext(ctx, `SELECT path FROM baseline WHERE item_id = ?`, outcome.ItemID).
		Scan(&before.previousPath); {
	case errors.Is(scanErr, sql.ErrNoRows):
		return before, nil
	case scanErr != nil:
		return baselineBeforeOutcome{}, fmt.Errorf("sync: reading the baseline path of moved item %s: %w", outcome.ItemID, scanErr)
	default:
		return before, nil
	}
}

// settlePendingCreatesTx ends or moves, in the transaction that commits
// outcome, the records the outcome settles. before is what the baseline held
// before the outcome was applied.
//
//   - A file outcome that commits empty remote content (a zero-byte create,
//     an upload of an empty file into a placeholder, an empty file downloaded
//     or found equal) drops the row at its path and the row naming its item:
//     nothing is left to upload, and an echo of an empty file is no change.
//     Without it, the row of a zero-byte create that observation claimed
//     before the attempt's clear stayed for good, and a later change of the
//     file on OneDrive was absorbed as the placeholder following its eTag.
//   - A move takes the row naming its item, and for a folder every row below
//     it, to the new path (movePendingCreatesTx): the placeholder it moves
//     can still receive the upload, and only there.
//   - An outcome that removed the item it found at its path from the
//     baseline (a delete on either side, another item's row taking the path)
//     drops the row naming that item: the item can no longer receive this
//     client's upload. This runs first, so a move onto that item's path finds
//     the path free and keeps the row of the item it moves.
//
// An outcome that commits content keeps its row for the stale echo of the
// empty state (placeholderStale).
func settlePendingCreatesTx(ctx context.Context, tx SqlTxRunner, outcome *syncdomain.BaselineMutation, before baselineBeforeOutcome) error {
	kind, err := classifyMutation(outcome)
	if err != nil {
		return err
	}
	if before.occupant != "" {
		if _, goneErr := tx.ExecContext(ctx,
			`DELETE FROM pending_creates WHERE item_id = ? AND NOT EXISTS (SELECT 1 FROM baseline WHERE item_id = ?)`,
			before.occupant, before.occupant,
		); goneErr != nil {
			return fmt.Errorf("sync: dropping the pending create of %s, which left the baseline: %w", before.occupant, goneErr)
		}
	}
	switch {
	case kind == baselineMutationUpsert && outcome.ItemType == syncdomain.ItemTypeFile && outcome.ItemID != "" &&
		outcome.RemoteSizeKnown && outcome.RemoteSize == 0:
		if _, emptyErr := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? OR item_id = ?`,
			outcome.Path, outcome.ItemID); emptyErr != nil {
			return fmt.Errorf("sync: dropping the pending create settled by the empty outcome at %s: %w", outcome.Path, emptyErr)
		}
	case kind == baselineMutationMove:
		if moveErr := movePendingCreatesTx(ctx, tx, outcome, before.previousPath); moveErr != nil {
			return moveErr
		}
	}

	return nil
}

// movePendingCreatesTx moves, with the move outcome commits, the row naming
// the moved item to its new path, and, for a folder move, every row below the
// folder's previous baseline path to the same place below the new one, as
// commitMove does for the baseline. Left at the old path, a row was found by
// a create there that met 409, which uploaded into the item at its new path.
// A new path that already holds a row keeps it, and the moved row ends there
// instead of staying where its item left.
func movePendingCreatesTx(ctx context.Context, tx SqlTxRunner, outcome *syncdomain.BaselineMutation, previousPath string) error {
	if outcome.ItemID != "" {
		if err := movePendingCreateOfItemTx(ctx, tx, outcome.ItemID, outcome.Path); err != nil {
			return err
		}
	}
	if !isFolderPathMove(outcome) || previousPath == "" || previousPath == outcome.Path {
		return nil
	}

	oldPrefix, newPrefix := previousPath+"/", outcome.Path+"/"
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates
		WHERE substr(path, 1, length(?)) = ? COLLATE BINARY
			AND EXISTS (SELECT 1 FROM pending_creates taken
				WHERE taken.path = ? || substr(pending_creates.path, length(?) + 1))`,
		oldPrefix, oldPrefix, newPrefix, oldPrefix); err != nil {
		return fmt.Errorf("sync: dropping the pending creates below %s whose new path holds one: %w", previousPath, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE pending_creates
		SET path = ? || substr(path, length(?) + 1)
		WHERE substr(path, 1, length(?)) = ? COLLATE BINARY`,
		newPrefix, oldPrefix, oldPrefix, oldPrefix); err != nil {
		return fmt.Errorf("sync: moving the pending creates below %s to %s: %w", previousPath, outcome.Path, err)
	}

	return nil
}

// movePendingCreateOfItemTx moves the row naming itemID to path. A path that
// already holds another row keeps that one, and this row ends: two rows never
// name one item, and none stays at a path its item left.
func movePendingCreateOfItemTx(ctx context.Context, tx SqlTxRunner, itemID, path string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE OR IGNORE pending_creates SET path = ? WHERE item_id = ?`,
		path, itemID); err != nil {
		return fmt.Errorf("sync: moving the record of placeholder %s to %s: %w", itemID, path, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE item_id = ? AND path <> ?`,
		itemID, path); err != nil {
		return fmt.Errorf("sync: dropping the record of placeholder %s left behind on its way to %s: %w", itemID, path, err)
	}

	return nil
}

// baselineOccupantAtTx returns the item the baseline holds at path, or "".
func baselineOccupantAtTx(ctx context.Context, tx SqlTxRunner, path string) (string, error) {
	var itemID string
	switch err := tx.QueryRowContext(ctx, `SELECT item_id FROM baseline WHERE path = ?`, path).Scan(&itemID); {
	case errors.Is(err, sql.ErrNoRows):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("sync: reading the baseline at %s: %w", path, err)
	default:
		return itemID, nil
	}
}

// listCreateIntentsTx returns the rows that record only an intent: every one,
// or the one at path when path is not empty.
func listCreateIntentsTx(ctx context.Context, tx SqlTxRunner, path string) ([]syncdomain.PendingCreate, error) {
	rows, err := tx.QueryContext(ctx, `SELECT path, intended_at FROM pending_creates
		WHERE item_id = '' AND (? = '' OR path = ?) ORDER BY path`, path, path)
	if err != nil {
		return nil, fmt.Errorf("sync: listing create intents: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor; iteration errors are reported by rows.Err

	var intents []syncdomain.PendingCreate
	for rows.Next() {
		var intent syncdomain.PendingCreate
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
func localStateIsEmptyAtTx(ctx context.Context, tx SqlTxRunner, path string) (bool, error) {
	var entries int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM local_state WHERE path = ?`, path).Scan(&entries); err != nil {
		return false, fmt.Errorf("sync: reading local_state at %s: %w", path, err)
	}

	return entries == 0, nil
}

// PendingCreateAt returns the recorded create at path; found is false when
// there is none.
func (s *SyncStore) PendingCreateAt(ctx context.Context, path string) (pending syncdomain.PendingCreate, found bool, err error) {
	return scanPendingCreate(s.db.QueryRowContext(ctx, sqlSelectPendingCreate+` WHERE path = ?`, path))
}

const sqlSelectPendingCreate = `SELECT item_id, drive_id, path, parent_id, created_etag, local_size, local_hash, intended_at
	FROM pending_creates`

func deletePendingCreateTx(ctx context.Context, tx SqlTxRunner, itemID string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE item_id = ?`, itemID); err != nil {
		return fmt.Errorf("sync: clearing pending create of item %s: %w", itemID, err)
	}

	return nil
}

func scanPendingCreate(row *sql.Row) (syncdomain.PendingCreate, bool, error) {
	var (
		pending syncdomain.PendingCreate
		driveID string
	)
	err := row.Scan(&pending.ItemID, &driveID, &pending.Path, &pending.ParentID,
		&pending.CreatedETag, &pending.LocalSize, &pending.LocalHash, &pending.IntendedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return syncdomain.PendingCreate{}, false, nil
	case err != nil:
		return syncdomain.PendingCreate{}, false, fmt.Errorf("sync: reading pending create: %w", err)
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
//
// The dropped row's item loses its record too, if it has one (an adopted
// placeholder renamed online, whose record followed it): kept, the record
// named an item without a baseline row, which the next observation would
// adopt at its new path as an untouched placeholder, and with no local file
// there the plan would delete it online.
func freeBaselinePathForPlaceholderTx(ctx context.Context, tx SqlTxRunner, path string) (bool, error) {
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
	if err := deletePendingCreateTx(ctx, tx, occupantID); err != nil {
		return false, err
	}

	return true, nil
}

// placeholderDecision is what observation does with an observed item that has
// a pending-create record.
type placeholderDecision int

const (
	placeholderNotPending  placeholderDecision = iota // no record: observe normally
	placeholderAdopt                                  // our untouched placeholder, not yet synced
	placeholderAdopted                                // our placeholder, adopted and still empty
	placeholderStale                                  // our placeholder state, after the upload committed
	placeholderResolved                               // no longer a placeholder: drop the record
	placeholderSizeUnknown                            // not known to be empty: observe normally, keep the record
)

// classifyObservedPlaceholderTx decides how observation treats item when it
// has a pending-create record. now decides whether an intent without an item
// ID is still fresh enough to claim an empty file observed at its path.
//
// Stale and adopt read the item as still empty, which takes a known size of
// zero. An item whose size Graph did not report arrives as size 0
// (normalizeRemoteFacts); read as empty, it was dropped as a stale echo once
// the upload had committed (a remote change swallowed because a record
// existed) or adopted as known empty on both sides (an item adopted because a
// record existed, which the next upload by ID could overwrite). It is
// observed like any item, and the record stays for an observation that knows.
func classifyObservedPlaceholderTx(
	ctx context.Context, tx SqlTxRunner, item *syncdomain.ObservedItem, now time.Time,
) (*syncdomain.PendingCreate, placeholderDecision, error) {
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
	switch {
	case item.IsDeleted, !item.SizeUnknown && item.Size != 0:
		return &pending, placeholderResolved, nil
	case item.SizeUnknown:
		return &pending, placeholderSizeUnknown, nil
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
// ID. The updated row is appended to adopted for the in-memory baseline. An
// observation whose size Graph did not report never gets here
// (placeholderSizeUnknown): the item may hold content by now, and a row that
// took its eTag would let the next upload by ID overwrite it.
//
// Renamed or moved online while still empty, the item is observed like any
// item and its record moves to the item's new path. Left at the old path, the
// record was found by a later create there that met 409, which uploaded into
// the item at its new path. Dropped, it would lose the stale-echo rule: an
// upload by ID into the item may already be on its way (the eTag it is
// conditional on is checked when its session opens, not by every fragment),
// and once its content commits, a window captured before that shows the
// empty item again, which only the record tells from a remote change. A path
// that already holds another record keeps that one, and this record ends
// (movePendingCreateOfItemTx): left where it was, it named an item that is no
// longer there, for a create there to upload into.
func followAdoptedPlaceholderTx(
	ctx context.Context, tx SqlTxRunner, item *syncdomain.ObservedItem, pending *syncdomain.PendingCreate,
	adopted *[]*syncdomain.BaselineEntry,
) error {
	if item.Path != pending.Path {
		return movePendingCreateOfItemTx(ctx, tx, item.ItemID, item.Path)
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
	ctx context.Context, tx SqlTxRunner, itemID string, driveID driveid.ID,
) (entry *syncdomain.BaselineEntry, found bool, err error) {
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
	ctx context.Context, tx SqlTxRunner, item *syncdomain.ObservedItem, pending *syncdomain.PendingCreate,
	adopted *[]*syncdomain.BaselineEntry,
) error {
	if item.Path != pending.Path {
		// Moved or renamed meanwhile: not the create this record describes.
		return deletePendingCreateTx(ctx, tx, item.ItemID)
	}
	if free, err := freeBaselinePathForPlaceholderTx(ctx, tx, item.Path); err != nil || !free {
		return err
	}

	entry := &syncdomain.BaselineEntry{
		Path: item.Path, DriveID: item.DriveID, ItemID: item.ItemID, ParentID: pending.ParentID,
		ItemType: syncdomain.ItemTypeFile, LocalHash: item.Hash, RemoteHash: item.Hash,
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
// within PendingCreateIntentTTL of the intent, and not already synced, is the
// placeholder the dying process created: the row takes its ID and eTag.
// Anything else at the path (content, a folder, a deletion) or an expired
// intent means no placeholder of ours is there, and the intent is dropped.
//
// So does an empty file the baseline already holds: the client knows it, so
// it is no create whose answer was lost. It is a zero-byte file whose create
// completed (such a create never runs the hook) while its intent outlived it,
// because the attempt was canceled as it succeeded or the clear failed.
// Claimed, the record named a file that stays empty, and nothing dropped it.
//
// A live file whose size Graph did not report (R-2.4.16) may be that empty
// file or not: a fresh intent neither claims it nor is dropped, and an
// observation that knows the size decides. Nor does the deletion of a folder
// at the path (observationLeavesIntent).
func claimIntendedPlaceholderTx(
	ctx context.Context, tx SqlTxRunner, item *syncdomain.ObservedItem, now time.Time,
) (syncdomain.PendingCreate, bool, error) {
	pending, found, err := scanPendingCreate(tx.QueryRowContext(ctx,
		sqlSelectPendingCreate+` WHERE path = ? AND item_id = ''`, item.Path))
	if err != nil || !found {
		return syncdomain.PendingCreate{}, false, err
	}
	if observationLeavesIntent(item, &pending, now) {
		return syncdomain.PendingCreate{}, false, nil
	}
	isLiveFile := !item.IsDeleted && item.ItemType == syncdomain.ItemTypeFile
	claimable := pending.IntentIsFresh(now) && isLiveFile && !item.SizeUnknown && item.Size == 0
	if claimable {
		synced, syncedErr := baselineHoldsItemTx(ctx, tx, item.ItemID)
		if syncedErr != nil {
			return syncdomain.PendingCreate{}, false, syncedErr
		}
		claimable = !synced
	}
	if !claimable {
		if _, err := tx.ExecContext(ctx, `DELETE FROM pending_creates WHERE path = ? AND item_id = ''`, item.Path); err != nil {
			return syncdomain.PendingCreate{}, false, fmt.Errorf("sync: dropping pending create intent at %s: %w", item.Path, err)
		}
		return syncdomain.PendingCreate{}, false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE pending_creates SET item_id = ?, created_etag = ? WHERE path = ? AND item_id = ''`,
		item.ItemID, item.ETag, item.Path); err != nil {
		return syncdomain.PendingCreate{}, false, fmt.Errorf(
			"sync: claiming placeholder %s for the intent at %s: %w", item.ItemID, item.Path, err)
	}
	pending.ItemID = item.ItemID
	pending.CreatedETag = item.ETag

	return pending, true, nil
}

// observationLeavesIntent reports an observation at an intent's path that
// neither claims the intent nor drops it. A folder's deletion there is another
// item than the file the intent describes: sync deletes a folder before it
// creates the file that replaced it locally, and the delta that follows
// reports both, so it says nothing of the file. A live file whose size Graph
// did not report may be the empty file or not, so a fresh intent waits for an
// observation that knows.
func observationLeavesIntent(item *syncdomain.ObservedItem, pending *syncdomain.PendingCreate, now time.Time) bool {
	if item.IsDeleted {
		return item.ItemType == syncdomain.ItemTypeFolder
	}

	return item.ItemType == syncdomain.ItemTypeFile && item.SizeUnknown && pending.IntentIsFresh(now)
}

// baselineHoldsItemTx reports whether the baseline has a row for itemID.
func baselineHoldsItemTx(ctx context.Context, tx SqlTxRunner, itemID string) (bool, error) {
	var rows int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM baseline WHERE item_id = ?`, itemID).Scan(&rows); err != nil {
		return false, fmt.Errorf("sync: reading the baseline of %s: %w", itemID, err)
	}

	return rows > 0, nil
}
