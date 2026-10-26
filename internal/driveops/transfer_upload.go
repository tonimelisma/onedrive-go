package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// UploadOpts configures a single upload operation.
type UploadOpts struct {
	Mtime                    time.Time
	Progress                 graph.ProgressFunc
	ExpectedHash             string
	HashMismatchError        func(localPath, expectedHash, actualHash string) error
	ValidateSourceBeforeRead func() error
}

// UploadResult reports the outcome of a successful upload.
type UploadResult struct {
	Item      *graph.Item
	LocalHash string
	Size      int64
	Mtime     time.Time
	// RemoteMtimePending is true when the content landed but OneDrive did
	// not accept the modified time (a zero-byte upload whose follow-on PATCH
	// failed). Item then carries the server's receipt time, not Mtime.
	RemoteMtimePending bool
}

// persistedSessionMinSize is the size above which an upload's session is
// persisted for cross-restart resume. Smaller files still upload through a
// session (for atomic fileSystemInfo), but re-sending at most 4 MiB costs
// less than a session record's bookkeeping.
const persistedSessionMinSize = 4 * 1024 * 1024

type validatingReaderAt struct {
	io.ReaderAt
	Validate func() error
}

func (r validatingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if r.Validate != nil {
		if err := r.Validate(); err != nil {
			return 0, fmt.Errorf("validating upload source before read: %w", err)
		}
	}

	n, err := r.ReaderAt.ReadAt(p, off)
	if err != nil {
		return n, fmt.Errorf("reading upload source at offset %d: %w", off, err)
	}

	return n, nil
}

// validateUploadParams checks required upload parameters up front to produce
// clear error messages instead of confusing downstream failures.
func validateUploadParams(parentID, name, localPath string) error {
	if parentID == "" {
		return fmt.Errorf("upload: parent ID must not be empty")
	}

	if name == "" {
		return fmt.Errorf("upload: file name must not be empty")
	}

	if localPath == "" {
		return fmt.Errorf("upload: local path must not be empty")
	}

	return nil
}

func validateUploadToItemParams(itemID, localPath string) error {
	if itemID == "" {
		return fmt.Errorf("upload: item ID must not be empty")
	}

	if localPath == "" {
		return fmt.Errorf("upload: local path must not be empty")
	}

	return nil
}

func uploadMtime(info os.FileInfo, opts UploadOpts) time.Time {
	if opts.Mtime.IsZero() {
		return info.ModTime()
	}

	return opts.Mtime
}

func uploadHashMismatchError(opts UploadOpts, localPath, actualHash string) error {
	if opts.HashMismatchError != nil {
		return opts.HashMismatchError(localPath, opts.ExpectedHash, actualHash)
	}

	return fmt.Errorf("upload source hash changed for %s: expected %s got %s", localPath, opts.ExpectedHash, actualHash)
}

func validateUploadSourceBeforeRead(opts UploadOpts) error {
	if opts.ValidateSourceBeforeRead == nil {
		return nil
	}

	return opts.ValidateSourceBeforeRead()
}

func validateUploadExpectedHash(opts UploadOpts, localPath, localHash string) error {
	if opts.ExpectedHash == "" || localHash == opts.ExpectedHash {
		return nil
	}

	return uploadHashMismatchError(opts, localPath, localHash)
}

func (tm *TransferManager) uploadGraphItem(
	ctx context.Context,
	su SessionUploader,
	hasSessionUploader bool,
	content io.ReaderAt,
	driveID driveid.ID,
	parentID string,
	name string,
	localPath string,
	localHash string,
	size int64,
	mtime time.Time,
	progress graph.ProgressFunc,
) (*graph.Item, bool, error) {
	if size > persistedSessionMinSize && tm.sessionStore != nil && hasSessionUploader {
		item, err := tm.sessionUpload(ctx, su, content, driveID, parentID, name, localPath, localHash, size, mtime, progress)
		return item, false, err
	}

	item, err := tm.uploads.Upload(ctx, driveID, parentID, name, content, size, mtime, progress)

	return tm.uploadedItemOrError(localPath, item, err)
}

// uploadedItemOrError separates "the content landed but the mtime did not"
// from a failed upload. Failing that upload re-sent content that was already
// on OneDrive, one more version per attempt, and still never set the mtime
// (D-034).
func (tm *TransferManager) uploadedItemOrError(localPath string, item *graph.Item, err error) (*graph.Item, bool, error) {
	if err == nil {
		return item, false, nil
	}

	var mtimeErr *graph.MtimeNotAppliedError
	if errors.As(err, &mtimeErr) && mtimeErr.Item != nil {
		tm.logger.Warn("uploaded the file, but OneDrive didn't accept its modified time",
			slog.String("path", localPath),
			slog.String("error", mtimeErr.Err.Error()),
		)

		return mtimeErr.Item, true, nil
	}

	return nil, false, fmt.Errorf("uploading %s: %w", localPath, err)
}

