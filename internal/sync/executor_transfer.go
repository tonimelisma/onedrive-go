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

// maxHashRetries is the number of additional download attempts when the
// downloaded content hash doesn't match the remote hash. This is separate from
// network-level retries (withRetry) because hash mismatches indicate corrupted
// content, not transport failures. iOS .heic files are a known source of stale
// remote hashes — after exhausting retries we accept the download to prevent
// an infinite re-download loop (B-132).
const maxHashRetries = 2

// executeDownload downloads a remote file with S3 safety: write to .partial,
// verify hash with retry, atomic rename.
func (e *Executor) executeDownload(ctx context.Context, action *Action) Outcome {
	targetPath := filepath.Join(e.syncRoot, action.Path)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:mnd // standard dir perms
		return e.failedOutcome(action, ActionDownload, fmt.Errorf("creating parent dir for %s: %w", action.Path, err))
	}

	driveID := e.resolveDriveID(action)

	// Extract remote hash before the retry loop so we can compare after each attempt.
	remoteHash := ""
	if action.View != nil && action.View.Remote != nil {
		remoteHash = action.View.Remote.Hash
	}

	var localHash string
	var size int64

	for attempt := range maxHashRetries + 1 {
		// Network-level download with retries (handles 429/5xx/transport errors).
		var dlErr error

		err := e.withRetry(ctx, "download "+action.Path, func() error {
			localHash, size, dlErr = e.downloadToPartial(ctx, action, driveID, targetPath)
			return dlErr
		})
		if err != nil {
			return e.failedOutcome(action, ActionDownload, err)
		}

		// Hash verification — skip if remote didn't provide a hash.
		if remoteHash == "" || localHash == remoteHash {
			break
		}

		if attempt < maxHashRetries {
			// Mismatch: clean up .partial and retry the download.
			os.Remove(targetPath + ".partial")
			e.logger.Warn("download hash mismatch, retrying",
				slog.String("path", action.Path),
				slog.Int("attempt", attempt+1),
				slog.String("local_hash", localHash),
				slog.String("remote_hash", remoteHash),
			)

			continue
		}

		// All hash retries exhausted — accept the download to prevent an infinite
		// re-download loop. The remote hash is likely stale (iOS .heic files).
		// Override remoteHash so baseline records matching hashes.
		e.logger.Warn("download hash mismatch after all retries, accepting download",
			slog.String("path", action.Path),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)

		remoteHash = localHash
	}

	// Set mtime from remote on the partial file before atomic rename.
	if action.View != nil && action.View.Remote != nil && action.View.Remote.Mtime != 0 {
		partialPath := targetPath + ".partial"
		mtime := time.Unix(0, action.View.Remote.Mtime)

		if err := os.Chtimes(partialPath, mtime, mtime); err != nil {
			e.logger.Warn("failed to set mtime on partial", slog.String("path", action.Path), slog.String("error", err.Error()))
		}
	}

	// Atomic rename: .partial -> target.
	if err := os.Rename(targetPath+".partial", targetPath); err != nil {
		return e.failedOutcome(action, ActionDownload, fmt.Errorf("renaming partial to %s: %w", action.Path, err))
	}

	e.logger.Debug("download complete", slog.String("path", action.Path), slog.Int64("size", size))

	return e.downloadOutcome(action, driveID, localHash, remoteHash, size)
}

// downloadToPartial streams a remote file to a .partial file while computing
// QuickXorHash in a single pass. If a .partial file already exists and the
// downloader supports range requests (RangeDownloader), it resumes from the
// existing file (B-085). The hash always covers the full file from byte 0.
// Returns the local hash and size.
func (e *Executor) downloadToPartial(
	ctx context.Context, action *Action, driveID driveid.ID, targetPath string,
) (string, int64, error) {
	partialPath := targetPath + ".partial"

	// B-085: Check for existing .partial file and attempt resume.
	if rd, ok := e.downloads.(RangeDownloader); ok {
		if info, statErr := os.Stat(partialPath); statErr == nil && info.Size() > 0 {
			return e.resumeDownload(ctx, action, driveID, rd, partialPath, info.Size())
		}
	}

	return e.freshDownload(ctx, action, driveID, partialPath)
}

