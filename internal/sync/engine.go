package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
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
	baseline                 *SyncStore
	planner                  *Planner
	execCfg                  *ExecutorConfig
	fetcher                  DeltaFetcher
	socketIOFetcher          SocketIOEndpointFetcher
	itemsClient              ItemClient
	driveVerifier            DriveVerifier      // optional (B-074)
	folderDelta              FolderDeltaFetcher // optional: mount-root delta observation
	recursiveLister          RecursiveLister    // optional: mount-root recursive enumeration fallback
	permHandler              *PermissionHandler // encapsulates all permission logic (6.4c)
	dataDir                  string
	syncRoot                 string
	syncTree                 *synctree.Root
	expectedSyncRootIdentity *synctree.FileIdentity
	driveID                  driveid.ID
	driveType                string
	remoteRootItemID         string
	remoteRootDeltaCapable   bool
	logger                   *slog.Logger
	perfCollector            *perf.Collector
	sessionStore             *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers          int                    // goroutine count for the worker pool
	checkWorkers             int                    // goroutine limit for parallel file hashing
	contentFilter            ContentFilterConfig
	protectedRoots           []ProtectedRoot
	localRules               LocalObservationRules
	shortcutNamespaceID      string
	shortcutChildWorkSink    ShortcutChildWorkSink
	enableWebsocket          bool
	minFreeSpace             int64 // startup disk-scope revalidation threshold
	diskAvailableFn          func(string) (uint64, error)

	// Test/debug-only invariant checks. Production keeps this disabled;
	// tests enable it to catch lifecycle and scope regressions immediately.
	assertInvariants bool
	debugEventHook   func(DebugEvent)

	// nowFn is the engine's clock. Defaults to time.Now. Tests inject a
	// controllable clock for deterministic trial timer and scope timing.
	nowFn func() time.Time

	// localWatcherFactory overrides the default fsnotify watcher factory
	// for the local observer. Tests inject a mock factory to simulate
	// inotify watch limit exhaustion (ENOSPC).
	localWatcherFactory func() (FsWatcher, error)

	// socketIOWakeSourceFactory is a test seam for watch-mode websocket
	// wakeups. Production uses NewSocketIOWakeSourceWithOptions.
	socketIOWakeSourceFactory func(
		SocketIOEndpointFetcher,
		driveid.ID,
		SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner

	afterFunc func(time.Duration, func()) syncTimer
	newTicker func(time.Duration) syncTicker
	sleepFn   func(context.Context, time.Duration) error
	jitterFn  func(time.Duration) time.Duration
	nextRunID atomic.Int64
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

	if !SyncRootExists(cfg.SyncRoot) {
		return nil, fmt.Errorf("sync: opening sync tree %q: %w", cfg.SyncRoot, ErrMountRootUnavailable)
	}

	bm, err := openEngineSyncStore(ctx, cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}
	storeOwnedByEngine := false
	defer closeEngineSyncStoreOnStartupFailure(ctx, cfg.Logger, bm, &storeOwnedByEngine)()
	syncTree, err := openSyncTreeWithExpectedIdentity(cfg.SyncRoot, cfg.ExpectedSyncRootIdentity)
	if err != nil {
		return nil, fmt.Errorf("sync: opening sync tree: %w", err)
	}

	execCfg := NewExecutorConfig(
		cfg.Items,
		cfg.Downloads,
		cfg.Uploads,
		syncTree,
		cfg.DriveID,
		cfg.Logger,
		cfg.PathConvergence,
	)
	execCfg.SetRemoteRootItemID(cfg.RemoteRootItemID)
	execCfg.SetContentFilter(cfg.ContentFilter)

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
		baseline:                 bm,
		planner:                  NewPlanner(cfg.Logger),
		execCfg:                  execCfg,
		fetcher:                  cfg.Fetcher,
		socketIOFetcher:          cfg.SocketIOFetcher,
		itemsClient:              cfg.Items,
		driveVerifier:            cfg.DriveVerifier,
		folderDelta:              cfg.FolderDelta,
		recursiveLister:          cfg.RecursiveLister,
		sessionStore:             sessionStore,
		dataDir:                  cfg.DataDir,
		syncRoot:                 cfg.SyncRoot,
		syncTree:                 syncTree,
		expectedSyncRootIdentity: cloneFileIdentity(cfg.ExpectedSyncRootIdentity),
		driveID:                  cfg.DriveID,
		driveType:                cfg.DriveType,
		remoteRootItemID:         cfg.RemoteRootItemID,
		remoteRootDeltaCapable:   cfg.RemoteRootDeltaCapable,
		logger:                   cfg.Logger,
		perfCollector:            cfg.PerfCollector,
		transferWorkers:          cfg.TransferWorkers,
		checkWorkers:             cfg.CheckWorkers,
		contentFilter:            cfg.ContentFilter,
		localRules:               cfg.LocalRules,
		shortcutNamespaceID:      cfg.ShortcutNamespaceID,
		shortcutChildWorkSink:    cfg.ShortcutChildWorkSink,
		enableWebsocket:          cfg.EnableWebsocket,
		minFreeSpace:             cfg.MinFreeSpace,
		diskAvailableFn:          driveops.DiskAvailable,
		nowFn:                    time.Now,
		afterFunc:                realAfterFunc,
		newTicker:                realNewTicker,
		sleepFn:                  realSleep,
		jitterFn:                 realJitter,
		socketIOWakeSourceFactory: func(
			fetcher SocketIOEndpointFetcher,
			driveID driveid.ID,
			opts SocketIOWakeSourceOptions,
		) socketIOWakeSourceRunner {
			return NewSocketIOWakeSourceWithOptions(fetcher, driveID, opts)
		},
	}

	if err := e.refreshProtectedRootsFromStore(ctx); err != nil {
		return nil, fmt.Errorf("sync: loading parent shortcut protected roots: %w", err)
	}

	e.permHandler = newEnginePermissionHandler(e, cfg, syncTree)

	storeOwnedByEngine = true
	return e, nil
}

