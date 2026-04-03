package driveops

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// defaultMaxHashRetries is the default number of additional download attempts
// when the content hash doesn't match the remote hash.
const defaultMaxHashRetries = 2

// maxSaneRetries caps MaxHashRetries to prevent integer overflow in the
// `range maxRetries + 1` loop. Any value above this is almost certainly a bug.
const maxSaneRetries = 100

// Download temp files may contain credentials or personal data, so both the
// partial file and its parent directory stay owner-only.
const (
	downloadTempDirPerms  = 0o700
	downloadTempFilePerms = 0o600
)

// resolveMaxRetries returns the effective max hash retries, applying the
// default and overflow-safe upper bound.
func resolveMaxRetries(configured int) int {
	if configured <= 0 {
		return defaultMaxHashRetries
	}

	if configured > maxSaneRetries {
		return maxSaneRetries
	}

	return configured
}

// DownloadOpts configures a single download operation.
type DownloadOpts struct {
	RemoteHash     string // expected hash; empty = skip verification
	RemoteMtime    int64  // nanoseconds; 0 = don't set (not Unix epoch)
	RemoteSize     int64  // expected size; 0 = don't validate
	MaxHashRetries int    // 0 = use default (2 retries, meaning 3 total download attempts)
}

// UploadOpts configures a single upload operation.
type UploadOpts struct {
	Mtime    time.Time
	Progress graph.ProgressFunc
}

// DownloadResult reports the outcome of a successful download.
type DownloadResult struct {
	LocalHash           string
	Size                int64
	EffectiveRemoteHash string // remote hash after possible exhaustion override
	HashVerified        bool   // false when hash retries exhausted and mismatch accepted
}

// UploadResult reports the outcome of a successful upload.
type UploadResult struct {
	Item      *graph.Item
	LocalHash string
	Size      int64
	Mtime     time.Time
}

// TransferManager provides unified download/upload with resume, shared between
// the CLI (files.go) and the sync engine (executor_transfer.go). Handles
// .partial files, range-based resume, hash verification with retry, session
// persistence for uploads, disk space pre-checks, and atomic rename.
type TransferManager struct {
	downloads    Downloader
	uploads      Uploader
	sessionStore *SessionStore // nil = no session persistence for uploads
	logger       *slog.Logger
	hashFunc     func(string) (string, error)

	// minFreeSpace is the minimum free disk space (bytes) required before
	// downloads. Zero disables the check (R-6.4.7). Configured via WithDiskCheck.
	minFreeSpace int64

	// diskAvailableFunc queries available disk space. nil = skip disk check.
	// Injected via WithDiskCheck to allow testing without real statfs calls.
	diskAvailableFunc func(string) (uint64, error)
}

// Option configures a TransferManager. Pass to NewTransferManager.
type Option func(*TransferManager)

// WithDiskCheck enables disk space pre-checks before each download (R-6.2.6).
// minFreeSpace is the minimum free space in bytes; zero disables the check
// (R-6.4.7). fn queries available disk space for a path — typically DiskAvailable.
func WithDiskCheck(minFreeSpace int64, fn func(string) (uint64, error)) Option {
	return func(tm *TransferManager) {
		tm.minFreeSpace = minFreeSpace
		tm.diskAvailableFunc = fn
	}
}

// NewTransferManager creates a TransferManager. sessionStore may be nil if
// upload session persistence is not needed (e.g., small-file-only workflows).
// Options configure optional behaviors like disk space checking.
func NewTransferManager(
	dl Downloader, ul Uploader, store *SessionStore, logger *slog.Logger, opts ...Option,
) *TransferManager {
	tm := &TransferManager{
		downloads:    dl,
		uploads:      ul,
		sessionStore: store,
		logger:       logger,
		hashFunc:     ComputeQuickXorHash,
	}

	for _, opt := range opts {
		opt(tm)
	}

	return tm
}

