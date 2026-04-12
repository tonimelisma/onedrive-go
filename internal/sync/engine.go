package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type reconcileResult struct {
	events    []ChangeEvent
	shortcuts []synctypes.Shortcut
}

type driveIdentityProof struct {
	attempted bool
	drive     *graph.Drive
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single-drive only; multi-drive orchestration is handled by internal/multisync.
type Engine struct {
	baseline              *syncstore.SyncStore
	planner               *Planner
	execCfg               *ExecutorConfig
	fetcher               DeltaFetcher
	socketIOFetcher       SocketIOEndpointFetcher
	itemsClient           ItemClient
	driveVerifier         DriveVerifier      // optional (B-074)
	folderDelta           FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister       RecursiveLister    // optional: for shortcut observation (6.4b)
	permHandler           *PermissionHandler // encapsulates all permission logic (6.4c)
	syncRoot              string
	syncTree              *synctree.Root
	driveID               driveid.ID
	driveType             string
	rootItemID            string
	logger                *slog.Logger
	perfCollector         *perf.Collector
	sessionStore          *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers       int                    // goroutine count for the worker pool
	checkWorkers          int                    // goroutine limit for parallel file hashing
	localFilter           LocalFilterConfig
	localRules            LocalObservationRules
	syncScopeConfig       syncscope.Config
	enableWebsocket       bool
	deleteSafetyThreshold int   // from config; 0 means use default
	minFreeSpace          int64 // startup disk-scope revalidation threshold
	diskAvailableFn       func(string) (uint64, error)

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

	// retryBatchLimit lets tests lower the retrier sweep batch size so stress
	// runs can exercise the batching contract without seeding thousands of
	// durable failures per iteration. Production leaves this zero and uses the
	// compiled default in engine_retry_trial.go.
	retryBatchLimit int

	// socketIOWakeSourceFactory is a test seam for watch-mode websocket
	// wakeups. Production uses NewSocketIOWakeSource.
	socketIOWakeSourceFactory func(
		SocketIOEndpointFetcher,
		driveid.ID,
		SocketIOWakeSourceOptions,
	) socketIOWakeSourceRunner

	// watchRuntimeHook is a test-only seam that exposes the newly constructed
	// watch runtime before initWatchInfra wires timers, observers, and workers.
	// Production keeps this nil.
	watchRuntimeHook func(*watchRuntime)

	afterFunc func(time.Duration, func()) syncTimer
	newTicker func(time.Duration) syncTicker
	sleepFn   func(context.Context, time.Duration) error
	jitterFn  func(time.Duration) time.Duration
	nextRunID atomic.Int64
}

// newEngine creates an Engine, initializing the SyncStore (which opens
// the SQLite database and applies the canonical schema). Returns an error if DB init fails
// or if DriveID is zero (indicates a config/login issue).
func newEngine(ctx context.Context, cfg *engineInputs) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: engine requires non-zero drive ID")
	}

	bm, err := syncstore.NewSyncStore(ctx, cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	syncTree, err := synctree.Open(cfg.SyncRoot)
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
		cfg.PathConvergenceFactory,
	)
	execCfg.SetRootItemID(cfg.RootItemID)

	if cfg.UseLocalTrash {
		execCfg.SetTrashFunc(localtrash.Default)
	}

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
	))

	// Default threshold if not set by config.
	deleteSafetyThreshold := cfg.DeleteSafetyThreshold
	if deleteSafetyThreshold == 0 {
		deleteSafetyThreshold = DefaultDeleteSafetyThreshold
	}

	e := &Engine{
		baseline:              bm,
		planner:               NewPlanner(cfg.Logger),
		execCfg:               execCfg,
		fetcher:               cfg.Fetcher,
		socketIOFetcher:       cfg.SocketIOFetcher,
		itemsClient:           cfg.Items,
		driveVerifier:         cfg.DriveVerifier,
		folderDelta:           cfg.FolderDelta,
		recursiveLister:       cfg.RecursiveLister,
		sessionStore:          sessionStore,
		syncRoot:              cfg.SyncRoot,
		syncTree:              syncTree,
		driveID:               cfg.DriveID,
		driveType:             cfg.DriveType,
		rootItemID:            cfg.RootItemID,
		logger:                cfg.Logger,
		perfCollector:         cfg.PerfCollector,
		transferWorkers:       cfg.TransferWorkers,
		checkWorkers:          cfg.CheckWorkers,
		localFilter:           cfg.LocalFilter,
		localRules:            cfg.LocalRules,
		syncScopeConfig:       cfg.SyncScope,
		enableWebsocket:       cfg.EnableWebsocket,
		deleteSafetyThreshold: deleteSafetyThreshold,
		minFreeSpace:          cfg.MinFreeSpace,
		diskAvailableFn:       driveops.DiskAvailable,
		nowFn:                 time.Now,
		afterFunc:             realAfterFunc,
		newTicker:             realNewTicker,
		sleepFn:               realSleep,
		jitterFn:              realJitter,
		socketIOWakeSourceFactory: func(
			fetcher SocketIOEndpointFetcher,
			driveID driveid.ID,
			opts SocketIOWakeSourceOptions,
		) socketIOWakeSourceRunner {
			return NewSocketIOWakeSourceWithOptions(fetcher, driveID, opts)
		},
	}

	e.permHandler = &PermissionHandler{
		baseline:     e.baseline,
		permChecker:  cfg.PermChecker,
		syncTree:     syncTree,
		driveID:      cfg.DriveID,
		accountEmail: cfg.AccountEmail,
		rootItemID:   cfg.RootItemID,
		logger:       cfg.Logger,
		nowFn:        e.nowFunc,
	}

	return e, nil
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
// and watch-mode bootstrap. The proof remains ephemeral; persisted auth state
// is still owned by scope_blocks.
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
			"sync: drive ID mismatch: configured %s, remote returned %s", e.driveID, drive.ID)
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

