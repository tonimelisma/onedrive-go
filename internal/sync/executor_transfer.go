package sync

import (
	"context"
	"encoding/base64"
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
// QuickXorHash in a single pass. Returns the local hash and size.
func (e *Executor) downloadToPartial(
	ctx context.Context, action *Action, driveID driveid.ID, targetPath string,
) (string, int64, error) {
	partialPath := targetPath + ".partial"

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

// executeUpload uploads a local file to OneDrive via the Uploader interface,
// which encapsulates the simple-vs-chunked decision and session lifecycle.
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

	var item *graph.Item

	progress := func(uploaded, total int64) {
		e.logger.Debug("upload progress",
			slog.String("path", action.Path),
			slog.Int64("uploaded", uploaded),
			slog.Int64("total", total),
		)
	}

	// Retry wraps the full Upload() call. On retry of a chunked upload, this
	// restarts the session from scratch. Acceptable because: (1) io.ReaderAt
	// allows re-reading from offset 0, (2) Graph API auto-cleans abandoned
	// sessions, (3) executor-level retry is rare (graph client retries
	// 429/5xx internally). Session resume tracked as B-037.
	uploadErr := e.withRetry(ctx, "upload "+action.Path, func() error {
		var retryErr error
		item, retryErr = e.uploads.Upload(ctx, driveID, parentID, name, f, size, mtime, progress)

		return retryErr
	})
	if uploadErr != nil {
		return e.failedOutcome(action, ActionUpload, uploadErr)
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
