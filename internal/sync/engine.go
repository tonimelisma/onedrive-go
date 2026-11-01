package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

type driveIdentityProof struct {
	attempted bool
	drive     *graph.Drive
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single mounted content root only; multi-mount orchestration is handled by
// internal/multisync.
type Engine struct {
	baseline        *SyncStore
	planner         *planner
	execCfg         *executorConfig
	fetcher         deltaFetcher
	socketIOFetcher socketIOEndpointFetcher
	itemsClient     itemClient
	driveVerifier   driveVerifier      // optional (B-074)
	folderDelta     folderDeltaFetcher // optional: mount-root delta observation
	recursiveLister recursiveLister    // optional: mount-root recursive enumeration fallback
	permHandler     *permissionHandler // encapsulates all permission logic (6.4c)
	dataDir         string
	syncRoot        string
	syncTree        *synctree.Root
	// rootLock holds the sync root for the engine's lifetime (R-2.9.5).
	rootLock *syncRootLock
	// syncRootIdentity is the one owner of the identity the sync folder must
	// match; the executor and every observer read it, never a copy.
	syncRootIdentity       *syncRootIdentity
	driveID                driveid.ID
	driveType              string
	remoteRootItemID       string
	remoteRootDeltaCapable bool
	logger                 *slog.Logger
	perfCollector          *perf.Collector
	sessionStore           *driveops.SessionStore // for CleanStale() housekeeping
	trash                  *localtrash.Trash      // local deletes and replaced files (P-4)
	// trashPurgedAt is when housekeeping last purged expired trash entries;
	// watch maintenance purges at most once per trashPurgeInterval.
	trashPurgedAt   atomic.Int64
	transferWorkers int // goroutine count for the worker pool
	checkWorkers    int // goroutine limit for parallel file hashing
	contentFilter   ContentFilterConfig
	// protectedRootsMu guards protectedRoots. The loop goroutine reads them
	// each time it builds an observer; refreshProtectedRootsFromStore rewrites
	// them from the full-refresh goroutine and from the remote observer's
	// expired-delta recovery, which run beside the loop. Read through
	// currentProtectedRoots, write through setProtectedRoots (R-6.3.4).
	protectedRootsMu stdsync.Mutex
	protectedRoots   []ProtectedRoot

	localRules            LocalObservationRules
	shortcutNamespaceID   string
	shortcutChildWorkSink ShortcutChildWorkSink
	enableWebsocket       bool
	minFreeSpace          int64 // startup disk-scope revalidation threshold
	diskAvailableFn       func(string) (uint64, error)

	// Test/debug-only invariant checks. Production keeps this disabled;
	// tests enable it to catch lifecycle and scope regressions immediately.
	assertInvariants bool
	debugEventHook   func(DebugEvent)
	activityHook     func(Activity)

	// dispatchOrder reorders every ready frontier before it is dispatched
	// (D-090). Tests run scenarios forward, reversed or in a seeded shuffle
	// so a bug that depends on dispatch order cannot pass by timing; nil, as
	// in production, keeps the dependency graph's order.
	dispatchOrder func([]*trackedAction) []*trackedAction
	// serializeWorkers runs one worker instead of the pool, so with a
	// dispatch order execution is deterministic. Test-only.
	serializeWorkers bool
	// oneShotPassLimit, when positive, lowers the passes one RunOnce makes
	// below oneShotMaxPasses. Test-only: a test that inspects what a single
	// pass committed sets it to 1.
	oneShotPassLimit int

	// localWatcherFactory overrides the default fsnotify watcher factory
	// for the local observer. Tests inject a mock factory to simulate
	// inotify watch limit exhaustion (ENOSPC).
	localWatcherFactory func() (fsWatcher, error)

	// socketIOWakeSourceFactory is a test seam for watch-mode websocket
	// wakeups. Production uses NewSocketIOWakeSourceWithOptions.
	socketIOWakeSourceFactory func(
		socketIOEndpointFetcher,
		driveid.ID,
		socketIOWakeSourceOptions,
	) socketIOWakeSourceRunner

	// clock is the engine's single time capability: now, delayed callbacks,
	// tickers, sleeps, and retry jitter. See syncClock for why these are one
	// value rather than five injectable fields.
	clock     syncClock
	nextRunID atomic.Int64

	// mountRootDeltaFallbackReported is set once the engine has reported, at
	// Info, that folder delta is unavailable for its shared-folder root and
	// observation is listing the folder instead. It clears when folder delta
	// succeeds again, so a later fallback is reported once more. Atomic
	// because one-shot and watch observation can run on different goroutines.
	mountRootDeltaFallbackReported atomic.Bool

	// protectedRootRefreshFailing and trashPurgeFailing keep two housekeeping
	// failures to one Warn per failure streak (R-6.6.30): a protected-root
	// refresh that keeps failing, and a trash purge that keeps failing. Each
	// clears when its operation succeeds again, so the next streak is
	// reported once more. Atomic for the same reason as the flag above.
	protectedRootRefreshFailing atomic.Bool
	trashPurgeFailing           atomic.Bool
}

// newEngine creates an Engine and opens the per-mount SyncStore. Existing
// unreadable, incompatible, or unsupported state DBs fail with a typed
// store-incompatible error so higher layers can guide the user without mutating
// durable state during startup. Returns an error if DB init fails or if
// DriveID is zero.
func newEngine(ctx context.Context, cfg *engineInputs) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: engine requires non-zero drive ID")
	}

	if !syncRootExists(cfg.SyncRoot) {
		return nil, fmt.Errorf("%w: %s is missing or is not a folder", ErrMountRootUnavailable, cfg.SyncRoot)
	}

	// The root lock comes first and goes last: a second engine is refused
	// before it opens (and possibly migrates or checkpoints) the store, and
	// the store is closed before the next owner can start. Deferred cleanups
	// run in reverse, so the store's startup-failure close runs before this
	// release.
	rootLock, err := acquireSyncRootLock(syncRootLockDataDir(cfg), cfg.SyncRoot, cfg.Logger, cfg.rootLockFlock)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	storeOwnedByEngine := false
	defer releaseSyncRootLockOnStartupFailure(cfg.Logger, rootLock, &storeOwnedByEngine)

	bm, err := openEngineSyncStore(ctx, cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}
	defer closeEngineSyncStoreOnStartupFailure(ctx, cfg.Logger, bm, &storeOwnedByEngine)()
	syncTree, syncRootBinding, err := openBoundSyncTree(ctx, bm, cfg)
	if err != nil {
		return nil, err
	}

	execCfg := newExecutorConfig(
		cfg.Items,
		cfg.Downloads,
		cfg.Uploads,
		syncTree,
		cfg.DriveID,
		cfg.Logger,
		cfg.PathConvergence,
	)
	rootIdentity := newSyncRootIdentity(syncRootBinding, syncRootRefusalHint(cfg))
	trash := configureEngineExecutor(execCfg, cfg, bm, rootIdentity)

	// Construct sessionStore and TransferManager together so the TM is
	// immutable after creation (no post-hoc field mutation). Disk space
	// checking is configured via WithDiskCheck so every download (sync
	// and CLI) gets automatic protection (R-6.2.6).
	var sessionStore *driveops.SessionStore
	if cfg.DataDir != "" {
		sessionStore = driveops.NewSessionStore(cfg.DataDir, cfg.Logger)
	}

	execCfg.SetTransferMgr(driveops.NewTransferManager(cfg.Downloads, cfg.Uploads, sessionStore, cfg.Logger,
		driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable),
		driveops.WithSessionScope(cfg.MountID, cfg.SyncRoot),
	))

	e := &Engine{
		baseline:               bm,
		planner:                newPlanner(cfg.Logger),
		execCfg:                execCfg,
		fetcher:                cfg.Fetcher,
		socketIOFetcher:        cfg.SocketIOFetcher,
		itemsClient:            cfg.Items,
		driveVerifier:          cfg.DriveVerifier,
		folderDelta:            cfg.FolderDelta,
		recursiveLister:        cfg.RecursiveLister,
		sessionStore:           sessionStore,
		trash:                  trash,
		dataDir:                cfg.DataDir,
		syncRoot:               cfg.SyncRoot,
		syncTree:               syncTree,
		syncRootIdentity:       rootIdentity,
		rootLock:               rootLock,
		driveID:                cfg.DriveID,
		driveType:              cfg.DriveType,
		remoteRootItemID:       cfg.RemoteRootItemID,
		remoteRootDeltaCapable: cfg.RemoteRootDeltaCapable,
		logger:                 cfg.Logger,
		perfCollector:          cfg.PerfCollector,
		transferWorkers:        cfg.TransferWorkers,
		checkWorkers:           cfg.CheckWorkers,
		contentFilter:          cfg.ContentFilter,
		localRules:             cfg.LocalRules,
		shortcutNamespaceID:    cfg.ShortcutNamespaceID,
		shortcutChildWorkSink:  cfg.ShortcutChildWorkSink,
		enableWebsocket:        cfg.EnableWebsocket,
		minFreeSpace:           cfg.MinFreeSpace,
		diskAvailableFn:        driveops.DiskAvailable,
		clock:                  realClock{},
		socketIOWakeSourceFactory: func(
			fetcher socketIOEndpointFetcher,
			driveID driveid.ID,
			opts socketIOWakeSourceOptions,
		) socketIOWakeSourceRunner {
			return newSocketIOWakeSourceWithOptions(fetcher, driveID, opts)
		},
	}

	if err := e.refreshProtectedRootsFromStore(ctx); err != nil {
		return nil, fmt.Errorf("sync: loading parent shortcut protected roots: %w", err)
	}

	e.permHandler = newEnginePermissionHandler(e, cfg, syncTree)
	// One clock: the store's retry and trial schedule and the executor's
	// timestamps read the engine clock, or a retry scheduled by one clock is
	// judged due by another.
	bm.setNowFunc(e.nowFunc)
	execCfg.SetNowFunc(e.nowFunc)

	storeOwnedByEngine = true
	return e, nil
}

