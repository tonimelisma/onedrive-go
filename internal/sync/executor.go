package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// graphRootID is the Graph API parent reference for top-level items.
// Distinct from strRoot in types.go which serializes the ItemTypeRoot enum.
const graphRootID = "root"

// localDirPerms are the standard permissions for directories we create inside
// the sync root. Group/world execute keeps traversal semantics for normal dirs.
const localDirPerms = 0o755

// executorConfig holds the immutable configuration for creating per-call
// Executor instances. Separated from mutable state to prevent temporal
// coupling and enable thread safety.
type executorConfig struct {
	items            itemClient
	downloads        driveops.Downloader
	uploads          driveops.Uploader
	syncTree         *synctree.Root
	driveID          driveid.ID // mount remote drive context (B-068)
	remoteRootItemID string     // mounted remote root for mount-root engines; empty = drive root
	logger           *slog.Logger
	ignoreJunkFiles  bool
	// contentFilter tells a folder delete which leftover children sync
	// deliberately does not own (D-012).
	contentFilter contentFilter
	// expectedSyncRoot is the sync folder identity the engine validated;
	// a remote delete re-checks it before acting on local absence (D-011).
	syncRootIdentity *syncRootIdentity

	// transferMgr handles unified download/upload with resume and disk
	// space pre-checks (R-6.2.6). Disk check is configured via
	// driveops.WithDiskCheck when constructing the TransferManager.
	transferMgr *driveops.TransferManager

	// Injectable for testing.
	nowFunc  func() time.Time
	hashFunc func(filePath string) (string, error)
	// hashReader hashes an open descriptor; destructive actions verify the
	// exact inode they then remove or replace through it (D-010).
	hashReader      func(r io.Reader) (string, error)
	pathConvergence driveops.PathConvergence

	// trash keeps what local deletes remove and what downloads replace
	// (P-4). Nil only in executor-only tests; then removal is permanent.
	trash *localtrash.Trash

	// pendingCreates records new files whose empty item exists before their
	// content is sent (R-5.6.15). Nil in executor-only tests.
	pendingCreates pendingCreateStore
}

// SetTrash wires the drive's local trash.
func (cfg *executorConfig) SetTrash(trash *localtrash.Trash) {
	cfg.trash = trash
}

// SetPendingCreates wires the store that records pending creates.
func (cfg *executorConfig) SetPendingCreates(store pendingCreateStore) {
	cfg.pendingCreates = store
}

// executor executes individual actions against the Graph API and local
// filesystem, producing Outcomes. Created per-action via NewExecution.
// Thread-safe: all mutable state is per-call, no shared mutation.
type executor struct {
	*executorConfig
	baseline *Baseline
}

// newExecutorConfig creates an immutable executor configuration bound to a
// specific drive and sync root. Use NewExecution to create per-call executors.
func newExecutorConfig(
	items itemClient, downloads driveops.Downloader, uploads driveops.Uploader,
	syncTree *synctree.Root, driveID driveid.ID, logger *slog.Logger, pathConvergence driveops.PathConvergence,
) *executorConfig {
	cfg := &executorConfig{
		items:           items,
		downloads:       downloads,
		uploads:         uploads,
		syncTree:        syncTree,
		driveID:         driveID,
		logger:          logger,
		nowFunc:         time.Now,
		hashFunc:        driveops.ComputeQuickXorHash,
		hashReader:      driveops.ComputeQuickXorHashFromReader,
		pathConvergence: pathConvergence,
	}

	return cfg
}

// SetTransferMgr sets the transfer manager for unified download/upload with
// resume and disk space pre-checks. Must be called before any Executor is
// created from this config.
func (cfg *executorConfig) SetTransferMgr(mgr *driveops.TransferManager) {
	cfg.transferMgr = mgr
}

// SetRemoteRootItemID sets the mounted remote root item for mount-root engines.
// Empty keeps the normal owner-drive root semantics.
func (cfg *executorConfig) SetRemoteRootItemID(itemID string) {
	cfg.remoteRootItemID = itemID
}

// SetContentFilter installs executor-relevant cleanup policy from the
// sync-owned content filter. The executor does not decide visibility; it only
// needs to know whether ignored junk may be removed when blocking a folder
// delete.
func (cfg *executorConfig) SetContentFilter(filter ContentFilterConfig) {
	cfg.ignoreJunkFiles = filter.IgnoreJunkFiles
	cfg.contentFilter = newContentFilter(cloneContentFilterConfig(filter))
}