func recordUploadPerf(ctx context.Context, size int64, startedAt time.Time) {
	collector := perf.FromContext(ctx)
	if collector == nil {
		return
	}

	collector.RecordTransfer(perf.TransferKindUpload, size, time.Since(startedAt))
	collector.AddCommandItems(1)
	collector.AddCommandBytes(size)
}

// UploadFile uploads a local file to OneDrive. For large files when a
// SessionStore and SessionUploader are available, the upload session is
// persisted for cross-crash resume.
func (tm *TransferManager) UploadFile(
	ctx context.Context, driveID driveid.ID, parentID, name, localPath string, opts UploadOpts,
) (*UploadResult, error) {
	startedAt := time.Now()

	if err := validateUploadParams(parentID, name, localPath); err != nil {
		return nil, err
	}

	tm.logger.Debug("UploadFile",
		slog.String("drive_id", driveID.String()),
		slog.String("path", localPath),
		slog.String("name", name),
	)

	info, err := localpath.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", localPath, err)
	}
	if validationErr := validateUploadSourceBeforeRead(opts); validationErr != nil {
		return nil, validationErr
	}

	size := info.Size()
	if size > MaxOneDriveFileSize {
		return nil, fileTooLargeForOneDriveError(localPath, size)
	}

	// Hash the file first (opens, reads, closes internally). The hash is
	// needed before upload starts for session record matching. The file is
	// then re-opened below for the actual upload. This double-open is
	// intentional: hashing consumes an io.Reader sequentially, while upload
	// requires io.ReaderAt for random-access (session-based resume).
	localHash, err := tm.hashFunc(localPath)
	if err != nil {
		return nil, err
	}
	if validationErr := validateUploadExpectedHash(opts, localPath, localHash); validationErr != nil {
		return nil, validationErr
	}
	if validationErr := validateUploadSourceBeforeRead(opts); validationErr != nil {
		return nil, validationErr
	}

	mtime := uploadMtime(info, opts)

	f, err := localpath.OpenRegular(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s for upload: %w", localPath, err)
	}
	defer f.Close() //nolint:errcheck // the file is opened read-only for upload; there is nothing buffered to flush

	progress := opts.Progress

	// For large files with session store + SessionUploader, use session-based upload.
	su, hasSU := tm.uploads.(SessionUploader)
	item, mtimePending, err := tm.uploadGraphItem(
		ctx,
		su,
		hasSU,
		validatingReaderAt{ReaderAt: f, Validate: opts.ValidateSourceBeforeRead},
		driveID,
		parentID,
		name,
		localPath,
		localHash,
		size,
		mtime,
		progress,
	)
	if err != nil {
		return nil, err
	}

	if item == nil {
		return nil, fmt.Errorf("upload of %s returned nil item", localPath)
	}

	// Post-upload hash verification. SelectHash is defined in
	// hash.go — it picks QuickXorHash > SHA256Hash (B-222).
	remoteHash := SelectHash(item)
	if remoteHash != "" && localHash != remoteHash {
		// Info, not Warn: the upload succeeded, and SharePoint rewrites some
		// Office files server-side, so this can recur for every such upload
		// with nothing for the user to do.
		tm.logger.Info("upload hash mismatch",
			slog.String("path", localPath),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)
	}

	tm.logger.Debug("upload complete",
		slog.String("path", localPath),
		slog.String("item_id", item.ID),
		slog.Int64("size", size),
	)
	recordUploadPerf(ctx, size, startedAt)

	return &UploadResult{Item: item, LocalHash: localHash, Size: size, Mtime: mtime, RemoteMtimePending: mtimePending}, nil
}

