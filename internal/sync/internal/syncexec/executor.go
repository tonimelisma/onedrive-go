package syncexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// GraphRootID is the Graph API parent reference for top-level items.
// Distinct from StrRoot in types.go which serializes the ItemTypeRoot enum.
const GraphRootID = "root"

// ItemClient is the Graph item calls the executor makes: the reads that
// resolve a parent's child and the conditional writes an action performs.
type ItemClient interface {
	GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	// GetChildByName and ListChildrenPages look one name up in a folder: the
	// one read first, the listing a page at a time only when the read cannot
	// settle it, so a lookup does not cost every page of a large folder.
	GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	ListChildrenPages(ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc) error
	CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItemIfMatch(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error)
	DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
}

// ExecutorConfig holds the configuration every per-action executor shares.
// NewExecutorConfig builds it whole and no run changes it, except
// parentChecks, which guards itself and is reset for each plan.
type ExecutorConfig struct {
	items            ItemClient
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
	// (R-6.4.9).
	trash *localtrash.Trash

	// pendingCreates records new files whose empty item exists before their
	// content is sent (R-5.6.15).
	pendingCreates PendingCreateStore

	// moveIntents records a local file move before its rename, so an old
	// name the move could not remove is its known leftover (R-2.12.9).
	moveIntents LocalMoveIntentStore

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
// DriveID, Logger, Trash, PendingCreates, MoveIntents and Now are required;
// the rest may be zero.
type ExecutorInputs struct {
	Items     ItemClient
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
	// (R-6.4.9).
	Trash *localtrash.Trash
	// PendingCreates records new files whose empty item exists before their
	// content is sent (R-5.6.15).
	PendingCreates PendingCreateStore
	// MoveIntents records each local file move before its rename
	// (R-2.12.9).
	MoveIntents LocalMoveIntentStore
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
		moveIntents:      in.MoveIntents,
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
		{name: "Trash", missing: in.Trash == nil},
		{name: "PendingCreates", missing: in.PendingCreates == nil},
		{name: "MoveIntents", missing: in.MoveIntents == nil},
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

// DropUploadSessions ends the recorded upload sessions of the uploads of
// relPaths (sync-root relative) that sync gave up, under every absolute path
// an upload of them could have used: the stored path and its spelling on
// disk, where the file still is, or its parent's spelling, where it is gone.
// It returns how many records went. It calls Graph, so it never runs under a
// store transaction.
func (cfg *ExecutorConfig) DropUploadSessions(ctx context.Context, relPaths []string) (int, error) {
	if cfg == nil || cfg.transferMgr == nil || len(relPaths) == 0 {
		return 0, nil
	}
	candidates := make([]string, 0, len(relPaths)*uploadPathSpellings)
	for _, rel := range relPaths {
		for _, spelled := range []string{rel, cfg.syncTree.Spelling(rel), cfg.syncTree.SpellingOfParent(rel)} {
			abs, err := cfg.syncTree.Abs(spelled)
			if err != nil {
				continue
			}
			candidates = append(candidates, abs)
		}
	}

	dropped, err := cfg.transferMgr.DropUploadSessions(ctx, candidates)
	if err != nil {
		return dropped, fmt.Errorf("drop upload sessions: %w", err)
	}

	return dropped, nil
}

// uploadPathSpellings is how many absolute paths DropUploadSessions tries for
// one relative path.
const uploadPathSpellings = 3

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
// (R-5.10.2). It adopts no entry it did not create (synctree.MkdirExact,
// R-2.12.10): an entry another spelling of the name already holds (on a
// volume that folds case or form, or its other Unicode form on one that
// stores names byte for byte, where mkdir would make a second entry of one
// OneDrive name), or a file at
// the name, appeared after the plan observed the path free (a user's folder
// made between plan and dispatch in watch mode, an entry the plan never
// saw), so the create is a stale
// precondition and the path is planned again from what is there.
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

	folder, err := e.syncTree.MkdirExact(folder)
	if errors.Is(err, synctree.ErrTargetExists) {
		return e.failedOutcomeWithFailure(
			action,
			syncdomain.ActionFolderCreate,
			fmt.Errorf("%w: local folder %s is held by an entry the plan did not see: %w",
				syncdomain.ErrActionPreconditionChanged, action.Path, err),
			action.Path,
			syncdomain.PermissionCapabilityLocalWrite,
		)
	}
	if err != nil {
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
// parent holds under the name (childrenHoldingName, classifyCreateConflict).
// A folder of the
// wanted name is the outcome the create wanted (a lost response to our own
// earlier create, or another device creating the same folder): adopt it, as
// observing it would have. A sibling spelled differently that OneDrive treats
// as the same name is a durable name collision; anything else under the name
// (a file, a sibling of another name that only shares the stored form, or
// nothing listed yet) is a stale precondition for the planner to resolve.
// Retried as it is, the create would meet the same 409 every time.
//
// A read or listing that fails is that request's failure, classified by its
// own answer: it proves nothing about the name. Joined to the 409, an outage
// or a throttle would be recorded under the 409's status as a per-item
// refusal.
func (e *Executor) adoptExistingRemoteFolder(
	ctx context.Context, driveID driveid.ID, parentID, name string, conflictErr error,
) (*graph.Item, error) {
	children, err := e.childrenHoldingName(ctx, driveID, parentID, name)
	if err != nil {
		return nil, fmt.Errorf("resolving the refused create of folder %s: %w", name, err)
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
		return fmt.Errorf("%w: %w", errPathEscapesSyncRoot, err)
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

// reachCrashPoint calls the crash hook at point (crash_points.go).
func (cfg *ExecutorConfig) reachCrashPoint(point string) error {
	if cfg == nil || cfg.hooks.CrashAt == nil {
		return nil
	}

	return cfg.hooks.CrashAt(point)
}

// errPathEscapesSyncRoot is returned when a relative path would resolve
// outside the sync root directory (path traversal attack prevention).
var errPathEscapesSyncRoot = errors.New("path leaves the sync folder")
