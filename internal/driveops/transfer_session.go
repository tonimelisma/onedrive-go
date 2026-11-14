package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ErrUploadSessionInvalidated reports a resumable upload whose server
// session vanished before its expiry while the item does not hold the bytes
// being uploaded. Live probe L-02 showed what that means: a session opened
// on an item that another writer then changed answers its next fragment
// with 404 "The upload session was not found". Restarting the upload would
// silently overwrite the other writer's change, so the caller re-observes
// the item instead (sync replans; put reports it).
var ErrUploadSessionInvalidated = errors.New("the file changed on OneDrive while it was being uploaded")

// SessionCanceler cancels a server-side upload session. Satisfied by
// *graph.Client and type-asserted from the uploader.
type SessionCanceler interface {
	CancelUploadSession(ctx context.Context, session *graph.UploadSession) error
}

// sessionTransport is what a persisted upload needs from the uploader once
// the session exists. SessionUploader and ItemSessionUploader both provide
// it.
type sessionTransport interface {
	UploadFromSession(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
	ResumeUpload(
		ctx context.Context, session *graph.UploadSession,
		content io.ReaderAt, totalSize int64, progress graph.ProgressFunc,
	) (*graph.Item, error)
}

// persistedUpload is one resumable upload whose session is recorded in the
// SessionStore under key.
type persistedUpload struct {
	target    uploadTarget
	key       string
	content   io.ReaderAt
	localPath string
	localHash string
	size      int64
	progress  graph.ProgressFunc
	create    func() (*graph.UploadSession, error)
	// onCreated records a create's new item before content is sent; nil for
	// overwrites (see UploadOpts.OnCreated).
	onCreated func(itemID, createdETag string) error
	// createForCreated opens a session on an item an earlier attempt of
	// this create already made; nil for overwrites.
	createForCreated func(itemID, createdETag string) (*graph.UploadSession, error)
	// createdItem is the empty item this create made (ID and creation eTag),
	// once known.
	createdItem *graph.Item
	sessions    sessionTransport
	// sent describes the upload for verification and adoption.
	sent uploadedContent
}

// existingItemSessionKey is the session-store key of an overwrite by item ID.
func existingItemSessionKey(localPath, itemID string) string {
	return "item::" + itemID + "::" + localPath
}

// placeholderRemovalTimeout bounds the best-effort removal of the empty item a
// failed create left (removePlaceholder): every read and conditional delete of
// RemoveEmptyItem together. The cancel of a dropped session is a different
// request, bounded by graph.SessionCancelTimeout.
const placeholderRemovalTimeout = 10 * time.Second

// persistedSessionUpload uploads through a recorded, resumable session.
//
//   - A record for different content is canceled on the server before it is
//     dropped: leaked sessions hold quota and, on Business, block a new
//     session for the same item (D-037).
//   - A resume whose session is gone, and a final fragment that failed, look
//     the item up first: when it already holds these bytes the upload
//     committed and is adopted, instead of uploading the whole file again,
//     possibly forever for a file whose commit outlasts the response timeout
//     (D-036). A lookup that fails leaves the outcome unknown: the record
//     stays, and the error carries the lookup's failure only
//     (uploadOutcomeUnknownError).
//   - A session that is gone before its expiry while the item holds other
//     bytes was invalidated by a concurrent change (L-02):
//     ErrUploadSessionInvalidated, never a blind restart.
func (tm *TransferManager) persistedSessionUpload(ctx context.Context, up *persistedUpload) (*graph.Item, error) {
	item, err := tm.persistedSessionUploadOnce(ctx, up)
	if err != nil && up.createdItem != nil {
		// Failures after this upload created its item name that item, so the
		// caller looks the outcome up by ID (M2).
		return nil, &graph.CreatedItemError{Item: up.createdItem, Err: err}
	}

	return item, err
}

// loadUsableSessionRecord loads the upload's session record, dropping one
// recorded for different content, and adopts a recorded create's item.
func (tm *TransferManager) loadUsableSessionRecord(ctx context.Context, up *persistedUpload, driveStr string) (*SessionRecord, bool) {
	rec, found, loadErr := tm.sessionStore.Load(driveStr, up.key)
	if loadErr != nil {
		// The upload proceeds with a fresh session, which for a large file
		// means sending every byte again. SessionStore.Load reports an
		// invalid record itself, once, whether or not it could remove the
		// file; a stat or read failure comes back unreported, so it is
		// named here, once per attempt.
		if errors.Is(loadErr, ErrInvalidSessionRecord) {
			tm.logger.Debug("saved upload session record was invalid; starting the upload over",
				slog.String("path", up.localPath),
				slog.Any("error", loadErr),
			)
		} else {
			tm.logger.Warn("Could not read the saved upload session; starting the upload over",
				slog.String("path", up.localPath),
				slog.Any("error", loadErr),
			)
		}
	}

	if !found && loadErr == nil {
		rec, found = tm.loadCreateRecordForOverwrite(ctx, up, driveStr)
	}

	if found && rec.FileHash != up.localHash {
		tm.dropSession(ctx, driveStr, up.key, graph.UploadURL(rec.SessionURL))
		return nil, false
	}
	if found && rec.ItemID != "" && rec.CreatedETag != "" {
		// The record names the item a create made: look it up by ID, not
		// by name (M2).
		up.target.itemID = rec.ItemID
		up.createdItem = &graph.Item{ID: rec.ItemID, ETag: rec.CreatedETag}
	}

	return rec, found
}

// loadCreateRecordForOverwrite finds the session a crashed create left for an
// overwrite of the very item that create made. A create records its session
// under the local path; after a crash the next pass adopts the empty item the
// create left (R-5.6.15) and uploads to it by ID, whose record key names the
// item, so the create's record was never found and every byte it had sent
// went again (R-5.2.4). The record is taken only when it names this item and
// this content, and it is re-keyed so the overwrite's own delete and drop
// paths find it. The creation eTag is not carried over: an overwrite that
// fails must leave the item, not remove it as its own placeholder.
func (tm *TransferManager) loadCreateRecordForOverwrite(
	ctx context.Context, up *persistedUpload, driveStr string,
) (*SessionRecord, bool) {
	if up.target.itemID == "" || up.key == up.localPath {
		return nil, false
	}

	rec, found, err := tm.sessionStore.Load(driveStr, up.localPath)
	if err != nil || !found || rec.ItemID != up.target.itemID {
		return nil, false
	}
	if rec.FileHash != up.localHash {
		tm.dropSession(ctx, driveStr, up.localPath, graph.UploadURL(rec.SessionURL))
		return nil, false
	}

	adopted := *rec
	adopted.CreatedETag = ""
	if saveErr := tm.sessionStore.Save(driveStr, up.key, &adopted); saveErr != nil {
		// The resume still works this attempt; the record just stays under
		// the create's key until the stale-session cleanup removes it.
		tm.logger.Debug("could not re-key the saved upload session of an adopted create",
			slog.String("path", up.localPath),
			slog.Any("error", saveErr),
		)
	} else {
		tm.deleteSession(driveStr, up.localPath)
	}

	tm.logger.Debug("resuming the upload session a crashed create left",
		slog.String("path", up.localPath),
		slog.String("item_id", up.target.itemID),
	)

	return &adopted, true
}

// openSessionForUpload opens the session a fresh attempt uploads through and,
// for a create, records the created item through onCreated before any
// content is sent. A session request that failed after the create made its
// item records that item too (recordItemOfFailedSession).
func (tm *TransferManager) openSessionForUpload(ctx context.Context, up *persistedUpload, driveStr string) (*graph.UploadSession, error) {
	create := up.create
	if up.createdItem != nil && up.createForCreated != nil {
		// An earlier attempt already created the item: a fresh session goes
		// to that item, conditional on its creation eTag. Creating the file
		// again would meet its own placeholder with 409.
		createdItem := up.createdItem
		create = func() (*graph.UploadSession, error) {
			session, sessionErr := up.createForCreated(createdItem.ID, createdItem.ETag)
			if session != nil {
				session.CreatedETag = createdItem.ETag
			}
			return session, sessionErr
		}
	}

	session, err := create()
	if err != nil {
		tm.recordItemOfFailedSession(up, err)
		return nil, fmt.Errorf("creating upload session for %s: %w", up.localPath, err)
	}
	if session.CreatedETag == "" || session.ItemID == "" {
		return session, nil
	}

	up.target.itemID = session.ItemID
	up.createdItem = &graph.Item{ID: session.ItemID, ETag: session.CreatedETag}
	if up.onCreated != nil {
		if hookErr := up.onCreated(session.ItemID, session.CreatedETag); hookErr != nil {
			tm.dropSession(ctx, driveStr, up.key, session.UploadURL)
			tm.removePlaceholder(ctx, up.target.driveID, session.ItemID)
			return nil, fmt.Errorf("recording the new file %s: %w", up.localPath, hookErr)
		}
	}

	return session, nil
}

// recordItemOfFailedSession runs onCreated for the item a create made before
// its session request failed: graph.CreateUploadSession is one call for both
// steps and names that item in a *graph.CreatedItemError. Graph has already
// tried to remove the empty item, but only best effort (its bounded read-back
// and conditional delete can fail or give up), and an empty file nobody
// recorded reads on the next pass as someone else's (R-5.6.15). Before this,
// the hook ran only once a session was open, and sync cleared the intent of
// an attempt whose hook never ran. The item is not adopted as up.createdItem:
// it may be gone, and a retry by its ID would then meet 404 instead of
// creating the file again. A hook failure is Debug; the session's error is
// what the caller sees.
func (tm *TransferManager) recordItemOfFailedSession(up *persistedUpload, err error) {
	var createdErr *graph.CreatedItemError
	if up.onCreated == nil || up.createdItem != nil || !errors.As(err, &createdErr) ||
		createdErr.Item == nil || createdErr.Item.ID == "" {
		return
	}

	if hookErr := up.onCreated(createdErr.Item.ID, createdErr.Item.ETag); hookErr != nil {
		tm.logger.Debug("couldn't record the file a failed upload session created",
			slog.String("path", up.localPath),
			slog.String("item_id", createdErr.Item.ID),
			slog.Any("error", hookErr),
		)
	}
}

func (tm *TransferManager) persistedSessionUploadOnce(ctx context.Context, up *persistedUpload) (*graph.Item, error) {
	driveStr := up.target.driveID.String()
	sent := up.sent

	rec, found := tm.loadUsableSessionRecord(ctx, up, driveStr)

	if found {
		item, done, err := tm.resumePersistedSession(ctx, up, rec, sent)
		if done {
			return item, err
		}
	}

	session, err := tm.openSessionForUpload(ctx, up, driveStr)
	if err != nil {
		return nil, err
	}

	if saveErr := tm.sessionStore.Save(driveStr, up.key, &SessionRecord{
		MountID:     tm.sessionMountID,
		LocalRoot:   tm.sessionLocalRoot,
		SessionURL:  string(session.UploadURL),
		FileHash:    up.localHash,
		FileSize:    up.size,
		ExpiresAt:   session.ExpirationTime,
		ItemID:      session.ItemID,
		CreatedETag: session.CreatedETag,
	}); saveErr != nil {
		tm.logger.Warn("failed to save upload session — resume after crash will not work for this file",
			slog.String("path", up.localPath),
			slog.Any("error", saveErr),
		)
	}

	item, err := up.sessions.UploadFromSession(ctx, session, up.content, up.size, up.progress)
	if err == nil {
		tm.deleteSession(driveStr, up.key)
		return item, nil
	}

	switch {
	case errors.Is(err, graph.ErrUploadCompletedWithoutItem):
		// The session finished; settleUploadedItem looks the item up.
		tm.deleteSession(driveStr, up.key)
	case graph.IsFinalFragmentUncertain(err):
		landed, adopted, lookupErr := tm.adoptLandedUpload(ctx, up.target, sent)
		if lookupErr != nil {
			// Unknown outcome: keep the record, the next attempt resumes
			// or looks again.
			return nil, outcomeUnknownAfter(fmt.Sprintf("uploading %s: %s", up.localPath, err), lookupErr)
		}
		if adopted {
			tm.deleteSession(driveStr, up.key)
			return landed, nil
		}
		if errors.Is(err, graph.ErrNotFound) {
			tm.deleteSession(driveStr, up.key)
			return nil, fmt.Errorf("uploading %s: %w", up.localPath, ErrUploadSessionInvalidated)
		}
	case errors.Is(err, graph.ErrNotFound):
		// A fragment's session vanished mid-upload: invalidated (L-02).
		tm.deleteSession(driveStr, up.key)
		return nil, fmt.Errorf("uploading %s: %w", up.localPath, ErrUploadSessionInvalidated)
	}

	// Otherwise the session file persists for the next attempt to resume.
	return nil, fmt.Errorf("uploading %s: %w", up.localPath, err)
}

// resumePersistedSession resumes a recorded session. done is false when the
// caller should start a fresh session.
func (tm *TransferManager) resumePersistedSession(
	ctx context.Context, up *persistedUpload, rec *SessionRecord, sent uploadedContent,
) (item *graph.Item, done bool, err error) {
	driveStr := up.target.driveID.String()
	tm.logger.Debug("attempting upload session resume", slog.String("path", up.localPath))

	session := &graph.UploadSession{UploadURL: graph.UploadURL(rec.SessionURL), ExpirationTime: rec.ExpiresAt}

	item, resumeErr := up.sessions.ResumeUpload(ctx, session, up.content, up.size, up.progress)
	if resumeErr == nil {
		tm.deleteSession(driveStr, up.key)
		return item, true, nil
	}

	if ctx.Err() != nil {
		// Shutting down: keep the record so the next run resumes.
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, resumeErr)
	}

	switch {
	case graph.IsFinalFragmentUncertain(resumeErr),
		errors.Is(resumeErr, graph.ErrUploadSessionExpired),
		errors.Is(resumeErr, graph.ErrUploadSessionComplete):
		// The session finished or is gone: it completed (a crash after
		// the final fragment, or a status with no range left to upload),
		// expired, or was invalidated by a concurrent change. Look before
		// deciding.
	case errors.Is(resumeErr, graph.ErrNotFound):
		// A fragment answered 404 mid-upload: invalidated (L-02).
		tm.deleteSession(driveStr, up.key)
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, ErrUploadSessionInvalidated)
	default:
		// Any other resume failure drops the session (canceled on the
		// server) and the next attempt starts fresh, preventing an endless
		// resume loop (B-208).
		tm.dropSession(ctx, driveStr, up.key, session.UploadURL)
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, resumeErr)
	}

	landed, adopted, lookupErr := tm.adoptLandedUpload(ctx, up.target, sent)
	if lookupErr != nil {
		// Unknown outcome: keep the record and retry; never conclude
		// "invalidated" from a lookup that failed.
		return nil, true, outcomeUnknownAfter(fmt.Sprintf("resuming upload of %s: %s", up.localPath, resumeErr), lookupErr)
	}

	if adopted {
		tm.deleteSession(driveStr, up.key)
		return landed, true, nil
	}

	if errors.Is(resumeErr, graph.ErrUploadSessionComplete) ||
		(graph.IsFinalFragmentUncertain(resumeErr) && !errors.Is(resumeErr, graph.ErrNotFound)) {
		// The session has nothing left to upload, or the final fragment
		// failed for another reason, and the item is not ours: drop the
		// session (canceled on the server) and start over.
		tm.dropSession(ctx, driveStr, up.key, session.UploadURL)
		return nil, false, nil
	}

	tm.deleteSession(driveStr, up.key)

	if !rec.ExpiresAt.IsZero() && time.Now().Before(rec.ExpiresAt) {
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, ErrUploadSessionInvalidated)
	}

	tm.logger.Info("upload session expired, creating fresh session", slog.String("path", up.localPath))

	return nil, false, nil
}

