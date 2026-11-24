package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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
	baseline        *syncstore.SyncStore
	planner         *syncplan.Planner
	execCfg         *syncexec.ExecutorConfig
	fetcher         DeltaFetcher
	socketIOFetcher SocketIOEndpointFetcher
	itemsClient     ItemClient
	driveVerifier   DriveVerifier      // optional (B-074)
	folderDelta     FolderDeltaFetcher // optional: mount-root delta observation
	recursiveLister RecursiveLister    // optional: mount-root recursive enumeration fallback
	permHandler     *permissionHandler // encapsulates all permission logic (6.4c)
	dataDir         string
	syncRoot        string
	syncTree        *synctree.Root
	// locks hold the sync root and the state store for the engine's lifetime
	// (R-2.9.5).
	locks engineLocks
	// syncRootIdentity is the one owner of the identity the sync folder must
	// match; the executor and every observer read it, never a copy.
	syncRootIdentity       *syncdomain.SyncRootIdentity
	driveID                driveid.ID
	driveType              string
	remoteRootItemID       string
	remoteRootDeltaCapable bool
	logger                 *slog.Logger
	perfCollector          *perf.Collector
	transferWorkers        int // goroutine count for the worker pool
	checkWorkers           int // goroutine limit for parallel file hashing
	contentFilter          syncdomain.ContentFilterConfig
	// protectedRoots are the shortcut child roots below this mount, which
	// observation and housekeeping leave alone; the set guards itself.
	protectedRoots *protectedRootSet
	// upkeep runs the cleanup after a sync pass and the watch's hourly trash
	// purge, and keeps their failure streaks.
	upkeep *housekeeper

	// accountCanonicalID names the catalog record of the account the engine
	// syncs with, the one whose sign-in requirement a 401 marks and the
	// startup proof clears; zero leaves the catalog alone.
	accountCanonicalID driveid.CanonicalID
	// runOwnerCheck is the current run's owner check, nil outside a run and
	// in production. Written by beginRun on the run's goroutine.
	runOwnerCheck func()

	localRules            syncdomain.LocalObservationRules
	shortcutNamespaceID   string
	shortcutChildWorkSink syncshortcut.ShortcutChildWorkSink
	enableWebsocket       bool

	debugEventHook func(syncdomain.DebugEvent)
	activityHook   func(Activity)

	// hooks are the engine's test seams; the zero value is production.
	hooks engineHooks

	// wallClock is the engine's real clock: the run clock (runClock) is it
	// unless a test replaced it, and every observer the engine builds runs on it, except
	// that a pass's full local scan, which runs no timer, measures its
	// racily-clean window on the run's clock (observeLocal). Its timer panics
	// carry a timer callback's panic to the run it ends (beginRun).
	wallClock syncdomain.RealClock
	// nextRunID numbers the engine's runs. One engine runs one RunOnce or
	// RunWatch at a time, and that run's goroutine alone takes the next.
	nextRunID int64

	// mountRootDeltaFallbackReported is set once the engine has reported, at
	// Info, that folder delta is unavailable for its shared-folder root and
	// observation is listing the folder instead. It clears when folder delta
	// succeeds again, so a later fallback is reported once more. Atomic
	// because one-shot and watch observation can run on different goroutines.
	mountRootDeltaFallbackReported atomic.Bool

	// releasedRootJunkFailing and releaseRefusalFailing keep the same one Warn
	// per streak for a removed shared folder's release that cannot remove the
	// junk left in its emptied sync folder (emptyReleasedRoot) or a synced
	// entry its local delete refuses (reportReleaseRefusals), which every
	// later drain meets again. These two are also written from the multisync
	// orchestrator's goroutine, whose final-drain acknowledgement runs the
	// release through the ack handle.
	releasedRootJunkFailing atomic.Bool
	releaseRefusalFailing   atomic.Bool

	// heldDeleteConfirms carries a user's confirmation of held deletes from
	// any goroutine (ConfirmHeldDeletes) to the engine's running watch loop,
	// which alone receives on it, and only while running steadily. Unbuffered:
	// a confirmation is taken by a loop or by nobody.
	heldDeleteConfirms chan heldDeleteConfirmRequest
}

