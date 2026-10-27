package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// maxHashRetries is the number of additional download attempts when the
// downloaded bytes don't match the remote metadata. This is separate from
// transport-level retries because a mismatch indicates wrong content, not a
// transport failure. Only a deterministic server transform is accepted after
// them (driveops); anything else fails as download_integrity and keeps the
// local file.
const maxHashRetries = 2

// ExecuteDownload downloads a remote file via TransferManager with .partial
// safety, hash verification with retry, and atomic rename.
func (e *executor) ExecuteDownload(ctx context.Context, action *Action) actionOutcome {
	// The download itself runs through the rooted sync tree, which refuses
	// any escape; this check additionally refuses writing through a
	// symlinked folder that stays inside the root.
	if err := e.validateNoSymlinkBoundary(action.Path, "download"); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			err,
			action.Path,
			permissionCapabilityLocalWrite,
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
			permissionCapabilityRemoteRead,
		)
	}
	targetPreconditionErr := e.validateDownloadTargetPrecondition(action)
	if targetPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			targetPreconditionErr,
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	opts := driveops.DownloadOpts{
		MaxHashRetries: maxHashRetries,
		Target:         syncDownloadTarget{tree: e.syncTree},
		ValidateTargetBeforeRename: func() error {
			return e.validateDownloadTargetPrecondition(action)
		},
	}

	if action.View != nil && action.View.Remote != nil {
		opts.RemoteHash = action.View.Remote.Hash
		opts.RemoteMtime = action.View.Remote.Mtime
		opts.RemoteSize = action.View.Remote.Size
		opts.RemoteSizeKnown = action.View.Remote.SizeKnown
		opts.RemoteETag = action.View.Remote.ETag
	}

	result, err := e.transferMgr.DownloadToFile(ctx, driveID, action.ItemID, action.Path, opts)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			err,
			action.Path,
			downloadTransferFailureCapability(err),
		)
	}

	outcome := e.downloadOutcome(action, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
	return outcome
}

// downloadOutcome builds a successful ActionOutcome after download.
func (e *executor) downloadOutcome(
	action *Action, driveID driveid.ID, localHash, remoteHash string, size int64,
) actionOutcome {
	o := actionOutcome{
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
	// The local half records what the filesystem holds after the download,
	// not the mtime that was asked for: a filesystem with coarse timestamps
	// (FAT, SMB, some FUSE mounts) stores a rounded time, and recording the
	// intended one made every later scan see a local change (D-046).
	if info, statErr := e.syncTree.Stat(action.Path); statErr == nil {
		o.LocalMtime = info.ModTime().UnixNano()
	}
	o.ParentID = e.resolvedParentIDForOutcome(action, nil)
	e.observeLocalIdentity(&o)

	return o
}

// ExecuteUpload uploads a local file to OneDrive via TransferManager.
//
// A pre-upload freshness check prevents silently overwriting concurrent remote
// changes. When current remote truth no longer matches the planned overwrite,
// the upload is aborted as superseded so the engine replans from fresh truth.
func (e *executor) ExecuteUpload(ctx context.Context, action *Action) actionOutcome {
	driveID := e.resolveDriveID(action)

	// Always validate the baseline eTag before overwriting a known remote item.
	// Planning is snapshot-based, so execution must defend against remote drift
	// regardless of whether the current runtime is one-shot or watch.
	liveETag, freshnessErr := e.validateRemoteUploadPrecondition(ctx, driveID, action)
	if freshnessErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			freshnessErr,
			action.Path,
			permissionCapabilityRemoteRead,
		)
	}
	if sourceErr := e.validateUploadSourcePrecondition(action); sourceErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			sourceErr,
			action.Path,
			permissionCapabilityLocalRead,
		)
	}

	localPath, err := e.syncTree.Abs(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionUpload,
			normalizeSyncTreePathError(err),
			action.Path,
			permissionCapabilityUnknown,
		)
	}

	var (
		parentID string
		result   *driveops.UploadResult
	)

	if shouldOverwriteKnownRemoteItem(action) {
		// Conditional on the eTag the preflight just read: a remote edit made
		// while a long upload runs answers 412 instead of being replaced
		// (D-022, L-02). Not the planned eTag: a planned remote move bumps it
		// before this content upload.
		opts := e.uploadOpts(action)
		opts.IfMatch = liveETag
		result, err = e.transferMgr.UploadFileToItem(ctx, driveID, action.ItemID, localPath, opts)
		err = uploadTransferError(err)
		if err != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				err,
				action.Path,
				uploadTransferFailureCapability(err),
			)
		}
		parentID = e.resolvedParentIDForOutcome(action, result.Item)
	} else {
		parentID, err = e.ResolveParentID(action.Path)
		if err != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, err, action.Path, permissionCapabilityUnknown)
		}
		waitErr := e.waitRemoteParentVisible(ctx, action)
		parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, parentID, action, "upload create")
		if parentPreconditionErr != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				parentPreconditionErr,
				action.Path,
				permissionCapabilityRemoteRead,
			)
		}
		if waitErr != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, waitErr, action.Path, permissionCapabilityUnknown)
		}

		name := filepath.Base(action.Path)
		opts := e.uploadOpts(action)
		opts.OnCreated = e.recordPendingCreate(ctx, action, driveID, parentID)
		result, err = e.transferMgr.UploadFile(ctx, driveID, parentID, name, localPath, opts)
		if errors.Is(err, graph.ErrConflict) {
			result, err = e.resolveCreateConflict(ctx, action, driveID, parentID, localPath, err)
		}
		err = uploadTransferError(err)
		if err != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionUpload,
				err,
				action.Path,
				uploadTransferFailureCapability(err),
			)
		}
	}

	e.confirmRemotePathVisible(ctx, action)

	outcome := e.uploadOutcome(action, driveID, parentID, result)
	return outcome
}