func newEnginePermissionHandler(
	engine *Engine,
	cfg *engineInputs,
	syncTree *synctree.Root,
) *permissionHandler {
	return &permissionHandler{
		store:            engine.baseline,
		permChecker:      cfg.PermChecker,
		syncTree:         syncTree,
		driveID:          cfg.DriveID,
		accountEmail:     cfg.AccountEmail,
		remoteRootItemID: cfg.RemoteRootItemID,
		logger:           cfg.Logger,
		nowFn:            engine.nowFunc,
	}
}

func closeEngineSyncStoreOnStartupFailure(
	ctx context.Context,
	logger *slog.Logger,
	store *SyncStore,
	storeOwnedByEngine *bool,
) func() {
	return func() {
		if storeOwnedByEngine != nil && *storeOwnedByEngine {
			return
		}
		if closeErr := store.Close(context.WithoutCancel(ctx)); closeErr != nil && logger != nil {
			logger.Debug("could not close sync store after engine startup failure",
				slog.Any("error", closeErr),
			)
		}
	}
}

// syncRootRefusalHint is the rebind hint for a primary root; a shortcut
// child's identity is the parent's to change, so it gets none.
func syncRootRefusalHint(cfg *engineInputs) string {
	if cfg.ExpectedSyncRootIdentity != nil {
		return ""
	}
	return errSyncRootRebindHint
}