// SetSyncRootIdentity installs the engine's sync folder identity, which
// remote deletes re-validate before acting on local absence. It is shared,
// not copied, so a binding made after startup is seen here too.
func (cfg *executorConfig) SetSyncRootIdentity(identity *syncRootIdentity) {
	cfg.syncRootIdentity = identity
}

// Items returns the item client for direct API access (e.g., for trial
// observation in the engine's reobserve path).
func (cfg *executorConfig) Items() itemClient {
	return cfg.items
}

func (e *executor) confirmRemotePathVisible(ctx context.Context, action *Action) {
	pathConvergence, remotePath, ok := e.pathConvergenceForAction(action)
	if !ok {
		return
	}

	_, err := pathConvergence.WaitPathVisible(ctx, remotePath)
	if err == nil {
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
		slog.String("error", err.Error()),
	)
}

func (e *executor) pathConvergenceForAction(action *Action) (driveops.PathConvergence, string, bool) {
	if action == nil {
		return nil, "", false
	}

	return e.pathConvergenceForPath(action, action.Path)
}

func (e *executor) pathConvergenceForPath(action *Action, actionPath string) (driveops.PathConvergence, string, bool) {
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
func (e *executor) waitRemoteParentVisible(ctx context.Context, action *Action) error {
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

	if _, err := pathConvergence.WaitPathVisible(ctx, remotePath); err != nil {
		return fmt.Errorf("confirm parent folder %s is on OneDrive: %w", parentPath, err)
	}

	return nil
}

// SetNowFunc overrides the time source. Used in tests to produce deterministic
// timestamps without mocking the real clock.
func (cfg *executorConfig) SetNowFunc(fn func() time.Time) {
	cfg.nowFunc = fn
}

// Downloads returns the configured downloader. Used by tests that need to
// replace the downloader mid-test and rebuild the TransferManager.
func (cfg *executorConfig) Downloads() driveops.Downloader {
	return cfg.downloads
}

// SetDownloads replaces the downloader. Used in tests to inject a mock that
// simulates download failures after the initial ExecutorConfig is constructed.
func (cfg *executorConfig) SetDownloads(dl driveops.Downloader) {
	cfg.downloads = dl
}

// Uploads returns the configured uploader. Used by tests that need to
// replace the uploader mid-test and rebuild the TransferManager.
func (cfg *executorConfig) Uploads() driveops.Uploader {
	return cfg.uploads
}

// SetUploads replaces the uploader. Used in tests to inject a mock that
// simulates upload failures after the initial ExecutorConfig is constructed.
func (cfg *executorConfig) SetUploads(ul driveops.Uploader) {
	cfg.uploads = ul
}

// newExecution creates an ephemeral Executor for a single action execution.
// Baseline is used for parent ID resolution (thread-safe via locked accessors).
func newExecution(cfg *executorConfig, bl *Baseline) *executor {
	return &executor{
		executorConfig: cfg,
		baseline:       bl,
	}
}

// ExecuteFolderCreate dispatches to local or remote folder creation.
func (e *executor) ExecuteFolderCreate(ctx context.Context, action *Action) actionOutcome {
	if action.CreateSide == createLocal {
		return e.createLocalFolder(action)
	}

	return e.createRemoteFolder(ctx, action)
}

// createLocalFolder creates a directory on the local filesystem.
func (e *executor) createLocalFolder(action *Action) actionOutcome {
	if err := e.validateNoSymlinkBoundary(action.Path, "create folder"); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			err,
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	if err := e.syncTree.MkdirAll(action.Path, localDirPerms); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			fmt.Errorf("creating local folder %s: %w", action.Path, normalizeSyncTreePathError(err)),
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	// Set mtime from remote if available.
	if action.View != nil && action.View.Remote != nil && action.View.Remote.Mtime != 0 {
		mtime := time.Unix(0, action.View.Remote.Mtime)
		if err := e.syncTree.Chtimes(action.Path, mtime, mtime); err != nil {
			// Debug: the folder exists; some filesystems (SMB, FUSE) refuse
			// directory times, which would otherwise warn once per folder.
			e.logger.Debug("failed to set folder mtime", slog.String("path", action.Path), slog.String("error", err.Error()))
		}
	}

	e.logger.Debug("created local folder", slog.String("path", action.Path))

	return e.folderOutcome(action)
}

func (e *executor) resolvedParentIDForOutcome(action *Action, item *graph.Item) string {
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

// adoptExistingRemoteFolder handles a 409 on a folder create. A folder with
// exactly this name already being there is the outcome the create wanted (a
// lost response to our own earlier create, or another device creating the
// same folder): adopt it, as observing it would have. Anything else under the
// name, a file or a differently spelled folder OneDrive treats as the same
// name, is a stale precondition for the planner to resolve; the 409 used to
// be retried as is, forever.
func (e *executor) adoptExistingRemoteFolder(
	ctx context.Context, driveID driveid.ID, parentID, name string, conflictErr error,
) (*graph.Item, error) {
	children, err := e.items.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, fmt.Errorf("listing parent after folder create conflict: %w", errors.Join(conflictErr, err))
	}

	for i := range children {
		if children[i].IsFolder && children[i].ID != "" && nfcNormalize(children[i].Name) == nfcNormalize(name) {
			e.logger.Debug("adopted existing remote folder after create conflict",
				slog.String("name", name),
				slog.String("item_id", children[i].ID),
			)

			return &children[i], nil
		}
	}

	return nil, stalePreconditionError("folder create %s: OneDrive already has another item with this name", name)
}

// createRemoteFolder creates a folder on OneDrive. The DAG guarantees parent
// folder creates complete before children, so ResolveParentID finds the parent
// in the baseline (committed by CommitMutation before depGraph.Complete).
func (e *executor) createRemoteFolder(ctx context.Context, action *Action) actionOutcome {
	parentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionFolderCreate, err, action.Path, permissionCapabilityUnknown)
	}

	if waitErr := e.waitRemoteParentVisible(ctx, action); waitErr != nil {
		return e.failedOutcomeWithFailure(action, ActionFolderCreate, waitErr, action.Path, permissionCapabilityUnknown)
	}

	driveID := e.resolveDriveID(action)
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, parentID, action, "folder create")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			parentPreconditionErr,
			action.Path,
			permissionCapabilityRemoteRead,
		)
	}
	name := filepath.Base(action.Path)

	item, err := e.items.CreateFolder(ctx, driveID, parentID, name)
	if errors.Is(err, graph.ErrConflict) {
		item, err = e.adoptExistingRemoteFolder(ctx, driveID, parentID, name, err)
	}
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			fmt.Errorf("creating remote folder %s: %w", action.Path, err),
			action.Path,
			inferFailureCapabilityFromError(err, permissionCapabilityUnknown, permissionCapabilityRemoteWrite),
		)
	}

	e.logger.Debug("created remote folder",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	o := actionOutcome{
		Action:     ActionFolderCreate,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     item.ID,
		ParentID:   parentID,
		ItemType:   ItemTypeFolder,
		RemoteHash: driveops.SelectHash(item), // SelectHash: driveops package (B-222)
		ETag:       item.ETag,
	}
	// The folder already exists locally; its observed identity is what lets a
	// later local rename of it pair as one move instead of delete + create.
	if action.View != nil && action.View.Local != nil {
		o.LocalCtime = action.View.Local.LocalCtime
		o.LocalDevice = action.View.Local.LocalDevice
		o.LocalInode = action.View.Local.LocalInode
		o.LocalHasIdentity = action.View.Local.LocalHasIdentity
		o.LocalIdentityObserved = true
	}

	return o
}