func (e *Engine) hasPersistedAuthScope(ctx context.Context) (bool, error) {
	blocks, err := e.baseline.ListScopeBlocks(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: listing scope blocks: %w", err)
	}

	for i := range blocks {
		if blocks[i].Key == synctypes.SKAuthAccount() {
			return true, nil
		}
	}

	return false, nil
}

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote truth
//  3. Observe local truth
//  4. Buffer and flush changes plus durable retry/replay work
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Build DepGraph, start worker pool
//  9. Wait for completion, commit delta token

// ListConflicts returns all unresolved conflicts from the database.
func (e *Engine) ListConflicts(ctx context.Context) ([]syncstore.ConflictRecord, error) {
	conflicts, err := e.baseline.ListConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing unresolved conflicts: %w", err)
	}

	return conflicts, nil
}

// ListAllConflicts returns all conflicts (resolved and unresolved) from the
// database. Used by `status --history`.
func (e *Engine) ListAllConflicts(ctx context.Context) ([]syncstore.ConflictRecord, error) {
	conflicts, err := e.baseline.ListAllConflicts(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: listing conflict history: %w", err)
	}

	return conflicts, nil
}

// ResolveConflict queues and immediately processes a conflict resolution
// through the same engine-owned path used by daemon/user-intent processing.
// CLI commands must not call this; they persist requests via SyncStore or the
// control socket so a running engine remains the sole side-effect owner.
func (e *Engine) ResolveConflict(ctx context.Context, conflictID, resolution string) error {
	result, err := e.baseline.RequestConflictResolution(ctx, conflictID, resolution)
	if err != nil {
		return fmt.Errorf("sync: request conflict resolution: %w", err)
	}

	switch result.Status {
	case syncstore.ConflictRequestQueued, syncstore.ConflictRequestAlreadyQueued:
		_, err := e.processQueuedConflictResolutions(ctx)
		return err
	case syncstore.ConflictRequestAlreadyResolved:
		return nil
	case syncstore.ConflictRequestAlreadyApplying:
		return fmt.Errorf("sync: conflict %s resolution request is %s", conflictID, result.Status)
	default:
		return fmt.Errorf("sync: conflict %s resolution request is %s", conflictID, result.Status)
	}
}

// resolveKeepLocal establishes the chosen on-disk layout for "keep local" by
// restoring the newest untracked conflict copy back to the canonical path and
// removing only the current unresolved conflict-copy artifacts. Any later
// upload is ordinary sync work driven by normal observation/planning.
func (e *Engine) resolveKeepLocal(ctx context.Context, c *syncstore.ConflictRecord) ([]string, error) {
	copies, err := e.untrackedConflictCopyPaths(ctx, c.Path)
	if err != nil {
		return nil, fmt.Errorf("glob conflict copies for keep-local: %w", err)
	}
	if len(copies) == 0 {
		return nil, fmt.Errorf("restoring conflict copy to %s: conflict copy not found", c.Path)
	}

	selected := copies[len(copies)-1]
	if err := e.syncTree.Rename(selected, c.Path); err != nil {
		return nil, fmt.Errorf("restoring conflict copy to %s: %w", c.Path, err)
	}

	e.logger.Debug("restored conflict copy for keep-local",
		slog.String("from", filepath.Base(selected)),
		slog.String("to", c.Path),
	)

	if err := e.cleanupUntrackedConflictCopies(copies[:len(copies)-1]); err != nil {
		return nil, err
	}

	return []string{c.Path}, nil
}