// configureEngineExecutor installs the mount-owned executor policy: the remote
// root, pending-create store, local trash (P-4), content filter and the sync
// folder identity remote deletes re-check (D-011). It returns the trash so the
// engine's housekeeping purges the same one.
func configureEngineExecutor(
	execCfg *executorConfig,
	cfg *engineInputs,
	store *SyncStore,
	rootIdentity *syncRootIdentity,
) *localtrash.Trash {
	execCfg.SetRemoteRootItemID(cfg.RemoteRootItemID)
	execCfg.SetPendingCreates(store)
	trash := localtrash.Open(localtrash.DirForStatePath(cfg.DBPath), nil)
	execCfg.SetTrash(trash)
	execCfg.SetContentFilter(cfg.ContentFilter)
	execCfg.SetSyncRootIdentity(rootIdentity)
	return trash
}

// openBoundSyncTree opens the sync tree and resolves the folder identity
// every scan of it is validated against (D-001).
func openBoundSyncTree(ctx context.Context, store *SyncStore, cfg *engineInputs) (*synctree.Root, resolvedSyncRoot, error) {
	syncTree, err := openSyncTree(cfg.SyncRoot)
	if err != nil {
		return nil, resolvedSyncRoot{}, fmt.Errorf("sync: opening sync tree: %w", err)
	}
	if cfg.interposeSyncTree != nil {
		syncTree = syncTree.Interpose(cfg.interposeSyncTree)
	}
	warnAboutForeignSyncClients(cfg.SyncRoot, cfg.Logger)
	binding, err := resolveExpectedSyncRoot(ctx, store, syncTree, cfg.ExpectedSyncRootIdentity)
	if err != nil {
		return nil, resolvedSyncRoot{}, err
	}
	return syncTree, binding, nil
}

