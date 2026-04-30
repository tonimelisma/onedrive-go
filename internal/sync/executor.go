package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// graphRootID is the Graph API parent reference for top-level items.
// Distinct from strRoot in types.go which serializes the ItemTypeRoot enum.
const graphRootID = "root"

// localDirPerms are the standard permissions for directories we create inside
// the sync root. Group/world execute keeps traversal semantics for normal dirs.
const localDirPerms = 0o755

// ExecutorConfig holds the immutable configuration for creating per-call
// Executor instances. Separated from mutable state to prevent temporal
// coupling and enable thread safety.
type ExecutorConfig struct {
	items            ItemClient
	downloads        driveops.Downloader
	uploads          driveops.Uploader
	syncTree         *synctree.Root
	driveID          driveid.ID // mount remote drive context (B-068)
	remoteRootItemID string     // mounted remote root for mount-root engines; empty = drive root
	logger           *slog.Logger
	ignoreJunkFiles  bool

	// transferMgr handles unified download/upload with resume and disk
	// space pre-checks (R-6.2.6). Disk check is configured via
	// driveops.WithDiskCheck when constructing the TransferManager.
	transferMgr *driveops.TransferManager

	// Injectable for testing.
	nowFunc         func() time.Time
	hashFunc        func(filePath string) (string, error)
	pathConvergence driveops.PathConvergence
}

// Executor executes individual actions against the Graph API and local
// filesystem, producing Outcomes. Created per-action via NewExecution.
// Thread-safe: all mutable state is per-call, no shared mutation.
type Executor struct {
	*ExecutorConfig
	baseline *Baseline
}

// NewExecutorConfig creates an immutable executor configuration bound to a
// specific drive and sync root. Use NewExecution to create per-call executors.
func NewExecutorConfig(
	items ItemClient, downloads driveops.Downloader, uploads driveops.Uploader,
	syncTree *synctree.Root, driveID driveid.ID, logger *slog.Logger, pathConvergence driveops.PathConvergence,
) *ExecutorConfig {
	cfg := &ExecutorConfig{
		items:           items,
		downloads:       downloads,
		uploads:         uploads,
		syncTree:        syncTree,
		driveID:         driveID,
		logger:          logger,
		nowFunc:         time.Now,
		hashFunc:        driveops.ComputeQuickXorHash,
		pathConvergence: pathConvergence,
	}

	return cfg
}

// SetTransferMgr sets the transfer manager for unified download/upload with
// resume and disk space pre-checks. Must be called before any Executor is
// created from this config.
func (cfg *ExecutorConfig) SetTransferMgr(mgr *driveops.TransferManager) {
	cfg.transferMgr = mgr
}

// SetRemoteRootItemID sets the mounted remote root item for mount-root engines.
// Empty keeps the normal owner-drive root semantics.
func (cfg *ExecutorConfig) SetRemoteRootItemID(itemID string) {
	cfg.remoteRootItemID = itemID
}

// SetContentFilter installs executor-relevant cleanup policy from the
// sync-owned content filter. The executor does not decide visibility; it only
// needs to know whether ignored junk may be removed when blocking a folder
// delete.
func (cfg *ExecutorConfig) SetContentFilter(filter ContentFilterConfig) {
	cfg.ignoreJunkFiles = filter.IgnoreJunkFiles
}

// Items returns the item client for direct API access (e.g., for trial
// observation in the engine's reobserve path).
func (cfg *ExecutorConfig) Items() ItemClient {
	return cfg.items
}

func (e *Executor) confirmRemotePathVisible(ctx context.Context, action *Action) {
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

	e.logger.Warn(message,
		slog.String("path", action.Path),
		slog.String("target_path", remotePath),
		slog.String("error", err.Error()),
	)
}

func (e *Executor) pathConvergenceForAction(action *Action) (driveops.PathConvergence, string, bool) {
	if action == nil {
		return nil, "", false
	}

	return e.pathConvergenceForPath(action, action.Path)
}