// ExecuteMove dispatches to local or remote move.
func (e *executor) ExecuteMove(ctx context.Context, action *Action) actionOutcome {
	if action.Type == ActionLocalMove {
		return e.ExecuteLocalMove(action)
	}

	return e.ExecuteRemoteMove(ctx, action)
}

// ExecuteLocalMove renames a local file/folder.
func (e *executor) ExecuteLocalMove(action *Action) actionOutcome {
	// Both endpoints matter: an unchecked destination would still create
	// parent directories outside the sync root, and rename replaces whatever
	// sits at the destination.
	for _, endpoint := range []string{action.OldPath, action.Path} {
		if err := e.validateNoSymlinkBoundary(endpoint, "move"); err != nil {
			return e.failedOutcomeWithFailure(
				action,
				ActionLocalMove,
				err,
				endpoint,
				permissionCapabilityLocalWrite,
			)
		}
	}

	if err := e.validateLocalMovePrecondition(action); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			err,
			action.OldPath,
			permissionCapabilityLocalWrite,
		)
	}

	// Ensure parent directory exists.
	if err := e.syncTree.MkdirAll(filepath.Dir(action.Path), localDirPerms); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			fmt.Errorf("creating parent for move target %s: %w", action.Path, normalizeSyncTreePathError(err)),
			action.Path,
			permissionCapabilityLocalWrite,
		)
	}

	if err := e.renameLocal(action.OldPath, action.Path); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, normalizeSyncTreePathError(err)),
			action.OldPath,
			permissionCapabilityLocalWrite,
		)
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	return e.moveOutcome(action)
}

