package syncexec

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// maxHashRetries is the number of additional download attempts when the
// downloaded content hash doesn't match the remote hash. This is separate from
// transport-level retries because hash mismatches indicate corrupted content,
// not transport failures. iOS .heic files are a known source of stale
// remote hashes — after exhausting retries we accept the download to prevent
// an infinite re-download loop (B-132).
const maxHashRetries = 2

// ExecuteDownload downloads a remote file via TransferManager with .partial
// safety, hash verification with retry, and atomic rename. Exported for use
// by the engine's conflict resolution path.
func (e *Executor) ExecuteDownload(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	targetPath, err := ContainedPath(e.syncRoot, action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionDownload, err)
	}

	driveID := e.resolveDriveID(action)

	opts := driveops.DownloadOpts{MaxHashRetries: maxHashRetries}

	if action.View != nil && action.View.Remote != nil {
		opts.RemoteHash = action.View.Remote.Hash
		opts.RemoteMtime = action.View.Remote.Mtime
		opts.RemoteSize = action.View.Remote.Size
	}

	result, err := e.transferMgr.DownloadToFile(ctx, driveID, action.ItemID, targetPath, opts)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionDownload, err)
	}

	return e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
}

// downloadOutcome builds a successful Outcome after download.
func (e *Executor) downloadOutcome(
	action *synctypes.Action, driveID driveid.ID, localHash, remoteHash string, size int64,
) synctypes.Outcome {
	o := synctypes.Outcome{
		Action:     synctypes.ActionDownload,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     action.ItemID,
		ItemType:   synctypes.ItemTypeFile,
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

// ExecuteUpload uploads a local file to OneDrive via TransferManager. Exported
// for use by the engine's conflict resolution path.
//
// In watch mode, a pre-upload eTag freshness check prevents silently
// overwriting concurrent remote changes. When the remote eTag differs from
// the baseline, the upload is aborted with a descriptive error. The engine
// records this as a sync_failure; on the next pass the remote observer will
// have polled, and the planner will see both changes and detect a conflict.
func (e *Executor) ExecuteUpload(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	driveID := e.resolveDriveID(action)

	// Watch-mode freshness check: verify the remote hasn't changed since
	// our last observation. This catches the race where a local change
	// triggers an upload before the remote observer has polled the
	// collaborator's edit.
	if e.watchMode && action.ItemID != "" &&
		action.View != nil && action.View.Baseline != nil &&
		action.View.Baseline.ETag != "" {
		currentItem, fetchErr := e.items.GetItem(ctx, driveID, action.ItemID)
		if fetchErr == nil && currentItem.ETag != action.View.Baseline.ETag {
			return e.failedOutcome(action, synctypes.ActionUpload,
				fmt.Errorf("remote eTag changed since last sync (baseline=%s current=%s): potential conflict",
					action.View.Baseline.ETag, currentItem.ETag))
		}
		// If GetItem fails (transient error, item deleted), proceed with
		// the upload — the server-side conflict resolution (or a 404) will
		// handle it.
	}

	parentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionUpload, err)
	}

	localPath, err := ContainedPath(e.syncRoot, action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionUpload, err)
	}

	name := filepath.Base(action.Path)

	result, err := e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, driveops.UploadOpts{})
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionUpload, err)
	}

	// driveops.SelectHash picks the best available hash from the item metadata (B-222).
	remoteHash := driveops.SelectHash(result.Item)

	return synctypes.Outcome{
		Action:     synctypes.ActionUpload,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     result.Item.ID,
		ParentID:   parentID,
		ItemType:   synctypes.ItemTypeFile,
		LocalHash:  result.LocalHash,
		RemoteHash: remoteHash,
		Size:       result.Size,
		Mtime:      result.Mtime.UnixNano(),
		ETag:       result.Item.ETag,
	}
}