func (e *Executor) pathConvergenceForPath(action *Action, actionPath string) (driveops.PathConvergence, string, bool) {
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
func (e *Executor) waitRemoteParentVisible(ctx context.Context, action *Action) error {
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
		return fmt.Errorf("wait for parent path %q visibility: %w", parentPath, err)
	}

	return nil
}

// SetNowFunc overrides the time source. Used in tests to produce deterministic
// timestamps without mocking the real clock.
func (cfg *ExecutorConfig) SetNowFunc(fn func() time.Time) {
	cfg.nowFunc = fn
}

// Downloads returns the configured downloader. Used by tests that need to
// replace the downloader mid-test and rebuild the TransferManager.
func (cfg *ExecutorConfig) Downloads() driveops.Downloader {
	return cfg.downloads
}

// SetDownloads replaces the downloader. Used in tests to inject a mock that
// simulates download failures after the initial ExecutorConfig is constructed.
func (cfg *ExecutorConfig) SetDownloads(dl driveops.Downloader) {
	cfg.downloads = dl
}

// Uploads returns the configured uploader. Used by tests that need to
// replace the uploader mid-test and rebuild the TransferManager.
func (cfg *ExecutorConfig) Uploads() driveops.Uploader {
	return cfg.uploads
}

// SetUploads replaces the uploader. Used in tests to inject a mock that
// simulates upload failures after the initial ExecutorConfig is constructed.
func (cfg *ExecutorConfig) SetUploads(ul driveops.Uploader) {
	cfg.uploads = ul
}

// NewExecution creates an ephemeral Executor for a single action execution.
// Baseline is used for parent ID resolution (thread-safe via locked accessors).
func NewExecution(cfg *ExecutorConfig, bl *Baseline) *Executor {
	return &Executor{
		ExecutorConfig: cfg,
		baseline:       bl,
	}
}

// ExecuteFolderCreate dispatches to local or remote folder creation.
func (e *Executor) ExecuteFolderCreate(ctx context.Context, action *Action) ActionOutcome {
	if action.CreateSide == CreateLocal {
		return e.createLocalFolder(action)
	}

	return e.createRemoteFolder(ctx, action)
}

// createLocalFolder creates a directory on the local filesystem.
func (e *Executor) createLocalFolder(action *Action) ActionOutcome {
	if err := e.syncTree.MkdirAll(action.Path, localDirPerms); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			fmt.Errorf("creating local folder %s: %w", action.Path, normalizeSyncTreePathError(err)),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	// Set mtime from remote if available.
	if action.View != nil && action.View.Remote != nil && action.View.Remote.Mtime != 0 {
		mtime := time.Unix(0, action.View.Remote.Mtime)
		if err := e.syncTree.Chtimes(action.Path, mtime, mtime); err != nil {
			e.logger.Warn("failed to set folder mtime", slog.String("path", action.Path), slog.String("error", err.Error()))
		}
	}

	e.logger.Debug("created local folder", slog.String("path", action.Path))

	return e.folderOutcome(action)
}

