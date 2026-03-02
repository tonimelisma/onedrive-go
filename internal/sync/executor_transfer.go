package sync

import (
	"context"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

	opts := driveops.DownloadOpts{MaxHashRetries: maxHashRetries}

	if action.View != nil && action.View.Remote != nil {
		opts.RemoteHash = action.View.Remote.Hash
		opts.RemoteMtime = action.View.Remote.Mtime
		opts.RemoteSize = action.View.Remote.Size
	}

	var result *driveops.DownloadResult

	err := e.withRetry(ctx, "download "+action.Path, func() error {
		var dlErr error
		result, dlErr = e.transferMgr.DownloadToFile(ctx, driveID, action.ItemID, targetPath, opts)

		return dlErr
	})
	if err != nil {
		return e.failedOutcome(action, ActionDownload, err)
	}

	return e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
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

// executeUpload uploads a local file to OneDrive via TransferManager.
func (e *Executor) executeUpload(ctx context.Context, action *Action) Outcome {
	driveID := e.resolveDriveID(action)

	parentID, err := e.resolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	localPath := filepath.Join(e.syncRoot, action.Path)
	name := filepath.Base(action.Path)

	var result *driveops.UploadResult

	err = e.withRetry(ctx, "upload "+action.Path, func() error {
		var ulErr error
		result, ulErr = e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, driveops.UploadOpts{})

		return ulErr
	})
	if err != nil {
		return e.failedOutcome(action, ActionUpload, err)
	}

	// driveops.SelectHash picks the best available hash from the item metadata (B-222).
	remoteHash := driveops.SelectHash(result.Item)

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
		Size:       result.Size,
		Mtime:      result.Mtime.UnixNano(),
		ETag:       result.Item.ETag,
	}
}