// DownloadToFile downloads a remote file to targetPath with .partial safety:
// write to .partial, optionally resume from existing .partial, verify hash
// with retry, set mtime, atomic rename to target.
func (tm *TransferManager) DownloadToFile(
	ctx context.Context, driveID driveid.ID, itemID, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	if targetPath == "" {
		return nil, fmt.Errorf("download: target path must not be empty")
	}

	if itemID == "" {
		return nil, fmt.Errorf("download: item ID must not be empty")
	}

	tm.logger.Debug("DownloadToFile",
		slog.String("drive_id", driveID.String()),
		slog.String("target", targetPath),
		slog.String("item_id", itemID),
	)

	// Disk space pre-check (R-6.2.6, R-2.10.43, R-2.10.44).
	// Runs before directory creation or download to avoid partial writes.
	if err := tm.checkDiskSpace(targetPath, opts.RemoteSize); err != nil {
		return nil, err
	}

	// Ensure parent directory exists.
	if err := localpath.MkdirAll(filepath.Dir(targetPath), downloadTempDirPerms); err != nil {
		return nil, fmt.Errorf("creating parent dir for %s: %w", targetPath, err)
	}

	partialPath := targetPath + ".partial"
	remoteHash := opts.RemoteHash
	hashVerified := remoteHash != "" // no verification possible without a remote hash (B-021)
	var localHash string
	var size int64

	// Fast path: no remote hash means no verification — download once, skip retry loop.
	if remoteHash == "" {
		var err error

		localHash, size, err = tm.downloadToPartial(ctx, driveID, itemID, partialPath)
		if err != nil {
			return nil, err
		}

		tm.logger.Warn("remote item has no content hash, skipping verification",
			slog.String("target", targetPath),
		)
	} else {
		var err error
		localHash, size, remoteHash, hashVerified, err = tm.downloadWithHashRetry(
			ctx, driveID, itemID, partialPath, targetPath, remoteHash, opts.MaxHashRetries)
		if err != nil {
			return nil, err
		}
	}

	// Warn if downloaded size doesn't match expected remote size.
	if opts.RemoteSize > 0 && size != opts.RemoteSize {
		tm.logger.Warn("download size mismatch",
			slog.String("target", targetPath),
			slog.Int64("local_size", size),
			slog.Int64("remote_size", opts.RemoteSize),
		)
	}

	// Set mtime on the partial file before atomic rename.
	if opts.RemoteMtime != 0 {
		mtime := time.Unix(0, opts.RemoteMtime)
		if err := localpath.Chtimes(partialPath, mtime, mtime); err != nil {
			tm.logger.Warn("failed to set mtime on partial",
				slog.String("target", targetPath),
				slog.String("error", err.Error()),
			)
		}
	}

	// Atomic rename: .partial -> target. On failure the .partial file is
	// intentionally preserved so the next attempt can resume from it rather
	// than re-downloading the entire file (B-207).
	if err := localpath.Rename(partialPath, targetPath); err != nil {
		return nil, fmt.Errorf("renaming partial to %s: %w", targetPath, err)
	}

	tm.logger.Debug("download complete",
		slog.String("target", targetPath),
		slog.Int64("size", size),
	)

	return &DownloadResult{
		LocalHash:           localHash,
		Size:                size,
		EffectiveRemoteHash: remoteHash,
		HashVerified:        hashVerified,
	}, nil
}

// downloadWithHashRetry downloads a file and retries on hash mismatch. On
// mismatch we discard and re-download the entire file. If the first attempt
// was a resume, the resume bytes are wasted — acceptable because mismatches
// are rare and correctness trumps bandwidth savings.
func (tm *TransferManager) downloadWithHashRetry(
	ctx context.Context, driveID driveid.ID, itemID, partialPath, targetPath, remoteHash string,
	maxHashRetries int,
) (localHash string, size int64, effectiveRemoteHash string, hashVerified bool, err error) {
	effectiveRemoteHash = remoteHash
	hashVerified = true

	// Go 1.22 range-over-int: `range N` iterates 0..N-1, so `range maxRetries+1`
	// gives exactly maxRetries+1 iterations (1 initial + maxRetries retries) (B-221).
	maxRetries := resolveMaxRetries(maxHashRetries)

	for attempt := range maxRetries + 1 {
		localHash, size, err = tm.downloadToPartial(ctx, driveID, itemID, partialPath)
		if err != nil {
			return "", 0, "", false, err
		}

		// Hash matches — verification succeeded.
		if localHash == effectiveRemoteHash {
			return localHash, size, effectiveRemoteHash, true, nil
		}

		if attempt < maxRetries {
			if cleanupErr := localpath.Remove(partialPath); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrNotExist) {
				tm.logger.Warn("failed to remove partial download before retry",
					slog.String("path", partialPath),
					slog.String("error", cleanupErr.Error()))
			}
			tm.logger.Warn("download hash mismatch, retrying",
				slog.String("target", targetPath),
				slog.Int("attempt", attempt+1),
				slog.String("local_hash", localHash),
				slog.String("remote_hash", effectiveRemoteHash),
			)

			continue
		}

		// All hash retries exhausted — accept to prevent infinite loop.
		tm.logger.Warn("download hash mismatch after all retries, accepting download",
			slog.String("target", targetPath),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", effectiveRemoteHash),
		)

		effectiveRemoteHash = localHash
		hashVerified = false
	}

	return localHash, size, effectiveRemoteHash, hashVerified, nil
}

