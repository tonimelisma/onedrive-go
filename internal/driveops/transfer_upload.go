package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/humanunit"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// UploadOpts configures a single upload operation.
type UploadOpts struct {
	Mtime        time.Time
	Progress     graph.ProgressFunc
	ExpectedHash string
	// PlannedRemoteETag is the eTag the overwritten item had when the
	// upload was planned. An uncertain upload adopts a hashless or enriched
	// item only when its eTag moved on from this one.
	PlannedRemoteETag string
	// IfMatch, when set, makes an overwrite by item ID conditional on this
	// eTag: Graph answers 412 (graph.ErrPreconditionFailed) if the item
	// changed after the caller's preflight read it, so a remote edit made
	// during a long upload is not silently replaced (D-022, L-02). Sync
	// passes the eTag its live preflight saw, not PlannedRemoteETag: a
	// planned remote move bumps the eTag before the content upload.
	IfMatch string
	// OnCreated, when set, runs after a new file's empty item exists and
	// before its content is sent, with that item's ID and eTag. Sync records
	// the pending create durably there (R-5.6.15); an error aborts the upload
	// and removes the empty item.
	OnCreated                func(itemID, createdETag string) error
	HashMismatchError        func(localPath, expectedHash, actualHash string) error
	ValidateSourceBeforeRead func() error
}

// UploadResult reports the outcome of a successful upload.
type UploadResult struct {
	Item      *graph.Item
	LocalHash string
	Size      int64
	Mtime     time.Time
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

// uploadMtime is the modification time an upload, a create or an overwrite,
// sets on OneDrive: the caller's, or else the local file's own.
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

	return fmt.Errorf("upload source hash changed: expected %s got %s", opts.ExpectedHash, actualHash)
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
	onCreated func(itemID, createdETag string) error,
	sent uploadedContent,
) (*graph.Item, error) {
	if size > persistedSessionMinSize && tm.sessionStore != nil && hasSessionUploader {
		item, err := tm.persistedSessionUpload(ctx, &persistedUpload{
			target:  uploadTarget{driveID: driveID, parentID: parentID, name: name},
			key:     localPath,
			content: content, localPath: localPath, localHash: localHash, size: size, progress: progress,
			create: func() (*graph.UploadSession, error) {
				return su.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
			},
			onCreated: onCreated,
			createForCreated: func(itemID, createdETag string) (*graph.UploadSession, error) {
				isu, ok := tm.uploads.(ItemSessionUploader)
				if !ok {
					return nil, fmt.Errorf("uploader cannot open a session for an existing item")
				}
				return isu.CreateUploadSessionForItem(ctx, driveID, itemID, createdETag, size, mtime)
			},
			sessions: su,
			sent:     sent,
		})
		return item, err
	}

	if creating, ok := tm.uploads.(CreatingUploader); ok && onCreated != nil {
		item, err := creating.UploadCreating(ctx, driveID, parentID, name, content, size, mtime, progress,
			func(created *graph.Item) error { return onCreated(created.ID, created.ETag) })

		return tm.uploadedItemOrError(localPath, item, err)
	}

	item, err := tm.uploads.Upload(ctx, driveID, parentID, name, content, size, mtime, progress)

	return tm.uploadedItemOrError(localPath, item, err)
}

// uploadedItemOrError separates "the content landed but the mtime did not"
// from a failed upload. Failing that upload re-sent content that was already
// on OneDrive, one more version per attempt, and still never set the mtime
// (D-034).
func (tm *TransferManager) uploadedItemOrError(localPath string, item *graph.Item, err error) (*graph.Item, error) {
	if err == nil {
		return item, nil
	}

	var mtimeErr *graph.MtimeNotAppliedError
	if errors.As(err, &mtimeErr) && mtimeErr.Item != nil {
		tm.logger.Warn("uploaded the file, but OneDrive didn't accept its modified time",
			slog.String("path", localPath),
			slog.Any("error", mtimeErr.Err),
		)

		return mtimeErr.Item, nil
	}

	return nil, fmt.Errorf("sending the file: %w", err)
}

// recordUploadPerf counts one finished upload, a create or an overwrite, of
// size bytes in the command's performance collector, when ctx carries one.
func recordUploadPerf(ctx context.Context, size int64, startedAt time.Time) {
	collector := perf.FromContext(ctx)
	if collector == nil {
		return
	}

	collector.RecordTransfer(perf.TransferKindUpload, size, time.Since(startedAt))
	collector.AddCommandItems(1)
	collector.AddCommandBytes(size)
}

// uploadSource is a local file checked, hashed and opened for one upload.
type uploadSource struct {
	file  *os.File
	size  int64
	hash  string
	mtime time.Time
}