func newEnginePermissionHandler(
	engine *Engine,
	cfg *engineInputs,
	syncTree *synctree.Root,
) *PermissionHandler {
	return &PermissionHandler{
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
			logger.Warn("closed sync store after engine startup failure",
				slog.String("error", closeErr.Error()),
			)
		}
	}
}

func openSyncTreeWithExpectedIdentity(
	syncRoot string,
	expected *synctree.FileIdentity,
) (*synctree.Root, error) {
	syncTree, err := synctree.Open(syncRoot)
	if err != nil {
		return nil, fmt.Errorf("opening sync tree: %w", err)
	}
	if err := validateExpectedSyncRootIdentity(syncTree, expected); err != nil {
		return nil, err
	}
	return syncTree, nil
}

func validateExpectedSyncRootIdentity(root *synctree.Root, expected *synctree.FileIdentity) error {
	if root == nil || expected == nil {
		return nil
	}
	actual, err := root.IdentityNoFollow("")
	if err != nil {
		return fmt.Errorf("sync: verifying mount root identity: %w: %w", ErrMountRootUnavailable, err)
	}
	if !synctree.SameIdentity(actual, *expected) {
		return fmt.Errorf("sync: verifying mount root identity: %w", ErrMountRootUnavailable)
	}
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
			e.logger.Warn("failed to clean stale sessions on close",
				slog.String("error", err.Error()),
			)
		} else if n > 0 {
			e.logger.Info("cleaned stale upload sessions on close",
				slog.Int("deleted", n),
			)
		}
	}

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
			"sync: drive ID mismatch: mount %s, remote returned %s", e.driveID, drive.ID)
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
	ctx context.Context,
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
	if err := config.ClearAccountAuthRequirement(
		e.dataDir,
		e.permHandler.accountEmail,
		config.AuthClearSourceSyncStartupProof,
	); err != nil {
		return fmt.Errorf("sync: clearing catalog auth requirement: %w", err)
	}

	e.logger.Info("released catalog auth requirement after successful startup proof",
		slog.String("account", e.permHandler.accountEmail),
	)

	_ = ctx
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
