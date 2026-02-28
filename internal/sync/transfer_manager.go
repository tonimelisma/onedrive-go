package sync

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
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// defaultMaxHashRetries is the default number of additional download attempts
// when the content hash doesn't match the remote hash.
const defaultMaxHashRetries = 2

// DownloadOpts configures a single download operation.
type DownloadOpts struct {
	RemoteHash     string // expected hash; empty = skip verification
	RemoteMtime    int64  // nanoseconds; 0 = don't set
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
// persistence for uploads, and atomic rename.
type TransferManager struct {
	downloads    Downloader
	uploads      Uploader
	sessionStore *SessionStore // nil = no session persistence for uploads
	logger       *slog.Logger
	hashFunc     func(string) (string, error)
}

// NewTransferManager creates a TransferManager. sessionStore may be nil if
// upload session persistence is not needed (e.g., small-file-only workflows).
func NewTransferManager(
	dl Downloader, ul Uploader, store *SessionStore, logger *slog.Logger,
) *TransferManager {
	return &TransferManager{
		downloads:    dl,
		uploads:      ul,
		sessionStore: store,
		logger:       logger,
		hashFunc:     computeQuickXorHash,
	}
}

// DownloadToFile downloads a remote file to targetPath with .partial safety:
// write to .partial, optionally resume from existing .partial, verify hash
// with retry, set mtime, atomic rename to target.
func (tm *TransferManager) DownloadToFile(
	ctx context.Context, driveID driveid.ID, itemID, targetPath string, opts DownloadOpts,
) (*DownloadResult, error) {
	tm.logger.Debug("DownloadToFile",
		slog.String("drive_id", driveID.String()),
		slog.String("target", targetPath),
		slog.String("item_id", itemID),
	)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o700); err != nil { //nolint:mnd // owner-only dir perms
		return nil, fmt.Errorf("creating parent dir for %s: %w", targetPath, err)
	}

	partialPath := targetPath + ".partial"
	maxRetries := opts.MaxHashRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxHashRetries
	}

	remoteHash := opts.RemoteHash
	var localHash string
	var size int64

	// On hash mismatch with retry, we discard and re-download the entire file.
	// If the first attempt was a resume, the resume bytes are wasted — this is
	// acceptable because hash mismatches are rare and correctness trumps
	// bandwidth savings.
	for attempt := range maxRetries + 1 {
		var err error

		localHash, size, err = tm.downloadToPartial(ctx, driveID, itemID, partialPath)
		if err != nil {
			return nil, err
		}

		// Hash verification — skip if remote didn't provide a hash.
		if remoteHash == "" || localHash == remoteHash {
			break
		}

		if attempt < maxRetries {
			os.Remove(partialPath)
			tm.logger.Warn("download hash mismatch, retrying",
				slog.String("target", targetPath),
				slog.Int("attempt", attempt+1),
				slog.String("local_hash", localHash),
				slog.String("remote_hash", remoteHash),
			)

			continue
		}

		// All hash retries exhausted — accept to prevent infinite loop.
		tm.logger.Warn("download hash mismatch after all retries, accepting download",
			slog.String("target", targetPath),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)

		remoteHash = localHash
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
		if err := os.Chtimes(partialPath, mtime, mtime); err != nil {
			tm.logger.Warn("failed to set mtime on partial",
				slog.String("target", targetPath),
				slog.String("error", err.Error()),
			)
		}
	}

	// Atomic rename: .partial -> target.
	if err := os.Rename(partialPath, targetPath); err != nil {
		return nil, fmt.Errorf("renaming partial to %s: %w", targetPath, err)
	}

	tm.logger.Debug("download complete",
		slog.String("target", targetPath),
		slog.Int64("size", size),
	)

	return &DownloadResult{LocalHash: localHash, Size: size, EffectiveRemoteHash: remoteHash}, nil
}

// downloadToPartial streams a remote file to a .partial file while computing
// the QuickXorHash. If a .partial file already exists and the downloader
// supports range requests, it resumes from the existing file.
func (tm *TransferManager) downloadToPartial(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	// Check for existing .partial file and attempt resume.
	if rd, ok := tm.downloads.(RangeDownloader); ok {
		if info, statErr := os.Stat(partialPath); statErr == nil && info.Size() > 0 {
			return tm.resumeDownload(ctx, driveID, itemID, rd, partialPath, info.Size())
		}
	}

	return tm.freshDownload(ctx, driveID, itemID, partialPath)
}

// freshDownload performs a full download to a new .partial file.
func (tm *TransferManager) freshDownload(
	ctx context.Context, driveID driveid.ID, itemID, partialPath string,
) (string, int64, error) {
	f, err := os.Create(partialPath)
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
		if ctx.Err() == nil {
			os.Remove(partialPath)
		}

		return "", 0, fmt.Errorf("downloading to %s: %w", partialPath, err)
	}

	if err := f.Close(); err != nil {
		os.Remove(partialPath)

		return "", 0, fmt.Errorf("closing partial file %s: %w", partialPath, err)
	}

	localHash := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return localHash, size, nil
}