// freshDownload performs a full download to a new .partial file.
func (e *Executor) freshDownload(
	ctx context.Context, action *Action, driveID driveid.ID, partialPath string,
) (string, int64, error) {
	f, err := os.Create(partialPath)
	if err != nil {
		return "", 0, fmt.Errorf("creating partial file %s: %w", partialPath, err)
	}

	// No defer f.Close() — both exit paths close explicitly to avoid double-close.

	h := quickxorhash.New()
	w := io.MultiWriter(f, h)

	size, err := e.downloads.Download(ctx, driveID, action.ItemID, w)
	if err != nil {
		f.Close()
		os.Remove(partialPath)

		return "", 0, fmt.Errorf("downloading %s: %w", action.Path, err)
	}

	if err := f.Close(); err != nil {
		os.Remove(partialPath)

		return "", 0, fmt.Errorf("closing partial file %s: %w", partialPath, err)
	}

	localHash := base64.StdEncoding.EncodeToString(h.Sum(nil))

	return localHash, size, nil
}

// resumeDownload appends bytes to an existing .partial file using Range
// requests, then hashes the complete file from byte 0 (B-085).
func (e *Executor) resumeDownload(
	ctx context.Context, action *Action, driveID driveid.ID,
	rd RangeDownloader, partialPath string, existingSize int64,
) (string, int64, error) {
	e.logger.Debug("resuming download from partial file",
		slog.String("path", action.Path),
		slog.Int64("existing_bytes", existingSize),
	)

	f, err := os.OpenFile(partialPath, os.O_APPEND|os.O_WRONLY, 0o644) //nolint:mnd // standard file perms
	if err != nil {
		// Fall back to fresh download if we can't open for append.
		e.logger.Warn("cannot open partial file for resume, starting fresh",
			slog.String("path", action.Path), slog.String("error", err.Error()))
		os.Remove(partialPath)

		return e.freshDownload(ctx, action, driveID, partialPath)
	}

	n, err := rd.DownloadRange(ctx, driveID, action.ItemID, f, existingSize)
	f.Close()

	if err != nil {
		// On range download failure, remove partial and fall back to fresh.
		e.logger.Warn("range download failed, falling back to fresh download",
			slog.String("path", action.Path), slog.String("error", err.Error()))
		os.Remove(partialPath)

		return e.freshDownload(ctx, action, driveID, partialPath)
	}

	totalSize := existingSize + n

	// Hash the complete .partial file from byte 0 — resume appended bytes
	// but the hash must cover everything for integrity verification.
	localHash, err := computeQuickXorHash(partialPath)
	if err != nil {
		os.Remove(partialPath)
		return "", 0, fmt.Errorf("hashing resumed partial file %s: %w", partialPath, err)
	}

	return localHash, totalSize, nil
}

// downloadOutcome builds a successful Outcome after download.
func (e *Executor) downloadOutcome(
	action *Action, driveID driveid.ID, localHash, remoteHash string, size int64,
) Outcome {
	o := Outcome{
		Action:     ActionDownload,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     action.ItemID,
		ItemType:   ItemTypeFile,
		LocalHash:  localHash,
		RemoteHash: remoteHash,
		Size:       size,
	}

	if action.View != nil && action.View.Remote != nil {
		o.ETag = action.View.Remote.ETag
		o.ParentID = action.View.Remote.ParentID
		o.Mtime = action.View.Remote.Mtime
		o.RemoteMtime = action.View.Remote.Mtime

		if remoteHash == "" {
			o.RemoteHash = action.View.Remote.Hash
		}
	}

	return o
}

