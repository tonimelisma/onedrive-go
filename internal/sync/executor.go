package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
	items      synctypes.ItemClient
	downloads  driveops.Downloader
	uploads    driveops.Uploader
	syncTree   *synctree.Root
	driveID    driveid.ID // per-drive context (B-068)
	rootItemID string     // virtual root for folder-scoped shared drives; empty = drive root
	logger     *slog.Logger

	// transferMgr handles unified download/upload with resume and disk
	// space pre-checks (R-6.2.6). Disk check is configured via
	// driveops.WithDiskCheck when constructing the TransferManager.
	transferMgr *driveops.TransferManager

	// watchMode enables pre-upload eTag freshness checks. In watch mode,
	// a local fsnotify event can trigger an upload before the remote
	// observer has polled the collaborator's edit — the freshness check
	// catches this race by comparing the remote eTag against the baseline
	// before uploading.
	watchMode bool

	// Injectable for testing.
	nowFunc                func() time.Time
	hashFunc               func(filePath string) (string, error)
	trashFunc              func(absPath string) error // nil = permanent delete (os.Remove)
	pathConvergenceFactory driveops.PathConvergenceFactory
}

// Executor executes individual actions against the Graph API and local
// filesystem, producing Outcomes. Created per-action via NewExecution.
// Thread-safe: all mutable state is per-call, no shared mutation.
type Executor struct {
	*ExecutorConfig
	baseline *synctypes.Baseline
}

// NewExecutorConfig creates an immutable executor configuration bound to a
// specific drive and sync root. Use NewExecution to create per-call executors.
func NewExecutorConfig(
	items synctypes.ItemClient, downloads driveops.Downloader, uploads driveops.Uploader,
	syncTree *synctree.Root, driveID driveid.ID, logger *slog.Logger, pathConvergenceFactory driveops.PathConvergenceFactory,
) *ExecutorConfig {
	cfg := &ExecutorConfig{
		items:                  items,
		downloads:              downloads,
		uploads:                uploads,
		syncTree:               syncTree,
		driveID:                driveID,
		logger:                 logger,
		nowFunc:                time.Now,
		hashFunc:               driveops.ComputeQuickXorHash,
		pathConvergenceFactory: pathConvergenceFactory,
	}

	return cfg
}

// SetWatchMode enables or disables watch-mode-specific behavior. When enabled,
// uploads perform a pre-flight eTag freshness check to prevent silently
// overwriting concurrent remote changes (see executor_transfer.go).
func (cfg *ExecutorConfig) SetWatchMode(enabled bool) {
	cfg.watchMode = enabled
}

// IsWatchMode returns true if the executor is operating in watch mode.
func (cfg *ExecutorConfig) IsWatchMode() bool {
	return cfg.watchMode
}

// SetTrashFunc sets the trash function for moving deleted files to the OS trash
// instead of permanently deleting them. When nil, files are permanently deleted.
// Called by the engine when UseLocalTrash is configured.
func (cfg *ExecutorConfig) SetTrashFunc(fn func(absPath string) error) {
	cfg.trashFunc = fn
}

// SetTransferMgr sets the transfer manager for unified download/upload with
// resume and disk space pre-checks. Must be called before any Executor is
// created from this config.
func (cfg *ExecutorConfig) SetTransferMgr(mgr *driveops.TransferManager) {
	cfg.transferMgr = mgr
}

// SetRootItemID sets the configured virtual root item for folder-scoped shared
// drives. Empty keeps the normal owner-drive root semantics.
func (cfg *ExecutorConfig) SetRootItemID(itemID string) {
	cfg.rootItemID = itemID
}

// Items returns the item client for direct API access (e.g., for trial
// observation in the engine's reobserve path).
func (cfg *ExecutorConfig) Items() synctypes.ItemClient {
	return cfg.items
}