// renameLocal renames within the sync tree. A spelling-only rename is tried
// directly first: Linux filesystems and APFS store the new spelling that way.
// Only when the stored spelling did not change (a case-insensitive volume
// that treats the rename as a no-op) does it go through a temporary sibling,
// named so that local observation always excludes it
// (isSpellingRenameArtifactName): a crash between the two renames must not
// leave a file that syncs under the temporary name.
func (e *executor) renameLocal(oldPath, newPath string) error {
	if err := e.syncTree.Rename(oldPath, newPath); err != nil {
		return err //nolint:wrapcheck // synctree names both paths
	}
	if !isLocalSpellingOnlyMove(oldPath, newPath) {
		return nil
	}

	stored, err := e.storedNameSpelling(newPath)
	if err != nil || stored == filepath.Base(newPath) {
		return err
	}

	//nolint:wrapcheck // synctree names both paths
	return e.syncTree.RenameWithTemporarySibling(
		filepath.Join(filepath.Dir(newPath), stored), newPath,
		spellingRenameArtifactPrefix+filepath.Base(newPath), localSpellingRenameAttempts,
	)
}

// storedNameSpelling returns the spelling the parent directory holds for the
// entry OneDrive would call path's name.
func (e *executor) storedNameSpelling(path string) (string, error) {
	entries, err := e.syncTree.ReadDir(filepath.Dir(path))
	if err != nil {
		return "", fmt.Errorf("reading %s after a spelling rename: %w", filepath.Dir(path), normalizeSyncTreePathError(err))
	}

	base := filepath.Base(path)
	stored := base
	for _, entry := range entries {
		if entry.Name() == base {
			return base, nil
		}
		if namekey.Equal(entry.Name(), base) {
			stored = entry.Name()
		}
	}

	return stored, nil
}

// localSpellingRenameAttempts bounds the temporary sibling names tried for a
// spelling-only local rename.
const localSpellingRenameAttempts = 10

// ExecuteRemoteMove renames/moves an item on OneDrive.
func (e *executor) ExecuteRemoteMove(ctx context.Context, action *Action) actionOutcome {
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
			ActionRemoteMove,
			sourcePreconditionErr,
			action.OldPath,
			permissionCapabilityRemoteRead,
		)
	}
	newParentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionRemoteMove, err, action.OldPath, permissionCapabilityUnknown)
	}
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, newParentID, action, "remote move")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteMove,
			parentPreconditionErr,
			action.Path,
			permissionCapabilityRemoteRead,
		)
	}

	newName := filepath.Base(action.Path)
	// No If-Match for a folder move: its eTag also changes when a child
	// changes, which does not make moving the folder unsafe, and a busy folder
	// would otherwise keep failing the move. Files keep the conditional.
	if plannedRemoteItemType(action) == ItemTypeFolder {
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
				ActionRemoteMove,
				stalePreconditionError("remote move item %s changed before mutation", action.ItemID),
				action.OldPath,
				permissionCapabilityRemoteWrite,
			)
		}

		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteMove,
			fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err),
			action.OldPath,
			inferFailureCapabilityFromError(err, permissionCapabilityUnknown, permissionCapabilityRemoteWrite),
		)
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
// This used to match on the message text. That is fragile across a package
// boundary -- rewording the sync tree's error silently stops the mapping --
// and it could only ever recognize the two phrasings it knew about.
func normalizeSyncTreePathError(err error) error {
	if err == nil {
		return nil
	}

	// One line, not errors.Join: the console prints this as a failure
	// reason, and a joined error renders on two lines.
	if errors.Is(err, synctree.ErrUnsafePath) {
		return fmt.Errorf("%w: %w", errPathEscapesSyncRoot, err)
	}

	return err
}