// resumeDownload appends bytes to an existing .partial file using Range
// requests, then hashes the complete file from byte 0.
func (tm *TransferManager) resumeDownload(
	ctx context.Context, driveID driveid.ID, itemID string,
	rd RangeDownloader, partialPath string, existingSize int64,
) (string, int64, error) {
	tm.logger.Debug("resuming download from partial file",
		slog.String("path", partialPath),
		slog.Int64("existing_bytes", existingSize),
	)

	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0o600) //nolint:mnd // owner-only
	if err != nil {
		tm.logger.Warn("cannot open partial file for resume, starting fresh",
			slog.String("path", partialPath), slog.String("error", err.Error()))

		if ctx.Err() == nil {
			os.Remove(partialPath)
		}

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	n, err := rd.DownloadRange(ctx, driveID, itemID, f, existingSize)

	if closeErr := f.Close(); closeErr != nil {
		tm.logger.Warn("failed to close partial file after range download",
			slog.String("path", partialPath), slog.String("error", closeErr.Error()))

		if ctx.Err() == nil {
			os.Remove(partialPath)
		}

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	if err != nil {
		tm.logger.Warn("range download failed, falling back to fresh download",
			slog.String("path", partialPath), slog.String("error", err.Error()))

		if ctx.Err() == nil {
			os.Remove(partialPath)
		}

		return tm.freshDownload(ctx, driveID, itemID, partialPath)
	}

	totalSize := existingSize + n

	localHash, err := computeQuickXorHash(partialPath)
	if err != nil {
		if ctx.Err() == nil {
			os.Remove(partialPath)
		}

		return "", 0, fmt.Errorf("hashing resumed partial file %s: %w", partialPath, err)
	}

	return localHash, totalSize, nil
}

// UploadFile uploads a local file to OneDrive. For large files when a
// SessionStore and SessionUploader are available, the upload session is
// persisted for cross-crash resume.
func (tm *TransferManager) UploadFile(
	ctx context.Context, driveID driveid.ID, parentID, name, localPath string, opts UploadOpts,
) (*UploadResult, error) {
	tm.logger.Debug("UploadFile",
		slog.String("drive_id", driveID.String()),
		slog.String("path", localPath),
		slog.String("name", name),
	)

	info, err := os.Stat(localPath)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", localPath, err)
	}

	localHash, err := tm.hashFunc(localPath)
	if err != nil {
		return nil, fmt.Errorf("hashing %s: %w", localPath, err)
	}

	size := info.Size()
	mtime := opts.Mtime
	if mtime.IsZero() {
		mtime = info.ModTime()
	}

	f, err := os.Open(localPath)
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

	// Post-upload hash verification.
	remoteHash := selectHash(item)
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
func (tm *TransferManager) sessionUpload(
	ctx context.Context, su SessionUploader, content io.ReaderAt,
	driveID driveid.ID, parentID, name, remotePath, localHash string,
	size int64, mtime time.Time, progress graph.ProgressFunc,
) (*graph.Item, error) {
	tm.logger.Debug("sessionUpload",
		slog.String("path", remotePath),
		slog.Int64("size", size),
	)

	driveStr := driveID.String()

	// Check for existing session.
	rec, loadErr := tm.sessionStore.Load(driveStr, remotePath)
	if loadErr != nil {
		tm.logger.Warn("failed to load upload session",
			slog.String("path", remotePath),
			slog.String("error", loadErr.Error()),
		)
	}

	if rec != nil && rec.FileHash == localHash {
		tm.logger.Debug("attempting upload session resume", slog.String("path", remotePath))

		session := &graph.UploadSession{UploadURL: rec.SessionURL}

		item, resumeErr := su.ResumeUpload(ctx, session, content, size, progress)
		if resumeErr == nil {
			tm.deleteSession(driveStr, remotePath)
			return item, nil
		}

		// Delete stale session on any resume failure. Forces fresh session on
		// next attempt, preventing infinite retry loops (B-208).
		tm.deleteSession(driveStr, remotePath)

		if !errors.Is(resumeErr, graph.ErrUploadSessionExpired) {
			return nil, fmt.Errorf("resuming upload of %s: %w", remotePath, resumeErr)
		}

		tm.logger.Info("upload session expired, creating fresh session", slog.String("path", remotePath))
	}

	// Fresh session-based upload.
	session, err := su.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", remotePath, err)
	}

	if saveErr := tm.sessionStore.Save(driveStr, remotePath, &SessionRecord{
		SessionURL: session.UploadURL,
		FileHash:   localHash,
		FileSize:   size,
	}); saveErr != nil {
		tm.logger.Warn("failed to save upload session — resume after crash will not work for this file",
			slog.String("path", remotePath),
			slog.String("error", saveErr.Error()),
		)
	}

	item, err := su.UploadFromSession(ctx, session, content, size, progress)
	if err != nil {
		// Session file persists for next retry.
		return nil, fmt.Errorf("uploading %s: %w", remotePath, err)
	}

	tm.deleteSession(driveStr, remotePath)

	return item, nil
}

// deleteSession removes an upload session file, logging on failure.
func (tm *TransferManager) deleteSession(driveID, remotePath string) {
	if err := tm.sessionStore.Delete(driveID, remotePath); err != nil {
		tm.logger.Warn("failed to delete session file",
			slog.String("path", remotePath),
			slog.String("error", err.Error()),
		)
	}
}
