package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	target := e.downloadTargetSpelling(action)

	// The download itself runs through the rooted sync tree, which refuses
	// any escape; this check additionally refuses writing through a
	// symlinked folder that stays inside the root.
	if err := e.validateNoSymlinkBoundary(target, "download"); err != nil {
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
	targetPreconditionErr := e.validateDownloadTargetPrecondition(ctx, action, target)
	if targetPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			targetPreconditionErr,
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	// The replaced version goes to the trash once per action; a retried
	// rename re-checks the target the ordinary way.
	preserved := false
	opts := driveops.DownloadOpts{
		MaxHashRetries: maxHashRetries,
		Target:         syncDownloadTarget{tree: e.syncTree},
		ValidateTargetBeforeRename: func() error {
			if preserved {
				return e.validateDownloadTargetPrecondition(ctx, action, target)
			}
			if err := e.validateDownloadTargetBeforeReplace(ctx, action, target); err != nil {
				return err
			}
			preserved = true
			return nil
		},
	}

	if action.View != nil && action.View.Remote != nil {
		opts.RemoteHash = action.View.Remote.Hash
		opts.RemoteMtime = remoteModTime(action.View.Remote.Mtime)
		opts.RemoteSize = action.View.Remote.Size
		opts.RemoteSizeKnown = action.View.Remote.SizeKnown
		opts.RemoteETag = action.View.Remote.ETag
	}

	result, err := e.transferMgr.DownloadToFile(ctx, driveID, action.ItemID, target, opts)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionDownload,
			err,
			action.Path,
			downloadTransferFailureCapability(err),
		)
	}

	if crashErr := e.reachCrashPoint(crashPointDownloadRenamed); crashErr != nil {
		return e.failedOutcomeWithFailure(action, ActionDownload, crashErr, action.Path, permissionCapabilityUnknown)
	}

	outcome := e.downloadOutcome(action, target, driveID, result.LocalHash, result.EffectiveRemoteHash, result.Size)
	return outcome
}

// downloadTargetSpelling is the rooted path a download writes. A download over
// an existing file replaces that file by its spelling on disk, which on a
// byte-exact filesystem can differ from the stored NFC path; writing the
// stored spelling would leave a second, NFC-named file beside it (R-2.13.2).
// A new file is written under the stored spelling inside its parent folders'
// spellings on disk, and its own folder is not listed to find it.
func (e *executor) downloadTargetSpelling(action *Action) string {
	if plannedLocalState(action) != nil {
		return e.syncTree.Spelling(action.Path)
	}

	return e.syncTree.SpellingOfParent(action.Path)
}

// downloadOutcome builds a successful actionOutcome after a download wrote
// target, the rooted spelling on disk of action.Path.
func (e *executor) downloadOutcome(
	action *Action, target string, driveID driveid.ID, localHash, remoteHash string, size int64,
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
		// The time the download set is remoteModTime of the stored value,
		// whose Unix nanoseconds are that value again.
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
	// intended one made every later scan see a local change (D-046). A stat
	// that fails here (the file went away the moment it landed) is dropped on
	// purpose and keeps the intended mtime: the download succeeded, and the
	// next scan observes whatever the path holds.
	if info, statErr := e.syncTree.Stat(target); statErr == nil {
		o.LocalMtime = info.ModTime().UnixNano()
	}
	o.ParentID = e.resolvedParentIDForOutcome(action, nil)
	e.observeLocalIdentity(&o, target)

	return o
}