func openSyncTree(syncRoot string) (*synctree.Root, error) {
	resolved, err := resolveSyncRootPath(syncRoot)
	if err != nil {
		return nil, err
	}
	syncTree, err := synctree.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("opening sync tree: %w", err)
	}
	return syncTree, nil
}

// commitLocalSnapshot replaces local_state and, while a primary engine still
// has to bind its sync folder, records the folder's identity in the same
// transaction. Binding first writes a fresh token on the folder (an extended
// attribute) so later starts recognize it by name rather than by inode; a
// filesystem without extended attributes binds on inode and fsid alone.
// Mark-and-bind is serialized: two commits racing would otherwise write two
// tokens, and the folder would keep the one the store did not record.
func (e *Engine) commitLocalSnapshot(ctx context.Context, rows []localStateRow, crossedMounts []string) error {
	identity := e.syncRootIdentity
	if identity == nil || !identity.bindPending.Load() {
		if _, err := e.baseline.ReplaceLocalStateBindingRoot(ctx, rows, nil, crossedMounts); err != nil {
			return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
		}
		return nil
	}

	identity.bindMu.Lock()
	defer identity.bindMu.Unlock()
	if !identity.bindPending.Load() {
		if _, err := e.baseline.ReplaceLocalStateBindingRoot(ctx, rows, nil, crossedMounts); err != nil {
			return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
		}
		return nil
	}

	expected := identity.Expected()
	if expected == nil {
		return fmt.Errorf("sync: binding sync folder: no identity was taken at startup")
	}
	bind := *expected
	bind.Token = ""
	token, err := e.syncTree.MarkRoot()
	switch {
	case err == nil:
		bind.Token = token
	case errors.Is(err, synctree.ErrRootMarkUnsupported):
		e.logger.Debug("sync folder cannot carry a bind token; binding on inode and filesystem id",
			slog.Any("error", err))
	default:
		return fmt.Errorf("sync: binding sync folder: %w", err)
	}

	bound, err := e.baseline.ReplaceLocalStateBindingRoot(ctx, rows, &bind, crossedMounts)
	if err != nil {
		return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}
	if bound != nil && bound.Token != bind.Token && bound.Token != "" {
		// Another binding won in the store; the folder must carry the token
		// the store recorded, not the one just written.
		if markErr := e.syncTree.SetRootMark(bound.Token); markErr != nil {
			return fmt.Errorf("sync: restoring the recorded sync folder marker: %w", markErr)
		}
	}
	if bound == nil {
		bound = &bind
	}
	identity.current.Store(bound)
	identity.bindPending.Store(false)
	return nil
}

func (e *Engine) collector() *perf.Collector {
	if e == nil {
		return nil
	}

	return e.perfCollector
}