// adoptLandedUpload looks the upload's item up after an uncertain outcome
// and reports whether it is provably the upload's own commit. A matching
// QuickXor proves it. Without one (a hashless item, or Business enrichment
// rewriting the bytes) a matching size proves nothing: the old version can
// have the same size. Then the evidence must be positive: the item carries
// the modification time the upload sent and an eTag other than the one the
// upload was planned against. A failed lookup is returned as an error: the
// caller retries, it does not conclude anything.
func (tm *TransferManager) adoptLandedUpload(
	ctx context.Context, target uploadTarget, sent uploadedContent,
) (*graph.Item, bool, error) {
	item, err := tm.lookupUploadTarget(ctx, target)
	if errors.Is(err, graph.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("looking up the item of an uncertain upload: %w", err)
	}

	adopted := false
	switch verdict := verifyTransfer(sent, item); {
	case item.QuickXorHash != "" && verdict == transferVerified:
		adopted = true
	case verdict == transferVerified || verdict == transferEnrichedKnown:
		adopted = committedByThisUpload(sent, item)
	}

	if adopted {
		tm.logger.Debug("upload had already committed; adopting the stored item",
			slog.String("item_id", item.ID),
		)
	}

	return item, adopted, nil
}

// committedByThisUpload is the positive evidence for an item whose bytes
// cannot be compared: OneDrive stores the sent time to the second (live
// probe 6), clamped to its earliest accepted time.
func committedByThisUpload(sent uploadedContent, item *graph.Item) bool {
	if sent.mtime.IsZero() || item.ModifiedAt.IsZero() || item.ETag == "" || item.ETag == sent.planETag {
		return false
	}

	wire := sent.mtime
	if wire.Before(graph.EarliestFileSystemTime()) {
		wire = graph.EarliestFileSystemTime()
	}

	return item.ModifiedAt.Unix() == wire.Unix()
}

