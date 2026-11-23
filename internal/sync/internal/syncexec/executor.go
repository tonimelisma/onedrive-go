package syncexec

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// GraphRootID is the Graph API parent reference for top-level items.
// Distinct from StrRoot in types.go which serializes the ItemTypeRoot enum.
const GraphRootID = "root"

// ExecutorConfig holds the configuration every per-action executor shares.
// NewExecutorConfig builds it whole and no run changes it, except
// parentChecks, which guards itself and is reset for each plan.
type ExecutorConfig struct {
	items            syncdomain.ItemClient
	downloads        driveops.Downloader
	uploads          driveops.Uploader
	syncTree         *synctree.Root
	driveID          driveid.ID // mount remote drive context (B-068)
	remoteRootItemID string     // mounted remote root for mount-root engines; empty = drive root
	logger           *slog.Logger
	ignoreJunkFiles  bool
	// contentFilter tells a folder delete which leftover children sync
	// deliberately does not own (D-012).
	contentFilter syncdomain.ContentFilter
	// syncRootIdentity is the sync folder identity the engine validated;
	// a remote delete re-checks it before acting on local absence (D-011).
	syncRootIdentity *syncdomain.SyncRootIdentity

	// transferMgr handles unified download/upload with resume and disk
	// space pre-checks (R-6.2.6). Disk check is configured via
	// driveops.WithDiskCheck when constructing the TransferManager.
	transferMgr *driveops.TransferManager

	// hooks are the executor's and its worker pools' test seams; the zero
	// value is production.
	hooks Hooks

	nowFunc         func() time.Time
	pathConvergence driveops.PathConvergence

	// trash keeps what local deletes remove and what downloads replace
	// (P-4). Nil only in executor-only tests; then removal is permanent.
	trash *localtrash.Trash

	// pendingCreates records new files whose empty item exists before their
	// content is sent (R-5.6.15). Nil in executor-only tests.
	pendingCreates PendingCreateStore

	// parentChecks remembers the remote parents already read live for the
	// plan being executed, so sibling creates do not read them again.
	parentChecks *remoteParentChecks
}

// executor executes individual actions against the Graph API and local
// filesystem, producing Outcomes. Created per-action via NewExecution.
// Thread-safe: its own state is per-call, and the one mutable member of the
// shared ExecutorConfig, parentChecks, guards itself.
type Executor struct {
	*ExecutorConfig
	baseline syncdomain.BaselineView
}

// ExecutorInputs is everything NewExecutorConfig builds an executor
// configuration from. Items, Downloads, Uploads, Transfers, SyncTree,
// DriveID, Logger and Now are required; the rest may be zero.
type ExecutorInputs struct {
	Items     syncdomain.ItemClient
	Downloads driveops.Downloader
	Uploads   driveops.Uploader
	// Transfers handles unified download/upload with resume and disk space
	// pre-checks (R-6.2.6).
	Transfers *driveops.TransferManager
	SyncTree  *synctree.Root
	DriveID   driveid.ID
	// RemoteRootItemID is the mounted remote root of a mount-root engine;
	// empty is the drive root.
	RemoteRootItemID string
	Logger           *slog.Logger
	// ContentFilter tells a folder delete which leftover children sync
	// deliberately does not own (D-012).
	ContentFilter syncdomain.ContentFilterConfig
	// SyncRootIdentity is the sync folder identity remote deletes re-check
	// before acting on local absence (D-011). It is shared, not copied, so a
	// binding made after startup is seen here too.
	SyncRootIdentity *syncdomain.SyncRootIdentity
	PathConvergence  driveops.PathConvergence
	// Trash keeps what local deletes remove and what downloads replace
	// (P-4); nil makes removal permanent.
	Trash *localtrash.Trash
	// PendingCreates records new files whose empty item exists before their
	// content is sent (R-5.6.15).
	PendingCreates PendingCreateStore
	// Now is the executor's clock, the engine's run clock in production.
	Now   func() time.Time
	Hooks Hooks
}

// NewExecutorConfig builds the executor configuration bound to one drive and
// sync root, refusing inputs that lack a required field. Use NewExecution to
// create per-call executors.
func NewExecutorConfig(in *ExecutorInputs) (*ExecutorConfig, error) {
	if missing := in.missingRequiredField(); missing != "" {
		return nil, fmt.Errorf("syncexec: building executor config: %s is required", missing)
	}

	return &ExecutorConfig{
		items:            in.Items,
		downloads:        in.Downloads,
		uploads:          in.Uploads,
		transferMgr:      in.Transfers,
		syncTree:         in.SyncTree,
		driveID:          in.DriveID,
		remoteRootItemID: in.RemoteRootItemID,
		logger:           in.Logger,
		ignoreJunkFiles:  in.ContentFilter.IgnoreJunkFiles,
		contentFilter:    syncdomain.NewContentFilter(syncdomain.CloneContentFilterConfig(in.ContentFilter)),
		syncRootIdentity: in.SyncRootIdentity,
		pathConvergence:  in.PathConvergence,
		trash:            in.Trash,
		pendingCreates:   in.PendingCreates,
		nowFunc:          in.Now,
		hooks:            in.Hooks,
		parentChecks:     newRemoteParentChecks(),
	}, nil
}