// syncDownloadTarget is the driveops.TargetFS sync downloads write through:
// every operation is rooted in the sync tree, so a parent swapped for a
// symlink after planning cannot redirect the write outside the root, and a
// parent is never created. Downloads used to MkdirAll the parent and write
// through absolute paths, so a local rename between planning and dispatch
// re-created the old folder holding the download (D-043). Sync's rule: no
// executor side effect creates namespace state the plan did not observe; a
// missing parent is a stale plan, and the engine replans.
type syncDownloadTarget struct {
	tree *synctree.Root
}

func (t syncDownloadTarget) PrepareParent(dir string) error {
	if dir == "" || dir == "." {
		return nil
	}

	info, err := t.tree.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return stalePreconditionError("download parent %s is missing", dir)
	}
	if err != nil {
		return normalizeSyncTreePathError(err)
	}
	if !info.IsDir() {
		return stalePreconditionError("download parent %s is no longer a folder", dir)
	}

	return nil
}

func (t syncDownloadTarget) OpenFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	return t.tree.OpenFile(name, flag, perm) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) Open(name string) (*os.File, error) {
	return t.tree.Open(name) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) Stat(name string) (os.FileInfo, error) {
	return t.tree.Stat(name) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) Remove(name string) error {
	return t.tree.Remove(name) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) Rename(oldName, newName string) error {
	return t.tree.Rename(oldName, newName) //nolint:wrapcheck // synctree names the paths
}

func (t syncDownloadTarget) Chtimes(name string, atime, mtime time.Time) error {
	return t.tree.Chtimes(name, atime, mtime) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) SyncDir(dir string) error {
	return t.tree.SyncDir(dir) //nolint:wrapcheck // synctree names the path
}

func (t syncDownloadTarget) Abs(name string) (string, error) {
	return t.tree.Abs(name) //nolint:wrapcheck // synctree names the path
}

func downloadTransferFailureCapability(err error) permissionCapability {
	if errors.Is(err, errActionPreconditionChanged) {
		return permissionCapabilityLocalWrite
	}

	return inferFailureCapabilityFromError(err, permissionCapabilityLocalWrite, permissionCapabilityRemoteRead)
}

// uploadTransferError translates transfer outcomes that mean "the plan is
// stale" into sync's stale-precondition error. An invalidated upload session
// means another writer changed the item mid-upload (L-02): the engine must
// re-observe it, and may raise a conflict, rather than upload again.
// uploadTransferError turns the upload answers that mean "remote truth moved
// on" into a stale precondition, so the engine replans from fresh truth
// instead of retrying the same request:
//   - an invalidated session (L-02);
//   - 412 on a conditional overwrite: the item changed after the preflight
//     (D-022);
//   - 409 on a create: OneDrive already holds the name, or one it treats as
//     the same, and a create never replaces (R-5.6.14). Retried as is, it
//     failed with "graph: conflict" forever.
func uploadTransferError(err error) error {
	var nameTaken *remoteNameTakenError
	if errors.As(err, &nameTaken) {
		return err
	}
	if errors.Is(err, driveops.ErrUploadSessionInvalidated) ||
		errors.Is(err, graph.ErrPreconditionFailed) ||
		errors.Is(err, graph.ErrConflict) {
		return fmt.Errorf("%w: %w", errActionPreconditionChanged, err)
	}

	return err
}

func uploadTransferFailureCapability(err error) permissionCapability {
	if errors.Is(err, errActionPreconditionChanged) {
		return permissionCapabilityLocalRead
	}

	return inferFailureCapabilityFromError(err, permissionCapabilityLocalRead, permissionCapabilityRemoteWrite)
}

