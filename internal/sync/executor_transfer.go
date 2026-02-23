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

// Upload size thresholds.
const (
	simpleUploadMaxBytes = 4 * 1024 * 1024  // 4 MiB — use simple upload below this
	chunkedUploadChunk   = 10 * 1024 * 1024 // 10 MiB per chunk
)

// executeDownload downloads a remote file with S3 safety: write to .partial,
// verify hash, atomic rename. Warns on hash mismatch (iOS .heic bug).
func (e *Executor) executeDownload(ctx context.Context, action *Action) Outcome {
	targetPath := filepath.Join(e.syncRoot, action.Path)

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil { //nolint:mnd // standard dir perms
		return e.failedOutcome(action, ActionDownload, fmt.Errorf("creating parent dir for %s: %w", action.Path, err))
	}

	driveID := e.resolveDriveID(action)

	var localHash string
	var size int64
	var dlErr error

	err := e.withRetry(ctx, "download "+action.Path, func() error {
		localHash, size, dlErr = e.downloadToPartial(ctx, action, driveID, targetPath)
		return dlErr
	})
	if err != nil {
		return e.failedOutcome(action, ActionDownload, err)
	}

	// Verify hash if remote provided one.
	remoteHash := ""
	if action.View != nil && action.View.Remote != nil {
		remoteHash = action.View.Remote.Hash
	}

	if remoteHash != "" && localHash != remoteHash {
		// Warn but don't fail — iOS .heic files can have stale hashes.
		e.logger.Warn("download hash mismatch (proceeding anyway)",
			slog.String("path", action.Path),
			slog.String("local_hash", localHash),
			slog.String("remote_hash", remoteHash),
		)
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
	defer f.Close()

	h := quickxorhash.New()
	w := io.MultiWriter(f, h)

	size, err := e.transfers.Download(ctx, driveID, action.ItemID, w)
	if err != nil {
		// Clean up partial file on download failure.
		f.Close()
		os.Remove(partialPath)

		return "", 0, fmt.Errorf("downloading %s: %w", action.Path, err)
	}

	if err := f.Close(); err != nil {
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

		if remoteHash == "" {
			o.RemoteHash = action.View.Remote.Hash
		}
	}

	return o
}

// executeUpload uploads a local file to OneDrive. Uses simple upload for
// files <= 4 MiB, chunked upload for larger files.
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

	var item *graph.Item
	var uploadErr error

	if size <= simpleUploadMaxBytes {
		uploadErr = e.withRetry(ctx, "upload "+action.Path, func() error {
			item, err = e.simpleUpload(ctx, driveID, parentID, name, localPath, size)
			return err
		})
	} else {
		uploadErr = e.withRetry(ctx, "upload "+action.Path, func() error {
			item, err = e.chunkedUpload(ctx, driveID, parentID, name, localPath, size, mtime)
			return err
		})
	}

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

// simpleUpload performs a single-request upload for small files (<= 4 MiB).
func (e *Executor) simpleUpload(
	ctx context.Context, driveID driveid.ID, parentID, name, localPath string, size int64,
) (*graph.Item, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s for upload: %w", localPath, err)
	}
	defer f.Close()

	return e.transfers.SimpleUpload(ctx, driveID, parentID, name, f, size)
}

// chunkedUpload performs a resumable upload for large files (> 4 MiB).
func (e *Executor) chunkedUpload(
	ctx context.Context, driveID driveid.ID, parentID, name, localPath string, size int64, mtime time.Time,
) (*graph.Item, error) {
	session, err := e.transfers.CreateUploadSession(ctx, driveID, parentID, name, size, mtime)
	if err != nil {
		return nil, fmt.Errorf("creating upload session for %s: %w", localPath, err)
	}

	f, err := os.Open(localPath)
	if err != nil {
		return nil, fmt.Errorf("opening %s for chunked upload: %w", localPath, err)
	}
	defer f.Close()

	return e.uploadChunks(ctx, session, f, size)
}

// uploadChunks sends file data in chunks and returns the completed Item.
func (e *Executor) uploadChunks(
	ctx context.Context, session *graph.UploadSession, f *os.File, total int64,
) (*graph.Item, error) {
	var offset int64

	for offset < total {
		chunkSize := int64(chunkedUploadChunk)
		if offset+chunkSize > total {
			chunkSize = total - offset
		}

		chunk := io.LimitReader(f, chunkSize)

		item, err := e.transfers.UploadChunk(ctx, session, chunk, offset, chunkSize, total)
		if err != nil {
			return nil, fmt.Errorf("uploading chunk at offset %d: %w", offset, err)
		}

		// Final chunk returns the completed item.
		if item != nil {
			return item, nil
		}

		offset += chunkSize
	}

	return nil, fmt.Errorf("sync: upload completed all chunks but received no final item")
}