// newEngine creates an Engine and opens the per-mount SyncStore. Existing
// unreadable, incompatible, or unsupported state DBs fail with a typed
// store-incompatible error so higher layers can guide the user without mutating
// durable state during startup. Returns an error if DB init fails or if
// DriveID is zero.
func newEngine(ctx context.Context, cfg *engineInputs) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, errors.New("sync: engine requires non-zero drive ID")
	}

	if !syncobserve.SyncRootExists(cfg.SyncRoot) {
		return nil, fmt.Errorf("%w: %s is missing or is not a folder", syncdomain.ErrMountRootUnavailable, cfg.SyncRoot)
	}

	// The folder and store locks come first and go last (engine_locks.go): a
	// second engine, or a store an unfinished account email change moves, is
	// refused before it opens (and possibly migrates or checkpoints) the
	// store, and the store is closed before the next owner can start.
	// Deferred cleanups run in reverse, so the store's startup-failure close
	// runs before this release.
	locks, err := acquireEngineLocks(cfg)
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}
	storeOwnedByEngine := false
	defer releaseEngineLocksOnStartupFailure(cfg.Logger, locks, &storeOwnedByEngine)

	// The engine is declared before its store opens, so the store runs on
	// the engine's run clock from its first read: one clock, or a retry
	// scheduled by one clock is judged due by another. The clock is read
	// late (the method value reads the engine's hooks at each call), so a
	// test that replaces the run clock after construction moves the store
	// with it. The store, the sync tree, its identity and the protected roots
	// are filled in below, before the engine leaves this function.
	e := &Engine{
		planner:                syncplan.NewPlanner(cfg.Logger),
		fetcher:                cfg.Fetcher,
		socketIOFetcher:        cfg.SocketIOFetcher,
		itemsClient:            cfg.Items,
		driveVerifier:          cfg.DriveVerifier,
		folderDelta:            cfg.FolderDelta,
		recursiveLister:        cfg.RecursiveLister,
		accountCanonicalID:     cfg.AccountCanonicalID,
		dataDir:                cfg.DataDir,
		syncRoot:               cfg.SyncRoot,
		locks:                  locks,
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
		wallClock:              syncdomain.NewRealClock(cfg.Logger, &syncdomain.TimerPanics{}),
		hooks:                  cfg.hooks,
		heldDeleteConfirms:     make(chan heldDeleteConfirmRequest),
	}

	bm, err := syncstore.OpenEngineSyncStore(ctx, cfg.DBPath, cfg.Logger, e.nowFunc, cfg.hooks.store)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}
	defer closeEngineSyncStoreOnStartupFailure(ctx, cfg.Logger, bm, &storeOwnedByEngine)()
	syncTree, syncRootBinding, err := openBoundSyncTree(ctx, bm, cfg)
	if err != nil {
		return nil, err
	}
	rootIdentity := syncdomain.NewSyncRootIdentity(syncRootBinding, syncRootRefusalHint(cfg))
	e.baseline = bm
	e.syncTree = syncTree
	e.syncRootIdentity = rootIdentity
	e.protectedRoots = newProtectedRootSet(bm, cfg.ShortcutNamespaceID)

	trashDir := cfg.TrashDir
	if trashDir == "" {
		trashDir = localtrash.DirForStatePath(cfg.DBPath)
	}
	trash := localtrash.Open(trashDir, nil)

	// The session store is built here and the TransferManager below, on the
	// engine's run clock; the TM is immutable after creation (no post-hoc
	// field mutation). Disk space checking is configured via WithDiskCheck so
	// every download (sync and CLI) gets automatic protection (R-6.2.6).
	var sessionStore *driveops.SessionStore
	if cfg.DataDir != "" {
		sessionStore = driveops.NewSessionStore(cfg.DataDir, cfg.Logger)
	}
	e.upkeep = &housekeeper{
		syncTree:      syncTree,
		sessionStore:  sessionStore,
		trash:         trash,
		contentFilter: cfg.ContentFilter,
		roots:         e.protectedRoots,
		logger:        cfg.Logger,
		now:           e.nowFunc,
	}

	if err := e.protectedRoots.refresh(ctx); err != nil {
		return nil, fmt.Errorf("sync: loading parent shortcut protected roots: %w", err)
	}

	e.permHandler = newEnginePermissionHandler(e, cfg, syncTree)
	transfers := driveops.NewTransferManager(cfg.Downloads, cfg.Uploads, sessionStore, cfg.Logger,
		driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable),
		driveops.WithSessionScope(cfg.MountID, cfg.SyncRoot),
		driveops.WithClock(e.nowFunc),
	)
	execCfg, execErr := newEngineExecutorConfig(cfg, syncTree, transfers, trash, rootIdentity, bm, e.nowFunc)
	if execErr != nil {
		return nil, execErr
	}
	e.execCfg = execCfg

	storeOwnedByEngine = true
	return e, nil
}