func (e *executor) uploadOutcome(
	action *Action,
	driveID driveid.ID,
	parentID string,
	result *driveops.UploadResult,
) actionOutcome {
	remoteHash := driveops.SelectHash(result.Item)
	remoteMtime := int64(0)
	if !result.Item.ModifiedAt.IsZero() {
		remoteMtime = result.Item.ModifiedAt.UnixNano()
	}

	outcome := actionOutcome{
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

func (e *executor) uploadOpts(action *Action) driveops.UploadOpts {
	plannedETag := ""
	if action.View != nil && action.View.Remote != nil {
		plannedETag = action.View.Remote.ETag
	}

	return driveops.UploadOpts{
		ExpectedHash:      expectedUploadHash(action),
		PlannedRemoteETag: plannedETag,
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

// validateRemoteUploadPrecondition checks an overwrite against live remote
// truth and returns the eTag it read, which the overwrite then sends as
// If-Match. A create has nothing to check.
func (e *executor) validateRemoteUploadPrecondition(ctx context.Context, driveID driveid.ID, action *Action) (string, error) {
	if !shouldOverwriteKnownRemoteItem(action) {
		return "", nil
	}
	return e.remoteSourcePreconditionETag(ctx, driveID, action, "upload overwrite")
}

func shouldOverwriteKnownRemoteItem(action *Action) bool {
	if action == nil || action.ItemID == "" {
		return false
	}

	if action.View != nil && action.View.Remote != nil && action.View.Remote.IsDeleted {
		return false
	}

	return true
}

// pendingCreateStore is the executor's narrow view of the sync store's
// pending-create records (store_pending_creates.go).
type pendingCreateStore interface {
	RecordPendingCreate(ctx context.Context, pending *PendingCreate) error
	PendingCreateAt(ctx context.Context, path string) (PendingCreate, bool, error)
}

// recordPendingCreate returns the hook that records a create's empty item
// durably before its content is sent, or nil without a store (tests).
func (e *executor) recordPendingCreate(
	ctx context.Context, action *Action, driveID driveid.ID, parentID string,
) func(itemID, createdETag string) error {
	if e.pendingCreates == nil {
		return nil
	}

	return func(itemID, createdETag string) error {
		pending := &PendingCreate{
			ItemID: itemID, DriveID: driveID, Path: action.Path, ParentID: parentID, CreatedETag: createdETag,
		}
		if action.View != nil && action.View.Local != nil {
			pending.LocalSize = action.View.Local.Size
			pending.LocalHash = action.View.Local.Hash
		}

		return e.pendingCreates.RecordPendingCreate(ctx, pending)
	}
}

// resolveCreateConflict handles a create that OneDrive refused because the
// name is taken (409; a create never replaces, R-5.6.14).
//
//   - Our own placeholder from an interrupted earlier attempt (a pending
//     create at this path whose item is still empty with its creation eTag):
//     upload the content into it by ID, conditionally. Going stale instead
//     replanned the same create every pass.
//   - A sibling OneDrive treats as the same name but spelled differently
//     (STRASSE.txt next to straße.txt, a.txt next to a😀.txt): a durable
//     name collision naming both, not a retry every pass.
//   - The same spelling (another device created it meanwhile), or nothing
//     visible yet: stale, so the replan sees the remote item and keeps both.
func (e *executor) resolveCreateConflict(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, localPath string, conflictErr error,
) (*driveops.UploadResult, error) {
	if e.pendingCreates != nil {
		pending, found, err := e.pendingCreates.PendingCreateAt(ctx, action.Path)
		if err != nil {
			return nil, errors.Join(conflictErr, err)
		}
		if found {
			if item, getErr := e.items.GetItem(ctx, driveID, pending.ItemID); getErr == nil &&
				item.Size == 0 && item.ETag == pending.CreatedETag {
				opts := e.uploadOpts(action)
				opts.IfMatch = pending.CreatedETag
				return e.transferMgr.UploadFileToItem(ctx, driveID, pending.ItemID, localPath, opts) //nolint:wrapcheck // driveops names the file
			}
		}
	}

	name := filepath.Base(action.Path)
	children, err := e.items.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, errors.Join(conflictErr, err)
	}
	for i := range children {
		if children[i].ID == "" || !namekey.Equal(children[i].Name, name) {
			continue
		}
		if nfcNormalize(children[i].Name) == nfcNormalize(name) {
			break
		}

		return nil, &remoteNameTakenError{localName: name, remoteName: children[i].Name, err: conflictErr}
	}

	return nil, conflictErr
}

// remoteNameTakenError is a create refused because OneDrive already holds a
// sibling whose name it treats as the same, spelled differently. It is a
// durable name-collision condition naming both (classifyResult), not work to
// retry as is.
type remoteNameTakenError struct {
	localName  string
	remoteName string
	err        error
}

func (e *remoteNameTakenError) Error() string {
	return fmt.Sprintf("OneDrive already has %q, which it treats as the same name as %q; rename one of them", e.remoteName, e.localName)
}

func (e *remoteNameTakenError) Unwrap() error { return e.err }
