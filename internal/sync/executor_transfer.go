package sync

import (
	"context"
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

	sourcePreconditionErr := e.validateRemoteSourcePrecondition(ctx, driveID, action, "download")
	if sourcePreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			sourcePreconditionErr,
			action.Path,
			PermissionCapabilityRemoteRead,
		)
	}
	targetPreconditionErr := e.validateDownloadTargetPrecondition(action)
	if targetPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			targetPreconditionErr,
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	opts := driveops.DownloadOpts{
		MaxHashRetries: maxHashRetries,
		ValidateTargetBeforeRename: func() error {
			return e.validateDownloadTargetPrecondition(action)
		},
	}

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
// A pre-upload freshness check prevents silently overwriting concurrent remote
// changes. When current remote truth no longer matches the planned overwrite,
// the upload is aborted as superseded so the engine replans from fresh truth.
func (e *Executor) ExecuteUpload(ctx context.Context, action *Action) ActionOutcome {
	driveID := e.resolveDriveID(action)

	// Always validate the baseline eTag before overwriting a known remote item.
	// Planning is snapshot-based, so execution must defend against remote drift
	// regardless of whether the current runtime is one-shot or watch.
	if freshnessErr := e.validateRemoteUploadPrecondition(ctx, driveID, action); freshnessErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			freshnessErr,
			action.Path,
			PermissionCapabilityRemoteRead,
		)
	}
	if sourceErr := e.validateUploadSourcePrecondition(action); sourceErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			sourceErr,
			action.Path,
			PermissionCapabilityLocalRead,
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
		result, err = e.transferMgr.UploadFileToItem(ctx, driveID, action.ItemID, localPath, e.uploadOpts(action))
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
		parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, parentID, action, "upload create")
		if parentPreconditionErr != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				parentPreconditionErr,
				action.Path,
				PermissionCapabilityRemoteRead,
			)
		}
		if waitErr := e.waitRemoteParentVisible(ctx, action); waitErr != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, waitErr, action.Path, PermissionCapabilityUnknown)
		}

		name := filepath.Base(action.Path)
		result, err = e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, e.uploadOpts(action))
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

	e.confirmRemotePathVisible(ctx, action)

	outcome := e.uploadOutcome(action, driveID, parentID, result)
	decorateConflictOutcome(action, &outcome)
	return outcome
}

func (e *Executor) uploadOutcome(
	action *Action,
	driveID driveid.ID,
	parentID string,
	result *driveops.UploadResult,
) ActionOutcome {
	remoteHash := driveops.SelectHash(result.Item)
	remoteMtime := int64(0)
	if !result.Item.ModifiedAt.IsZero() {
		remoteMtime = result.Item.ModifiedAt.UnixNano()
	}

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
	return outcome
}

func (e *Executor) uploadOpts(action *Action) driveops.UploadOpts {
	return driveops.UploadOpts{
		ExpectedHash: expectedUploadHash(action),
		HashMismatchError: func(_ string, expectedHash, actualHash string) error {
			return stalePreconditionError(
				"upload source %s changed hash (expected=%s current=%s)",
				action.Path,
				expectedHash,
				actualHash,
			)
		},
		ValidateSourceBeforeRead: func() error {
			return e.validateUploadSourcePrecondition(action)
		},
	}
}

func (e *Executor) validateRemoteUploadPrecondition(ctx context.Context, driveID driveid.ID, action *Action) error {
	if !shouldOverwriteKnownRemoteItem(action) {
		return nil
	}
	return e.validateRemoteSourcePrecondition(ctx, driveID, action, "upload overwrite")
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