// UploadFileToItem overwrites an existing remote file identified by item ID.
// For large files when a SessionStore and ItemSessionUploader are available,
// the upload session is persisted for cross-crash resume.
func (tm *TransferManager) UploadFileToItem(
	ctx context.Context, driveID driveid.ID, itemID, localPath string, opts UploadOpts,
) (*UploadResult, error) {
	startedAt := time.Now()

	if err := validateUploadToItemParams(itemID, localPath); err != nil {
		return nil, err
	}

	tm.logger.Debug("UploadFileToItem",
		slog.String("drive_id", driveID.String()),
		slog.String("path", localPath),
		slog.String("item_id", itemID),
	)

	info, err := localpath.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", localPath, err)
	}
	if validationErr := validateUploadSourceBeforeRead(opts); validationErr != nil {
		return nil, validationErr
	}

	size := info.Size()
	if size > MaxOneDriveFileSize {
		return nil, fileTooLargeForOneDriveError(localPath, size)
	}

	localHash, err := tm.hashFunc(localPath)
	if err != nil {
		return nil, err
	}
	if validationErr := validateUploadExpectedHash(opts, localPath, localHash); validationErr != nil {
		return nil, validationErr
	}
	if validationErr := validateUploadSourceBeforeRead(opts); validationErr != nil {
		return nil, validationErr
	}

	mtime := opts.Mtime
	if mtime.IsZero() {
		mtime = info.ModTime()
	}

	f, err := localpath.OpenRegular(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s for upload: %w", localPath, err)
	}
	defer f.Close() //nolint:errcheck // the file is opened read-only for upload; there is nothing buffered to flush

	item, mtimePending, err := tm.uploadExistingItem(
		ctx,
		validatingReaderAt{ReaderAt: f, Validate: opts.ValidateSourceBeforeRead},
		driveID,
		itemID,
		localPath,
		localHash,
		size,
		mtime,
		opts.Progress,
	)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, fmt.Errorf("upload of %s returned nil item", localPath)
	}

	remoteHash := SelectHash(item)
	if remoteHash != "" && localHash != remoteHash {
		// Info, not Warn: the upload succeeded, and SharePoint rewrites some
		// Office files server-side, so this can recur for every such upload
		// with nothing for the user to do.
		tm.logger.Info("upload hash mismatch",
			slog.String("path", localPath),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)
	}

	tm.logger.Debug("upload complete",
		slog.String("path", localPath),
		slog.String("item_id", item.ID),
		slog.Int64("size", size),
	)
	if collector := perf.FromContext(ctx); collector != nil {
		collector.RecordTransfer(perf.TransferKindUpload, size, time.Since(startedAt))
		collector.AddCommandItems(1)
		collector.AddCommandBytes(size)
	}

	return &UploadResult{Item: item, LocalHash: localHash, Size: size, Mtime: mtime, RemoteMtimePending: mtimePending}, nil
}

func (tm *TransferManager) uploadExistingItem(
	ctx context.Context,
	content io.ReaderAt,
	driveID driveid.ID,
	itemID string,
	localPath string,
	localHash string,
	size int64,
	mtime time.Time,
	progress graph.ProgressFunc,
) (*graph.Item, bool, error) {
	sessionUploader, hasSessionUploader := tm.uploads.(ItemSessionUploader)
	itemUploader, hasItemUploader := tm.uploads.(ItemUploader)

	if size > persistedSessionMinSize && tm.sessionStore != nil && hasSessionUploader {
		item, err := tm.sessionUploadToItem(
			ctx,
			sessionUploader,
			content,
			driveID,
			itemID,
			localPath,
			localHash,
			size,
			mtime,
			progress,
		)

		return item, false, err
	}

	if !hasItemUploader {
		return nil, false, fmt.Errorf("uploading %s: uploader does not support item-ID overwrite", localPath)
	}

	item, err := itemUploader.UploadToItem(ctx, driveID, itemID, content, size, mtime, progress)

	return tm.uploadedItemOrError(localPath, item, err)
}

// sessionUpload performs a session-based upload with persistence for resume.
// localPath is the filesystem path of the file being uploaded — used as a
// session store key and in log messages. (Named "localPath" not "remotePath"
// because it identifies the local file, not a remote OneDrive path.)
func (tm *TransferManager) sessionUpload(
	ctx context.Context, su SessionUploader, content io.ReaderAt,
	driveID driveid.ID, parentID, name, localPath, localHash string,
	size int64, mtime time.Time, progress graph.ProgressFunc,
) (*graph.Item, error) {
	tm.logger.Debug("sessionUpload",
		slog.String("path", localPath),
		slog.Int64("size", size),
	)

	driveStr := driveID.String()

	// Check for existing session.
	rec, found, loadErr := tm.sessionStore.Load(driveStr, localPath)
	if loadErr != nil {
		// Debug only: the store already reported what it did with the file,
		// and the upload proceeds with a fresh session.
		tm.logger.Debug("failed to load upload session",
			slog.String("path", localPath),
			slog.String("error", loadErr.Error()),
		)
	}

	if found && rec.FileHash == localHash {
		tm.logger.Debug("attempting upload session resume", slog.String("path", localPath))

		session := &graph.UploadSession{UploadURL: graph.UploadURL(rec.SessionURL)}

		item, resumeErr := su.ResumeUpload(ctx, session, content, size, progress)
		if resumeErr == nil {
			tm.deleteSession(driveStr, localPath)
			return item, nil
		}

		// Delete stale session on any resume failure. Forces fresh session on
		// next attempt, preventing infinite retry loops (B-208).
		tm.deleteSession(driveStr, localPath)

		if !errors.Is(resumeErr, graph.ErrUploadSessionExpired) {
			return nil, fmt.Errorf("resuming upload of %s: %w", localPath, resumeErr)
		}

		tm.logger.Info("upload session expired, creating fresh session", slog.String("path", localPath))
	}

	// Fresh session-based upload.
	session, err := su.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", localPath, err)
	}

	if saveErr := tm.sessionStore.Save(driveStr, localPath, &SessionRecord{
		MountID:    tm.sessionMountID,
		LocalRoot:  tm.sessionLocalRoot,
		SessionURL: string(session.UploadURL),
		FileHash:   localHash,
		FileSize:   size,
	}); saveErr != nil {
		tm.logger.Warn("failed to save upload session — resume after crash will not work for this file",
			slog.String("path", localPath),
			slog.String("error", saveErr.Error()),
		)
	}

	item, err := su.UploadFromSession(ctx, session, content, size, progress)
	if err != nil {
		// Session file persists for next retry.
		return nil, fmt.Errorf("uploading %s: %w", localPath, err)
	}

	tm.deleteSession(driveStr, localPath)

	return item, nil
}