func (e *Executor) confirmRemotePathVisible(ctx context.Context, action *synctypes.Action) {
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

func (e *Executor) pathConvergenceForAction(action *synctypes.Action) (driveops.PathConvergence, string, bool) {
	if e.pathConvergenceFactory == nil || action == nil {
		return nil, "", false
	}

	targetDriveID := e.resolveDriveID(action)
	if targetDriveID.IsZero() {
		return nil, "", false
	}

	if targetDriveID.Equal(e.driveID) {
		return e.pathConvergenceFactory.ForTarget(targetDriveID, e.rootItemID), action.Path, true
	}

	if action.TargetRootItemID == "" || action.TargetRootLocalPath == "" {
		e.logger.Debug("skip cross-drive path convergence without target root metadata",
			slog.String("path", action.Path),
			slog.String("target_drive_id", targetDriveID.String()),
			slog.String("target_root_item_id", action.TargetRootItemID),
			slog.String("target_root_local_path", action.TargetRootLocalPath),
		)

		return nil, "", false
	}

	targetPath, ok := targetRelativePath(action.Path, action.TargetRootLocalPath)
	if !ok {
		e.logger.Debug("skip cross-drive path convergence with mismatched target root prefix",
			slog.String("path", action.Path),
			slog.String("target_drive_id", targetDriveID.String()),
			slog.String("target_root_local_path", action.TargetRootLocalPath),
		)

		return nil, "", false
	}

	return e.pathConvergenceFactory.ForTarget(targetDriveID, action.TargetRootItemID), targetPath, true
}

func targetRelativePath(actionPath, targetRootLocalPath string) (string, bool) {
	cleanPath := filepath.ToSlash(actionPath)
	cleanRoot := filepath.ToSlash(targetRootLocalPath)
	if cleanPath == cleanRoot {
		return "", true
	}

	prefix := cleanRoot + "/"
	if !strings.HasPrefix(cleanPath, prefix) {
		return "", false
	}

	return strings.TrimPrefix(cleanPath, prefix), true
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
func NewExecution(cfg *ExecutorConfig, bl *synctypes.Baseline) *Executor {
	return &Executor{
		ExecutorConfig: cfg,
		baseline:       bl,
	}
}

// ExecuteFolderCreate dispatches to local or remote folder creation.
func (e *Executor) ExecuteFolderCreate(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	if action.CreateSide == synctypes.CreateLocal {
		return e.createLocalFolder(action)
	}

	return e.createRemoteFolder(ctx, action)
}

// createLocalFolder creates a directory on the local filesystem.
func (e *Executor) createLocalFolder(action *synctypes.Action) synctypes.Outcome {
	if err := e.syncTree.MkdirAll(action.Path, localDirPerms); err != nil {
		return e.failedOutcome(
			action,
			synctypes.ActionFolderCreate,
			fmt.Errorf("creating local folder %s: %w", action.Path, normalizeSyncTreePathError(err)),
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

// createRemoteFolder creates a folder on OneDrive. The DAG guarantees parent
// folder creates complete before children, so ResolveParentID finds the parent
// in the baseline (committed by CommitOutcome before depGraph.Complete).
func (e *Executor) createRemoteFolder(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	parentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionFolderCreate, err)
	}

	driveID := e.resolveDriveID(action)
	name := filepath.Base(action.Path)

	item, err := e.items.CreateFolder(ctx, driveID, parentID, name)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionFolderCreate, fmt.Errorf("creating remote folder %s: %w", action.Path, err))
	}

	e.logger.Debug("created remote folder",
		slog.String("path", action.Path),
		slog.String("item_id", item.ID),
	)

	e.confirmRemotePathVisible(ctx, action)

	return synctypes.Outcome{
		Action:     synctypes.ActionFolderCreate,
		Success:    true,
		Path:       action.Path,
		DriveID:    driveID,
		ItemID:     item.ID,
		ParentID:   parentID,
		ItemType:   synctypes.ItemTypeFolder,
		RemoteHash: driveops.SelectHash(item), // SelectHash: driveops package (B-222)
		ETag:       item.ETag,
	}
}

// ExecuteMove dispatches to local or remote move.
func (e *Executor) ExecuteMove(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	if action.Type == synctypes.ActionLocalMove {
		return e.ExecuteLocalMove(action)
	}

	return e.ExecuteRemoteMove(ctx, action)
}

// ExecuteLocalMove renames a local file/folder.
func (e *Executor) ExecuteLocalMove(action *synctypes.Action) synctypes.Outcome {
	// Ensure parent directory exists.
	if err := e.syncTree.MkdirAll(filepath.Dir(action.Path), localDirPerms); err != nil {
		return e.failedOutcome(
			action,
			synctypes.ActionLocalMove,
			fmt.Errorf("creating parent for move target %s: %w", action.Path, normalizeSyncTreePathError(err)),
		)
	}

	if err := e.syncTree.Rename(action.OldPath, action.Path); err != nil {
		return e.failedOutcome(
			action,
			synctypes.ActionLocalMove,
			fmt.Errorf("renaming %s -> %s: %w", action.OldPath, action.Path, normalizeSyncTreePathError(err)),
		)
	}

	e.logger.Debug("local move complete", slog.String("from", action.OldPath), slog.String("to", action.Path))

	return e.moveOutcome(action)
}

// ExecuteRemoteMove renames/moves an item on OneDrive.
func (e *Executor) ExecuteRemoteMove(ctx context.Context, action *synctypes.Action) synctypes.Outcome {
	driveID := e.resolveDriveID(action)

	newParentID, err := e.ResolveParentID(action.Path)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionRemoteMove, err)
	}

	newName := filepath.Base(action.Path)

	item, err := e.items.MoveItem(ctx, driveID, action.ItemID, newParentID, newName)
	if err != nil {
		return e.failedOutcome(action, synctypes.ActionRemoteMove, fmt.Errorf("moving %s -> %s: %w", action.OldPath, action.Path, err))
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

// ExecuteSyncedUpdate produces an Outcome from a PathView without I/O.
func (e *Executor) ExecuteSyncedUpdate(action *synctypes.Action) synctypes.Outcome {
	o := synctypes.Outcome{
		Action:   synctypes.ActionUpdateSynced,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: synctypes.ItemTypeFile,
	}

	if action.View != nil {
		if action.View.Remote != nil {
			o.RemoteHash = action.View.Remote.Hash
			o.RemoteSize = action.View.Remote.Size
			o.RemoteSizeKnown = true
			o.RemoteMtime = action.View.Remote.Mtime
			o.ETag = action.View.Remote.ETag
			o.ParentID = action.View.Remote.ParentID
			o.ItemType = action.View.Remote.ItemType
		}

		if action.View.Local != nil {
			o.LocalHash = action.View.Local.Hash
			o.LocalSize = action.View.Local.Size
			o.LocalSizeKnown = true
			o.LocalMtime = action.View.Local.Mtime
		}

		fillOutcomeFromBaseline(&o, action.View.Baseline)

		// Fall back to baseline ItemType when Remote is absent or had zero value.
		if o.ItemType == synctypes.ItemTypeFile && action.View.Baseline != nil && action.View.Baseline.ItemType != synctypes.ItemTypeFile {
			o.ItemType = action.View.Baseline.ItemType
		}
	}

	return o
}

// ExecuteCleanup signals baseline removal without I/O.
func (e *Executor) ExecuteCleanup(action *synctypes.Action) synctypes.Outcome {
	return synctypes.Outcome{
		Action:   synctypes.ActionCleanup,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: resolveActionItemType(action),
	}
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
		return "", fmt.Errorf("%w: %q", synctypes.ErrPathEscapesSyncRoot, relPath)
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
			synctypes.ErrPathEscapesSyncRoot, resolvedParent, resolvedRoot)
	}

	return absPath, nil
}