func (e *Executor) resolvedParentIDForOutcome(action *Action, item *graph.Item) string {
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

// createRemoteFolder creates a folder on OneDrive. The DAG guarantees parent
// folder creates complete before children, so ResolveParentID finds the parent
// in the baseline (committed by CommitMutation before depGraph.Complete).
func (e *Executor) createRemoteFolder(ctx context.Context, action *Action) ActionOutcome {
	parentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionFolderCreate, err, action.Path, PermissionCapabilityUnknown)
	}

	if waitErr := e.waitRemoteParentVisible(ctx, action); waitErr != nil {
		return e.failedOutcomeWithFailure(action, ActionFolderCreate, waitErr, action.Path, PermissionCapabilityUnknown)
	}

	driveID := e.resolveDriveID(action)
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, parentID, action, "folder create")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			parentPreconditionErr,
			action.Path,
			PermissionCapabilityRemoteRead,
		)
	}
	name := filepath.Base(action.Path)

	item, err := e.items.CreateFolder(ctx, driveID, parentID, name)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionFolderCreate,
			fmt.Errorf("creating remote folder %s: %w", action.Path, err),
			action.Path,
			inferFailureCapabilityFromError(err, PermissionCapabilityUnknown, PermissionCapabilityRemoteWrite),
		)
	}

	e.logger.Debug("created remote folder",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	return ActionOutcome{
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
}

// ExecuteMove dispatches to local or remote move.
func (e *Executor) ExecuteMove(ctx context.Context, action *Action) ActionOutcome {
	if action.Type == ActionLocalMove {
		return e.ExecuteLocalMove(action)
	}

	return e.ExecuteRemoteMove(ctx, action)
}

// ExecuteLocalMove renames a local file/folder.
func (e *Executor) ExecuteLocalMove(action *Action) ActionOutcome {
	if err := e.validateLocalMovePrecondition(action); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			err,
			action.OldPath,
			PermissionCapabilityLocalWrite,
		)
	}

	// Ensure parent directory exists.
	if err := e.syncTree.MkdirAll(filepath.Dir(action.Path), localDirPerms); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			fmt.Errorf("creating parent for move target %s: %w", action.Path, normalizeSyncTreePathError(err)),
			action.Path,
			PermissionCapabilityLocalWrite,
		)
	}

	if err := e.syncTree.Rename(action.OldPath, action.Path); err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionLocalMove,
			fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, normalizeSyncTreePathError(err)),
			action.OldPath,
			PermissionCapabilityLocalWrite,
		)
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	return e.moveOutcome(action)
}

// ExecuteRemoteMove renames/moves an item on OneDrive.
func (e *Executor) ExecuteRemoteMove(ctx context.Context, action *Action) ActionOutcome {
	driveID := e.resolveDriveID(action)

	newParentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcomeWithFailure(action, ActionRemoteMove, err, action.OldPath, PermissionCapabilityUnknown)
	}
	sourcePreconditionErr := e.validateRemoteSourcePrecondition(ctx, driveID, action, "remote move")
	if sourcePreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteMove,
			sourcePreconditionErr,
			action.OldPath,
			PermissionCapabilityRemoteRead,
		)
	}
	parentPreconditionErr := e.validateRemoteParentPrecondition(ctx, driveID, newParentID, action, "remote move")
	if parentPreconditionErr != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteMove,
			parentPreconditionErr,
			action.Path,
			PermissionCapabilityRemoteRead,
		)
	}

	newName := filepath.Base(action.Path)

	item, err := e.items.MoveItem(ctx, driveID, action.ItemID, newParentID, newName)
	if err != nil {
		return e.failedOutcomeWithFailure(
			action,
			ActionRemoteMove,
			fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err),
			action.OldPath,
			inferFailureCapabilityFromError(err, PermissionCapabilityUnknown, PermissionCapabilityRemoteWrite),
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

// ContainedPath joins syncRoot and relPath, returning the absolute path only
// if the result stays within syncRoot. Uses filepath.IsLocal (Go 1.20+) to
// reject traversal sequences, absolute paths, and empty strings. Additionally
// resolves symlinks to detect TOCTOU escape via symlinked path components.
func ContainedPath(syncRoot, relPath string) (string, error) {
	if !filepath.IsLocal(relPath) {
		return "", fmt.Errorf("%w: %q", ErrPathEscapesSyncRoot, relPath)
	}

	absPath := filepath.Join(syncRoot, relPath)

	// Resolve symlinks on the parent directory to detect escape via
	// symlinked path components. The file itself may not exist yet
	// (common for downloads), but its parent directory must exist for
	// symlink-based attacks to work.
	parentDir := filepath.Dir(absPath)

	resolvedRoot, err := filepath.EvalSymlinks(syncRoot)
	if err != nil {
		return "", fmt.Errorf("evaluating sync root symlinks: %w", err)
	}

	resolvedParent, parentKnown, err := resolveParentForContainment(parentDir)
	if err != nil {
		return "", err
	}
	if !parentKnown {
		return absPath, nil
	}

	if resolvedParent != resolvedRoot &&
		!strings.HasPrefix(resolvedParent, resolvedRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: symlink resolves to %q outside root %q",
			ErrPathEscapesSyncRoot, resolvedParent, resolvedRoot)
	}

	return absPath, nil
}