// executeUpload uploads a local file to OneDrive. For large files (>4 MiB),
// when a SessionStore and SessionUploader are available, the upload session is
// persisted to disk so it can be resumed after crash/restart.
func (e *Executor) executeUpload(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	parentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	localPath := filepath.Join(e.syncRoot, action.Path)

	info, err := os.Stat(localPath)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, fmt.Errorf("stat %s: %w", action.Path, err))
	}

	localHash, err := e.hashFunc(localPath)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, fmt.Errorf("hashing %s: %w", action.Path, err))
	}

	name := filepath.Base(action.Path)
	size := info.Size()
	mtime := info.ModTime()

	// Open file — *os.File satisfies io.ReaderAt for retry-safe uploads.
	f, err := os.Open(localPath)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, fmt.Errorf("opening %s for upload: %w", action.Path, err))
	}
	defer f.Close()

	progress := func(uploaded, total int64) {
		e.logger.Debug("upload progress",
			slog.String("path", action.Path),
			slog.Int64("uploaded", uploaded),
			slog.Int64("total", total),
		)
	}

	// For large files with session store + SessionUploader, use session-based upload.
	su, hasSU := e.uploads.(SessionUploader)

	var item *graph.Item

	if size > graph.SimpleUploadMaxSize && e.sessionStore != nil && hasSU {
		item, err = e.sessionUpload(ctx, action, su, f, driveID, parentID, name, localHash, size, mtime, progress)
	} else {
		// Small files or no session support: use the standard Uploader interface.
		err = e.withRetry(ctx, "upload "+action.Path, func() error {
			var retryErr error
			item, retryErr = e.uploads.Upload(ctx, driveID, parentID, name, f, size, mtime, progress)

			return retryErr
		})
	}

	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	// Post-upload hash verification.
	remoteHash := selectHash(item)
	if remoteHash != "" && localHash != remoteHash {
		e.logger.Warn("upload hash mismatch",
			slog.String("path", action.Path),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)
	}

	e.logger.Debug("upload complete",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
		slog.Int64("size", size),
	)

	return Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     item.ID,
		ParentID:   parentID,
		ItemType:   ItemTypeFile,
		LocalHash:  localHash,
		RemoteHash: remoteHash,
		Size:       size,
		Mtime:      mtime.UnixNano(),
		ETag:       item.ETag,
	}
}

// sessionUpload performs a session-based upload with persistence for resume.
// Checks the session store for an existing session matching the file hash;
// if found, attempts resume. Otherwise creates a fresh session.
func (e *Executor) sessionUpload(
	ctx context.Context, action *Action, su SessionUploader,
	content io.ReaderAt, driveID driveid.ID, parentID, name, localHash string,
	size int64, mtime time.Time, progress graph.ProgressFunc,
) (*graph.Item, error) {
	remotePath := action.Path
	driveStr := driveID.String()

	// Check for existing session.
	rec, loadErr := e.sessionStore.Load(driveStr, remotePath)
	if loadErr != nil {
		e.logger.Warn("failed to load upload session", slog.String("path", remotePath), slog.String("error", loadErr.Error()))
	}

	if rec != nil && rec.FileHash == localHash {
		e.logger.Debug("attempting upload session resume", slog.String("path", remotePath))

		session := &graph.UploadSession{UploadURL: rec.SessionURL}

		item, resumeErr := su.ResumeUpload(ctx, session, content, size, progress)
		if resumeErr == nil {
			e.deleteSession(driveStr, remotePath)
			return item, nil
		}

		if !errors.Is(resumeErr, graph.ErrUploadSessionExpired) {
			return nil, fmt.Errorf("resuming upload of %s: %w", remotePath, resumeErr)
		}

		// Session expired — fall through to fresh.
		e.deleteSession(driveStr, remotePath)
		e.logger.Info("upload session expired, creating fresh session", slog.String("path", remotePath))
	}

	// Fresh session-based upload.
	session, err := su.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", remotePath, err)
	}

	if saveErr := e.sessionStore.Save(driveStr, remotePath, &SessionRecord{
		SessionURL: session.UploadURL,
		FileHash:   localHash,
		FileSize:   size,
	}); saveErr != nil {
		e.logger.Warn("failed to save upload session", slog.String("path", remotePath), slog.String("error", saveErr.Error()))
	}

	item, err := su.UploadFromSession(ctx, session, content, size, progress)
	if err != nil {
		// Session file persists for next retry.
		return nil, fmt.Errorf("uploading %s: %w", remotePath, err)
	}

	e.deleteSession(driveStr, remotePath)

	return item, nil
}

// deleteSession removes an upload session file, logging on failure.
func (e *Executor) deleteSession(driveID, remotePath string) {
	if err := e.sessionStore.Delete(driveID, remotePath); err != nil {
		e.logger.Warn("failed to delete session file",
			slog.String("path", remotePath),
			slog.String("error", err.Error()),
		)
	}
}