// resolveKeepRemote treats the remote-version file already materialized at the
// canonical path during conflict detection as the chosen layout. It only
// removes the current unresolved conflict-copy artifacts; any later baseline
// convergence or download/upload trouble is ordinary sync work.
func (e *Engine) resolveKeepRemote(ctx context.Context, c *syncstore.ConflictRecord) ([]string, error) {
	if _, err := e.syncTree.Stat(c.Path); err != nil {
		return nil, fmt.Errorf("confirming remote version at %s: %w", c.Path, err)
	}

	copies, err := e.untrackedConflictCopyPaths(ctx, c.Path)
	if err != nil {
		return nil, fmt.Errorf("glob conflict copies for keep-remote: %w", err)
	}
	if err := e.cleanupUntrackedConflictCopies(copies); err != nil {
		return nil, err
	}

	return []string{c.Path}, nil
}

// resolveKeepBoth treats the current original path plus the unresolved
// conflict-copy artifacts as the chosen final layout. The conflict is resolved
// once that layout exists; later hashing, baseline refresh, or uploads are
// ordinary sync work driven by normal observation/planning.
func (e *Engine) resolveKeepBoth(ctx context.Context, c *syncstore.ConflictRecord) ([]string, error) {
	if _, err := e.syncTree.Stat(c.Path); err != nil {
		return nil, fmt.Errorf("confirming original file for keep-both: %w", err)
	}

	copies, err := e.untrackedConflictCopyPaths(ctx, c.Path)
	if err != nil {
		return nil, fmt.Errorf("glob conflict copies for keep-both: %w", err)
	}
	if len(copies) == 0 {
		return nil, fmt.Errorf("confirming conflict copies for keep-both: none found for %s", c.Path)
	}

	paths := append([]string{c.Path}, copies...)
	return paths, nil
}

func (e *Engine) untrackedConflictCopyPaths(ctx context.Context, relPath string) ([]string, error) {
	matches, err := e.syncTree.Glob(conflictCopyGlob(relPath))
	if err != nil {
		return nil, fmt.Errorf("glob conflict copy paths: %w", err)
	}
	sort.Strings(matches)

	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("load baseline for conflict copy classification: %w", err)
	}

	copies := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, tracked := bl.GetByPath(match); tracked {
			continue
		}
		copies = append(copies, match)
	}

	return copies, nil
}

func (e *Engine) cleanupUntrackedConflictCopies(paths []string) error {
	for _, relPath := range paths {
		if relPath == "" {
			continue
		}
		if err := e.syncTree.Remove(relPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove conflict copy %s: %w", filepath.Base(relPath), err)
		}
		e.logger.Debug("removed unresolved conflict copy",
			slog.String("file", filepath.Base(relPath)),
		)
	}

	return nil
}

func (e *Engine) conflictResolutionFollowUpChanges(
	ctx context.Context,
	paths []string,
) []PathChanges {
	if len(paths) == 0 {
		return nil
	}

	scopeSnapshot, err := e.buildScopeSnapshot(ctx)
	if err != nil {
		e.logger.Warn("build scope snapshot for conflict follow-up",
			slog.String("error", err.Error()),
		)
		return nil
	}

	uniquePaths := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path != "" {
			uniquePaths[path] = struct{}{}
		}
	}
	sortedPaths := make([]string, 0, len(uniquePaths))
	for path := range uniquePaths {
		sortedPaths = append(sortedPaths, path)
	}
	sort.Strings(sortedPaths)

	bl := e.baseline.Baseline()
	if bl == nil {
		var loadErr error
		bl, loadErr = e.baseline.Load(ctx)
		if loadErr != nil {
			e.logger.Warn("load baseline for conflict follow-up",
				slog.String("error", loadErr.Error()),
			)
			return nil
		}
	}

	var changes []PathChanges
	for _, path := range sortedPaths {
		var base *syncstore.BaselineEntry
		if entry, ok := bl.GetByPath(path); ok {
			base = entry
		}

		observation, observeErr := ObserveSinglePathWithScope(
			e.logger,
			e.syncTree,
			path,
			base,
			e.nowFunc().UnixNano(),
			nil,
			e.localFilter,
			e.localRules,
			scopeSnapshot,
		)
		if observeErr != nil {
			e.logger.Warn("observe conflict follow-up path",
				slog.String("path", path),
				slog.String("error", observeErr.Error()),
			)
			continue
		}
		if observation.Skipped != nil {
			e.logger.Warn("conflict follow-up path requires normal sync attention",
				slog.String("path", observation.Skipped.Path),
				slog.String("reason", observation.Skipped.Reason),
				slog.String("detail", observation.Skipped.Detail),
			)
			continue
		}
		changes = mergePathChangeBatches(changes, pathChangesFromEvent(observation.Event))
	}

	return changes
}

func conflictCopyGlob(relPath string) string {
	dir := filepath.Dir(relPath)
	name := filepath.Base(relPath)
	stem, ext := ConflictStemExt(name)
	pattern := fmt.Sprintf("%s.conflict-*%s", stem, ext)
	if dir == "." {
		return pattern
	}

	return filepath.Join(dir, pattern)
}
