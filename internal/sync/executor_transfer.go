package sync

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

	outcome := e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
	decorateConflictOutcome(action, &outcome)
	return outcome
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
		o.LocalMtime = action.View.Remote.Mtime
		o.RemoteMtime = action.View.Remote.Mtime
		o.RemoteSize = action.View.Remote.Size

		if remoteHash == "" {
			o.RemoteHash = action.View.Remote.Hash
		}
	}
	o.ParentID = e.resolvedParentIDForOutcome(action, nil)

	return o
}

// ExecuteUpload uploads a local file to OneDrive via TransferManager. Exported
// for use by the engine's conflict resolution path.
//
// A pre-upload eTag freshness check prevents silently overwriting concurrent
// remote changes. When the remote eTag differs from the baseline, the upload
// is aborted with a descriptive error. The engine records this as a
// sync_failure; on the next pass the observer/planner will see both changes
// and detect a conflict.
func (e *Executor) ExecuteUpload(ctx context.Context, action *Action) ActionOutcome {
	driveID := e.resolveDriveID(action)

	// Always validate the baseline eTag before overwriting a known remote item.
	// Planning is snapshot-based, so execution must defend against remote drift
	// regardless of whether the current runtime is one-shot or watch.
	if freshnessErr := e.remoteUploadFreshnessError(ctx, driveID, action); freshnessErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			freshnessErr,
			action.Path,
			PermissionCapabilityUnknown,
		)
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
		parentID = e.resolvedParentIDForOutcome(action, result.Item)
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

	outcome := ActionOutcome{
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
	if action.View != nil && action.View.Local != nil {
		outcome.LocalDevice = action.View.Local.LocalDevice
		outcome.LocalInode = action.View.Local.LocalInode
		outcome.LocalHasIdentity = action.View.Local.LocalHasIdentity
		outcome.LocalIdentityObserved = true
	}
	decorateConflictOutcome(action, &outcome)
	return outcome
}

func (e *Executor) remoteUploadFreshnessError(ctx context.Context, driveID driveid.ID, action *Action) error {
	if action.ItemID == "" ||
		action.View == nil ||
		action.View.Baseline == nil ||
		action.View.Baseline.ETag == "" {
		return nil
	}
	currentETag, ok := e.currentUploadETag(ctx, driveID, action.ItemID)
	if !ok || currentETag == action.View.Baseline.ETag {
		return nil
	}
	return fmt.Errorf(
		"remote eTag changed since last sync (baseline=%s current=%s): potential conflict",
		action.View.Baseline.ETag,
		currentETag,
	)
}

func (e *Executor) currentUploadETag(ctx context.Context, driveID driveid.ID, itemID string) (string, bool) {
	currentItem, err := e.items.GetItem(ctx, driveID, itemID)
	if err != nil {
		// If GetItem fails (transient error, item deleted), proceed with the
		// upload; the server-side conflict resolution or a 404 will handle it.
		return "", false
	}
	return currentItem.ETag, true
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