// missingRequiredField names the first required input that is zero, or is
// empty when none is.
func (in *ExecutorInputs) missingRequiredField() string {
	required := []struct {
		name    string
		missing bool
	}{
		{name: "Items", missing: in.Items == nil},
		{name: "Downloads", missing: in.Downloads == nil},
		{name: "Uploads", missing: in.Uploads == nil},
		{name: "Transfers", missing: in.Transfers == nil},
		{name: "SyncTree", missing: in.SyncTree == nil},
		{name: "DriveID", missing: in.DriveID.IsZero()},
		{name: "Logger", missing: in.Logger == nil},
		{name: "Now", missing: in.Now == nil},
	}
	for _, field := range required {
		if field.missing {
			return field.name
		}
	}

	return ""
}

// SyncTree is the sync root the executor acts in.
func (cfg *ExecutorConfig) SyncTree() *synctree.Root {
	return cfg.syncTree
}

func (e *Executor) confirmRemotePathVisible(ctx context.Context, action *syncdomain.Action) {
	pathConvergence, remotePath, ok := e.pathConvergenceForAction(action)
	if !ok {
		return
	}

	_, err := pathConvergence.WaitPathVisible(ctx, remotePath)
	if err == nil {
		// A folder just created or moved here is now a parent its
		// children's creates need not probe again this plan.
		e.parentChecks.markPathVisible(remotePath)
		return
	}

	message := "post-mutation remote visibility probe failed"
	if errors.Is(err, driveops.ErrPathNotVisible) {
		message = "remote path still not visible after mutation"
	}

	// Debug: the mutation itself succeeded; Graph's eventual consistency
	// only delays when the new path is listable, and nothing is asked of
	// the user.
	e.logger.Debug(message,
		slog.String("path", action.Path),
		slog.String("target_path", remotePath),
		slog.Any("error", err),
	)
}

func (e *Executor) pathConvergenceForAction(action *syncdomain.Action) (driveops.PathConvergence, string, bool) {
	if action == nil {
		return nil, "", false
	}

	return e.pathConvergenceForPath(action, action.Path)
}

func (e *Executor) pathConvergenceForPath(action *syncdomain.Action, actionPath string) (driveops.PathConvergence, string, bool) {
	if e.pathConvergence == nil || action == nil {
		return nil, "", false
	}

	cleanPath := filepath.ToSlash(actionPath)
	if cleanPath == "" || cleanPath == "." {
		return nil, "", false
	}

	return e.pathConvergence, cleanPath, true
}

// waitRemoteParentVisible blocks parent-based remote creates until the already
// known parent path becomes readable through the shared convergence boundary.
// This keeps sync from spending a freshly created parent item ID on child
// create/upload routes before Graph has converged the parent path.
func (e *Executor) waitRemoteParentVisible(ctx context.Context, action *syncdomain.Action) error {
	if action == nil {
		return nil
	}

	parentPath := filepath.Dir(action.Path)
	if parentPath == "." || parentPath == "" {
		return nil
	}

	pathConvergence, remotePath, ok := e.pathConvergenceForPath(action, parentPath)
	if !ok || remotePath == "" {
		return nil
	}
	if e.parentChecks.pathVisible(remotePath) {
		// A sibling's create, or the folder's own create, already waited
		// for this parent in this plan.
		return nil
	}

	if _, err := pathConvergence.WaitPathVisible(ctx, remotePath); err != nil {
		return fmt.Errorf("confirm parent folder %s is on OneDrive: %w", parentPath, err)
	}
	e.parentChecks.markPathVisible(remotePath)

	return nil
}

// NewExecution creates an ephemeral Executor for a single action execution.
// Baseline is used for parent ID resolution (thread-safe via locked accessors).
func NewExecution(cfg *ExecutorConfig, bl syncdomain.BaselineView) *Executor {
	return &Executor{
		ExecutorConfig: cfg,
		baseline:       bl,
	}
}

// ExecuteFolderCreate dispatches to local or remote folder creation.
func (e *Executor) ExecuteFolderCreate(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	if action.CreateSide == syncdomain.CreateLocal {
		return e.createLocalFolder(action)
	}

	return e.createRemoteFolder(ctx, action)
}