// downloadToPartial streams a remote file to a .partial file while computing
// the QuickXorHash. If a .partial file already exists and the downloader
// supports range requests, it resumes from the existing file.
//
// The .partial file is opened before stat to avoid a TOCTOU race where the
// file could be deleted between stat and open (B-211). If open fails with
// ErrNotExist, we fall through to a fresh download.
func (tm *TransferManager) downloadToPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	// Attempt resume: open existing .partial, then stat the handle.
	if rd, ok := tm.downloads.(RangeDownloader); ok {
		f, openErr := localpath.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, downloadTempFilePerms)
		if openErr == nil {
			info, statErr := f.Stat()
			if statErr != nil || info.Size() == 0 {
				// Empty or unreadable — close and start fresh.
				if closeErr := f.Close(); closeErr != nil {
					tm.logger.Warn("failed to close partial file before fresh download",
						slog.String("path", partialPath),
						slog.String("error", closeErr.Error()))
				}
			} else {
				return tm.resumeDownloadFromFile(ctx, driveID, itemID, rd, f, partialPath, info.Size())
			}
		} else if !errors.Is(openErr, os.ErrNotExist) {
			tm.logger.Warn("cannot open partial file for resume, starting fresh",
				slog.String("path", partialPath), slog.String("error", openErr.Error()))
		}
	}

	return tm.freshDownload(ctx, driveID, itemID, partialPath)
}

// removePartialIfNotCanceled removes a .partial file unless the context was
// canceled (preserving the partial for future resume). Extracted to deduplicate
// the identical pattern in freshDownload and resumeDownload.
func (tm *TransferManager) removePartialIfNotCanceled(ctx context.Context, path string) {
	if ctx.Err() == nil {
		if removeErr := localpath.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			tm.logger.Warn("failed to remove partial file after interrupted transfer")
		}
	}
}

// freshDownload performs a full download to a new .partial file.
func (tm *TransferManager) freshDownload(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	f, err := localpath.OpenFile(partialPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, downloadTempFilePerms)
	if err != nil {
		return "", 0, fmt.Errorf("creating partial file %s: %w", partialPath, err)
	}

	h := quickxorhash.New()
	w := io.MultiWriter(f, h)

	size, err := tm.downloads.Download(ctx, driveID, itemID, w)
	if err != nil {
		if closeErr := f.Close(); closeErr != nil {
			tm.logger.Warn("failed to close partial file after download error",
				slog.String("path", partialPath), slog.String("error", closeErr.Error()))
		}

		// Preserve partial on context cancellation so resume can reuse it.
		tm.removePartialIfNotCanceled(ctx, partialPath)

		return "", 0, fmt.Errorf("downloading to %s: %w", partialPath, err)
	}

	if err := f.Close(); err != nil {
		// Close failure is always an error regardless of context — the file is corrupt.
		baseErr := fmt.Errorf("closing partial file %s: %w", partialPath, err)
		if removeErr := localpath.Remove(partialPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", 0, errors.Join(baseErr, fmt.Errorf("removing corrupt partial file %s: %w", partialPath, removeErr))
		}

		return "", 0, baseErr
	}

	localHash := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return localHash, size, nil
}