// ExecuteUpload uploads a local file to OneDrive via TransferManager.
//
// A pre-upload freshness check prevents silently overwriting concurrent remote
// changes. When current remote truth no longer matches the planned overwrite,
// the upload is aborted as superseded so the engine replans from fresh truth.
func (e *executor) ExecuteUpload(ctx context.Context, action *Action) actionOutcome {
	driveID := e.resolveDriveID(action)

	// Always check the planned remote item against its live state before
	// overwriting it. Planning is snapshot-based, so execution must defend
	// against remote drift regardless of whether the current runtime is
	// one-shot or watch.
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
	// Read by its spelling on disk, not the stored NFC path (R-2.13.2).
	source := e.syncTree.Spelling(action.Path)
	if sourceErr := e.validateUploadSourcePrecondition(action, source); sourceErr != nil {
		return e.failedOutcomeWithFailure(action, ActionUpload, sourceErr, action.Path, permissionCapabilityLocalRead)
	}

	localPath, err := e.syncTree.Abs(source)
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
		opts := e.uploadOpts(action, source)
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

		result, err = e.uploadNewFile(ctx, action, driveID, parentID, source, localPath)
		if gone := remoteCreateParentGone(err, "upload create", parentID); gone != nil {
			return e.failedOutcomeWithFailure(action, ActionUpload, gone, action.Path, permissionCapabilityRemoteRead)
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

	if crashErr := e.reachCrashPoint(crashPointUploadRemoteDone); crashErr != nil {
		return e.failedOutcomeWithFailure(action, ActionUpload, crashErr, action.Path, permissionCapabilityUnknown)
	}

	// No visibility probe of the uploaded path: the upload's own response
	// carries the item, and nothing in sync reads a file by path afterwards.
	// Folder creates and moves still probe, because children and later
	// path-addressed work depend on the path having converged.
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

// uploadTransferError turns the upload answers that mean "remote truth moved
// on" (isRemoteTruthMovedOn) into a stale precondition, so the engine replans
// from fresh truth instead of retrying the same request. A durable name
// collision stays what it is. The Graph error stays in the chain for the
// log; the engine classifies the stale precondition before any status it
// carries (classifyResult).
func uploadTransferError(err error) error {
	var nameTaken *remoteNameTakenError
	if errors.As(err, &nameTaken) {
		return err
	}
	if isRemoteTruthMovedOn(err) {
		return fmt.Errorf("%w: %w", errActionPreconditionChanged, err)
	}

	return err
}

// isRemoteTruthMovedOn reports an upload answer that proves the item or its
// name changed on OneDrive after planning:
//   - an invalidated session: another writer changed the item mid-upload
//     (L-02);
//   - 412 on a conditional overwrite: the item changed after the preflight
//     (D-022);
//   - 409 on a create: OneDrive already holds the name, or one it treats as
//     the same, and a create never replaces (R-5.6.14). Retried as is, it
//     failed with "graph: conflict" forever.
func isRemoteTruthMovedOn(err error) bool {
	return errors.Is(err, driveops.ErrUploadSessionInvalidated) ||
		errors.Is(err, graph.ErrPreconditionFailed) ||
		errors.Is(err, graph.ErrConflict)
}

// uploadTransferFailureCapability names the side whose truth an upload
// failure is about. A stale precondition OneDrive's answer proved is a
// remote-write precondition, as a remote delete's or move's post-preflight
// 412 is; one the source check proved (the file changed while it was read)
// is a local-read one. Both were counted as local-read before.
func uploadTransferFailureCapability(err error) permissionCapability {
	switch {
	case errors.Is(err, errActionPreconditionChanged) && isRemoteTruthMovedOn(err):
		return permissionCapabilityRemoteWrite
	case errors.Is(err, errActionPreconditionChanged):
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
		// The encoding the item converter stores for the next observation
		// of this item, so the two compare equal.
		RemoteMtime: storedTimeNano(result.Item.ModifiedAt),
		ETag:        result.Item.ETag,
	}
	if action.View != nil && action.View.Local != nil {
		outcome.LocalCtime = action.View.Local.LocalCtime
		outcome.LocalDevice = action.View.Local.LocalDevice
		outcome.LocalInode = action.View.Local.LocalInode
		outcome.LocalBirthTime = action.View.Local.LocalBirthTime
		outcome.LocalHasIdentity = action.View.Local.LocalHasIdentity
		outcome.LocalIdentityObserved = true
	}
	return outcome
}

// uploadOpts are the transfer options of action's upload, whose source is the
// rooted path source (its spelling on disk).
func (e *executor) uploadOpts(action *Action, source string) driveops.UploadOpts {
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
			return e.validateUploadSourcePrecondition(action, source)
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
	RecordPendingCreateIntent(ctx context.Context, pending *PendingCreate) error
	RecordPendingCreate(ctx context.Context, pending *PendingCreate) error
	ClearPendingCreateIntent(ctx context.Context, path string) error
	PendingCreateAt(ctx context.Context, path string) (PendingCreate, bool, error)
}

// uploadNewFile creates action's file on OneDrive, with the pending-create
// records around the request (R-5.6.15): the intent before it, the item
// once the create hook has seen it (whether the attempt then succeeded or
// failed), and the intent cleared again when the attempt is known to have
// created nothing. A 409 is resolved rather than retried as is.
//
// An attempt whose create ended without an answer (a dropped connection, a
// 5xx, a timeout: graph.ErrCreateOutcomeUnknown) is not known to have created
// nothing, and its intent stays. The empty file such a create left is then
// this client's own: observation adopts it, and a retry that gets 409 on it
// finishes the upload into it (resolveCreateConflict). When the user renames
// or deletes the file instead, no retry comes, and the step after planning
// drops the intent once a pass has observed OneDrive and then found the file
// gone, or once it expires (pruneAbandonedCreateIntents). The intent was
// cleared here once, and the file read as someone else's: a create conflict,
// the user's file renamed to a conflict copy and the empty one downloaded in
// its place (live, 2026-11-11, during a network outage).
func (e *executor) uploadNewFile(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, source, localPath string,
) (*driveops.UploadResult, error) {
	// Read before this attempt records its own intent, which would otherwise
	// pass for an earlier one.
	priorIntent, err := e.priorCreateIntent(ctx, action)
	if err != nil {
		return nil, err
	}
	opts := e.uploadOpts(action, source)
	created := false
	opts.OnCreated = e.recordPendingCreate(ctx, action, driveID, parentID, &created)
	if intentErr := e.recordPendingCreateIntent(ctx, action, driveID, parentID); intentErr != nil {
		return nil, intentErr
	}
	result, err := e.transferMgr.UploadFile(ctx, driveID, parentID, filepath.Base(action.Path), localPath, opts)
	keepIntent := false
	switch {
	case errors.Is(err, graph.ErrCreateOutcomeUnknown):
		// Before the conflict case: a create that was answered 2xx and whose
		// item could then not be read is unknown whatever status the read got.
		keepIntent = true
		e.logger.Debug("kept the pending create intent of a create whose outcome is unknown",
			slog.String("path", action.Path), slog.Any("error", err))
	case errors.Is(err, graph.ErrConflict):
		result, keepIntent, err = e.resolveCreateConflict(ctx, action, driveID, parentID, source, localPath, err, priorIntent)
	}
	if !created && !keepIntent {
		e.clearPendingCreateIntent(ctx, action)
	}

	return result, err
}

// priorCreateIntent reports whether an earlier attempt left an intent to
// create action's file that never learned an item ID and is still within
// pendingCreateIntentTTL: the one state in which an empty file at the path
// can be that attempt's own (resolveCreateConflict). The store stamps intents
// with the engine's clock, which is the executor's too.
func (e *executor) priorCreateIntent(ctx context.Context, action *Action) (bool, error) {
	if e.pendingCreates == nil {
		return false, nil
	}

	pending, found, err := e.pendingCreates.PendingCreateAt(ctx, action.Path)
	if err != nil {
		return false, fmt.Errorf("reading the pending create at %s: %w", action.Path, err)
	}
	if !found || pending.ItemID != "" {
		return false, nil
	}

	return pending.intentIsFresh(e.nowFunc()), nil
}

// pendingCreateOf describes the create of action for the pending-create
// records.
func pendingCreateOf(action *Action, driveID driveid.ID, parentID string) *PendingCreate {
	pending := &PendingCreate{DriveID: driveID, Path: action.Path, ParentID: parentID}
	if action.View != nil && action.View.Local != nil {
		pending.LocalSize = action.View.Local.Size
		pending.LocalHash = action.View.Local.Hash
	}

	return pending
}

// recordPendingCreateIntent records that a create at action.Path is about to
// be requested (R-5.6.15): the one record that survives a process dying with
// the creating request in flight.
func (e *executor) recordPendingCreateIntent(ctx context.Context, action *Action, driveID driveid.ID, parentID string) error {
	if e.pendingCreates == nil {
		return nil
	}

	if err := e.pendingCreates.RecordPendingCreateIntent(ctx, pendingCreateOf(action, driveID, parentID)); err != nil {
		return fmt.Errorf("recording the intent to create %s: %w", action.Path, err)
	}

	return nil
}

// recordPendingCreate returns the hook that records a create's empty item
// durably before its content is sent, or nil without a store (tests). It
// sets *created when it runs, so the caller knows an item exists.
//
// The item exists on OneDrive from the moment the create answers, whatever
// happens to the action after, so the record is written on a context that
// keeps the action's values but not its cancellation, bounded by
// OutcomeCommitTimeout like a finished action's outcome commit. On the
// action's own context a shutdown landing right after the create failed the
// write, and only the intent was left (live, e2e
// TestE2E_SyncWatch_PauseResume).
func (e *executor) recordPendingCreate(
	ctx context.Context, action *Action, driveID driveid.ID, parentID string, created *bool,
) func(itemID, createdETag string) error {
	if e.pendingCreates == nil {
		return nil
	}

	return func(itemID, createdETag string) error {
		*created = true
		if crashErr := e.reachCrashPoint(crashPointUploadCreatedUnrecorded); crashErr != nil {
			return crashErr
		}
		pending := pendingCreateOf(action, driveID, parentID)
		pending.ItemID, pending.CreatedETag = itemID, createdETag

		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeCommitTimeout)
		defer cancel()

		return e.pendingCreates.RecordPendingCreate(recordCtx, pending)
	}
}

// clearPendingCreateIntent forgets the intent of an attempt that is known to
// have created nothing (uploadNewFile keeps the intent of one whose create
// ended without an answer). Best effort: a failure here leaves an intent that
// observation drops when it sees the file synced or with content, and that
// the step after planning prunes once the file is gone (after a pass that
// observed OneDrive first) or the intent is past its TTL.
//
// A canceled attempt keeps its intent, quietly. The create may have been in
// flight when the context was canceled, and OneDrive can still make the item
// after the client stopped waiting: that is the window the intent covers. The
// write would also fail on the canceled context and warn at a clean shutdown
// (R-6.6.21).
func (e *executor) clearPendingCreateIntent(ctx context.Context, action *Action) {
	if e.pendingCreates == nil {
		return
	}
	if ctx.Err() != nil {
		e.logger.Debug("kept the pending create intent of a canceled attempt",
			slog.String("path", action.Path), slog.Any("error", ctx.Err()))
		return
	}
	if err := e.pendingCreates.ClearPendingCreateIntent(ctx, action.Path); err != nil {
		e.logger.Warn("Could not clear a pending create intent", slog.String("path", action.Path), slog.Any("error", err))
	}
}

// resolveCreateConflict handles a create that OneDrive refused because the
// name is taken (409; a create never replaces, R-5.6.14).
//
//   - Our own placeholder from an interrupted earlier attempt (a pending
//     create at this path whose item is still empty): upload the content
//     into it by ID, conditional on the eTag just read (not the creation
//     eTag, which a canceled upload session moves; store_pending_creates.go).
//     Going stale instead replanned the same create every pass.
//   - The empty file of an earlier attempt whose create ended without an
//     answer (priorIntent: this path had an intent without an item ID, within
//     its TTL, before this attempt): the same spelling, live, a file, empty.
//     It is recorded as the pending create and finished the same way. A
//     prior intent is the whole license: without one, an empty file at the
//     path is someone else's and is never written to.
//   - A sibling OneDrive treats as the same name but spelled differently
//     (STRASSE.txt next to straße.txt, a.txt next to a😀.txt): a durable
//     name collision naming both, not a retry every pass.
//   - The same spelling otherwise (another device created it meanwhile):
//     stale, so the replan sees the remote item and keeps both.
//   - Nothing visible yet: stale as well. With a prior intent the name may be
//     taken by that attempt's file, not listed yet, so the intent stays for
//     the attempt or the observation that sees it.
//
// A read that fails while resolving (the store, the folder listing) is that
// read's failure, classified by its own answer: it proves nothing about the
// plan. Joined to the 409 it read as a stale precondition under the 409's
// status, which hid a throttled or failing listing.
//
// keepIntent tells the caller to leave the intent of this path in place.
func (e *executor) resolveCreateConflict(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, source, localPath string,
	conflictErr error, priorIntent bool,
) (result *driveops.UploadResult, keepIntent bool, err error) {
	if e.pendingCreates != nil {
		pending, found, readErr := e.pendingCreates.PendingCreateAt(ctx, action.Path)
		if readErr != nil {
			return nil, priorIntent, fmt.Errorf("resolving the refused create of %s: reading its pending create: %w",
				action.Path, readErr)
		}
		if found && pending.ItemID != "" {
			uploaded, finished, uploadErr := e.finishIntoPlaceholder(ctx, action, driveID, pending.ItemID, source, localPath)
			if finished {
				return uploaded, false, uploadErr
			}
		}
	}

	name := filepath.Base(action.Path)
	children, listErr := e.items.ListChildren(ctx, driveID, parentID)
	if listErr != nil {
		return nil, priorIntent, fmt.Errorf("resolving the refused create of %s: listing its folder: %w",
			action.Path, listErr)
	}
	for i := range children {
		child := &children[i]
		if child.ID == "" || !namekey.Equal(child.Name, name) {
			continue
		}
		if nfcNormalize(child.Name) != nfcNormalize(name) {
			return nil, false, &remoteNameTakenError{localName: name, remoteName: child.Name, err: conflictErr}
		}
		if priorIntent && isEmptyLiveFile(child) {
			return e.claimLostCreate(ctx, action, driveID, parentID, source, localPath, child, conflictErr)
		}

		return nil, false, conflictErr
	}

	return nil, priorIntent, conflictErr
}

// finishIntoPlaceholder uploads action's content into the empty item itemID,
// conditional on the eTag it reads now (not the creation eTag, which a
// canceled upload session moves). finished is false, and nothing was sent,
// when the item cannot be read or is no longer empty.
func (e *executor) finishIntoPlaceholder(
	ctx context.Context, action *Action, driveID driveid.ID, itemID, source, localPath string,
) (result *driveops.UploadResult, finished bool, err error) {
	if item, getErr := e.items.GetItem(ctx, driveID, itemID); getErr == nil && item.Size == 0 {
		opts := e.uploadOpts(action, source)
		opts.IfMatch = item.ETag
		result, err = e.transferMgr.UploadFileToItem(ctx, driveID, itemID, localPath, opts)

		return result, true, err //nolint:wrapcheck // driveops names the file
	}

	return nil, false, nil
}

// claimLostCreate finishes the create that an earlier attempt asked for and
// never heard back about: child is the empty file it left at action's path.
// The claim is recorded before anything is written, so observation knows the
// file as this client's own whatever becomes of this attempt, and the content
// then goes into it by ID, conditional on the eTag read at that moment. An
// empty local file goes the same way: a conditional write of no bytes, then
// the modification time, which the lost create never got to set.
//
// The test is the one observation applies when it claims an intent's file
// (claimIntendedPlaceholderTx): an empty file at the path within the intent's
// TTL. An empty file someone else created under that name in that window is
// taken for the lost one; it held nothing, and content written to it since it
// was read changes its eTag and refuses the upload.
func (e *executor) claimLostCreate(
	ctx context.Context, action *Action, driveID driveid.ID, parentID, source, localPath string,
	child *graph.Item, conflictErr error,
) (result *driveops.UploadResult, keepIntent bool, err error) {
	pending := pendingCreateOf(action, driveID, parentID)
	pending.ItemID, pending.CreatedETag = child.ID, child.ETag
	if recordErr := e.pendingCreates.RecordPendingCreate(ctx, pending); recordErr != nil {
		return nil, true, fmt.Errorf("claiming the empty file of the lost create of %s: %w", action.Path, recordErr)
	}
	e.logger.Debug("claimed the empty file of a create whose answer was lost",
		slog.String("path", action.Path), slog.String("item_id", child.ID))

	uploaded, finished, uploadErr := e.finishIntoPlaceholder(ctx, action, driveID, child.ID, source, localPath)
	if finished {
		return uploaded, false, uploadErr
	}

	return nil, false, conflictErr
}

// isEmptyLiveFile reports whether item, as listed, is a plain file holding no
// bytes: the only thing a create whose answer was lost can have left. The
// negative size Graph sometimes reports (R-2.4.16) is unknown, not empty.
func isEmptyLiveFile(item *graph.Item) bool {
	return item.ID != "" && !item.IsDeleted && !item.IsFolder && !item.IsRoot &&
		!item.IsPackage && !item.IsBundle && !item.TypeFacetMissing && item.Size == 0
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