// createLocalFolder creates a directory on the local filesystem, and any
// missing parent; the sync tree gives each the user-tree folder mode
// (R-5.10.2).
func (e *Executor) createLocalFolder(action *syncdomain.Action) syncdomain.ActionOutcome {
	// The stored name inside its parents' spellings on disk (R-2.13.2):
	// creating the stored spelling of an NFD-named parent would make a
	// second, NFC-named parent beside it.
	folder := e.syncTree.SpellingOfParent(action.Path)
	if err := e.validateNoSymlinkBoundary(folder, "create folder"); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionFolderCreate,
			err,
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	if err := e.syncTree.MkdirAll(folder); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionFolderCreate,
			fmt.Errorf("creating local folder %s: %w", action.Path, normalizeSyncTreePathError(err)),
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	// The remote folder's time, when one is known, as a download sets a file's.
	if action.View != nil && action.View.Remote != nil && syncdomain.StoredTimeIsSet(action.View.Remote.Mtime) {
		mtime := syncdomain.RemoteModTime(action.View.Remote.Mtime)
		if err := e.syncTree.Chtimes(folder, mtime, mtime); err != nil {
			// Debug: the folder exists; some filesystems (SMB, FUSE) refuse
			// directory times, which would otherwise warn once per folder.
			e.logger.Debug("failed to set folder mtime", slog.String("path", action.Path), slog.Any("error", err))
		}
	}

	e.logger.Debug("created local folder", slog.String("path", action.Path))

	return e.folderOutcome(action, folder)
}

// resolvedParentIDForOutcome is the parent ID a successful outcome records:
// the one Graph returned with the item, else the one the path resolves to,
// else the baseline's. A resolver failure is dropped on purpose: the mutation
// already succeeded, the next fallback still names a parent, and observation
// corrects a wrong one from remote truth.
func (e *Executor) resolvedParentIDForOutcome(action *syncdomain.Action, item *graph.Item) string {
	if item != nil && item.ParentID != "" {
		return item.ParentID
	}
	if action != nil {
		parentID, err := e.ResolveParentID(action.Path)
		if err == nil {
			return parentID
		}
	}
	if action != nil && action.View != nil && action.View.Baseline != nil {
		return action.View.Baseline.ParentID
	}

	return ""
}

// adoptExistingRemoteFolder handles a 409 on a folder create, by what the
// listing holds under the name (classifyCreateConflict). A folder of the
// wanted name is the outcome the create wanted (a lost response to our own
// earlier create, or another device creating the same folder): adopt it, as
// observing it would have. A sibling spelled differently that OneDrive treats
// as the same name is a durable name collision; anything else under the name
// (a file, a sibling of another name that only shares the stored form, or
// nothing listed yet) is a stale precondition for the planner to resolve.
// Retried as it is, the create would meet the same 409 every time.
//
// A listing that fails is the listing's failure, classified by its own
// answer: it proves nothing about the name. Joined to the 409, an outage or a
// throttle would be recorded under the 409's status as a per-item refusal.
func (e *Executor) adoptExistingRemoteFolder(
	ctx context.Context, driveID driveid.ID, parentID, name string, conflictErr error,
) (*graph.Item, error) {
	children, err := e.items.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, fmt.Errorf("resolving the refused create of folder %s: listing its parent: %w", name, err)
	}

	child, found := classifyCreateConflict(children, name)
	switch found {
	case createConflictWanted:
		if child.IsFolder {
			e.logger.Debug("adopted existing remote folder after create conflict",
				slog.String("name", name),
				slog.String("item_id", child.ID),
			)

			return child, nil
		}
	case createConflictRival:
		// A sibling spelled differently that OneDrive treats as the same
		// name is a durable name collision for the user, as for an upload;
		// as a stale precondition it was replanned and refused on every
		// pass.
		return nil, &RemoteNameTakenError{localName: name, remoteName: child.Name, err: conflictErr}
	case createConflictNothing:
	}

	return nil, stalePreconditionError("folder create %s: OneDrive already has another item with this name", name)
}

// createRemoteFolder creates a folder on OneDrive. The DAG guarantees parent
// folder creates complete before children, so ResolveParentID finds the parent
// in the baseline (committed by CommitMutation before DepGraph.Complete).
func (e *Executor) createRemoteFolder(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	parentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionFolderCreate, err, action.Path, syncdomain.PermissionCapabilityUnknown)
	}

	if waitErr := e.waitRemoteParentVisible(ctx, action); waitErr != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionFolderCreate, waitErr, action.Path, syncdomain.PermissionCapabilityUnknown)
	}

	driveID := e.resolveDriveID(action)
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, parentID, action, "folder create")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionFolderCreate,
			parentPreconditionErr,
			action.Path,
			syncdomain.PermissionCapabilityRemoteRead,
		)
	}
	name := filepath.Base(action.Path)

	item, err := e.items.CreateFolder(ctx, driveID, parentID, name)
	if errors.Is(err, graph.ErrConflict) {
		item, err = e.adoptExistingRemoteFolder(ctx, driveID, parentID, name, err)
	}
	if gone := remoteCreateParentGone(err, "folder create", parentID); gone != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionFolderCreate, gone, action.Path, syncdomain.PermissionCapabilityRemoteRead)
	}
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionFolderCreate,
			fmt.Errorf("creating remote folder %s: %w", action.Path, err),
			action.Path,
			inferFailureCapabilityFromError(err, syncdomain.PermissionCapabilityUnknown, syncdomain.PermissionCapabilityRemoteWrite),
		)
	}

	e.logger.Debug("created remote folder",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	o := syncdomain.ActionOutcome{
		Action:     syncdomain.ActionFolderCreate,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     item.ID,
		ParentID:   parentID,
		ItemType:   syncdomain.ItemTypeFolder,
		RemoteHash: driveops.SelectHash(item), // SelectHash: driveops package (B-222)
		ETag:       item.ETag,
	}
	// The folder already exists locally; its observed identity is what lets a
	// later local rename of it pair as one move instead of delete + create.
	if action.View != nil && action.View.Local != nil {
		o.LocalCtime = action.View.Local.LocalCtime
		o.LocalDevice = action.View.Local.LocalDevice
		o.LocalInode = action.View.Local.LocalInode
		o.LocalBirthTime = action.View.Local.LocalBirthTime
		o.LocalHasIdentity = action.View.Local.LocalHasIdentity
		o.LocalIdentityObserved = true
	}

	return o
}