// prepareUploadSource stats localPath, checks the source and the size limit,
// hashes the file, checks the expected hash and the source again, and opens
// the file for the upload, in that order. A create and an overwrite share it.
// The caller closes the returned file. localpath names the path in a failure
// of the file; no failure here names it again.
func (tm *TransferManager) prepareUploadSource(
	ctx context.Context, localPath string, opts UploadOpts,
) (*uploadSource, error) {
	info, err := localpath.Stat(localPath)
	if err != nil {
		return nil, err //nolint:wrapcheck // localpath names the stat and the path
	}
	if validationErr := validateUploadSourceBeforeRead(opts); validationErr != nil {
		return nil, validationErr
	}

	size := info.Size()
	if size > MaxOneDriveFileSize {
		return nil, fileTooLargeForOneDriveError(size)
	}

	// Hash the file first (opens, reads, closes internally). The hash is
	// needed before upload starts for session record matching. The file is
	// then re-opened below for the actual upload. This double-open is
	// intentional: hashing consumes an io.Reader sequentially, while upload
	// requires io.ReaderAt for random-access (session-based resume).
	localHash, err := tm.hashFunc(ctx, localPath)
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
		return nil, err //nolint:wrapcheck // localpath names the open and the path
	}

	return &uploadSource{file: f, size: size, hash: localHash, mtime: mtime}, nil
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

	src, err := tm.prepareUploadSource(ctx, localPath, opts)
	if err != nil {
		return nil, err
	}
	defer src.file.Close() //nolint:errcheck // the file is opened read-only for upload; there is nothing buffered to flush

	// For large files with session store + SessionUploader, use session-based upload.
	su, hasSU := tm.uploads.(SessionUploader)
	sent := uploadedContent{
		size: src.size, quickXorHash: src.hash, name: name, mtime: src.mtime, planETag: opts.PlannedRemoteETag,
	}
	item, err := tm.uploadGraphItem(
		ctx,
		su,
		hasSU,
		validatingReaderAt{ReaderAt: src.file, Validate: opts.ValidateSourceBeforeRead},
		driveID,
		parentID,
		name,
		localPath,
		src.hash,
		src.size,
		src.mtime,
		opts.Progress,
		opts.OnCreated,
		sent,
	)
	item, err = tm.settleUploadedItem(ctx,
		uploadTarget{driveID: driveID, parentID: parentID, name: name},
		sent,
		localPath, item, err)
	if err != nil {
		return nil, err
	}

	tm.logger.Debug("upload complete",
		slog.String("path", localPath),
		slog.String("item_id", item.ID),
		slog.Int64("size", src.size),
	)
	recordUploadPerf(ctx, src.size, startedAt)

	return &UploadResult{Item: item, LocalHash: src.hash, Size: src.size, Mtime: src.mtime}, nil
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

	src, err := tm.prepareUploadSource(ctx, localPath, opts)
	if err != nil {
		return nil, err
	}
	defer src.file.Close() //nolint:errcheck // the file is opened read-only for upload; there is nothing buffered to flush

	sent := uploadedContent{
		size: src.size, quickXorHash: src.hash, name: filepath.Base(localPath), mtime: src.mtime, planETag: opts.PlannedRemoteETag,
	}
	item, err := tm.uploadExistingItem(
		ctx,
		validatingReaderAt{ReaderAt: src.file, Validate: opts.ValidateSourceBeforeRead},
		driveID,
		itemID,
		localPath,
		src.hash,
		src.size,
		src.mtime,
		opts.Progress,
		opts.IfMatch,
		sent,
	)
	item, err = tm.settleUploadedItem(ctx,
		uploadTarget{driveID: driveID, itemID: itemID},
		sent,
		localPath, item, err)
	if err != nil {
		return nil, err
	}

	tm.logger.Debug("upload complete",
		slog.String("path", localPath),
		slog.String("item_id", item.ID),
		slog.Int64("size", src.size),
	)
	recordUploadPerf(ctx, src.size, startedAt)

	return &UploadResult{Item: item, LocalHash: src.hash, Size: src.size, Mtime: src.mtime}, nil
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
	ifMatch string,
	sent uploadedContent,
) (*graph.Item, error) {
	sessionUploader, hasSessionUploader := tm.uploads.(ItemSessionUploader)
	itemUploader, hasItemUploader := tm.uploads.(ItemUploader)

	if size > persistedSessionMinSize && tm.sessionStore != nil && hasSessionUploader {
		item, err := tm.persistedSessionUpload(ctx, &persistedUpload{
			target:  uploadTarget{driveID: driveID, itemID: itemID},
			key:     existingItemSessionKey(localPath, itemID),
			content: content, localPath: localPath, localHash: localHash, size: size, progress: progress,
			create: func() (*graph.UploadSession, error) {
				return sessionUploader.CreateUploadSessionForItem(ctx, driveID, itemID, ifMatch, size, mtime)
			},
			sessions: sessionUploader,
			sent:     sent,
		})

		return item, err
	}

	if !hasItemUploader {
		return nil, errors.New("sending the file: the uploader does not support item-ID overwrite")
	}

	item, err := itemUploader.UploadToItem(ctx, driveID, itemID, ifMatch, content, size, mtime, progress)

	return tm.uploadedItemOrError(localPath, item, err)
}

// deleteSession removes an upload session file, logging on failure. Callers
// use a fire-and-forget pattern since session deletion failures are non-fatal
// (worst case: a stale session file is retried next time).
func (tm *TransferManager) deleteSession(driveID, localPath string) {
	if err := tm.sessionStore.Delete(driveID, localPath); err != nil {
		tm.logger.Warn("failed to delete session file",
			slog.String("path", localPath),
			slog.Any("error", err),
		)
	}
}

// fileTooLargeForOneDriveError gives the file's size in human units; the
// sentinel supplies the limit, so the limit is stated once, and the caller
// names the file.
func fileTooLargeForOneDriveError(size int64) error {
	return fmt.Errorf("the file is %s: %w", humanunit.Size(size), ErrFileExceedsOneDriveLimit)
}