// removePlaceholder removes the empty item a create made once its upload
// failed, through the uploader's PlaceholderRemover: that reads the item and
// deletes it only while it is still empty, conditional on the eTag just read,
// and reads again when the delete answers 412 (R-5.6.14). The creation eTag
// does not work for this: opening and canceling an upload session on the
// empty item moves its eTag although no content landed (live, e2e
// TestE2E_SyncWatch_FileModification). The context is detached from the
// caller's, because the upload may be failing because that was canceled.
// Best effort: a leftover is an ordinary remote file, which sync's
// pending-create record identifies when one was written (R-5.6.15).
func (tm *TransferManager) removePlaceholder(ctx context.Context, driveID driveid.ID, itemID string) {
	remover, ok := tm.uploads.(PlaceholderRemover)
	if !ok || itemID == "" {
		return
	}

	removeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), placeholderRemovalTimeout)
	defer cancel()

	removed, err := remover.RemoveEmptyItem(removeCtx, driveID, itemID)
	if err != nil {
		tm.logger.Debug("couldn't remove the empty file a failed upload created",
			slog.String("item_id", itemID),
			slog.Any("error", err),
		)
		return
	}
	if !removed {
		tm.logger.Debug("left the file a failed upload created in place: it holds content, or has no eTag to delete it by",
			slog.String("item_id", itemID),
		)
	}
}

// dropSession cancels a server session best-effort, on a context detached
// from the caller's (the upload may be failing because that was canceled),
// and deletes its record.
func (tm *TransferManager) dropSession(ctx context.Context, driveID, key string, uploadURL graph.UploadURL) {
	tm.deleteSession(driveID, key)

	canceler, ok := tm.uploads.(SessionCanceler)
	if !ok || uploadURL == "" {
		return
	}

	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), graph.SessionCancelTimeout)
	defer cancel()

	if err := canceler.CancelUploadSession(cancelCtx, &graph.UploadSession{UploadURL: uploadURL}); err != nil {
		// Debug only: the session expires on its own; the user cannot act.
		tm.logger.Debug("couldn't cancel a dropped upload session",
			slog.Any("error", err),
		)
	}
}
