package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

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
	sessions  sessionTransport
}

// existingItemSessionKey is the session-store key of an overwrite by item ID.
func existingItemSessionKey(localPath, itemID string) string {
	return "item::" + itemID + "::" + localPath
}

// cancelSessionTimeout bounds the best-effort cancel of a dropped session.
const cancelSessionTimeout = 10 * time.Second

// persistedSessionUpload uploads through a recorded, resumable session.
//
//   - A record for different content is canceled on the server before it is
//     dropped: leaked sessions hold quota and, on Business, block a new
//     session for the same item (D-037).
//   - A resume whose session is gone, and a final fragment that failed, look
//     the item up first: when it already holds these bytes the upload
//     committed and is adopted, instead of uploading the whole file again,
//     possibly forever for a file whose commit outlasts the response timeout
//     (D-036).
//   - A session that is gone before its expiry while the item holds other
//     bytes was invalidated by a concurrent change (L-02):
//     ErrUploadSessionInvalidated, never a blind restart.
func (tm *TransferManager) persistedSessionUpload(ctx context.Context, up *persistedUpload) (*graph.Item, error) {
	driveStr := up.target.driveID.String()
	sent := uploadedContent{size: up.size, quickXorHash: up.localHash, name: up.target.name}

	rec, found, loadErr := tm.sessionStore.Load(driveStr, up.key)
	if loadErr != nil {
		// Debug only: the store already reported what it did with the file,
		// and the upload proceeds with a fresh session.
		tm.logger.Debug("failed to load upload session",
			slog.String("path", up.localPath),
			slog.String("error", loadErr.Error()),
		)
	}

	if found && rec.FileHash != up.localHash {
		tm.dropSession(ctx, driveStr, up.key, graph.UploadURL(rec.SessionURL))
		found = false
	}

	if found {
		item, done, err := tm.resumePersistedSession(ctx, up, rec, sent)
		if done {
			return item, err
		}
	}

	session, err := up.create()
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", up.localPath, err)
	}

	if saveErr := tm.sessionStore.Save(driveStr, up.key, &SessionRecord{
		MountID:    tm.sessionMountID,
		LocalRoot:  tm.sessionLocalRoot,
		SessionURL: string(session.UploadURL),
		FileHash:   up.localHash,
		FileSize:   up.size,
		ExpiresAt:  session.ExpirationTime,
	}); saveErr != nil {
		tm.logger.Warn("failed to save upload session — resume after crash will not work for this file",
			slog.String("path", up.localPath),
			slog.String("error", saveErr.Error()),
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
		if landed, ok := tm.adoptLandedUpload(ctx, up.target, sent); ok {
			tm.deleteSession(driveStr, up.key)
			return landed, nil
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

	if !errors.Is(resumeErr, graph.ErrUploadSessionExpired) {
		// Any other resume failure drops the session (canceled on the
		// server) and the next attempt starts fresh, preventing an endless
		// resume loop (B-208).
		tm.dropSession(ctx, driveStr, up.key, session.UploadURL)
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, resumeErr)
	}

	// The session is gone: it completed (a crash after the final fragment),
	// expired, or was invalidated by a concurrent change.
	tm.deleteSession(driveStr, up.key)

	if landed, ok := tm.adoptLandedUpload(ctx, up.target, sent); ok {
		return landed, true, nil
	}

	if !rec.ExpiresAt.IsZero() && time.Now().Before(rec.ExpiresAt) {
		return nil, true, fmt.Errorf("resuming upload of %s: %w", up.localPath, ErrUploadSessionInvalidated)
	}

	tm.logger.Info("upload session expired, creating fresh session", slog.String("path", up.localPath))

	return nil, false, nil
}

// adoptLandedUpload looks the upload's item up and reports whether it
// already holds exactly the bytes sent.
func (tm *TransferManager) adoptLandedUpload(
	ctx context.Context, target uploadTarget, sent uploadedContent,
) (*graph.Item, bool) {
	item, err := tm.lookupUploadTarget(ctx, target)
	if err != nil {
		tm.logger.Debug("could not look up the item of an uncertain upload",
			slog.String("error", err.Error()),
		)

		return nil, false
	}

	if verifyTransfer(sent, item) != transferVerified {
		return nil, false
	}

	tm.logger.Debug("upload had already committed; adopting the stored item",
		slog.String("item_id", item.ID),
	)

	return item, true
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

	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelSessionTimeout)
	defer cancel()

	if err := canceler.CancelUploadSession(cancelCtx, &graph.UploadSession{UploadURL: uploadURL}); err != nil {
		// Debug only: the session expires on its own; the user cannot act.
		tm.logger.Debug("couldn't cancel a dropped upload session",
			slog.String("error", err.Error()),
		)
	}
}