func normalizeSyncTreePathError(err error) error {
	if err == nil {
		return nil
	}

	if strings.Contains(err.Error(), "escapes root") || strings.Contains(err.Error(), "must not be absolute") {
		return errors.Join(ErrPathEscapesSyncRoot, err)
	}

	return err
}

func resolveParentForContainment(parentDir string) (string, bool, error) {
	resolvedParent, err := filepath.EvalSymlinks(parentDir)
	if err == nil {
		return resolvedParent, true, nil
	}

	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}

	return "", false, fmt.Errorf("evaluating parent directory symlinks: %w", err)
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

		return graphRootID, nil
	}

	// Normalize to forward slashes for map lookups.
	parentDir = filepath.ToSlash(parentDir)

	// Check baseline (DAG edges ensure parent folder creates commit before children).
	if entry, ok := e.baseline.GetByPath(parentDir); ok {
		return entry.ItemID, nil
	}

	return "", fmt.Errorf("sync: cannot resolve parent ID for %s (parent %s not in baseline)", relPath, parentDir)
}

// resolveDriveID returns the action's DriveID when present. Brand-new local
// items can still arrive with a zero action DriveID, so inherit the parent
// folder's baseline drive before falling back to the executor's content drive.
func (e *Executor) resolveDriveID(action *Action) driveid.ID {
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
func (e *Executor) failedOutcome(action *Action, actionType ActionType, err error) ActionOutcome {
	e.logger.Warn("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.String("error", err.Error()),
	)

	return ActionOutcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

func (e *Executor) failedOutcomeWithFailure(
	action *Action,
	actionType ActionType,
	err error,
	failurePath string,
	failureCapability PermissionCapability,
) ActionOutcome {
	outcome := e.failedOutcome(action, actionType, err)
	if failurePath != "" {
		outcome.FailurePath = failurePath
	}
	outcome.FailureCapability = failureCapability

	return outcome
}

func inferFailureCapabilityFromError(
	err error,
	localCapability PermissionCapability,
	remoteCapability PermissionCapability,
) PermissionCapability {
	switch {
	case errors.Is(err, os.ErrPermission):
		return localCapability
	case errors.Is(err, graph.ErrForbidden):
		return remoteCapability
	case ExtractHTTPStatus(err) == http.StatusForbidden:
		return remoteCapability
	default:
		return PermissionCapabilityUnknown
	}
}

// folderOutcome builds a successful ActionOutcome for a local folder create.
func (e *Executor) folderOutcome(action *Action) ActionOutcome {
	o := ActionOutcome{
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

	return o
}

// moveOutcome builds a successful ActionOutcome for a move action.
func (e *Executor) moveOutcome(action *Action) ActionOutcome {
	o := ActionOutcome{
		Action:   action.Type,
		Success:  true,
		Path:     action.Path,
		OldPath:  action.OldPath,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ParentID: e.resolvedParentIDForOutcome(action, nil),
	}

	if action.View != nil {
		if action.View.Remote != nil {
			o.RemoteHash = action.View.Remote.Hash
			o.RemoteSize = action.View.Remote.Size
			o.RemoteSizeKnown = true
			o.RemoteMtime = action.View.Remote.Mtime
			o.ETag = action.View.Remote.ETag
			o.ItemType = action.View.Remote.ItemType
		}

		if action.View.Local != nil {
			o.LocalHash = action.View.Local.Hash
			o.LocalSize = action.View.Local.Size
			o.LocalSizeKnown = true
			o.LocalMtime = action.View.Local.Mtime
			o.LocalDevice = action.View.Local.LocalDevice
			o.LocalInode = action.View.Local.LocalInode
			o.LocalHasIdentity = action.View.Local.LocalHasIdentity
			o.LocalIdentityObserved = true
		}

		fillOutcomeFromBaseline(&o, action.View.Baseline)
	}

	return o
}

func fillOutcomeFromBaseline(o *ActionOutcome, baseline *BaselineEntry) {
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
		o.LocalDevice = baseline.LocalDevice
		o.LocalInode = baseline.LocalInode
		o.LocalHasIdentity = baseline.LocalHasIdentity
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