func normalizeSyncTreePathError(err error) error {
	if err == nil {
		return nil
	}

	if strings.Contains(err.Error(), "escapes root") || strings.Contains(err.Error(), "must not be absolute") {
		return errors.Join(synctypes.ErrPathEscapesSyncRoot, err)
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

// resolveActionItemType extracts ItemType from the action's View, skipping
// zero values (ItemTypeFile) to find the actual type. Checks Remote → Baseline
// → Local, defaulting to ItemTypeFile if all are zero or View is nil.
func resolveActionItemType(action *synctypes.Action) synctypes.ItemType {
	if action.View != nil {
		if action.View.Remote != nil && action.View.Remote.ItemType != synctypes.ItemTypeFile {
			return action.View.Remote.ItemType
		}

		if action.View.Baseline != nil && action.View.Baseline.ItemType != synctypes.ItemTypeFile {
			return action.View.Baseline.ItemType
		}

		if action.View.Local != nil && action.View.Local.ItemType != synctypes.ItemTypeFile {
			return action.View.Local.ItemType
		}
	}

	return synctypes.ItemTypeFile
}

// ResolveParentID determines the remote parent ID for a given relative path.
// Checks baseline (which includes items committed by earlier DAG actions),
// then falls back to "root" for top-level items.
func (e *Executor) ResolveParentID(relPath string) (string, error) {
	parentDir := filepath.Dir(relPath)

	// Top-level item: parent is root.
	if parentDir == "." || parentDir == "" {
		if e.rootItemID != "" {
			return e.rootItemID, nil
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
// items under shortcut subtrees still arrive with a zero action DriveID, so
// inherit the parent folder's baseline drive before falling back to the
// executor's configured default drive.
func (e *Executor) resolveDriveID(action *synctypes.Action) driveid.ID {
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

// failedOutcome builds an Outcome for a failed action.
func (e *Executor) failedOutcome(action *synctypes.Action, actionType synctypes.ActionType, err error) synctypes.Outcome {
	e.logger.Warn("action failed",
		slog.String("action", actionType.String()),
		slog.String("path", action.Path),
		slog.String("error", err.Error()),
	)

	return synctypes.Outcome{
		Action:  actionType,
		Success: false,
		Error:   err,
		Path:    action.Path,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
	}
}

// folderOutcome builds a successful Outcome for a local folder create.
func (e *Executor) folderOutcome(action *synctypes.Action) synctypes.Outcome {
	o := synctypes.Outcome{
		Action:   synctypes.ActionFolderCreate,
		Success:  true,
		Path:     action.Path,
		DriveID:  e.resolveDriveID(action),
		ItemID:   action.ItemID,
		ItemType: synctypes.ItemTypeFolder,
	}

	if action.View != nil && action.View.Remote != nil {
		o.ItemID = action.View.Remote.ItemID
		o.ParentID = action.View.Remote.ParentID
		o.ETag = action.View.Remote.ETag
	}

	return o
}

// moveOutcome builds a successful Outcome for a move action.
func (e *Executor) moveOutcome(action *synctypes.Action) synctypes.Outcome {
	o := synctypes.Outcome{
		Action:  action.Type,
		Success: true,
		Path:    action.Path,
		OldPath: action.OldPath,
		DriveID: e.resolveDriveID(action),
		ItemID:  action.ItemID,
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
		}

		fillOutcomeFromBaseline(&o, action.View.Baseline)
	}

	return o
}

func fillOutcomeFromBaseline(o *synctypes.Outcome, baseline *synctypes.BaselineEntry) {
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
