package sync

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
func (e *Executor) ExecuteDownload(ctx context.Context, action *Action) ActionOutcome {
	targetPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityUnknown,
		)
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
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			err,
			action.Path,
			inferFailureCapabilityFromError(err, PermissionCapabilityLocalWrite, PermissionCapabilityRemoteRead),
		)
	}

	return e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
}

// downloadOutcome builds a successful ActionOutcome after download.
func (e *Executor) downloadOutcome(
	action *Action, driveID driveid.ID, localHash, remoteHash string, size int64,
) ActionOutcome {
	o := ActionOutcome{
		Action:          ActionDownload,
		Success:         true,
		Path:            action.Path,
		DriveID:         driveID,
		ItemID:          action.ItemID,
		ItemType:        ItemTypeFile,
		LocalHash:       localHash,
		RemoteHash:      remoteHash,
		LocalSize:       size,
		LocalSizeKnown:  true,
		RemoteSize:      size,
		RemoteSizeKnown: true,
	}

	if action.View != nil && action.View.Remote != nil {
		o.ETag = action.View.Remote.ETag
		o.ParentID = action.View.Remote.ParentID
		o.LocalMtime = action.View.Remote.Mtime
		o.RemoteMtime = action.View.Remote.Mtime
		o.RemoteSize = action.View.Remote.Size

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
func (e *Executor) ExecuteUpload(ctx context.Context, action *Action) ActionOutcome {
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
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				fmt.Errorf("remote eTag changed since last sync (baseline=%s current=%s): potential conflict",
					action.View.Baseline.ETag, currentItem.ETag),
				action.Path,
				PermissionCapabilityUnknown,
			)
		}
		// If GetItem fails (transient error, item deleted), proceed with
		// the upload — the server-side conflict resolution (or a 404) will
		// handle it.
	}

	localPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			normalizeSyncTreePathError(err),
			action.Path,
			PermissionCapabilityUnknown,
		)
	}

	var (
		parentID string
		result   *driveops.UploadResult
	)

	if shouldOverwriteKnownRemoteItem(action) {
		result, err = e.transferMgr.UploadFileToItem(ctx, driveID, action.ItemID, localPath, driveops.UploadOpts{})
		if err != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				err,
				action.Path,
				inferFailureCapabilityFromError(err, PermissionCapabilityLocalRead, PermissionCapabilityRemoteWrite),
			)
		}
		parentID = resolvedUploadParentID(action, result.Item)
	} else {
		parentID, err = e.ResolveParentID(action.Path)
		if err != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, err, action.Path, PermissionCapabilityUnknown)
		}
		if waitErr := e.waitRemoteParentVisible(ctx, action); waitErr != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, waitErr, action.Path, PermissionCapabilityUnknown)
		}

		name := filepath.Base(action.Path)
		result, err = e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, driveops.UploadOpts{})
		if err != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				err,
				action.Path,
				inferFailureCapabilityFromError(err, PermissionCapabilityLocalRead, PermissionCapabilityRemoteWrite),
			)
		}
	}

	// driveops.SelectHash picks the best available hash from the item metadata (B-222).
	remoteHash := driveops.SelectHash(result.Item)
	remoteMtime := int64(0)
	if !result.Item.ModifiedAt.IsZero() {
		remoteMtime = result.Item.ModifiedAt.UnixNano()
	}

	e.confirmRemotePathVisible(ctx, action)

	return ActionOutcome{
		Action:          ActionUpload,
		Success:         true,
		Path:            action.Path,
		DriveID:         driveID,
		ItemID:          result.Item.ID,
		ParentID:        parentID,
		ItemType:        ItemTypeFile,
		LocalHash:       result.LocalHash,
		RemoteHash:      remoteHash,
		LocalSize:       result.Size,
		LocalSizeKnown:  true,
		RemoteSize:      result.Item.Size,
		RemoteSizeKnown: true,
		LocalMtime:      result.Mtime.UnixNano(),
		RemoteMtime:     remoteMtime,
		ETag:            result.Item.ETag,
	}
}

func shouldOverwriteKnownRemoteItem(action *Action) bool {
	if action == nil || action.ItemID == "" {
		return false
	}

	if action.ConflictInfo != nil && action.ConflictInfo.ConflictType == ConflictEditDelete {
		return false
	}

	if action.View != nil && action.View.Remote != nil && action.View.Remote.IsDeleted {
		return false
	}

	return true
}

func resolvedUploadParentID(action *Action, item *graph.Item) string {
	if item != nil && item.ParentID != "" {
		return item.ParentID
	}
	if action == nil || action.View == nil {
		return ""
	}
	if action.View.Remote != nil && action.View.Remote.ParentID != "" {
		return action.View.Remote.ParentID
	}
	if action.View.Baseline != nil {
		return action.View.Baseline.ParentID
	}

	return ""
}