func (e *Engine) nextRuntimeRunID() string {
	return fmt.Sprintf("run-%d", e.nextRunID.Add(1))
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, and
// closes the database connection last. Safe to call more than once.
func (e *Engine) Close(ctx context.Context) error {
	// Clean stale upload session files (best-effort).
	if e.sessionStore != nil {
		if n, err := e.sessionStore.CleanStale(driveops.StaleSessionAge); err != nil {
			e.logger.Debug("failed to clean stale sessions on close",
				slog.Any("error", err),
			)
		} else if n > 0 {
			e.logger.Info("cleaned stale upload sessions on close",
				slog.Int("deleted", n),
			)
		}
	}

	// The folder stays locked until the state store is closed, so the next
	// owner never starts against a store this engine is still writing.
	defer func() {
		if releaseErr := e.rootLock.release(); releaseErr != nil {
			e.logger.Debug("release sync folder lock", slog.Any("error", releaseErr))
		}
	}()

	if e.baseline == nil {
		return nil
	}

	if err := e.baseline.Close(ctx); err != nil {
		return fmt.Errorf("sync: closing state store: %w", err)
	}

	return nil
}

// proveDriveIdentity performs the single startup proof call used by one-shot
// and watch-mode bootstrap. The proof remains ephemeral; durable account-auth
// state is owned by the managed catalog.
func (e *Engine) proveDriveIdentity(ctx context.Context) (driveIdentityProof, error) {
	if e.driveVerifier == nil {
		return driveIdentityProof{}, nil
	}

	drive, err := e.driveVerifier.Drive(ctx, e.driveID)
	if err != nil {
		return driveIdentityProof{attempted: true}, fmt.Errorf("sync: verifying drive identity: %w", err)
	}

	if drive.ID != e.driveID {
		return driveIdentityProof{attempted: true, drive: drive}, fmt.Errorf(
			"sync: this sync folder is set up for a different OneDrive drive than the account now returns "+
				"(expected drive %s, got %s); check the drive with onedrive-go drive list", e.driveID, drive.ID)
	}

	return driveIdentityProof{attempted: true, drive: drive}, nil
}

func (e *Engine) logVerifiedDrive(proof driveIdentityProof) {
	if !proof.attempted || proof.drive == nil {
		return
	}

	e.logger.Info("drive identity verified",
		slog.String("drive_id", proof.drive.ID.String()),
		slog.String("drive_type", proof.drive.DriveType),
	)
}

func (e *Engine) hasPersistedAccountAuthRequirement() (bool, error) {
	if e == nil || e.permHandler == nil || e.permHandler.accountEmail == "" {
		return false, nil
	}

	stored, err := config.LoadCatalogForDataDir(e.dataDir)
	if err != nil {
		return false, fmt.Errorf("sync: loading catalog: %w", err)
	}

	accountEntry, found := stored.AccountByEmail(e.permHandler.accountEmail)
	if !found {
		return false, nil
	}

	return accountEntry.AuthRequirementReason == authstate.ReasonSyncAuthRejected, nil
}

func (e *Engine) normalizePersistedAccountAuthRequirement(
	required bool,
	proof driveIdentityProof,
	proofErr error,
) error {
	if !required {
		return nil
	}
	if e == nil || e.permHandler == nil || e.permHandler.accountEmail == "" {
		return nil
	}
	if !proof.attempted {
		return fmt.Errorf("sync: revalidating catalog auth requirement: drive verifier required")
	}
	if proofErr != nil {
		return proofErr
	}
	if err := config.ClearAccountAuthRequirement(e.dataDir, e.permHandler.accountEmail); err != nil {
		return fmt.Errorf("sync: clearing catalog auth requirement: %w", err)
	}

	e.logger.Info("released catalog auth requirement after successful startup proof",
		slog.String("account", e.permHandler.accountEmail),
		slog.String("source", string(config.AuthClearSourceSyncStartupProof)),
	)

	return nil
}

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote truth
//  3. Observe local truth
//  4. Derive SQL comparison and reconciliation from current snapshots
//  5. Build the current actionable set in Go
//  6. Return early if dry-run
//  7. Build DepGraph, start worker pool
//  8. Wait for completion, commit delta token