// ExecuteMove dispatches to local or remote move.
func (e *Executor) ExecuteMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	if action.Type == syncdomain.ActionLocalMove {
		return e.ExecuteLocalMove(ctx, action)
	}

	return e.ExecuteRemoteMove(ctx, action)
}

// ExecuteLocalMove renames a local file/folder.
func (e *Executor) ExecuteLocalMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	// Both endpoints are addressed by their spelling on disk (R-2.13.2): the
	// source and the destination's existing parents, and an existing
	// destination the move takes out of the way. A destination that does not
	// exist yet keeps the stored name.
	source := e.syncTree.Spelling(action.OldPath)
	dest := e.syncTree.Spelling(action.Path)

	// Both endpoints matter: an unchecked destination would still create
	// parent directories outside the sync root, and a replacing move takes
	// the file at the destination out of the folder.
	for _, endpoint := range []string{source, dest} {
		if err := e.validateNoSymlinkBoundary(endpoint, "move"); err != nil {
			return e.failedOutcomeWithFailure(
				action,
				syncdomain.ActionLocalMove,
				err,
				endpoint,
				syncdomain.PermissionCapabilityLocalWrite,
			)
		}
	}

	if err := e.validateLocalMovePrecondition(ctx, action, source, dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.OldPath,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	// The parent must already exist. Planning orders a move after whatever
	// creates or moves its target parent (D-054); fabricating a missing parent
	// here would hide an ordering bug and leave a stray folder for the next
	// pass to upload.
	if err := e.validateLocalMoveParentExists(dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	if err := e.clearLocalMoveDestination(ctx, action, dest); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionLocalMove,
			err,
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}

	if err := e.renameLocal(source, dest, isLocalSpellingOnlyMove(action.OldPath, action.Path)); err != nil {
		if errors.Is(err, syncdomain.ErrActionPreconditionChanged) {
			// The rename found the destination taken: an entry took it after
			// the precondition checked it, or took the name a replacing move
			// had just freed. It is kept, and the move is replanned from what
			// is there now.
			return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove, err, action.Path, syncdomain.PermissionCapabilityLocalWrite)
		}
		return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove,
			fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, normalizeSyncTreePathError(err)),
			action.OldPath, syncdomain.PermissionCapabilityLocalWrite)
	}

	if err := e.reachCrashPoint(syncdomain.CrashPointLocalMoveDone); err != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionLocalMove, err, action.Path, syncdomain.PermissionCapabilityUnknown)
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	o := e.moveOutcome(action)
	e.observeMovedLocalChangeTime(&o, dest)

	return o
}

// observeMovedLocalChangeTime records the change time a local rename gave the
// moved file. rename(2) keeps the inode, birth time, content and mtime the
// outcome carries over from the source baseline, but updates the inode change
// time on Linux filesystems; left at the source baseline's, the next scan saw
// metadata drift and published a second, metadata-only baseline update for
// every applied remote file move.
//
// The new ctime is taken only from the entry that kept the moved file's
// identity. An entry swapped in at dest after the rename keeps the old
// recorded facts, which the next scan re-reads instead of trusting. dest is
// the destination's rooted spelling on disk.
func (e *Executor) observeMovedLocalChangeTime(o *syncdomain.ActionOutcome, dest string) {
	if o.ItemType != syncdomain.ItemTypeFile || !o.LocalHasIdentity {
		return
	}
	info, err := e.syncTree.Lstat(dest)
	if err != nil {
		e.logger.Debug("moved file change time unavailable",
			slog.String("path", o.Path),
			slog.Any("error", err),
		)
		return
	}
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return
	}
	recorded := synctree.FileIdentity{Device: o.LocalDevice, Inode: o.LocalInode, BirthTime: o.LocalBirthTime}
	if !synctree.SameIdentity(recorded, identity) {
		return
	}
	if ctime := syncdomain.FileChangeTime(info); ctime != 0 {
		o.LocalCtime = ctime
	}
}