func existingItemSessionKey(localPath, itemID string) string {
	return "item::" + itemID + "::" + localPath
}

func (tm *TransferManager) sessionUploadToItem(
	ctx context.Context, su ItemSessionUploader, content io.ReaderAt,
	driveID driveid.ID, itemID, localPath, localHash string,
	size int64, mtime time.Time, progress graph.ProgressFunc,
) (*graph.Item, error) {
	tm.logger.Debug("sessionUploadToItem",
		slog.String("path", localPath),
		slog.String("item_id", itemID),
		slog.Int64("size", size),
	)

	driveStr := driveID.String()
	sessionKey := existingItemSessionKey(localPath, itemID)

	rec, found, loadErr := tm.sessionStore.Load(driveStr, sessionKey)
	if loadErr != nil {
		// Debug only: see sessionUpload.
		tm.logger.Debug("failed to load upload session",
			slog.String("path", localPath),
			slog.String("item_id", itemID),
			slog.String("error", loadErr.Error()),
		)
	}

	if found && rec.FileHash == localHash {
		tm.logger.Debug("attempting upload session resume",
			slog.String("path", localPath),
			slog.String("item_id", itemID),
		)

		session := &graph.UploadSession{UploadURL: graph.UploadURL(rec.SessionURL)}

		item, resumeErr := su.ResumeUpload(ctx, session, content, size, progress)
		if resumeErr == nil {
			tm.deleteSession(driveStr, sessionKey)
			return item, nil
		}

		tm.deleteSession(driveStr, sessionKey)
		if !errors.Is(resumeErr, graph.ErrUploadSessionExpired) {
			return nil, fmt.Errorf("resuming upload of %s: %w", localPath, resumeErr)
		}

		tm.logger.Info("upload session expired, creating fresh session",
			slog.String("path", localPath),
			slog.String("item_id", itemID),
		)
	}

	session, err := su.CreateUploadSessionForItem(ctx, driveID, itemID, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", localPath, err)
	}

	if saveErr := tm.sessionStore.Save(driveStr, sessionKey, &SessionRecord{
		MountID:    tm.sessionMountID,
		LocalRoot:  tm.sessionLocalRoot,
		SessionURL: string(session.UploadURL),
		FileHash:   localHash,
		FileSize:   size,
	}); saveErr != nil {
		tm.logger.Warn("failed to save upload session — resume after crash will not work for this file",
			slog.String("path", localPath),
			slog.String("item_id", itemID),
			slog.String("error", saveErr.Error()),
		)
	}

	item, err := su.UploadFromSession(ctx, session, content, size, progress)
	if err != nil {
		return nil, fmt.Errorf("uploading %s: %w", localPath, err)
	}

	tm.deleteSession(driveStr, sessionKey)

	return item, nil
}

// deleteSession removes an upload session file, logging on failure. Callers
// use a fire-and-forget pattern since session deletion failures are non-fatal
// (worst case: a stale session file is retried next time).
func (tm *TransferManager) deleteSession(driveID, localPath string) {
	if err := tm.sessionStore.Delete(driveID, localPath); err != nil {
		tm.logger.Warn("failed to delete session file",
			slog.String("path", localPath),
			slog.String("error", err.Error()),
		)
	}
}

// fileTooLargeForOneDriveError names the file and its size in human units;
// the sentinel supplies the limit, so the limit is stated once.
func fileTooLargeForOneDriveError(localPath string, size int64) error {
	return fmt.Errorf("%s is %s: %w", localPath, formatSize(size), ErrFileExceedsOneDriveLimit)
}