// newEngineExecutorConfig builds the engine's executor configuration: the
// mount's clients and remote root, the sync tree and its folder identity
// that remote deletes re-check (D-011), the transfer manager, the local
// trash (R-6.4.9), the store as the pending-create record (R-5.6.15) and the
// engine's run clock.
func newEngineExecutorConfig(
	cfg *engineInputs,
	syncTree *synctree.Root,
	transfers *driveops.TransferManager,
	trash *localtrash.Trash,
	rootIdentity *syncdomain.SyncRootIdentity,
	store *syncstore.SyncStore,
	now func() time.Time,
) (*syncexec.ExecutorConfig, error) {
	execCfg, err := syncexec.NewExecutorConfig(&syncexec.ExecutorInputs{
		Items:            cfg.Items,
		Downloads:        cfg.Downloads,
		Uploads:          cfg.Uploads,
		Transfers:        transfers,
		SyncTree:         syncTree,
		DriveID:          cfg.DriveID,
		RemoteRootItemID: cfg.RemoteRootItemID,
		Logger:           cfg.Logger,
		ContentFilter:    cfg.ContentFilter,
		SyncRootIdentity: rootIdentity,
		PathConvergence:  cfg.PathConvergence,
		Trash:            trash,
		PendingCreates:   store,
		MoveIntents:      store,
		Now:              now,
		Hooks:            cfg.hooks.exec,
	})
	if err != nil {
		return nil, fmt.Errorf("sync: %w", err)
	}

	return execCfg, nil
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
		readOnlyDir:      cfg.hooks.readOnlyDirProbe(),
	}
}