// renameLocal renames within the sync tree, between rooted paths spelled as
// on disk, and never replaces an entry at newPath: every move starts with
// RenameNoReplace. A file moves without a window wherever the system has the
// no-replace flag or hard links, and a folder wherever it has the flag;
// elsewhere the entry takes a check of newPath and then a plain rename. A
// rename that finds newPath taken leaves the entry there, and its error
// matches ErrActionPreconditionChanged as well as synctree.ErrTargetExists.
// A rename that placed the entry but could not remove its old name
// (synctree.ErrSourceNotRemoved) is a move that happened and is logged.
//
// A spelling-only rename (spellingOnly: the stored paths differ only under
// OneDrive's name equivalence) whose no-replace rename finds newPath taken by
// the source itself, as on a volume that folds case, where both spellings
// name one entry, is then a plain rename. On a volume that tells the
// spellings apart, two hard links of one file named in the two spellings
// pass that check too, and the plain rename leaves both names. After a
// spelling-only rename, respellLocal makes sure the folder holds the new
// spelling.
func (e *Executor) renameLocal(oldPath, newPath string, spellingOnly bool) error {
	err := e.syncTree.RenameNoReplace(oldPath, newPath)
	if spellingOnly && errors.Is(err, synctree.ErrTargetExists) && e.namesOneLocalEntry(oldPath, newPath) {
		err = e.syncTree.Rename(oldPath, newPath)
	}

	switch {
	case errors.Is(err, synctree.ErrSourceNotRemoved):
		e.logger.Warn("Moved "+newPath+", but could not remove the name it was moved from",
			slog.String("path", newPath), slog.Any("error", err))
		return nil
	case errors.Is(err, synctree.ErrTargetExists):
		return fmt.Errorf("%w: local move destination %s was taken before the move: %w", syncdomain.ErrActionPreconditionChanged, newPath, err)
	case err != nil:
		return err //nolint:wrapcheck // synctree names both paths
	case !spellingOnly:
		return nil
	}

	return e.respellLocal(newPath)
}

// namesOneLocalEntry reports whether oldPath and newPath name one entry, as
// two spellings of a name do on a volume that folds case. A name that cannot
// be read names no entry, so the move is replanned instead.
func (e *Executor) namesOneLocalEntry(oldPath, newPath string) bool {
	oldInfo, err := e.syncTree.Lstat(oldPath)
	if err != nil {
		return false
	}
	newInfo, err := e.syncTree.Lstat(newPath)
	if err != nil {
		return false
	}

	return synctree.SameFile(oldInfo, newInfo)
}

// respellLocal gives the entry at newPath the spelling newPath when its
// folder still holds it under another one (a case-insensitive volume that
// takes a spelling-only rename as a no-op; Linux filesystems and APFS store
// the new spelling). It renames through a temporary sibling, both of whose
// steps never replace, named so that local observation always excludes it
// (isSpellingRenameArtifactName): a crash between the two renames must not
// leave a file that syncs under the temporary name (the engine's next start
// puts such a leftover back under its plain name). A failure of either step
// is the rename's own failure, even when the second finds newPath taken: the
// entry is then put back, or kept under the temporary name where it cannot
// be, and the failure says which.
func (e *Executor) respellLocal(newPath string) error {
	stored, err := e.storedNameSpelling(newPath)
	if err != nil || stored == filepath.Base(newPath) {
		return err
	}

	err = e.syncTree.RenameWithTemporarySibling(
		filepath.Join(filepath.Dir(newPath), stored), newPath,
		syncdomain.SpellingRenameArtifactPrefix+filepath.Base(newPath), syncdomain.SpellingRenameArtifactAttempts,
	)
	if errors.Is(err, synctree.ErrSourceNotRemoved) {
		// The entry has its new spelling; a name left behind is reported.
		e.logger.Warn("Renamed "+newPath+", but could not remove a name it was renamed from",
			slog.String("path", newPath), slog.Any("error", err))
		return nil
	}

	return err //nolint:wrapcheck // synctree names both paths
}

// storedNameSpelling returns the spelling the parent directory holds for the
// entry OneDrive would call path's name. It asks the OneDrive question of a
// local listing (namekey.Find): the entry spelled so, else the only one of a
// name OneDrive treats as the same; with none, path's own name. Two such
// entries and none spelled so are an error: renaming either onto the name
// would pick one of two files by listing order.
func (e *Executor) storedNameSpelling(path string) (string, error) {
	dir := filepath.Dir(path)
	entries, err := e.syncTree.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading %s after a spelling rename: %w", dir, normalizeSyncTreePathError(err))
	}

	base := filepath.Base(path)
	match := namekey.Find(entries, base, func(entry *fs.DirEntry) (string, bool) { return (*entry).Name(), true })
	switch {
	case match.Ambiguous:
		return "", fmt.Errorf("giving %s its spelling: %s holds more than one entry OneDrive treats as that name",
			path, dir)
	case !match.Found():
		return base, nil
	}

	return entries[match.Index].Name(), nil
}