// resumeDownloadFromFile appends bytes to an already-open .partial file using
// Range requests, then hashes the complete file from byte 0. The caller opens
// the file and passes it to avoid a TOCTOU race (B-211).
func (tm *TransferManager) resumeDownloadFromFile(
	ctx context.Context, driveID driveid.ID, itemID string,
	rd RangeDownloader, f *os.File, partialPath string, existingSize int64,
) (string, int64, error) {
	tm.logger.Debug("resuming download from partial file",
		slog.String("path", partialPath),
		slog.Int64("existing_bytes", existingSize),
	)

	n, err := rd.DownloadRange(ctx, driveID, itemID, f, existingSize)

	if closeErr := f.Close(); closeErr != nil {
		tm.logger.Warn("failed to close partial file after range download",
			slog.String("path", partialPath), slog.String("error", closeErr.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	if err != nil {
		tm.logger.Warn("range download failed, falling back to fresh download",
			slog.String("path", partialPath), slog.String("error", err.Error()))

		tm.removePartialIfNotCanceled(ctx, partialPath)

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	totalSize := existingSize + n

	localHash, err := tm.hashFunc(partialPath)
	if err != nil {
		tm.removePartialIfNotCanceled(ctx, partialPath)

		return "", 0, fmt.Errorf("hashing resumed partial file %s: %w", partialPath, err)
	}

	return localHash, totalSize, nil
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

// UploadFile uploads a local file to OneDrive. For large files when a
// SessionStore and SessionUploader are available, the upload session is
// persisted for cross-crash resume.
func (tm *TransferManager) UploadFile(
	ctx context.Context, driveID driveid.ID, parentID, name, localPath string, opts UploadOpts,
) (*UploadResult, error) {
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

	// Hash the file first (opens, reads, closes internally). The hash is
	// needed before upload starts for session record matching. The file is
	// then re-opened below for the actual upload. This double-open is
	// intentional: hashing consumes an io.Reader sequentially, while upload
	// requires io.ReaderAt for random-access (session-based resume).
	localHash, err := tm.hashFunc(localPath)
	if err != nil {
		return nil, fmt.Errorf("hashing %s: %w", localPath, err)
	}

	size := info.Size()
	mtime := opts.Mtime
	if mtime.IsZero() {
		mtime = info.ModTime()
	}

	f, err := localpath.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s for upload: %w", localPath, err)
	}
	defer f.Close()

	progress := opts.Progress

	// For large files with session store + SessionUploader, use session-based upload.
	su, hasSU := tm.uploads.(SessionUploader)

	var item *graph.Item

	if size > graph.SimpleUploadMaxSize && tm.sessionStore != nil && hasSU {
		item, err = tm.sessionUpload(ctx, su, f, driveID, parentID, name, localPath, localHash, size, mtime, progress)
	} else {
		item, err = tm.uploads.Upload(ctx, driveID, parentID, name, f, size, mtime, progress)
		if err != nil {
			err = fmt.Errorf("uploading %s: %w", localPath, err)
		}
	}

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
		tm.logger.Warn("upload hash mismatch",
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

	return &UploadResult{Item: item, LocalHash: localHash, Size: size, Mtime: mtime}, nil
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
		tm.logger.Warn("failed to load upload session",
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

// checkDiskSpace verifies sufficient disk space before a download (R-6.2.6).
// No-op when diskAvailableFunc is nil or minFreeSpace is 0.
//
// Two-tier check:
//   - Available < minFreeSpace → ErrDiskFull (scope block in sync engine)
//   - Available ≥ minFreeSpace but < fileSize + minFreeSpace → ErrFileTooLargeForSpace (per-file skip)
//
// Fails open on statfs errors: a transient statfs failure should not block
// all downloads.
func (tm *TransferManager) checkDiskSpace(targetPath string, fileSize int64) error {
	if tm.diskAvailableFunc == nil || tm.minFreeSpace == 0 {
		return nil
	}

	// Check the filesystem containing the target's parent directory.
	checkPath := filepath.Dir(targetPath)

	available, err := tm.diskAvailableFunc(checkPath)
	if err != nil {
		// Fail open: a transient statfs error should not block downloads.
		tm.logger.Warn("disk space check failed, proceeding with download",
			slog.String("path", checkPath),
			slog.String("error", err.Error()),
		)

		return nil
	}

	minFree := uint64(tm.minFreeSpace)
	if available < minFree {
		return fmt.Errorf("%w: available %d bytes < min_free_space %d bytes", ErrDiskFull, available, minFree)
	}

	// Per-file check: can this specific file fit within available - minFreeSpace?
	if fileSize > 0 && available < uint64(fileSize)+minFree {
		return fmt.Errorf("%w: need %d bytes, available %d bytes (after reserving %d)",
			ErrFileTooLargeForSpace, fileSize, available-minFree, minFree)
	}

	return nil
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
