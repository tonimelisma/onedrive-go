package sync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// maxHashRetries is the number of additional download attempts when the
// downloaded content hash doesn't match the remote hash. This is separate from
// network-level retries (withRetry) because hash mismatches indicate corrupted
// content, not transport failures. iOS .heic files are a known source of stale
// remote hashes — after exhausting retries we accept the download to prevent
// an infinite re-download loop (B-132).
const maxHashRetries = 2

// executeDownload downloads a remote file via TransferManager with .partial
// safety, hash verification with retry, and atomic rename.
func (e *Executor) executeDownload(ctx context.Context, action *Action) Outcome {
	targetPath := filepath.Join(e.syncRoot, action.Path)
	driveID := e.resolveDriveID(action)

	opts := DownloadOpts{MaxHashRetries: maxHashRetries}

	if action.View != nil && action.View.Remote != nil {
		opts.RemoteHash = action.View.Remote.Hash
		opts.RemoteMtime = action.View.Remote.Mtime
		opts.RemoteSize = action.View.Remote.Size
	}

	result, err := e.transferMgr.DownloadToFile(ctx, driveID, action.ItemID, targetPath, opts)
	if err != nil {
		return e.failedOutcome(action, ActionDownload, err)
	}

	return e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
}

// (download helpers moved to TransferManager in transfer_manager.go)

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

// executeUpload uploads a local file to OneDrive via TransferManager.
func (e *Executor) executeUpload(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	parentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	localPath := filepath.Join(e.syncRoot, action.Path)
	name := filepath.Base(action.Path)

	result, err := e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, UploadOpts{})
	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	info, statErr := os.Stat(localPath)
	if statErr != nil {
		return e.failedOutcome(action, ActionUpload, fmt.Errorf("stat after upload %s: %w", action.Path, statErr))
	}

	remoteHash := selectHash(result.Item)

	return Outcome{
		Action:     ActionUpload,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     result.Item.ID,
		ParentID:   parentID,
		ItemType:   ItemTypeFile,
		LocalHash:  result.LocalHash,
		RemoteHash: remoteHash,
		Size:       info.Size(),
		Mtime:      info.ModTime().UnixNano(),
		ETag:       result.Item.ETag,
	}
}

// (session upload helpers moved to TransferManager in transfer_manager.go)