// ExecuteRemoteMove renames/moves an item on OneDrive.
func (e *Executor) ExecuteRemoteMove(ctx context.Context, action *syncdomain.Action) syncdomain.ActionOutcome {
	driveID := e.resolveDriveID(action)

	live, sourcePreconditionErr := e.remoteSourcePrecondition(ctx, driveID, action, "remote move")
	sourceETag := live.eTag
	if errors.Is(sourcePreconditionErr, errRemoteMoveAlreadyApplied) {
		// Already where the plan wants it (an earlier attempt or a parent
		// folder move got there first): record the move instead of
		// superseding work that has nothing left to do.
		e.logger.Debug("remote move already applied",
			slog.String("from", action.OldPath),
			slog.String("to", action.Path),
		)
		o := e.moveOutcome(action)
		o.ETag = sourceETag
		return o
	}
	if sourcePreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			sourcePreconditionErr,
			action.OldPath,
			syncdomain.PermissionCapabilityRemoteRead,
		)
	}
	newParentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionRemoteMove, err, action.OldPath, syncdomain.PermissionCapabilityUnknown)
	}
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, newParentID, action, "remote move")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			parentPreconditionErr,
			action.Path,
			syncdomain.PermissionCapabilityRemoteRead,
		)
	}

	newName := filepath.Base(action.Path)
	// No If-Match for a folder move: its eTag also changes when a child
	// changes, which does not make moving the folder unsafe, and a busy folder
	// would otherwise keep failing the move. Files keep the conditional.
	if plannedRemoteItemType(action) == syncdomain.ItemTypeFolder {
		sourceETag = ""
	}

	// The live modification time rides along: a rename on OneDrive otherwise
	// resets it to now, and the next comparison would see a change (D-150).
	item, err := e.items.MoveItemIfMatch(ctx, driveID, action.ItemID, newParentID, newName, graph.MoveOptions{
		IfMatch:      sourceETag,
		KeepModified: live.modified,
	})
	if err != nil {
		if errors.Is(err, graph.ErrPreconditionFailed) {
			return e.failedOutcomeWithFailure(
				action,
				syncdomain.ActionRemoteMove,
				stalePreconditionError("remote move item %s changed before mutation", action.ItemID),
				action.OldPath,
				syncdomain.PermissionCapabilityRemoteWrite,
			)
		}

		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionRemoteMove,
			fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err),
			action.OldPath,
			inferFailureCapabilityFromError(err, syncdomain.PermissionCapabilityUnknown, syncdomain.PermissionCapabilityRemoteWrite),
		)
	}

	if crashErr := e.reachCrashPoint(syncdomain.CrashPointRemoteMoveDone); crashErr != nil {
		return e.failedOutcomeWithFailure(action, syncdomain.ActionRemoteMove, crashErr, action.OldPath, syncdomain.PermissionCapabilityUnknown)
	}

	e.logger.Debug("remote move complete",
		slog.String("from", action.OldPath),
		slog.String("to", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	o := e.moveOutcome(action)
	o.ItemID = item.ID
	o.ETag = item.ETag

	return o
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// normalizeSyncTreePathError adds the sync-domain escape sentinel to a
// containment failure from the sync tree.
//
// It matches the sync tree's sentinel, not its message: a match on text
// would stop silently when the sync tree rewords its error, and would know
// only the phrasings it was written for.
func normalizeSyncTreePathError(err error) error {
	if err == nil {
		return nil
	}

	// One line, not errors.Join: the console prints this as a failure
	// reason, and a joined error renders on two lines.
	if errors.Is(err, synctree.ErrUnsafePath) {
		return fmt.Errorf("%w: %w", syncdomain.ErrPathEscapesSyncRoot, err)
	}

	return err
}

// ResolveParentID determines the remote parent ID for a given relative path.
// Checks baseline (which includes items committed by earlier DAG actions),
// then falls back to "root" for top-level items.
func (e *Executor) ResolveParentID(relPath string) (string, error) {
	parentDir := filepath.Dir(relPath)

	// Top-level item: parent is root.
	if parentDir == "." || parentDir == "" {
		if e.remoteRootItemID != "" {
			return e.remoteRootItemID, nil
		}

		return GraphRootID, nil
	}

	// Normalize to forward slashes for map lookups.
	parentDir = filepath.ToSlash(parentDir)

	// Check baseline (DAG edges ensure parent folder creates commit before children).
	if entry, ok := e.baseline.GetByPath(parentDir); ok {
		return entry.ItemID, nil
	}

	return "", fmt.Errorf("parent folder %s has not been synced yet", parentDir)
}

// resolveDriveID returns the action's DriveID when present. Brand-new local
// items can still arrive with a zero action DriveID, so inherit the parent
// folder's baseline drive before falling back to the executor's content drive.
func (e *Executor) resolveDriveID(action *syncdomain.Action) driveid.ID {
	if !action.DriveID.IsZero() {
		return action.DriveID
	}

	if action != nil {
		parentDir := filepath.ToSlash(filepath.Dir(action.Path))
		if parentDir != "." && parentDir != "" {
			if entry, ok := e.baseline.GetByPath(parentDir); ok && !entry.DriveID.IsZero() {
				return entry.DriveID
			}
		}
	}

	return e.driveID
}

// failedOutcome builds an ActionOutcome for a failed action.
//
// The failure is logged at Debug only. Its user-facing report is the sync
// console's activity line ("Failed to upload X: reason", or the --verbose
// "will retry" line), which also knows to hide superseded and retried
// failures; a Warn here printed every failure a second time.
func (e *Executor) failedOutcome(action *syncdomain.Action, actionType syncdomain.ActionType, err error) syncdomain.ActionOutcome {
	e.logger.Debug("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.Any("error", err),
	)

	return syncdomain.ActionOutcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

func (e *Executor) failedOutcomeWithFailure(
	action *syncdomain.Action,
	actionType syncdomain.ActionType,
	err error,
	failurePath string,
	failureCapability syncdomain.PermissionCapability,
) syncdomain.ActionOutcome {
	outcome := e.failedOutcome(action, actionType, err)
	if failurePath != "" {
		outcome.FailurePath = failurePath
	}
	outcome.FailureCapability = failureCapability

	return outcome
}

func inferFailureCapabilityFromError(
	err error,
	localCapability syncdomain.PermissionCapability,
	remoteCapability syncdomain.PermissionCapability,
) syncdomain.PermissionCapability {
	switch {
	case errors.Is(err, os.ErrPermission):
		return localCapability
	case errors.Is(err, graph.ErrForbidden):
		return remoteCapability
	case syncdomain.ExtractHTTPStatus(err) == http.StatusForbidden:
		return remoteCapability
	default:
		return syncdomain.PermissionCapabilityUnknown
	}
}

// folderOutcome builds a successful ActionOutcome for a local folder create
// of folder, the rooted spelling on disk of action.Path.
func (e *Executor) folderOutcome(action *syncdomain.Action, folder string) syncdomain.ActionOutcome {
	o := syncdomain.ActionOutcome{
		Action:   syncdomain.ActionFolderCreate,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: syncdomain.ItemTypeFolder,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View != nil && action.View.Remote != nil {
		o.ItemID = action.View.Remote.ItemID
		o.ETag = action.View.Remote.ETag
	}
	e.observeLocalIdentity(&o, folder)

	return o
}

// observeLocalIdentity records the filesystem identity and ctime of the entry
// this executor just materialized at o.Path, so a later local rename of it can
// pair as a move and the next scan can trust the recorded hash (R-2.2.4). The
// identity is always marked observed: an entry the executor just (re)created
// must never inherit a stale baseline identity or ctime, so a failed lookup
// records "unknown" rather than falling back, and unknown forces a re-read.
// local is the entry's rooted spelling on disk (R-2.13.2).
func (e *Executor) observeLocalIdentity(o *syncdomain.ActionOutcome, local string) {
	o.LocalIdentityObserved = true
	o.LocalCtime = 0
	info, err := e.syncTree.Lstat(local)
	if err != nil {
		e.logger.Debug("local identity unavailable after materializing entry",
			slog.String("path", o.Path),
			slog.Any("error", err),
		)
		return
	}
	o.LocalCtime = syncdomain.FileChangeTime(info)
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return
	}
	o.LocalDevice = identity.Device
	o.LocalInode = identity.Inode
	o.LocalBirthTime = identity.BirthTime
	o.LocalHasIdentity = true
}

// moveOutcome builds a successful ActionOutcome for a move action.
//
// The baseline row it publishes must describe only what was observed or proven:
//   - ActionRemoteMove propagates a local move, so the local half comes from the
//     local truth the planner observed at the destination (MovedLocal), never
//     from the source baseline. An unknown destination hash stays unknown.
//   - ActionLocalMove propagates a remote move. The rename preserves content,
//     size, mtime and filesystem identity, and validateLocalMovePrecondition
//     proved the source still matched its baseline, so the source baseline's
//     local half is exactly what now sits at the destination.
//   - If the remote item's content changed as well as its path, the local copy
//     still holds the old content: the remote half keeps the source baseline's
//     content facts so the next pass sees the remote edit and downloads it.
func (e *Executor) moveOutcome(action *syncdomain.Action) syncdomain.ActionOutcome {
	o := syncdomain.ActionOutcome{
		Action:   action.Type,
		Success:  true,
		Path:     action.Path,
		OldPath:  action.OldPath,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View == nil {
		return o
	}
	baseline := action.View.Baseline

	if remote := action.View.Remote; remote != nil {
		o.ItemType = remote.ItemType
		o.ETag = remote.ETag
		o.RemoteHash = remote.Hash
		o.RemoteSize = remote.Size
		o.RemoteSizeKnown = true
		o.RemoteMtime = remote.Mtime
		// A move publishes only what it changed: the path. Remote content
		// that changed since the baseline is still pending in either
		// direction. Recording it here (a local rename racing a remote
		// edit of the same file) marked the edit synced while the local
		// file kept the old bytes: a silent, permanent divergence.
		if syncdomain.RemoteContentDiffersFromBaseline(remote, baseline) {
			o.ETag = baseline.ETag
			o.RemoteHash = baseline.RemoteHash
			o.RemoteSize = baseline.RemoteSize
			o.RemoteSizeKnown = baseline.RemoteSizeKnown
			o.RemoteMtime = baseline.RemoteMtime
		}
	}

	// A local move's view describes what sat at the destination before the
	// move (normally nothing, or the file it replaces); the moved item's local
	// half is the source baseline's, which the rename preserves.
	local := action.View.Local
	if action.Type == syncdomain.ActionLocalMove {
		local = nil
	}
	if action.Type == syncdomain.ActionRemoteMove && action.MovedLocal != nil {
		local = action.MovedLocal
	}
	if local != nil {
		o.LocalHash = local.Hash
		o.LocalSize = local.Size
		o.LocalSizeKnown = true
		o.LocalMtime = local.Mtime
		o.LocalCtime = local.LocalCtime
		o.LocalDevice = local.LocalDevice
		o.LocalInode = local.LocalInode
		o.LocalBirthTime = local.LocalBirthTime
		o.LocalHasIdentity = local.LocalHasIdentity
		o.LocalIdentityObserved = true
		fillRemoteOutcomeFromBaseline(&o, baseline, action.View.Remote != nil)
	} else {
		fillOutcomeFromBaseline(&o, baseline, action.View.Remote != nil)
	}
	if o.ItemType == syncdomain.ItemTypeFile && baseline != nil && baseline.ItemType != syncdomain.ItemTypeFile {
		o.ItemType = baseline.ItemType
	}

	return o
}

// plannedRemoteItemType is the remote item type the planner saw for action.
func plannedRemoteItemType(action *syncdomain.Action) syncdomain.ItemType {
	if planned := plannedRemoteStateForPrecondition(action); planned != nil {
		return planned.ItemType
	}

	return syncdomain.ResolveItemType(action.View)
}

func fillOutcomeFromBaseline(o *syncdomain.ActionOutcome, baseline *syncdomain.BaselineEntry, remoteObserved bool) {
	if baseline == nil {
		return
	}

	if o.LocalHash == "" {
		o.LocalHash = baseline.LocalHash
	}
	if !o.LocalSizeKnown {
		o.LocalSize = baseline.LocalSize
		o.LocalSizeKnown = baseline.LocalSizeKnown
	}
	if o.LocalMtime == 0 {
		o.LocalMtime = baseline.LocalMtime
	}
	if !o.LocalIdentityObserved {
		o.LocalCtime = baseline.LocalCtime
		o.LocalDevice = baseline.LocalDevice
		o.LocalInode = baseline.LocalInode
		o.LocalBirthTime = baseline.LocalBirthTime
		o.LocalHasIdentity = baseline.LocalHasIdentity
	}
	fillRemoteOutcomeFromBaseline(o, baseline, remoteObserved)
}

// fillRemoteOutcomeFromBaseline fills only unknown remote-side facts. It is
// used on its own when the local half was observed, because an observed but
// unknown local hash must stay unknown rather than inherit the baseline's.
// The remote modification time is filled only when the view did not observe
// the remote side, as in a publication (fillMutationFromBaselineDefaults): an
// observed item with no time is recorded with none, which is what the
// planner's comparison reads from the remote row.
func fillRemoteOutcomeFromBaseline(o *syncdomain.ActionOutcome, baseline *syncdomain.BaselineEntry, remoteObserved bool) {
	if baseline == nil {
		return
	}

	if o.RemoteHash == "" {
		o.RemoteHash = baseline.RemoteHash
	}
	if !o.RemoteSizeKnown {
		o.RemoteSize = baseline.RemoteSize
		o.RemoteSizeKnown = baseline.RemoteSizeKnown
	}
	if !syncdomain.StoredTimeIsSet(o.RemoteMtime) && !remoteObserved {
		o.RemoteMtime = baseline.RemoteMtime
	}
	if o.ETag == "" {
		o.ETag = baseline.ETag
	}
}

// reachCrashPoint calls the crash hook at point (crash_points.go).
func (cfg *ExecutorConfig) reachCrashPoint(point string) error {
	if cfg == nil || cfg.hooks.CrashAt == nil {
		return nil
	}

	return cfg.hooks.CrashAt(point)
}