// ResolveParentID determines the remote parent ID for a given relative path.
// Checks baseline (which includes items committed by earlier DAG actions),
// then falls back to "root" for top-level items.
func (e *executor) ResolveParentID(relPath string) (string, error) {
	parentDir := filepath.Dir(relPath)

	// Top-level item: parent is root.
	if parentDir == "." || parentDir == "" {
		if e.remoteRootItemID != "" {
			return e.remoteRootItemID, nil
		}

		return graphRootID, nil
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
func (e *executor) resolveDriveID(action *Action) driveid.ID {
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
func (e *executor) failedOutcome(action *Action, actionType actionType, err error) actionOutcome {
	e.logger.Debug("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.String("error", err.Error()),
	)

	return actionOutcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

func (e *executor) failedOutcomeWithFailure(
	action *Action,
	actionType actionType,
	err error,
	failurePath string,
	failureCapability permissionCapability,
) actionOutcome {
	outcome := e.failedOutcome(action, actionType, err)
	if failurePath != "" {
		outcome.FailurePath = failurePath
	}
	outcome.FailureCapability = failureCapability

	return outcome
}

func inferFailureCapabilityFromError(
	err error,
	localCapability permissionCapability,
	remoteCapability permissionCapability,
) permissionCapability {
	switch {
	case errors.Is(err, os.ErrPermission):
		return localCapability
	case errors.Is(err, graph.ErrForbidden):
		return remoteCapability
	case extractHTTPStatus(err) == http.StatusForbidden:
		return remoteCapability
	default:
		return permissionCapabilityUnknown
	}
}

// folderOutcome builds a successful ActionOutcome for a local folder create.
func (e *executor) folderOutcome(action *Action) actionOutcome {
	o := actionOutcome{
		Action:   ActionFolderCreate,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: ItemTypeFolder,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View != nil && action.View.Remote != nil {
		o.ItemID = action.View.Remote.ItemID
		o.ETag = action.View.Remote.ETag
	}
	e.observeLocalIdentity(&o)

	return o
}

// observeLocalIdentity records the filesystem identity and ctime of the entry
// this executor just materialized at o.Path, so a later local rename of it can
// pair as a move and the next scan can trust the recorded hash (R-2.2.4). The
// identity is always marked observed: an entry the executor just (re)created
// must never inherit a stale baseline identity or ctime, so a failed lookup
// records "unknown" rather than falling back, and unknown forces a re-read.
func (e *executor) observeLocalIdentity(o *actionOutcome) {
	o.LocalIdentityObserved = true
	o.LocalCtime = 0
	info, err := e.syncTree.Lstat(o.Path)
	if err != nil {
		e.logger.Debug("local identity unavailable after materializing entry",
			slog.String("path", o.Path),
			slog.String("error", err.Error()),
		)
		return
	}
	o.LocalCtime = fileChangeTime(info)
	identity, ok := synctree.IdentityFromFileInfo(info)
	if !ok {
		return
	}
	o.LocalDevice = identity.Device
	o.LocalInode = identity.Inode
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
func (e *executor) moveOutcome(action *Action) actionOutcome {
	o := actionOutcome{
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
		if remoteContentDiffersFromBaseline(remote, baseline) {
			o.ETag = baseline.ETag
			o.RemoteHash = baseline.RemoteHash
			o.RemoteSize = baseline.RemoteSize
			o.RemoteSizeKnown = baseline.RemoteSizeKnown
			o.RemoteMtime = baseline.RemoteMtime
		}
	}

	local := action.View.Local
	if action.Type == ActionRemoteMove && action.MovedLocal != nil {
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
		o.LocalHasIdentity = local.LocalHasIdentity
		o.LocalIdentityObserved = true
		fillRemoteOutcomeFromBaseline(&o, baseline)
	} else {
		fillOutcomeFromBaseline(&o, baseline)
	}
	if o.ItemType == ItemTypeFile && baseline != nil && baseline.ItemType != ItemTypeFile {
		o.ItemType = baseline.ItemType
	}

	return o
}

// plannedRemoteItemType is the remote item type the planner saw for action.
func plannedRemoteItemType(action *Action) ItemType {
	if planned := plannedRemoteStateForPrecondition(action); planned != nil {
		return planned.ItemType
	}

	return resolveItemType(action.View)
}

// remoteContentDiffersFromBaseline reports whether remote content facts prove
// the item's content changed since the baseline. eTag and mtime churn alone
// (a Graph move rewrites both) are not content changes.
func remoteContentDiffersFromBaseline(remote *remoteState, baseline *BaselineEntry) bool {
	if remote == nil || baseline == nil || baseline.ItemType != ItemTypeFile {
		return false
	}
	if remote.Hash != "" && baseline.RemoteHash != "" && remote.Hash != baseline.RemoteHash {
		return true
	}

	return baseline.RemoteSizeKnown && remote.Size != baseline.RemoteSize
}

func fillOutcomeFromBaseline(o *actionOutcome, baseline *BaselineEntry) {
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
		o.LocalHasIdentity = baseline.LocalHasIdentity
	}
	fillRemoteOutcomeFromBaseline(o, baseline)
}

// fillRemoteOutcomeFromBaseline fills only unknown remote-side facts. It is
// used on its own when the local half was observed, because an observed but
// unknown local hash must stay unknown rather than inherit the baseline's.
func fillRemoteOutcomeFromBaseline(o *actionOutcome, baseline *BaselineEntry) {
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
	if o.RemoteMtime == 0 {
		o.RemoteMtime = baseline.RemoteMtime
	}
	if o.ETag == "" {
		o.ETag = baseline.ETag
	}
}