func closeEngineSyncStoreOnStartupFailure(
	ctx context.Context,
	logger *slog.Logger,
	store *syncstore.SyncStore,
	storeOwnedByEngine *bool,
) func() {
	return func() {
		if storeOwnedByEngine != nil && *storeOwnedByEngine {
			return
		}
		if closeErr := store.Close(ctx); closeErr != nil && logger != nil {
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

// openBoundSyncTree opens the sync tree and resolves the folder identity
// every scan of it is validated against (D-001).
func openBoundSyncTree(
	ctx context.Context, store *syncstore.SyncStore, cfg *engineInputs,
) (*synctree.Root, syncdomain.ResolvedSyncRoot, error) {
	syncTree, err := openSyncTree(cfg.SyncRoot)
	if err != nil {
		return nil, syncdomain.ResolvedSyncRoot{}, fmt.Errorf("sync: opening sync tree: %w", err)
	}
	if cfg.hooks.interposeSyncTree != nil {
		syncTree = syncTree.Interpose(cfg.hooks.interposeSyncTree)
	}
	if cfg.hooks.adaptSyncTree != nil {
		syncTree = cfg.hooks.adaptSyncTree(syncTree)
	}
	warnAboutForeignSyncClients(cfg.SyncRoot, cfg.Logger)
	binding, err := resolveExpectedSyncRoot(ctx, store, syncTree, cfg.ExpectedSyncRootIdentity)
	if err != nil {
		return nil, syncdomain.ResolvedSyncRoot{}, err
	}
	warnAboutPutBackWindow(syncTree, cfg.Logger)
	return syncTree, binding, nil
}

// putBackWindowWarning is the start-up warning of a sync folder on which a
// put-back can replace a save.
const putBackWindowWarning = "This sync folder's filesystem has neither hard links nor a no-replace rename " +
	"(a FAT-family filesystem): a file saved at the very moment sync puts a file it had set aside back " +
	"under the same name can be replaced"

// warnAboutPutBackWindow warns once, as the engine starts, when the sync
// folder's filesystem offers neither hard links nor the no-replace rename
// flag (synctree.Root.ProbeNoReplaceSupport). There a set-aside file is put
// back, and a rename that must not replace is made, by a check that the name
// is free and a plain rename, and a save landing between the two is replaced:
// a known limit (sync-execution.md, R-6.4.8), the only alternative being to
// leave the file under a hidden name the next sync would read as a delete. A
// probe that fails decides nothing and is only logged at Debug: the
// operations themselves still fall back as they must.
func warnAboutPutBackWindow(syncTree *synctree.Root, logger *slog.Logger) {
	if logger == nil {
		return
	}
	support, err := syncTree.ProbeNoReplaceSupport()
	if err != nil {
		logger.Debug("could not probe the sync folder's no-replace rename",
			slog.String("path", syncTree.Path()), slog.Any("error", err))
		return
	}
	if !support.PutBackCanReplace() {
		return
	}
	logger.Warn(putBackWindowWarning, slog.String("path", syncTree.Path()))
}

func openSyncTree(syncRoot string) (*synctree.Root, error) {
	resolved, err := syncobserve.ResolveSyncRootPath(syncRoot)
	if err != nil {
		return nil, err
	}
	syncTree, err := synctree.Open(resolved)
	if err != nil {
		return nil, fmt.Errorf("opening sync tree: %w", err)
	}
	return syncTree, nil
}

func (e *Engine) collector() *perf.Collector {
	if e == nil {
		return nil
	}

	return e.perfCollector
}

// withOwnCollector puts the engine's perf collector on ctx, so what the HTTP
// transports, the store and the transfers record on the collector their
// context carries counts on this engine's, whatever its caller's context
// carries. RunOnce and RunWatch (through beginRun) and Close call it first. A
// context that already carries the engine's collector is returned as it is,
// so nothing is counted twice; one that carries another collector has it
// replaced, since the engine's collector rolls up to its parent. An engine
// without a collector leaves ctx alone.
func (e *Engine) withOwnCollector(ctx context.Context) context.Context {
	own := e.collector()
	if own == nil || perf.FromContext(ctx) == own {
		return ctx
	}

	return perf.WithCollector(ctx, own)
}

func (e *Engine) nextRuntimeRunID() string {
	e.nextRunID++
	return fmt.Sprintf("run-%d", e.nextRunID)
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, closes the
// database connection, and only then releases the store and folder locks.
// Safe to call more than once.
func (e *Engine) Close(ctx context.Context) error {
	ctx = e.withOwnCollector(ctx)

	// Clean stale upload session files. A failure does not stop the close, but
	// it is reported at Warn with the count removed, as after a sync pass: a
	// session file that cannot be removed would otherwise go unseen.
	if e.upkeep != nil && e.upkeep.sessionStore != nil {
		if n, err := e.upkeep.sessionStore.CleanStale(driveops.StaleSessionAge); err != nil {
			e.logger.Warn("failed to clean stale sessions on close",
				slog.Int("deleted", n),
				slog.Any("error", err),
			)
		} else if n > 0 {
			e.logger.Info("cleaned stale upload sessions on close",
				slog.Int("deleted", n),
			)
		}
	}

	// The folder and the store stay locked until the store is closed, so the
	// next owner never starts against a store this engine is still writing.
	defer func() {
		if releaseErr := e.locks.release(); releaseErr != nil {
			e.logger.Debug("release engine locks", slog.Any("error", releaseErr))
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

// hasPersistedAccountAuthRequirement reports whether the catalog record of
// the engine's own account, found by its canonical ID, holds the requirement a
// sync's 401 left: another account at the same address is not this one.
func (e *Engine) hasPersistedAccountAuthRequirement() (bool, error) {
	if e == nil || e.accountCanonicalID.IsZero() {
		return false, nil
	}

	stored, err := config.LoadCatalogForDataDir(e.dataDir)
	if err != nil {
		return false, fmt.Errorf("sync: loading catalog: %w", err)
	}

	accountEntry, found := stored.AccountByCanonicalID(e.accountCanonicalID)
	if !found {
		return false, nil
	}

	return accountEntry.AuthRequirementReason == authstate.ReasonSyncAuthRejected, nil
}

func (e *Engine) normalizePersistedAccountAuthRequirement(
	ctx context.Context,
	required bool,
	proof driveIdentityProof,
	proofErr error,
	clearRequirement func(ctx context.Context, dataDir string, account driveid.CanonicalID) error,
) error {
	if !required {
		return nil
	}
	if e == nil || e.accountCanonicalID.IsZero() {
		return nil
	}
	if !proof.attempted {
		return errors.New("sync: revalidating catalog auth requirement: drive verifier required")
	}
	if proofErr != nil {
		return proofErr
	}
	// clearRequirement is the run's hook (flow.hooks), nil in production.
	if clearRequirement == nil {
		clearRequirement = config.ClearAccountAuthRequirement
	}
	if err := clearRequirement(ctx, e.dataDir, e.accountCanonicalID); err != nil {
		if !errors.Is(err, config.ErrConfigLocked) {
			return fmt.Errorf("sync: clearing catalog auth requirement: %w", err)
		}
		// Another onedrive-go process holds the catalog, by flock or, where
		// the filesystem cannot flock, by its lock file. Nothing was written: the clear is
		// a best-effort mark, never made without the lock. The proof has
		// passed, so the sync starts; the stored requirement stands until a
		// start that can take the lock clears it.
		e.logger.Warn("Could not clear the stored sign-in requirement; the next start clears it",
			slog.String("account", e.accountCanonicalID.String()),
			slog.Any("error", err),
		)

		return nil
	}

	e.logger.Info("released catalog auth requirement after successful startup proof",
		slog.String("account", e.accountCanonicalID.String()),
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
//  7. Build the dependency graph, start worker pool
//  8. Wait for completion, commit delta token
