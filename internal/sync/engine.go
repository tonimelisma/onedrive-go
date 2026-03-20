package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/syncplan"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// forceSafetyMax is the maximum threshold used when --force is set,
// effectively disabling big-delete protection.
const forceSafetyMax = math.MaxInt32

// periodicScanJitterDivisor controls the jitter window for periodic full
// scans. With a divisor of 10, each tick sleeps 0-10% of the interval to
// prevent thundering-herd I/O spikes in multi-drive mode.
const periodicScanJitterDivisor = 10

// retryBatchSize limits how many sync_failures are processed per retrier
// sweep in the drain loop. Prevents drain loop stalls when thousands of
// items become retryable at once (e.g., after a scope clear).
const retryBatchSize = 1024

// trialPendingTTL is the maximum time a trial entry lingers in trialPending
// before being considered stale and cleaned up. 15× the debounce window.
const trialPendingTTL = 30 * time.Second

// trialEntry tracks a pending trial action in the pipeline. Created when
// the trial timer fires and reobserve succeeds, consumed when the planner's
// fresh action arrives at admitAndDispatch or admitReady.
type trialEntry struct {
	scopeKey synctypes.ScopeKey
	created  time.Time
}

// watchState bundles all watch-mode-only state. Nil in one-shot mode.
// A single e.watch != nil check replaces scattered nil guards for
// scopeGate, scopeState, buf, deleteCounter, etc.
type watchState struct {
	// Scope gate — watch-mode only (§2.3: scope blocking is watch-mode only;
	// one-shot never creates scope blocks).
	scopeGate *syncdispatch.ScopeGate

	// Scope detection — sliding window failure tracking.
	scopeState *syncdispatch.ScopeState

	// Event buffer — drain-loop retrier injects events via e.watch.buf.Add().
	buf *syncobserve.Buffer

	// Big-delete protection: rolling counter + external change detection.
	// deleteCounter is nil even in watch mode when force=true.
	deleteCounter   *syncdispatch.DeleteCounter
	lastDataVersion int64

	// Trial management (drain-goroutine-only state).
	trialPending map[string]trialEntry
	trialTimer   *time.Timer
	trialMu      stdsync.Mutex

	// Retry timer — drain-loop retrier sweeps sync_failures on each tick.
	retryTimer   *time.Timer
	retryTimerCh chan struct{} // persistent, buffered(1)

	// Observer references — set in startObservers, nil'd on Close.
	remoteObs *syncobserve.RemoteObserver
	localObs  *syncobserve.LocalObserver

	// Monotonic action ID counter. Zeroed at watch start. Each action gets
	// a unique ID via e.watch.nextActionID.Add(1) — prevents ID collisions
	// across batches.
	nextActionID atomic.Int64

	// Throttling: tracks last recheckPermissions call time (R-2.10.9).
	lastPermRecheck time.Time

	// Deduplication: caches last actionable issue count for watch summaries.
	lastSummaryTotal int

	// Async reconciliation guard — prevents concurrent full reconciliations.
	// CompareAndSwap(false, true) at start, Store(false) in defer.
	// Zero value (false) is the correct initial state.
	reconcileRunning atomic.Bool

	// afterReconcileCommit is a test-only hook called after CommitObservation
	// succeeds in runFullReconciliationAsync. Nil in production. Allows tests
	// to inject actions (e.g. context cancellation) at an otherwise unreachable
	// point between commit and buffer feeding.
	afterReconcileCommit func()
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single-drive only; multi-drive orchestration is handled by the Orchestrator.
type Engine struct {
	baseline           *syncstore.SyncStore
	planner            *syncplan.Planner
	execCfg            *syncexec.ExecutorConfig
	fetcher            synctypes.DeltaFetcher
	driveVerifier      synctypes.DriveVerifier      // optional (B-074)
	folderDelta        synctypes.FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister    synctypes.RecursiveLister    // optional: for shortcut observation (6.4b)
	permHandler        *PermissionHandler           // encapsulates all permission logic (6.4c)
	syncRoot           string
	driveID            driveid.ID
	logger             *slog.Logger
	sessionStore       *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers    int                    // goroutine count for the worker pool
	checkWorkers       int                    // goroutine limit for parallel file hashing
	bigDeleteThreshold int                    // from config; 0 means use default

	// watchShortcuts holds the latest shortcuts for use by the drain
	// goroutine in watch mode. Updated by the watch goroutine after
	// observation; read by drainWorkerResults for 403 handling. Stays on
	// Engine because setShortcuts is called from RunOnce (one-shot)
	// where watch is nil.
	watchShortcuts   []synctypes.Shortcut
	watchShortcutsMu stdsync.RWMutex

	// depGraph is the pure dependency graph. Tracks action dependencies and
	// readiness. Set during executePlan (one-shot) or initWatchPipeline
	// (watch mode).
	depGraph *syncdispatch.DepGraph

	// readyCh feeds admitted actions to the worker pool. The engine sends
	// actions that pass the scope gate (or bypass it in one-shot mode).
	// Workers read from this channel via the WorkerPool.
	readyCh chan *synctypes.TrackedAction

	// trialCh is the persistent, buffered(1) channel for trial timer signals.
	// Created in NewEngine. In one-shot mode, no writer → harmlessly blocks
	// in select.
	trialCh chan struct{}

	// watch bundles all watch-mode-only state. Nil in one-shot mode.
	watch *watchState

	// Engine-owned result counters. Workers are pure executors — the engine
	// classifies results and owns all final disposition counts (R-6.8.9).
	succeeded    atomic.Int32
	failed       atomic.Int32
	syncErrors   []error
	syncErrorsMu stdsync.Mutex

	// nowFn is the engine's clock. Defaults to time.Now. Tests inject a
	// controllable clock for deterministic trial timer and scope timing.
	nowFn func() time.Time

	// localWatcherFactory overrides the default fsnotify watcher factory
	// for the local observer. Tests inject a mock factory to simulate
	// inotify watch limit exhaustion (ENOSPC).
	localWatcherFactory func() (syncobserve.FsWatcher, error)
}

// NewEngine creates an Engine, initializing the SyncStore (which opens
// the SQLite database and runs migrations). Returns an error if DB init fails
// or if DriveID is zero (indicates a config/login issue).
func NewEngine(cfg *synctypes.EngineConfig) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: engine requires non-zero drive ID")
	}

	bm, err := syncstore.NewSyncStore(cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	execCfg := syncexec.NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, cfg.SyncRoot, cfg.DriveID, cfg.Logger)

	if cfg.UseLocalTrash {
		execCfg.SetTrashFunc(syncstore.DefaultTrashFunc)
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
	bdThreshold := cfg.BigDeleteThreshold
	if bdThreshold == 0 {
		bdThreshold = synctypes.DefaultBigDeleteThreshold
	}

	e := &Engine{
		baseline:           bm,
		planner:            syncplan.NewPlanner(cfg.Logger),
		execCfg:            execCfg,
		fetcher:            cfg.Fetcher,
		driveVerifier:      cfg.DriveVerifier,
		folderDelta:        cfg.FolderDelta,
		recursiveLister:    cfg.RecursiveLister,
		sessionStore:       sessionStore,
		syncRoot:           cfg.SyncRoot,
		driveID:            cfg.DriveID,
		logger:             cfg.Logger,
		transferWorkers:    cfg.TransferWorkers,
		checkWorkers:       cfg.CheckWorkers,
		bigDeleteThreshold: bdThreshold,
		nowFn:              time.Now,
		trialCh:            make(chan struct{}, 1),
	}

	e.permHandler = &PermissionHandler{
		baseline:    e.baseline,
		permChecker: cfg.PermChecker,
		permCache:   newPermissionCache(),
		syncRoot:    cfg.SyncRoot,
		driveID:     cfg.DriveID,
		logger:      cfg.Logger,
		nowFn:       e.nowFunc,
		scopeMgr:    e,
	}

	return e, nil
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, and
// closes the database connection last. Safe to call more than once.
func (e *Engine) Close() error {
	// Nil out observer references to prevent dangling reads after Close.
	if e.watch != nil {
		e.watch.remoteObs = nil
		e.watch.localObs = nil
	}

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

	return e.baseline.Close()
}

// verifyDriveIdentity checks that the configured drive ID matches the remote
// API (B-074). Returns nil if no synctypes.DriveVerifier is configured (optional check).
func (e *Engine) verifyDriveIdentity(ctx context.Context) error {
	if e.driveVerifier == nil {
		return nil
	}

	drive, err := e.driveVerifier.Drive(ctx, e.driveID)
	if err != nil {
		return fmt.Errorf("sync: verifying drive identity: %w", err)
	}

	if drive.ID != e.driveID {
		return fmt.Errorf("sync: drive ID mismatch: configured %s, remote returned %s", e.driveID, drive.ID)
	}

	e.logger.Info("drive identity verified",
		slog.String("drive_id", drive.ID.String()),
		slog.String("drive_type", drive.DriveType),
	)

	return nil
}

// RunOnce executes a single sync pass:
//  1. Load baseline
//  2. Observe remote (skip if upload-only)
//  3. Observe local (skip if download-only)
//  4. Buffer and flush changes
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Build DepGraph, start worker pool
//  9. Wait for completion, commit delta token
func (e *Engine) RunOnce(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) (*synctypes.SyncReport, error) {
	start := time.Now()

	e.logger.Info("sync pass starting",
		slog.String("mode", mode.String()),
		slog.Bool("dry_run", opts.DryRun),
		slog.Bool("force", opts.Force),
	)

	// Step 0: Verify drive identity (B-074).
	if err := e.verifyDriveIdentity(ctx); err != nil {
		return nil, err
	}

	// Crash recovery: reset any in-progress states from a previous crash.
	// Also creates sync_failures entries so the retrier can rediscover items
	// that were mid-execution when the crash occurred.
	if err := e.baseline.ResetInProgressStates(ctx, e.syncRoot, retry.Reconcile.Delay); err != nil {
		e.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
	}

	// Step 1: Load baseline.
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}

	// Step 1b: Load shortcuts (needed for permission recheck + result drain).
	shortcuts, scErr := e.baseline.ListShortcuts(ctx)
	if scErr != nil {
		e.logger.Warn("failed to load shortcuts",
			slog.String("error", scErr.Error()),
		)
	}

	// Recheck permissions — clear any permission_denied issues
	// for folders that have become writable since the last pass.
	if e.permHandler.HasPermChecker() && scErr == nil {
		e.permHandler.recheckPermissions(ctx, bl, shortcuts)
	}

	// Recheck local permission denials — clear scope blocks for
	// directories that have become accessible since the last pass (R-2.10.13).
	e.permHandler.recheckLocalPermissions(ctx)

	// Steps 2-4: Observe remote + local, buffer, and flush.
	// The pending delta token is returned but NOT committed yet — it is
	// deferred until after the planner approves the changes (step 6).
	changes, pendingDeltaToken, err := e.observeChanges(ctx, bl, mode, opts.DryRun, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	// Step 5: Early return if no changes.
	if len(changes) == 0 {
		e.logger.Info("sync pass complete: no changes detected",
			slog.Duration("duration", time.Since(start)),
		)

		report := &synctypes.SyncReport{
			Mode:     mode,
			DryRun:   opts.DryRun,
			Duration: time.Since(start),
		}
		// Persist sync metadata even when no changes detected.
		if metaErr := e.baseline.WriteSyncMetadata(ctx, report); metaErr != nil {
			e.logger.Warn("failed to write sync metadata", slog.String("error", metaErr.Error()))
		}

		return report, nil
	}

	// Step 6: Plan actions.
	safety := e.resolveSafetyConfig(opts.Force)
	denied := e.permHandler.DeniedPrefixes()

	plan, err := e.planner.Plan(changes, bl, mode, safety, denied)
	if err != nil {
		// Big-delete protection (or other planner errors) — the delta token
		// is NOT committed, so the next sync replays the same events.
		return nil, err
	}

	// Planner approved — commit the deferred delta token now.
	if err := e.commitDeferredDeltaToken(ctx, pendingDeltaToken); err != nil {
		return nil, err
	}

	// Step 7: Build report from plan counts.
	counts := syncplan.CountByType(plan.Actions)
	report := buildReportFromCounts(counts, mode, opts)

	if opts.DryRun {
		report.Duration = time.Since(start)

		e.logger.Info("dry-run complete: no changes applied",
			slog.Duration("duration", report.Duration),
		)

		return report, nil
	}

	// Store shortcuts so the drain goroutine can access them via getShortcuts.
	e.setShortcuts(shortcuts)

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	e.executePlan(ctx, plan, report, bl)

	report.Duration = time.Since(start)

	e.logger.Info("sync pass complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
	)

	e.postSyncHousekeeping()

	// Persist sync metadata for status command queries.
	if metaErr := e.baseline.WriteSyncMetadata(ctx, report); metaErr != nil {
		e.logger.Warn("failed to write sync metadata", slog.String("error", metaErr.Error()))
	}

	return report, nil
}

// postSyncHousekeeping runs non-critical cleanup after a sync pass:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (e *Engine) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(e.syncRoot, e.sessionStore, e.logger)
}

// executePlan populates the dependency graph and runs the worker pool.
// The engine processes results concurrently while workers run, classifying
// each result and calling depGraph.Complete (R-6.8.9).
//
// One-shot mode has NO scope gate — all actions with satisfied deps go
// directly to readyCh. Scope detection (ScopeState) is nil in one-shot;
// feedScopeDetection is nil-guarded → no-op.
func (e *Engine) executePlan(
	ctx context.Context, plan *synctypes.ActionPlan, report *synctypes.SyncReport,
	bl *synctypes.Baseline,
) {
	if len(plan.Actions) == 0 {
		return
	}

	// Invariant: Planner.Plan() always builds Deps with len(Actions).
	// Assert here to catch any future regression that breaks this contract.
	if len(plan.Actions) != len(plan.Deps) {
		e.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		report.Failed = len(plan.Actions)
		report.Errors = append(report.Errors,
			fmt.Errorf("plan invariant violation: %d actions but %d deps", len(plan.Actions), len(plan.Deps)))

		return
	}

	// Reset engine counters for this pass.
	e.resetResultStats()

	// One-shot mode: DepGraph + readyCh, no scope gate (e.watch == nil).
	// Actions that pass dependency resolution go straight to workers.
	// Scope blocking is watch-mode only (§2.3).
	depGraph := syncdispatch.NewDepGraph(e.logger)
	e.depGraph = depGraph
	e.readyCh = make(chan *synctypes.TrackedAction, len(plan.Actions))

	// Two-phase graph population: Register all actions first, then wire
	// dependencies. This avoids forward-reference issues where a parent
	// folder delete at index 0 depends on a child file delete at index 5 —
	// single-pass Add would silently drop the unregistered depID.
	for i := range plan.Actions {
		e.setDispatch(ctx, &plan.Actions[i])
		depGraph.Register(&plan.Actions[i], int64(i))
	}

	for i := range plan.Actions {
		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		if ta := depGraph.WireDeps(int64(i), depIDs); ta != nil {
			e.readyCh <- ta
		}
	}

	pool := syncexec.NewWorkerPool(e.execCfg, e.readyCh, depGraph.Done(), e.baseline, e.logger, len(plan.Actions))
	pool.Start(ctx, e.transferWorkers)

	// Process results concurrently — engine classifies and calls Complete.
	// The drain goroutine reads from the results channel while workers run.
	// drainDone signals when the drain goroutine has finished processing all
	// results, including side effects (counter updates, failure recording).
	// Without this, resultStats() could race with the drain goroutine.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		e.drainWorkerResults(ctx, pool.Results(), bl)
	}()

	pool.Wait() // blocks until depGraph.Done() (all actions at terminal state)
	pool.Stop() // cancels workers, closes results → drain goroutine exits
	<-drainDone // wait for drain goroutine to finish all side effects

	// End-of-pass failure summary — aggregates failures by issue type so
	// bulk sync produces WARN summaries instead of per-item noise (R-6.6.12).
	e.logFailureSummary()

	report.Succeeded, report.Failed, report.Errors = e.resultStats()
}

// buildReportFromCounts populates a synctypes.SyncReport with plan counts.
func buildReportFromCounts(counts map[synctypes.ActionType]int, mode synctypes.SyncMode, opts synctypes.RunOpts) *synctypes.SyncReport {
	return &synctypes.SyncReport{
		Mode:          mode,
		DryRun:        opts.DryRun,
		FolderCreates: counts[synctypes.ActionFolderCreate],
		Moves:         counts[synctypes.ActionLocalMove] + counts[synctypes.ActionRemoteMove],
		Downloads:     counts[synctypes.ActionDownload],
		Uploads:       counts[synctypes.ActionUpload],
		LocalDeletes:  counts[synctypes.ActionLocalDelete],
		RemoteDeletes: counts[synctypes.ActionRemoteDelete],
		Conflicts:     counts[synctypes.ActionConflict],
		SyncedUpdates: counts[synctypes.ActionUpdateSynced],
		Cleanups:      counts[synctypes.ActionCleanup],
	}
}

// observeRemote fetches delta changes from the Graph API. Automatically
// retries with an empty token if synctypes.ErrDeltaExpired is returned (full resync).
func (e *Engine) observeRemote(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	savedToken, err := e.baseline.GetDeltaToken(ctx, e.driveID.String(), "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: getting delta token: %w", err)
	}

	obs := syncobserve.NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, synctypes.ErrDeltaExpired) {
			return nil, "", err
		}

		// Delta token expired — retry with empty token for full resync.
		e.logger.Warn("delta token expired, performing full resync")

		events, token, err = obs.FullDelta(ctx, "")
		if err != nil {
			return nil, "", fmt.Errorf("sync: full resync after delta expiry: %w", err)
		}
	}

	return events, token, nil
}

// observeLocal scans the local filesystem for changes and collects skipped
// items (invalid names, path too long, file too large) for failure recording.
func (e *Engine) observeLocal(ctx context.Context, bl *synctypes.Baseline) (synctypes.ScanResult, error) {
	obs := syncobserve.NewLocalObserver(bl, e.logger, e.checkWorkers)

	result, err := obs.FullScan(ctx, e.syncRoot)
	if err != nil {
		return synctypes.ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// observeChanges runs remote and local observers based on mode, buffers their
// events, and returns the flushed change set plus a pending delta token.
//
// Observations (remote_state rows) are committed immediately. The delta token
// is returned but NOT committed — the caller must commit it only after the
// planner approves the changes (prevents big-delete protection from
// permanently consuming deletion events). Skipped entirely for dry-run.
//
// When fullReconcile is true, runs a fresh delta with empty token (enumerates
// ALL remote items) and detects orphans — baseline entries not in the full
// enumeration, representing missed delta deletions.
func (e *Engine) observeChanges(
	ctx context.Context, bl *synctypes.Baseline, mode synctypes.SyncMode, dryRun, fullReconcile bool,
) ([]synctypes.PathChanges, string, error) {
	var remoteEvents []synctypes.ChangeEvent
	var pendingDeltaToken string

	var err error

	if mode != synctypes.SyncUploadOnly {
		if fullReconcile {
			e.logger.Info("full reconciliation: enumerating all remote items")
			remoteEvents, pendingDeltaToken, err = e.observeAndCommitRemoteFull(ctx, bl)
		} else if dryRun {
			// Dry-run: observe without committing delta token or observations.
			// A subsequent real sync must see the same remote changes.
			remoteEvents, _, err = e.observeRemote(ctx, bl)
		} else {
			remoteEvents, pendingDeltaToken, err = e.observeAndCommitRemote(ctx, bl)
		}

		if err != nil {
			return nil, "", err
		}
	}

	// Process shortcuts: register new ones, remove deleted ones, observe content.
	// During throttle:account or service scope blocks, suppress shortcut
	// observation to avoid wasting API calls (R-2.10.30).
	var shortcutEvents []synctypes.ChangeEvent

	if e.isObservationSuppressed() {
		e.logger.Debug("suppressing shortcut observation — global scope block active")
	} else {
		shortcutEvents, err = e.processShortcuts(ctx, remoteEvents, bl, dryRun)
		if err != nil {
			e.logger.Warn("shortcut processing failed, continuing without shortcut content",
				slog.String("error", err.Error()),
			)
		}
	}

	// Filter out synctypes.ChangeShortcut events from primary events — they were consumed
	// by processShortcuts and should not enter the planner as regular events.
	remoteEvents = filterOutShortcuts(remoteEvents)

	var localResult synctypes.ScanResult

	if mode != synctypes.SyncDownloadOnly {
		localResult, err = e.observeLocal(ctx, bl)
		if err != nil {
			return nil, "", err
		}

		// Record observation-time issues (invalid names, path too long, file too large).
		e.recordSkippedItems(ctx, localResult.Skipped)
		e.clearResolvedSkippedItems(ctx, localResult.Skipped)

		// R-2.10.10: If the scanner observed paths that were previously blocked
		// by local permission denials, clear the failures (scanner success = proof
		// of accessibility).
		e.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromEvents(localResult.Events))
	}

	buf := syncobserve.NewBuffer(e.logger)
	buf.AddAll(remoteEvents)
	buf.AddAll(shortcutEvents)
	buf.AddAll(localResult.Events)

	return buf.FlushImmediate(), pendingDeltaToken, nil
}

// observeAndCommitRemote wraps observeRemote to persist observations
// and return the pending delta token for deferred commitment.
//
// Observations (remote_state rows) are committed immediately so the baseline
// reflects the current remote state. The delta token is NOT committed here —
// it is returned to the caller, who must commit it only after the planner
// approves the changes. This prevents big-delete protection from permanently
// consuming deletion events: if the planner rejects the plan, the token stays
// at its old position and the next sync replays the same delta window.
//
// When delta returns 0 events, the token is NOT advanced. The old token
// still covers the same window — replaying it costs nothing (O(1)). But if
// a deletion was still propagating to the Graph change log, advancing would
// permanently skip it. Deletions are delivered exactly once in a narrow
// window (ci_issues.md §20).
func (e *Engine) observeAndCommitRemote(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	events, deltaToken, err := e.observeRemote(ctx, bl)
	if err != nil {
		return nil, "", err
	}

	// Skip token advancement when no events were returned. The old token
	// replays the same empty window at zero cost, but avoids advancing
	// past deletions still propagating through the Graph change log.
	if len(events) == 0 {
		e.logger.Debug("delta returned 0 events, skipping token advancement")
		return events, "", nil
	}

	// Commit observations WITHOUT the delta token. The token is deferred
	// until after the planner approves the changes.
	observed := changeEventsToObservedItems(e.logger, events)
	if commitErr := e.baseline.CommitObservation(ctx, observed, "", e.driveID); commitErr != nil {
		return nil, "", fmt.Errorf("sync: committing observations: %w", commitErr)
	}

	return events, deltaToken, nil
}

// commitDeferredDeltaToken advances the delta token after the planner approves
// the changes. No-op when token is empty (upload-only mode, 0-event delta).
// If the process crashes between this call and execution, the next sync
// replays the same delta window — the state machine handles re-observation
// idempotently (same hash → no-op, same delete → no-op).
func (e *Engine) commitDeferredDeltaToken(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}

	if err := e.baseline.CommitDeltaToken(
		ctx, token, e.driveID.String(), "", e.driveID.String(),
	); err != nil {
		return fmt.Errorf("sync: committing deferred delta token: %w", err)
	}

	return nil
}

// observeRemoteFull runs a fresh delta with empty token (enumerates ALL remote
// items) and compares against the baseline to find orphans: items in baseline
// but not in the full enumeration = deleted remotely but missed by incremental
// delta. Returns all events (creates/modifies from the full enumeration +
// synthesized deletes for orphans) and the new delta token.
func (e *Engine) observeRemoteFull(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	obs := syncobserve.NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)

	// Full enumeration: empty token returns ALL items as create/modify events.
	events, token, err := obs.FullDelta(ctx, "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: full reconciliation delta: %w", err)
	}

	// Build seen set from all non-deleted events in the full enumeration.
	seen := make(map[driveid.ItemKey]struct{}, len(events))
	for i := range events {
		if events[i].IsDeleted {
			continue
		}

		key := driveid.NewItemKey(events[i].DriveID, events[i].ItemID)
		seen[key] = struct{}{}
	}

	// Detect orphans: baseline entries whose ItemID is not in the seen set.
	orphans := bl.FindOrphans(seen, e.driveID, "")

	if len(orphans) > 0 {
		e.logger.Info("full reconciliation: detected orphaned items",
			slog.Int("orphans", len(orphans)),
		)

		events = append(events, orphans...)
	}

	e.logger.Info("full reconciliation complete",
		slog.Int("total_events", len(events)),
		slog.Int("orphans", len(orphans)),
	)

	return events, token, nil
}

// observeAndCommitRemoteFull wraps observeRemoteFull to persist observations
// and return the pending delta token for deferred commitment (same deferral
// pattern as observeAndCommitRemote — see its doc comment for rationale).
func (e *Engine) observeAndCommitRemoteFull(ctx context.Context, bl *synctypes.Baseline) ([]synctypes.ChangeEvent, string, error) {
	events, deltaToken, err := e.observeRemoteFull(ctx, bl)
	if err != nil {
		return nil, "", err
	}

	// Commit observations without the delta token — token deferred to caller.
	observed := changeEventsToObservedItems(e.logger, events)
	if commitErr := e.baseline.CommitObservation(ctx, observed, "", e.driveID); commitErr != nil {
		return nil, "", fmt.Errorf("sync: committing full reconciliation: %w", commitErr)
	}

	return events, deltaToken, nil
}

// changeEventsToObservedItems converts remote ChangeEvents into ObservedItems
// for CommitObservation. Filters out local-source events and events with
// empty ItemIDs (defensive guard against malformed API responses).
func changeEventsToObservedItems(logger *slog.Logger, events []synctypes.ChangeEvent) []synctypes.ObservedItem {
	var items []synctypes.ObservedItem

	for i := range events {
		if events[i].Source != synctypes.SourceRemote {
			continue
		}

		if events[i].ItemID == "" {
			logger.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", events[i].Path),
			)

			continue
		}

		items = append(items, synctypes.ObservedItem{
			DriveID:   events[i].DriveID,
			ItemID:    events[i].ItemID,
			ParentID:  events[i].ParentID,
			Path:      events[i].Path,
			ItemType:  events[i].ItemType,
			Hash:      events[i].Hash,
			Size:      events[i].Size,
			Mtime:     events[i].Mtime,
			ETag:      events[i].ETag,
			IsDeleted: events[i].IsDeleted,
		})
	}

	return items
}

// resolveSafetyConfig returns the appropriate synctypes.SafetyConfig. The planner-level
// big-delete check is disabled (threshold=MaxInt32) when force is set or when
// the engine has a deleteCounter (watch mode — the rolling counter handles
// big-delete protection instead).
func (e *Engine) resolveSafetyConfig(force bool) *synctypes.SafetyConfig {
	if force || e.watch != nil {
		return &synctypes.SafetyConfig{
			BigDeleteThreshold: forceSafetyMax,
		}
	}

	return &synctypes.SafetyConfig{
		BigDeleteThreshold: e.bigDeleteThreshold,
	}
}

// ListConflicts returns all unresolved conflicts from the database.
func (e *Engine) ListConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return e.baseline.ListConflicts(ctx)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) from the
// database. Used by 'conflicts --history'.
func (e *Engine) ListAllConflicts(ctx context.Context) ([]synctypes.ConflictRecord, error) {
	return e.baseline.ListAllConflicts(ctx)
}

// ResolveConflict resolves a single conflict by ID. For keep_both, this is
// a DB-only update. For keep_local, the local file is uploaded to overwrite
// the remote. For keep_remote, the remote file is downloaded to overwrite
// the local. The conflict record and baseline are updated atomically.
func (e *Engine) ResolveConflict(ctx context.Context, conflictID, resolution string) error {
	c, err := e.baseline.GetConflict(ctx, conflictID)
	if err != nil {
		return err
	}

	switch resolution {
	case synctypes.ResolutionKeepBoth:
		// Update baseline entries for both the original file and the conflict
		// copy so the next sync sees them as unchanged. Without this, the
		// scanner would flag the original (stale hash) and the conflict copy
		// (no baseline entry) as needing action.
		if err := e.resolveKeepBoth(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepBoth, err)
		}

		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	case synctypes.ResolutionKeepLocal:
		if err := e.resolveKeepLocal(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepLocal, err)
		}

		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	case synctypes.ResolutionKeepRemote:
		if err := e.resolveKeepRemote(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, synctypes.ResolutionKeepRemote, err)
		}

		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	default:
		return fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}
}

// resolveKeepLocal restores the conflict copy (which holds the user's local
// version) to the original path and uploads it. During conflict detection,
// ExecuteConflict renamed the local file to a conflict copy and downloaded
// the remote content to the original path. "Keep local" means the user wants
// their pre-conflict local content — which lives in the conflict copy.
func (e *Engine) resolveKeepLocal(ctx context.Context, c *synctypes.ConflictRecord) error {
	absPath := filepath.Join(e.syncRoot, c.Path)
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob conflict copies for keep-local: %w", err)
	}

	// Restore the first conflict copy to the original path. If multiple
	// conflict copies exist (shouldn't normally happen), the first one is
	// the user's local version.
	if len(matches) > 0 {
		if renameErr := os.Rename(matches[0], absPath); renameErr != nil {
			return fmt.Errorf("restoring conflict copy to %s: %w", c.Path, renameErr)
		}

		e.logger.Debug("restored conflict copy for keep-local",
			slog.String("from", filepath.Base(matches[0])),
			slog.String("to", c.Path),
		)
	}

	return e.resolveTransfer(ctx, c, synctypes.ActionUpload)
}

// resolveKeepRemote downloads the remote file to overwrite the local version.
func (e *Engine) resolveKeepRemote(ctx context.Context, c *synctypes.ConflictRecord) error {
	return e.resolveTransfer(ctx, c, synctypes.ActionDownload)
}

// resolveTransfer executes a single transfer (upload or download) for conflict
// resolution and commits the result to the baseline.
//
// Hash verification is intentionally skipped here (B-153): conflict resolution
// is a user-initiated override ("keep mine" / "keep theirs") where the intent
// is to force one side to match the other, not to verify content integrity.
// The executor's per-action functions already verify hashes for normal syncs.
func (e *Engine) resolveTransfer(ctx context.Context, c *synctypes.ConflictRecord, actionType synctypes.ActionType) error {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline for resolve: %w", err)
	}

	exec := syncexec.NewExecution(e.execCfg, bl)

	action := &synctypes.Action{
		Type:    actionType,
		DriveID: c.DriveID,
		ItemID:  c.ItemID,
		Path:    c.Path,
		View:    &synctypes.PathView{Path: c.Path},
	}

	var outcome synctypes.Outcome
	if actionType == synctypes.ActionUpload {
		outcome = exec.ExecuteUpload(ctx, action)
	} else {
		outcome = exec.ExecuteDownload(ctx, action)
	}

	if !outcome.Success {
		return fmt.Errorf("transfer failed: %w", outcome.Error)
	}

	if err := e.baseline.CommitOutcome(ctx, &outcome); err != nil {
		return err
	}

	// Clean up conflict copies — the user has chosen one side, so the
	// conflict copy (holding the other side's content) serves no purpose.
	e.cleanupConflictCopies(c.Path)

	return nil
}

// resolveKeepBoth updates baseline entries for both the original file and its
// conflict copies so that the next sync treats them as unchanged. The original
// file's baseline was not updated during conflict detection (unresolved
// conflicts intentionally skip baseline upsert), so it still has a stale hash.
// Conflict copies have no baseline entry at all.
func (e *Engine) resolveKeepBoth(ctx context.Context, c *synctypes.ConflictRecord) error {
	absPath := filepath.Join(e.syncRoot, c.Path)

	// Update baseline for the original file with its current on-disk hash.
	if err := e.upsertBaselineFromDisk(ctx, c.DriveID, c.ItemID, c.Path, absPath); err != nil {
		return fmt.Errorf("updating baseline for original: %w", err)
	}

	// Find conflict copies and create baseline entries for each. A synthetic
	// item ID is used because the conflict copy has no remote counterpart yet.
	// The next upload-capable sync or full reconciliation will upload the file
	// and replace this entry with a real item ID.
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Errorf("glob conflict copies: %w", err)
	}

	for _, m := range matches {
		relPath, relErr := filepath.Rel(e.syncRoot, m)
		if relErr != nil {
			return fmt.Errorf("computing relative path for conflict copy: %w", relErr)
		}

		syntheticID := "conflict-copy-placeholder"
		if upsertErr := e.upsertBaselineFromDisk(ctx, c.DriveID, syntheticID, relPath, m); upsertErr != nil {
			return fmt.Errorf("updating baseline for conflict copy %s: %w", filepath.Base(m), upsertErr)
		}
	}

	return nil
}

// upsertBaselineFromDisk computes the QuickXorHash of a file on disk and
// commits a synthetic UpdateSynced outcome to the baseline. Used during
// conflict resolution to bring baseline entries up to date without a real
// transfer.
func (e *Engine) upsertBaselineFromDisk(ctx context.Context, driveID driveid.ID, itemID, relPath, absPath string) error {
	info, err := os.Stat(absPath)
	if err != nil {
		return fmt.Errorf("stat %s: %w", relPath, err)
	}

	hash, err := driveops.ComputeQuickXorHash(absPath)
	if err != nil {
		return fmt.Errorf("hashing %s: %w", relPath, err)
	}

	outcome := &synctypes.Outcome{
		Action:    synctypes.ActionUpdateSynced,
		Success:   true,
		Path:      relPath,
		DriveID:   driveID,
		ItemID:    itemID,
		ItemType:  synctypes.ItemTypeFile,
		LocalHash: hash,
		Size:      info.Size(),
		Mtime:     info.ModTime().UnixNano(),
	}

	return e.baseline.CommitOutcome(ctx, outcome)
}

// cleanupConflictCopies deletes all conflict copy files for the given
// relative path. Called after keep-local or keep-remote resolution — the
// user has chosen one side, so the other side's content is no longer needed.
func (e *Engine) cleanupConflictCopies(relPath string) {
	absPath := filepath.Join(e.syncRoot, relPath)
	pattern := syncexec.ConflictCopyGlob(absPath)

	matches, err := filepath.Glob(pattern)
	if err != nil {
		e.logger.Warn("glob for conflict copies",
			slog.String("path", relPath),
			slog.String("error", err.Error()),
		)

		return
	}

	for _, m := range matches {
		if removeErr := os.Remove(m); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			e.logger.Warn("removing conflict copy",
				slog.String("file", filepath.Base(m)),
				slog.String("error", removeErr.Error()),
			)
		} else {
			e.logger.Debug("removed conflict copy",
				slog.String("file", filepath.Base(m)),
			)
		}
	}
}

// ---------------------------------------------------------------------------
// Watch mode
// ---------------------------------------------------------------------------

// Default watch intervals.
const (
	defaultPollInterval = 5 * time.Minute
	defaultDebounce     = 2 * time.Second
	watchEventBuf       = 256
	// watchResultBuf is the buffer size for the worker result channel in watch
	// mode. Large enough for typical batches without blocking workers.
	watchResultBuf = 4096

	// deleteCounterWindow is the rolling time window for the watch-mode
	// big-delete counter. Deletes within this window accumulate toward
	// the threshold. Expired entries drop off, preventing normal sustained
	// file management from triggering false positives.
	deleteCounterWindow = 5 * time.Minute

	// recheckInterval is how often the engine checks for external DB
	// changes (e.g., `issues clear` via the CLI). Uses PRAGMA data_version
	// — one integer comparison per tick, essentially free.
	recheckInterval = 10 * time.Second
)

// defaultReconcileInterval is the default interval for periodic full
// reconciliation in daemon mode. A full enumeration of 100K items costs
// ~500 API calls (~17% of a single 5-minute rate window), so 24h is safe.
const defaultReconcileInterval = 24 * time.Hour

// setShortcuts updates the shortcuts used by the drain goroutine.
// Called by the watch goroutine after observation when shortcuts may have changed.
func (e *Engine) setShortcuts(shortcuts []synctypes.Shortcut) {
	e.watchShortcutsMu.Lock()
	e.watchShortcuts = shortcuts
	e.watchShortcutsMu.Unlock()
}

// getShortcuts returns the latest shortcuts for drain goroutine use.
func (e *Engine) getShortcuts() []synctypes.Shortcut {
	e.watchShortcutsMu.RLock()
	defer e.watchShortcutsMu.RUnlock()

	return e.watchShortcuts
}

// initDeleteProtection sets up the rolling delete counter and clears stale
// big_delete_held entries from a prior daemon session. Force mode disables
// the counter (deleteCounter stays nil). Also seeds lastDataVersion so
// the first recheck tick doesn't fire spuriously.
func (e *Engine) initDeleteProtection(ctx context.Context, force bool) {
	if !force {
		e.watch.deleteCounter = syncdispatch.NewDeleteCounter(e.bigDeleteThreshold, deleteCounterWindow, time.Now)
	}

	if err := e.baseline.ClearResolvedActionableFailures(ctx, synctypes.IssueBigDeleteHeld, nil); err != nil {
		e.logger.Warn("failed to clear stale big-delete-held entries",
			slog.String("error", err.Error()),
		)
	}

	if dv, dvErr := e.baseline.DataVersion(ctx); dvErr == nil {
		e.watch.lastDataVersion = dv
	}
}

// loadWatchState loads the baseline and shortcuts for the watch session.
// Both are loaded once after the initial sync. synctypes.Baseline is live-mutated
// under RWMutex; shortcuts are updated via setShortcuts when they change.
func (e *Engine) loadWatchState(ctx context.Context) (*synctypes.Baseline, error) {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	shortcuts, scErr := e.baseline.ListShortcuts(ctx)
	if scErr != nil {
		e.logger.Warn("failed to load shortcuts for watch mode",
			slog.String("error", scErr.Error()),
		)
	}

	e.setShortcuts(shortcuts)

	return bl, nil
}

// RunWatch runs a continuous sync loop: bootstrap sync through the watch
// pipeline, then watches for remote and local changes in batches.
// Blocks until the context is canceled, returning nil on clean shutdown.
//
// Flow: initWatchInfra → bootstrapSync → startObservers → runWatchLoop.
// Unlike the old approach (calling RunOnce with throwaway infrastructure),
// bootstrapSync dispatches through the same DepGraph, ScopeGate, and
// WorkerPool that the watch loop uses.
func (e *Engine) RunWatch(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error {
	e.logger.Info("watch mode starting",
		slog.String("mode", mode.String()),
		slog.Bool("force", opts.Force),
		slog.Duration("poll_interval", e.resolvePollInterval(opts)),
		slog.Duration("debounce", e.resolveDebounce(opts)),
	)

	// Step 1: Set up watch infrastructure (no observers yet).
	pipe, err := e.initWatchInfra(ctx, mode, opts)
	if err != nil {
		return err
	}
	defer pipe.cleanup()

	// Step 2: Bootstrap — observe, plan, execute through watch pipeline.
	if err := e.bootstrapSync(ctx, mode, pipe); err != nil {
		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Step 3: Start observers AFTER bootstrap — they see the post-bootstrap baseline.
	errs, activeObs, skippedCh := e.startObservers(ctx, pipe.bl, mode, e.watch.buf, opts)
	pipe.errs = errs
	pipe.activeObs = activeObs
	pipe.skippedCh = skippedCh

	// Step 4: Run the watch loop.
	return e.runWatchLoop(ctx, pipe)
}

// watchPipeline holds all handles needed by the watch select loop.
// Created by initWatchInfra; cleaned up by its cleanup method.
type watchPipeline struct {
	bl         *synctypes.Baseline
	safety     *synctypes.SafetyConfig
	ready      <-chan []synctypes.PathChanges
	errs       <-chan error
	skippedCh  <-chan []synctypes.SkippedItem
	reconcileC <-chan time.Time
	recheckC   <-chan time.Time
	activeObs  int
	mode       synctypes.SyncMode
	pool       *syncexec.WorkerPool // for bootstrapSync to access Results()
	drainDone  <-chan struct{}      // closed when drain goroutine exits
	cleanup    func()
}

// initWatchInfra sets up watch-mode infrastructure: watchState, DepGraph,
// ScopeGate, worker pool, buffer, and tickers. Does NOT load baseline,
// start observers, or launch the drain goroutine — those happen in
// bootstrapSync and RunWatch.
//
// Key differences from one-shot mode (executePlan):
//   - ScopeGate is initialized and loaded from DB (scope blocks survive restart)
//   - Done channel is never-closing — DepGraph.Done() fires when completed >= total,
//     which would prematurely close between batches. Workers exit only via ctx.Done().
//   - Retrier logic is integrated into the drain loop (retryTimerCh case) to
//     eliminate coordination problems (D-11).
//   - Buffer is promoted to e.watch.buf so the drain-loop retrier can inject events.
func (e *Engine) initWatchInfra(
	ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts,
) (*watchPipeline, error) {
	// Create watchState — all watch-mode-only fields live here.
	e.watch = &watchState{
		trialPending: make(map[string]trialEntry),
		retryTimerCh: make(chan struct{}, 1),
	}

	// Enable watch-mode-specific executor behavior (pre-upload eTag
	// freshness checks to prevent silently overwriting concurrent remote
	// changes — see executor_transfer.go).
	e.execCfg.SetWatchMode(true)

	e.initDeleteProtection(ctx, opts.Force)

	// DepGraph tracks action dependencies; ScopeGate handles scope-based
	// admission control. ScopeGate loads persisted blocks from DB — blocks
	// survive crash/restart (D-8).
	depGraph := syncdispatch.NewDepGraph(e.logger)
	e.depGraph = depGraph
	e.watch.scopeGate = syncdispatch.NewScopeGate(e.baseline, e.logger)

	if loadErr := e.watch.scopeGate.LoadFromStore(ctx); loadErr != nil {
		return nil, fmt.Errorf("sync: loading scope blocks: %w", loadErr)
	}

	e.watch.scopeState = syncdispatch.NewScopeState(e.nowFunc, e.logger)
	e.watch.nextActionID.Store(0)

	// readyCh feeds admitted actions to workers. Buffer is generous to avoid
	// backpressure when a batch produces many immediately-ready actions.
	e.readyCh = make(chan *synctypes.TrackedAction, watchResultBuf)

	// Never-closing done channel — DepGraph.Done() would fire prematurely
	// between batches when completed == total. Workers exit only via ctx.Done().
	neverDone := make(chan struct{})

	pool := syncexec.NewWorkerPool(e.execCfg, e.readyCh, neverDone, e.baseline, e.logger, watchResultBuf)
	pool.Start(ctx, e.transferWorkers)

	// Buffer promoted to watchState — drain-loop retrier injects events
	// via e.watch.buf.Add(). Retrier logic runs inside the drain loop's
	// retryTimerCh case.
	buf := syncobserve.NewBuffer(e.logger)
	e.watch.buf = buf
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	// Tickers.
	reconcileC, stopReconcile := e.initReconcileTicker(opts)
	recheckTicker := time.NewTicker(recheckInterval)

	// Arm retrier timer from DB — picks up items from prior crash or prior pass.
	e.armRetryTimer()
	e.armTrialTimer()

	pipe := &watchPipeline{
		safety:     e.resolveSafetyConfig(opts.Force),
		ready:      ready,
		reconcileC: reconcileC,
		recheckC:   recheckTicker.C,
		mode:       mode,
		pool:       pool,
	}

	pipe.cleanup = func() {
		recheckTicker.Stop()
		if stopReconcile != nil {
			stopReconcile()
		}

		inFlight := depGraph.InFlightCount()
		if inFlight > 0 {
			e.logger.Info("graceful shutdown: draining in-flight actions",
				slog.Int("in_flight", inFlight),
			)
		}

		pool.Stop() // closes results channel → drain goroutine exits
		// Wait for drain goroutine to finish all side effects (same
		// pattern as one-shot mode). drainDone is set by bootstrapSync
		// before the watch loop starts.
		if pipe.drainDone != nil {
			<-pipe.drainDone
		}
		e.logger.Info("watch mode stopped")
	}

	return pipe, nil
}

// quiescenceLogInterval is how often bootstrapSync logs while waiting
// for in-flight actions to complete.
const quiescenceLogInterval = 30 * time.Second

// bootstrapSync performs the initial sync using the watch pipeline. Unlike
// the old approach (calling RunOnce with throwaway infrastructure), this
// dispatches through the same DepGraph, ScopeGate, and WorkerPool that
// the watch loop uses. Blocks until all bootstrap actions complete.
//
// Must be called after initWatchInfra and before startObservers.
func (e *Engine) bootstrapSync(ctx context.Context, mode synctypes.SyncMode, pipe *watchPipeline) error {
	e.logger.Info("bootstrap sync starting", slog.String("mode", mode.String()))

	// Drive identity check (B-074).
	if err := e.verifyDriveIdentity(ctx); err != nil {
		return err
	}

	// Crash recovery: reset in-progress states from prior crash.
	if err := e.baseline.ResetInProgressStates(ctx, e.syncRoot, retry.Reconcile.Delay); err != nil {
		e.logger.Warn("failed to reset in-progress states", slog.String("error", err.Error()))
	}

	// Load baseline + shortcuts.
	bl, err := e.loadWatchState(ctx)
	if err != nil {
		return err
	}
	pipe.bl = bl

	// Start drain goroutine — needs bl for processWorkerResult.
	// Join via pipe.drainDone in cleanup to ensure all side effects complete.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		e.drainWorkerResults(ctx, pipe.pool.Results(), bl)
	}()
	pipe.drainDone = drainDone

	// Permission rechecks.
	if e.permHandler.HasPermChecker() {
		shortcuts := e.getShortcuts()
		e.permHandler.recheckPermissions(ctx, bl, shortcuts)
	}
	e.permHandler.recheckLocalPermissions(ctx)

	// Observe changes.
	changes, pendingToken, err := e.observeChanges(ctx, bl, mode, false, false)
	if err != nil {
		return fmt.Errorf("sync: bootstrap observation failed: %w", err)
	}

	if len(changes) == 0 {
		e.logger.Info("bootstrap sync complete: no changes detected")
		return nil
	}

	// Commit the deferred delta token before dispatching bootstrap actions.
	// Bootstrap uses watch-mode big-delete (rolling counter), not planner-level
	// threshold, so the token is always safe to commit here.
	if err := e.commitDeferredDeltaToken(ctx, pendingToken); err != nil {
		return err
	}

	// Dispatch through watch pipeline (same path as steady-state batches).
	e.processBatch(ctx, changes, bl, mode, pipe.safety)

	// Wait for all bootstrap actions to complete.
	if err := e.waitForQuiescence(ctx); err != nil {
		return fmt.Errorf("sync: bootstrap quiescence failed: %w", err)
	}

	e.postSyncHousekeeping()
	e.logger.Info("bootstrap sync complete")
	return nil
}

// waitForQuiescence blocks until all in-flight actions in the DepGraph
// complete. Used by bootstrapSync to ensure the initial sync finishes
// before starting observers. Returns when the graph empties or the
// context is canceled (SIGTERM/SIGINT).
//
// No timeout: every dispatched action produces exactly one synctypes.WorkerResult
// (worker.go guarantees this), drainWorkerResults reads every result,
// and processWorkerResult calls depGraph.Complete for every result.
// There is no code path where an action enters the DepGraph without
// being completed. Context cancellation handles daemon shutdown.
func (e *Engine) waitForQuiescence(ctx context.Context) error {
	emptyCh := e.depGraph.WaitForEmpty()

	ticker := time.NewTicker(quiescenceLogInterval)
	defer ticker.Stop()

	for {
		select {
		case <-emptyCh:
			return nil
		case <-ticker.C:
			e.logger.Info("bootstrap: waiting for in-flight actions",
				slog.Int("in_flight", e.depGraph.InFlightCount()),
			)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// runWatchLoop runs the main watch-mode select loop. Blocks until context
// is canceled or all observers exit.
func (e *Engine) runWatchLoop(ctx context.Context, p *watchPipeline) error {
	for {
		select {
		case batch, ok := <-p.ready:
			if !ok {
				return nil
			}

			e.processBatch(ctx, batch, p.bl, p.mode, p.safety)

		case skipped := <-p.skippedCh:
			e.recordSkippedItems(ctx, skipped)
			e.clearResolvedSkippedItems(ctx, skipped)

		case <-p.recheckC:
			e.handleRecheckTick(ctx)

		case <-p.reconcileC:
			e.runFullReconciliationAsync(ctx, p.bl)

		case obsErr := <-p.errs:
			if obsErr != nil {
				e.logger.Warn("observer error",
					slog.String("error", obsErr.Error()),
				)
			}

			p.activeObs--
			if p.activeObs == 0 {
				e.logger.Error("all observers have exited, stopping watch mode")
				return fmt.Errorf("sync: all observers exited")
			}

		case <-ctx.Done():
			return nil
		}
	}
}

// ---------------------------------------------------------------------------
// Result classification (R-6.8.15)
// ---------------------------------------------------------------------------

// resultClass categorizes a synctypes.WorkerResult for routing by processWorkerResult.
type resultClass int

const (
	resultSuccess    resultClass = iota // action succeeded
	resultRequeue                       // transient failure — re-queue with backoff
	resultScopeBlock                    // scope-level failure (429, 507, 5xx pattern)
	resultSkip                          // non-retryable — record and move on
	resultShutdown                      // context canceled — discard silently
	resultFatal                         // abort sync pass (401 unrecoverable auth)
)

// classifyResult is a pure function that maps a synctypes.WorkerResult to a result
// class and optional scope key. No side effects — classification is
// separate from routing ("functions do one thing").
//
//nolint:gocyclo // classification table — each HTTP status code is a distinct case
func classifyResult(r *synctypes.WorkerResult) (resultClass, synctypes.ScopeKey) {
	if r.Success {
		return resultSuccess, synctypes.ScopeKey{}
	}

	// Shutdown: context canceled or deadline exceeded — graceful drain.
	// NOT a failure — just a canceled operation. Don't record in sync_failures.
	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return resultShutdown, synctypes.ScopeKey{}
	}

	switch {
	case r.HTTPStatus == http.StatusUnauthorized:
		return resultFatal, synctypes.ScopeKey{}

	case r.HTTPStatus == http.StatusForbidden:
		return resultSkip, synctypes.ScopeKey{}

	case r.HTTPStatus == http.StatusTooManyRequests:
		return resultScopeBlock, synctypes.SKThrottleAccount

	case r.HTTPStatus == http.StatusInsufficientStorage:
		return resultScopeBlock, synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)

	case r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err):
		// Known outage pattern (e.g., "ObjectHandle is Invalid") — transient,
		// feeds scope detection. Distinguished from phantom drive 400s
		// (R-6.7.11) which are handled by drive filtering.
		return resultRequeue, synctypes.ScopeKey{}

	case r.HTTPStatus >= 500:
		return resultRequeue, synctypes.ScopeKey{}

	case r.HTTPStatus == http.StatusRequestTimeout ||
		r.HTTPStatus == http.StatusPreconditionFailed ||
		r.HTTPStatus == http.StatusNotFound ||
		r.HTTPStatus == http.StatusLocked:
		return resultRequeue, synctypes.ScopeKey{}

	case errors.Is(r.Err, driveops.ErrDiskFull):
		// Deterministic signal — immediate scope block, no sliding window (R-2.10.43).
		return resultScopeBlock, synctypes.SKDiskLocal

	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		// Per-file failure, no scope escalation — smaller files may fit (R-2.10.44).
		return resultSkip, synctypes.ScopeKey{}

	case errors.Is(r.Err, os.ErrPermission):
		return resultSkip, synctypes.ScopeKey{}

	default:
		return resultSkip, synctypes.ScopeKey{}
	}
}

// isOutagePattern returns true if the error matches known transient 400
// outage patterns. Per failure-redesign.md §7.6, some 400 errors (e.g.,
// "ObjectHandle is Invalid") are actually transient service outages.
func isOutagePattern(err error) bool {
	if err == nil {
		return false
	}

	var ge *graph.GraphError
	if !errors.As(err, &ge) {
		return false
	}

	return strings.Contains(ge.Message, "ObjectHandle is Invalid")
}

// extendTrialInterval extends the trial interval for the given scope key.
// Delegates to computeTrialInterval for the actual computation — the same
// function used by applyScopeBlock for initial intervals, ensuring a single
// code path for the Retry-After-vs-backoff policy.
func (e *Engine) extendTrialInterval(scopeKey synctypes.ScopeKey, retryAfter time.Duration) {
	if e.watch == nil {
		return
	}

	block, ok := e.watch.scopeGate.GetScopeBlock(scopeKey)
	if !ok {
		return // scope was released between dispatch and result
	}

	newInterval := computeTrialInterval(retryAfter, block.TrialInterval)

	e.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	if err := e.watch.scopeGate.ExtendTrialInterval(context.Background(), scopeKey, e.nowFunc().Add(newInterval), newInterval); err != nil {
		e.logger.Warn("extendTrialInterval: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}

	e.armTrialTimer()
}

// computeTrialInterval is the single source of truth for trial interval
// computation (R-2.10.14). Both initial scope block creation and subsequent
// trial extensions use this function, preventing policy divergence.
//
//   - retryAfter > 0: server-provided value used directly, no cap (R-2.10.7)
//   - retryAfter == 0, currentInterval > 0: double current, cap at defaultMaxTrialInterval
//   - retryAfter == 0, currentInterval == 0: use defaultInitialTrialInterval
func computeTrialInterval(retryAfter, currentInterval time.Duration) time.Duration {
	if retryAfter > 0 {
		return retryAfter
	}
	if currentInterval > 0 {
		doubled := currentInterval * 2
		if doubled > syncdispatch.DefaultMaxTrialInterval {
			return syncdispatch.DefaultMaxTrialInterval
		}
		return doubled
	}
	return syncdispatch.DefaultInitialTrialInterval
}

// isObservationSuppressed returns true if a global scope block
// (throttle:account or service) is active, meaning shortcut observation
// polling should be skipped to avoid wasting API calls (R-2.10.30).
func (e *Engine) isObservationSuppressed() bool {
	if e.watch == nil {
		return false
	}

	return e.watch.scopeGate.IsScopeBlocked(synctypes.SKThrottleAccount) || e.watch.scopeGate.IsScopeBlocked(synctypes.SKService)
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Handles both
// regular failures and 400 outage patterns. Called directly from the normal
// processWorkerResult switch — never called for trial results (the scope is
// already blocked, and re-detecting would overwrite the doubled interval).
func (e *Engine) feedScopeDetection(r *synctypes.WorkerResult) {
	if e.watch == nil {
		return
	}

	// Local errors (HTTPStatus==0) must not feed scope detection windows.
	// Only remote API errors should increment service/quota counters (R-6.7.27).
	if r.HTTPStatus == 0 {
		return
	}

	sr := e.watch.scopeState.UpdateScope(r)
	if sr.Block {
		e.applyScopeBlock(sr)
	}
	// Also check outage pattern for 400s.
	if r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err) {
		osr := e.watch.scopeState.UpdateScopeOutagePattern(r.Path)
		if osr.Block {
			e.applyScopeBlock(osr)
		}
	}
}

// isWatchMode reports whether the engine is running in watch mode.
// Part of the scopeManager interface.
func (e *Engine) isWatchMode() bool { return e.watch != nil }

// setScopeBlock writes a scope block to the ScopeGate.
func (e *Engine) setScopeBlock(key synctypes.ScopeKey, block *synctypes.ScopeBlock) {
	if e.watch == nil {
		return
	}

	if err := e.watch.scopeGate.SetScopeBlock(context.Background(), key, block); err != nil {
		e.logger.Warn("setScopeBlock: failed to persist scope block",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
	}
}

// applyScopeBlock creates a synctypes.ScopeBlock and tells the ScopeGate to block
// affected actions. Uses computeTrialInterval for the initial interval,
// ensuring the same Retry-After-vs-backoff policy as extendTrialInterval.
// Logs a WARN because a scope block is a degraded-but-recoverable state
// (R-6.6.10).
func (e *Engine) applyScopeBlock(sr synctypes.ScopeUpdateResult) {
	now := e.nowFunc()
	interval := computeTrialInterval(sr.RetryAfter, 0)

	e.setScopeBlock(sr.ScopeKey, &synctypes.ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	})

	e.logger.Warn("scope block active — actions held",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("issue_type", sr.IssueType),
		slog.Duration("trial_interval", interval),
	)

	e.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
}

// ---------------------------------------------------------------------------
// Phase 4: New engine methods for DepGraph + ScopeGate architecture
// ---------------------------------------------------------------------------

// admitAndDispatch checks scope admission for ready actions and sends
// admitted ones to readyCh. Scope-blocked actions are cascade-recorded as
// sync_failures and completed in the graph. Called from the MAIN goroutine
// (processBatch, executePlan for watch mode).
//
// In one-shot mode, scope gate is not initialized — all actions pass through.
func (e *Engine) admitAndDispatch(ctx context.Context, ready []*synctypes.TrackedAction) {
	for _, ta := range ready {
		// Trial interception — watch mode only (one-shot has no trials).
		if e.watch != nil {
			e.watch.trialMu.Lock()
			entry, isTrial := e.watch.trialPending[ta.Action.Path]
			if isTrial {
				delete(e.watch.trialPending, ta.Action.Path)
			}
			e.watch.trialMu.Unlock()

			if isTrial {
				e.handleTrialInterception(ctx, ta, entry)
				continue
			}
		}

		// Normal scope admission (watch mode only — e.watch is nil in one-shot).
		if e.watch != nil {
			if key := e.watch.scopeGate.Admit(ta); !key.IsZero() {
				e.cascadeRecordAndComplete(ctx, ta, key)
				e.armTrialTimer()
				continue
			}
		}

		e.setDispatch(ctx, &ta.Action)

		select {
		case e.readyCh <- ta:
		case <-ctx.Done():
			return
		}
	}
}

// admitReady is the drain-goroutine variant of admitAndDispatch. Returns
// actions for the outbox instead of sending to readyCh (actor-with-outbox
// pattern, section 3.5). This prevents deadlock when Complete returns many
// dependents.
func (e *Engine) admitReady(ctx context.Context, ready []*synctypes.TrackedAction) []*synctypes.TrackedAction {
	var dispatch []*synctypes.TrackedAction

	for _, ta := range ready {
		// Trial interception — watch mode only (one-shot has no trials).
		var isTrial bool
		var entry trialEntry
		if e.watch != nil {
			e.watch.trialMu.Lock()
			entry, isTrial = e.watch.trialPending[ta.Action.Path]
			if isTrial {
				delete(e.watch.trialPending, ta.Action.Path)
			}
			e.watch.trialMu.Unlock()
		}

		if isTrial {
			if entry.scopeKey.BlocksAction(ta.Action.Path,
				ta.Action.ShortcutKey(), ta.Action.Type,
				ta.Action.TargetsOwnDrive()) {
				ta.IsTrial = true
				ta.TrialScopeKey = entry.scopeKey
				dispatch = append(dispatch, ta)
			} else {
				// Trial candidate no longer matches scope — clear stale failure,
				// run normal admission. Best-effort: log on error, don't abort.
				if err := e.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID); err != nil {
					e.logger.Debug("admitReady: failed to clear stale trial failure",
						slog.String("path", ta.Action.Path),
						slog.String("error", err.Error()),
					)
				}

				// e.watch is guaranteed non-nil — admitReady is only
				// called from processBatch (watch-mode only).
				if key := e.watch.scopeGate.Admit(ta); key.IsZero() {
					e.setDispatch(ctx, &ta.Action)
					dispatch = append(dispatch, ta)
				}
				e.armTrialTimer()
			}

			continue
		}

		// Normal scope admission.
		if e.watch != nil {
			if key := e.watch.scopeGate.Admit(ta); !key.IsZero() {
				e.cascadeRecordAndComplete(ctx, ta, key)
				continue
			}
		}

		e.setDispatch(ctx, &ta.Action)
		dispatch = append(dispatch, ta)
	}

	return dispatch
}

// handleTrialInterception handles a trial-intercepted action in
// admitAndDispatch (main goroutine path). Called by admitAndDispatch when
// the action's path matches a trialPending entry.
func (e *Engine) handleTrialInterception(ctx context.Context, ta *synctypes.TrackedAction, entry trialEntry) {
	if entry.scopeKey.BlocksAction(ta.Action.Path,
		ta.Action.ShortcutKey(), ta.Action.Type,
		ta.Action.TargetsOwnDrive()) {
		ta.IsTrial = true
		ta.TrialScopeKey = entry.scopeKey

		select {
		case e.readyCh <- ta: // bypass scope gate
		case <-ctx.Done():
		}

		return
	}

	// Trial candidate no longer matches scope — clear stale failure,
	// run normal admission. Best-effort: log on error, don't abort.
	if err := e.baseline.ClearSyncFailure(ctx, ta.Action.Path, ta.Action.DriveID); err != nil {
		e.logger.Debug("handleTrialInterception: failed to clear stale trial failure",
			slog.String("path", ta.Action.Path),
			slog.String("error", err.Error()),
		)
	}

	// e.watch is guaranteed non-nil — handleTrialInterception is only called
	// from admitAndDispatch, which runs inside processBatch (watch-mode only).
	if key := e.watch.scopeGate.Admit(ta); key.IsZero() {
		e.setDispatch(ctx, &ta.Action)

		select {
		case e.readyCh <- ta:
		case <-ctx.Done():
		}
	}

	e.armTrialTimer()
}

// cascadeRecordAndComplete records a scope-blocked action and all its
// transitive dependents as sync_failures, completing each in the graph.
// Uses BFS to traverse the dependency tree. Each dependent inherits the
// parent's scope_key (section 3.4).
//
// Safe for concurrent use — depGraph.Complete uses a mutex. Two cascades
// from different goroutines cannot return the same dependent (depsLeft is
// atomic — the last parent to complete returns the dependent).
func (e *Engine) cascadeRecordAndComplete(ctx context.Context, ta *synctypes.TrackedAction, scopeKey synctypes.ScopeKey) {
	seen := make(map[int64]bool)
	queue := []*synctypes.TrackedAction{ta}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		e.recordScopeBlockedFailure(ctx, &current.Action, scopeKey)
		// No resetDispatchStatus — setDispatch was never called for blocked
		// actions (scope gate is checked BEFORE setDispatch, per section 2.2).
		ready, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, ready...)
	}
}

// cascadeFailAndComplete records each transitive dependent as a cascade
// failure and completes it in the DepGraph. BFS ensures grandchildren are
// not stranded. Used for worker failures (non-scope-related).
func (e *Engine) cascadeFailAndComplete(ctx context.Context, ready []*synctypes.TrackedAction, r *synctypes.WorkerResult) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		e.recordCascadeFailure(ctx, &current.Action, r)
		next, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown (context canceled — not a failure).
func (e *Engine) completeSubtree(ready []*synctypes.TrackedAction) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next, _ := e.depGraph.Complete(current.ID)
		queue = append(queue, next...)
	}
}

// recordScopeBlockedFailure records a sync_failure for an action that was
// blocked by a scope gate. Uses next_retry_at = NULL (nil delayFn) so the
// retrier ignores it until onScopeClear sets next_retry_at.
func (e *Engine) recordScopeBlockedFailure(ctx context.Context, action *synctypes.Action, scopeKey synctypes.ScopeKey) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	if err := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Category:  synctypes.CategoryTransient,
		ScopeKey:  scopeKey,
		ErrMsg:    "scope blocked: " + scopeKey.String(),
	}, nil); err != nil { // nil delayFn → next_retry_at = NULL
		e.logger.Warn("failed to record scope-blocked failure",
			slog.String("path", action.Path),
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}

// recordCascadeFailure records a sync_failure for a dependent whose parent
// failed. The dependent inherits the parent's error context but gets its
// own direction and a fresh failure_count. Uses retry.Reconcile.Delay for
// exponential backoff — the dependent retries independently.
func (e *Engine) recordCascadeFailure(ctx context.Context, action *synctypes.Action, parentResult *synctypes.WorkerResult) {
	direction := directionFromAction(action.Type)

	driveID := action.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	issueType := issueTypeForHTTPStatus(parentResult.HTTPStatus, parentResult.Err)

	if err := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:      action.Path,
		DriveID:   driveID,
		Direction: direction,
		Category:  synctypes.CategoryTransient,
		IssueType: issueType,
		ErrMsg:    "parent action failed: " + parentResult.ErrMsg,
	}, retry.Reconcile.Delay); err != nil {
		e.logger.Warn("failed to record cascade failure",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}

// onScopeClear atomically clears a scope block and makes all scope-blocked
// failures retryable. Called when a trial succeeds or an external condition
// resolves (permission grant, quota freed, etc.).
//
// Does NOT re-observe items inline — would block the drain loop. Instead,
// makes sync_failures retryable and lets the retrier handle re-processing
// at its own pace (section 3.7).
func (e *Engine) onScopeClear(ctx context.Context, key synctypes.ScopeKey) {
	now := e.nowFunc()

	// Atomic: delete scope_blocks row + set next_retry_at = NOW in one tx.
	if err := e.baseline.ClearScopeAndUnblockFailures(ctx, key, now); err != nil {
		e.logger.Warn("failed to clear scope and unblock failures",
			slog.String("scope_key", key.String()),
			slog.String("error", err.Error()),
		)
	}

	// Update in-memory cache. ClearScopeBlock tries to delete from DB again
	// (harmless — row already gone). We call it for the in-memory map update.
	if e.watch != nil {
		if err := e.watch.scopeGate.ClearScopeBlock(ctx, key); err != nil {
			e.logger.Debug("onScopeClear: in-memory cache update failed (non-fatal)",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)
		}
	}

	e.armRetryTimer()

	e.logger.Info("scope block cleared — failures unblocked",
		slog.String("scope_key", key.String()),
	)
}

// processWorkerResult replaces processWorkerResult + routeReadyActions with
// failure-aware dependent dispatch. Returns actions for the outbox (drain
// goroutine) instead of sending to readyCh. Called from the drain goroutine.
//
// Dependent routing is structured at the Complete level:
//   - Parent success → children admitted via admitReady (scope gate check)
//   - Parent failure → children cascade-recorded as sync_failures
//   - Parent shutdown → children silently completed (no dispatch, no failure)
func (e *Engine) processWorkerResult(ctx context.Context, r *synctypes.WorkerResult, bl *synctypes.Baseline) []*synctypes.TrackedAction {
	// Trial results handled separately (early return).
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		return e.processTrialResult(ctx, r)
	}

	class, _ := classifyResult(r)

	// Graph completion — all result classes call Complete.
	ready, _ := e.depGraph.Complete(r.ActionID)

	// Dependent routing — based on result class.
	var dispatched []*synctypes.TrackedAction

	switch class {
	case resultSuccess:
		dispatched = e.admitReady(ctx, ready)

	case resultShutdown:
		// Context canceled — silently complete all dependents. Don't dispatch
		// (workers shutting down), don't record failures (not a failure).
		// BFS via completeSubtree prevents grandchild stranding.
		e.completeSubtree(ready)

	case resultRequeue, resultScopeBlock, resultSkip, resultFatal:
		// Parent failed — cascade-record children as sync_failures.
		// BFS via cascadeFailAndComplete prevents grandchild stranding.
		e.cascadeFailAndComplete(ctx, ready, r)
	}

	// Per-class side effects (after dependent routing).
	e.applyResultSideEffects(ctx, class, r, bl)

	return dispatched
}

// applyResultSideEffects handles per-class side effects: counter updates,
// failure recording, scope detection, and retrier kicks. Called after
// dependent routing is complete.
func (e *Engine) applyResultSideEffects(ctx context.Context, class resultClass, r *synctypes.WorkerResult, bl *synctypes.Baseline) {
	switch class {
	case resultSuccess:
		e.succeeded.Add(1)
		e.clearFailureOnSuccess(ctx, r)
		if e.watch != nil {
			e.watch.scopeState.RecordSuccess(r)
		}

	case resultRequeue:
		e.recordFailure(ctx, r, retry.Reconcile.Delay)
		e.recordError(r)
		e.feedScopeDetection(r)
		e.armRetryTimer()

	case resultScopeBlock:
		e.recordFailure(ctx, r, retry.Reconcile.Delay)
		e.recordError(r)
		e.feedScopeDetection(r)
		e.armTrialTimer()
		e.armRetryTimer()

	case resultSkip:
		// Local permission errors get special handling — walk up to find the
		// denied directory and create a scope block (R-2.10.12).
		if errors.Is(r.Err, os.ErrPermission) {
			e.permHandler.handleLocalPermission(ctx, r)
			e.recordError(r)

			return
		}
		if r.HTTPStatus == http.StatusForbidden && e.permHandler.HasPermChecker() {
			e.permHandler.handle403(ctx, bl, r.Path, e.getShortcuts())
		}
		// Non-retryable: record with nil delayFn (no next_retry_at).
		e.recordFailure(ctx, r, nil)
		e.recordError(r)

	case resultFatal:
		e.recordFailure(ctx, r, nil) // no retry
		e.recordError(r)

	case resultShutdown:
		// no failure recording
	}
}

// processTrialResult handles trial results using the new architecture.
// Returns actions for the outbox. Called from the drain goroutine.
func (e *Engine) processTrialResult(ctx context.Context, r *synctypes.WorkerResult) []*synctypes.TrackedAction {
	class, _ := classifyResult(r)

	// Complete the trial action in the graph.
	ready, _ := e.depGraph.Complete(r.ActionID)

	if class == resultSuccess {
		e.onScopeClear(ctx, r.TrialScopeKey)
		e.succeeded.Add(1)
		if e.watch != nil {
			e.watch.scopeState.RecordSuccess(r)
		}
		// Dispatch any dependents that were waiting on the trial.
		return e.admitReady(ctx, ready)
	}

	if class == resultShutdown {
		// BFS via completeSubtree prevents grandchild stranding.
		e.completeSubtree(ready)
		return nil
	}

	// Trial failure: extend interval. Scope detection is NOT called — the
	// scope is already blocked, and re-detecting would overwrite the doubled
	// interval with a fresh initial interval (A2 bug prevention).
	e.extendTrialInterval(r.TrialScopeKey, r.RetryAfter)

	var delayFn func(int) time.Duration
	if class == resultRequeue || class == resultScopeBlock {
		delayFn = retry.Reconcile.Delay
	}

	e.recordFailure(ctx, r, delayFn)
	e.recordError(r)

	// Cascade-record dependents as failures.
	// BFS via cascadeFailAndComplete prevents grandchild stranding.
	e.cascadeFailAndComplete(ctx, ready, r)

	e.armRetryTimer()

	return nil
}

// armRetryTimer arms the retry timer for the next retrier sweep. Queries
// the earliest next_retry_at from sync_failures and sets the timer. If the
// retry timer channel is already signaled (non-blocking send to buffered(1)
// channel), the next drain loop select iteration processes it.
func (e *Engine) armRetryTimer() {
	if e.watch == nil {
		return
	}

	earliest, err := e.baseline.EarliestSyncFailureRetryAt(context.Background(), e.nowFunc())
	if err != nil || earliest.IsZero() {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		// Items already due — signal immediately.
		select {
		case e.watch.retryTimerCh <- struct{}{}:
		default:
		}
		return
	}

	e.watch.trialMu.Lock()
	defer e.watch.trialMu.Unlock()

	if e.watch.retryTimer != nil {
		e.watch.retryTimer.Stop()
	}
	e.watch.retryTimer = time.AfterFunc(delay, func() {
		select {
		case e.watch.retryTimerCh <- struct{}{}:
		default:
		}
	})
}

// retryTimerChan returns the retry timer notification channel. Returns a nil
// channel when retryTimerCh is not initialized (one-shot mode), which blocks
// forever in a select — effectively disabling the case.
func (e *Engine) retryTimerChan() <-chan struct{} {
	if e.watch == nil {
		return nil // nil channel blocks in select — disables retry case
	}

	return e.watch.retryTimerCh
}

// runTrialDispatch handles due scope trials. For each due scope, picks the
// oldest scope-blocked failure from sync_failures and synthesizes a
// re-observation event into the buffer. If no candidates exist for a scope,
// the scope is cleared (no items left to trial — condition resolved).
//
// Uses AllDueTrials to snapshot all due scopes at once, then iterates each
// exactly once. This is structurally incapable of infinite iteration —
// unlike the old NextDueTrial-in-a-loop approach which required state
// mutation (extendTrialInterval) as iteration control.
//
// Called from the drain loop when the trial timer fires.
func (e *Engine) runTrialDispatch(ctx context.Context) {
	now := e.nowFunc()

	// Clean stale trial entries before dispatching new ones.
	e.cleanStaleTrialPending(now)

	// Snapshot all due scopes — each visited exactly once.
	for _, key := range e.watch.scopeGate.AllDueTrials(now) {
		// Pick oldest scope-blocked failure for this scope.
		row, err := e.baseline.PickTrialCandidate(ctx, key)
		if err != nil {
			e.logger.Warn("runTrialDispatch: failed to pick trial candidate",
				slog.String("scope_key", key.String()),
				slog.String("error", err.Error()),
			)

			break
		}

		if row == nil {
			// No candidates — scope condition resolved externally.
			e.onScopeClear(ctx, key)

			continue
		}

		// Register the trial in trialPending so admitAndDispatch/admitReady
		// can intercept the resulting action from the planner.
		e.watch.trialMu.Lock()
		e.watch.trialPending[row.Path] = trialEntry{
			scopeKey: key,
			created:  now,
		}
		e.watch.trialMu.Unlock()

		// Re-observe the item with a real API call / FS access to confirm
		// whether the scope condition has cleared. Unlike the retrier (which
		// uses cached DB state), trials hit the live source of truth.
		ev, retryAfter := e.reobserve(ctx, row)
		if ev == nil {
			// Scope condition persists — extend the trial interval using
			// the server's Retry-After if provided (R-2.10.7), otherwise
			// exponential backoff.
			e.extendTrialInterval(key, retryAfter)

			continue
		}

		if e.watch != nil {
			e.watch.buf.Add(ev)
		}

		e.logger.Debug("trial dispatched",
			slog.String("scope_key", key.String()),
			slog.String("path", row.Path),
		)
	}

	e.armTrialTimer()
}

// runRetrierSweep processes a batch of due sync_failures, re-injecting them
// into the pipeline via the buffer. Runs inline in the drain loop for
// direct depGraph.HasInFlight
// access without coordination problems (D-11).
//
// Batch-limited to retryBatchSize to prevent drain loop stalls when many
// items become retryable at once (e.g., after a scope clear).
func (e *Engine) runRetrierSweep(ctx context.Context) {
	now := e.nowFunc()

	rows, err := e.baseline.ListSyncFailuresForRetry(ctx, now)
	if err != nil {
		e.logger.Warn("retrier sweep: failed to list retriable items",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(rows) == 0 {
		return
	}

	dispatched := 0

	for i := range rows {
		if dispatched >= retryBatchSize {
			// More items remain — re-arm immediately so the drain loop
			// picks up the next batch on the next iteration.
			select {
			case e.watch.retryTimerCh <- struct{}{}:
			default:
			}

			break
		}

		row := &rows[i]

		// Skip items already being processed by a worker.
		if e.depGraph.HasInFlight(row.Path) {
			continue
		}

		// D-11: skip stale failures whose underlying condition has resolved
		// through the normal pipeline (e.g., delta poll downloaded the file,
		// local file was deleted). Clears the sync_failure from DB.
		if e.isFailureResolved(ctx, row) {
			continue
		}

		// D-9: build a full-fidelity event from DB state (remote_state for
		// downloads, local FS for uploads) instead of sparse synthesized events.
		ev := e.createEventFromDB(ctx, row)
		if ev == nil {
			continue
		}

		if e.watch != nil {
			e.watch.buf.Add(ev)
		}

		dispatched++
	}

	if dispatched > 0 {
		e.logger.Info("retrier sweep",
			slog.Int("dispatched", dispatched),
		)
	}

	// Re-arm for the next due item.
	e.armRetryTimer()
}

// remoteStateToChangeEvent converts a synctypes.RemoteStateRow into a full-fidelity
// synctypes.ChangeEvent. Pure function — no I/O. Used by createEventFromDB for
// download/delete failures where the DB-cached remote_state provides all
// fields the planner needs (D-9 fix).
func remoteStateToChangeEvent(rs *synctypes.RemoteStateRow, path string) *synctypes.ChangeEvent {
	// Determine change type from sync_status: delete-family statuses
	// become synctypes.ChangeDelete, everything else becomes synctypes.ChangeModify.
	ct := synctypes.ChangeModify
	isDeleted := false

	switch rs.SyncStatus { //nolint:exhaustive // non-delete statuses all map to ChangeModify
	case synctypes.SyncStatusDeleted, synctypes.SyncStatusDeleting, synctypes.SyncStatusDeleteFailed, synctypes.SyncStatusPendingDelete:
		ct = synctypes.ChangeDelete
		isDeleted = true
	}

	return &synctypes.ChangeEvent{
		Source:    synctypes.SourceRemote,
		Type:      ct,
		Path:      path,
		ItemID:    rs.ItemID,
		ParentID:  rs.ParentID,
		DriveID:   rs.DriveID,
		ItemType:  rs.ItemType,
		Name:      filepath.Base(path),
		Size:      rs.Size,
		Hash:      rs.Hash,
		Mtime:     rs.Mtime,
		ETag:      rs.ETag,
		IsDeleted: isDeleted,
	}
}

// observeLocalFile stats and hashes a local file, returning a synctypes.ChangeEvent.
// File gone → synctypes.ChangeDelete. Transient FS error → nil. Used by both
// createEventFromDB and reobserve to avoid duplicating the upload path.
func (e *Engine) observeLocalFile(path, caller string) *synctypes.ChangeEvent {
	absPath := filepath.Join(e.syncRoot, path)

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &synctypes.ChangeEvent{
				Source:    synctypes.SourceLocal,
				Type:      synctypes.ChangeDelete,
				Path:      path,
				Name:      filepath.Base(path),
				ItemType:  synctypes.ItemTypeFile,
				IsDeleted: true,
			}
		}

		e.logger.Debug(caller+": stat failed",
			slog.String("path", path),
			slog.String("error", err.Error()),
		)

		return nil
	}

	it := synctypes.ItemTypeFile
	if info.IsDir() {
		it = synctypes.ItemTypeFolder
	}

	var hash string
	if it == synctypes.ItemTypeFile {
		hash, err = syncobserve.ComputeStableHash(absPath)
		if err != nil {
			e.logger.Debug(caller+": hash failed",
				slog.String("path", path),
				slog.String("error", err.Error()),
			)

			return nil
		}
	}

	return &synctypes.ChangeEvent{
		Source:   synctypes.SourceLocal,
		Type:     synctypes.ChangeModify,
		Path:     path,
		Name:     filepath.Base(path),
		ItemType: it,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}
}

// createEventFromDB builds a full-fidelity synctypes.ChangeEvent from database state
// and the local filesystem. Direction-based dispatch:
//   - Upload: stat + hash the local file. File gone → synctypes.ChangeDelete. Error → nil.
//   - Download/Delete: query remote_state from DB. Nil → nil (resolved).
//     Otherwise remoteStateToChangeEvent.
//
// No API calls — uploads use the local FS; downloads use DB-cached
// remote_state (kept fresh by delta polls). Fixes D-9: the planner receives
// complete PathViews with hash, size, mtime, etag, and name.
func (e *Engine) createEventFromDB(ctx context.Context, row *synctypes.SyncFailureRow) *synctypes.ChangeEvent {
	switch row.Direction { //nolint:exhaustive // download and delete share the same path
	case synctypes.DirectionUpload:
		return e.observeLocalFile(row.Path, "createEventFromDB")

	default: // download or delete
		rs, err := e.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
		if err != nil {
			e.logger.Debug("createEventFromDB: remote state lookup failed",
				slog.String("path", row.Path),
				slog.String("error", err.Error()),
			)

			return nil
		}

		if rs == nil {
			// No active remote_state → item was resolved through the normal
			// pipeline (delta poll downloaded it, or it was deleted remotely).
			return nil
		}

		return remoteStateToChangeEvent(rs, row.Path)
	}
}

// isFailureResolved checks whether a sync_failure's underlying condition has
// been resolved through the normal pipeline, making the failure stale. When
// resolved, the failure is cleared from the database. Fixes D-11: prevents
// the retrier from re-injecting events for items that no longer need action.
//
// Resolution conditions by direction:
//   - Download: remote_state is nil (deleted) OR sync_status is synced/deleted/filtered.
//   - Upload: local file no longer exists (os.ErrNotExist).
//   - Delete: no baseline entry exists for the path (already cleaned up).
func (e *Engine) isFailureResolved(ctx context.Context, row *synctypes.SyncFailureRow) bool {
	var resolved bool

	switch row.Direction {
	case synctypes.DirectionDownload:
		rs, err := e.baseline.GetRemoteStateByPath(ctx, row.Path, row.DriveID)
		if err != nil {
			// DB error — can't determine resolution, treat as unresolved.
			return false
		}

		// No active row means the item was deleted or never existed.
		if rs == nil {
			resolved = true
		} else {
			switch rs.SyncStatus { //nolint:exhaustive // only terminal statuses mean resolved
			case synctypes.SyncStatusSynced, synctypes.SyncStatusDeleted, synctypes.SyncStatusFiltered:
				resolved = true
			}
		}

	case synctypes.DirectionUpload:
		absPath := filepath.Join(e.syncRoot, row.Path)

		_, err := os.Stat(absPath)
		if errors.Is(err, os.ErrNotExist) {
			resolved = true
		}

	case synctypes.DirectionDelete:
		// Load baseline to check if the entry still exists.
		bl, err := e.baseline.Load(ctx)
		if err != nil {
			return false
		}

		_, exists := bl.GetByPath(row.Path)
		if !exists {
			resolved = true
		}
	}

	if resolved {
		if err := e.baseline.ClearSyncFailure(ctx, row.Path, row.DriveID); err != nil {
			e.logger.Debug("isFailureResolved: failed to clear resolved failure",
				slog.String("path", row.Path),
				slog.String("error", err.Error()),
			)
		}
	}

	return resolved
}

// reobserve makes a real API call or filesystem access to re-observe an item
// for trial dispatch. Unlike createEventFromDB (which reads cached DB state),
// reobserve hits the live source of truth to confirm whether a scope condition
// has actually cleared.
//
// Direction-based dispatch:
//   - Download/Delete: GetItem API call. 200 → full synctypes.ChangeEvent. 404 → synctypes.ChangeDelete.
//     429/507/5xx → nil (scope still blocked).
//   - Upload: stat + hash local file. Exists → full synctypes.ChangeEvent. Gone → synctypes.ChangeDelete.
//     Error → nil.
//
// Returns (nil, retryAfter) when the scope condition persists — caller
// forwards retryAfter to extendTrialInterval for server-driven backoff.
// Returns (event, 0) on success or when the item is gone.
func (e *Engine) reobserve(ctx context.Context, row *synctypes.SyncFailureRow) (*synctypes.ChangeEvent, time.Duration) {
	switch row.Direction { //nolint:exhaustive // download and delete share the same path
	case synctypes.DirectionUpload:
		// Local FS — no RetryAfter concept.
		return e.observeLocalFile(row.Path, "reobserve"), 0

	default: // download or delete
		item, err := e.execCfg.Items().GetItem(ctx, row.DriveID, row.ItemID)
		if err != nil {
			// Classify the error to determine whether the scope is still blocked
			// or the item is truly gone.
			var ge *graph.GraphError
			if errors.As(err, &ge) {
				if errors.Is(ge.Err, graph.ErrNotFound) {
					// Item was deleted remotely — return a delete event.
					return &synctypes.ChangeEvent{
						Source:    synctypes.SourceRemote,
						Type:      synctypes.ChangeDelete,
						Path:      row.Path,
						ItemID:    row.ItemID,
						DriveID:   row.DriveID,
						ItemType:  synctypes.ItemTypeFile,
						Name:      filepath.Base(row.Path),
						IsDeleted: true,
					}, 0
				}

				// 429, 507, 5xx — scope condition persists; return nil so the
				// caller extends the trial interval. Forward RetryAfter from
				// the server response (R-2.10.7) so the caller uses the
				// server-mandated wait instead of exponential backoff.
				if errors.Is(ge.Err, graph.ErrThrottled) || errors.Is(ge.Err, graph.ErrServerError) ||
					ge.StatusCode == http.StatusInsufficientStorage {
					e.logger.Debug("reobserve: scope condition persists",
						slog.String("path", row.Path),
						slog.Int("status", ge.StatusCode),
						slog.Duration("retry_after", ge.RetryAfter),
					)

					return nil, ge.RetryAfter
				}
			}

			// Unexpected error — log and return nil (skip this trial).
			e.logger.Debug("reobserve: GetItem failed",
				slog.String("path", row.Path),
				slog.String("error", err.Error()),
			)

			return nil, 0
		}

		// 200 — build a full synctypes.ChangeEvent from the live API response.
		it := synctypes.ItemTypeFile
		if item.IsFolder {
			it = synctypes.ItemTypeFolder
		} else if item.IsRoot {
			it = synctypes.ItemTypeRoot
		}

		ct := synctypes.ChangeModify
		isDeleted := false

		if item.IsDeleted {
			ct = synctypes.ChangeDelete
			isDeleted = true
		}

		return &synctypes.ChangeEvent{
			Source:    synctypes.SourceRemote,
			Type:      ct,
			Path:      row.Path,
			ItemID:    item.ID,
			ParentID:  item.ParentID,
			DriveID:   item.DriveID,
			ItemType:  it,
			Name:      item.Name,
			Size:      item.Size,
			Hash:      item.QuickXorHash,
			Mtime:     item.ModifiedAt.UnixNano(),
			ETag:      item.ETag,
			IsDeleted: isDeleted,
		}, 0
	}
}

// cleanStaleTrialPending removes stale entries from trialPending. Called
// under trialMu. Entries older than trialPendingTTL are cleared and the
// corresponding sync_failure is deleted (item is stale).
func (e *Engine) cleanStaleTrialPending(now time.Time) {
	e.watch.trialMu.Lock()
	defer e.watch.trialMu.Unlock()

	for path, entry := range e.watch.trialPending {
		if now.Sub(entry.created) > trialPendingTTL {
			delete(e.watch.trialPending, path)
			// Clear the stale sync_failure — this trial candidate was never
			// intercepted by the planner (item may have been deleted). Best-effort.
			if err := e.baseline.ClearSyncFailureByPath(context.Background(), path); err != nil {
				e.logger.Debug("cleanStaleTrialPending: failed to clear stale failure",
					slog.String("path", path),
					slog.String("error", err.Error()),
				)
			}
		}
	}
}

// recordError increments the failed counter and appends the error to the
// diagnostic error list.
func (e *Engine) recordError(r *synctypes.WorkerResult) {
	e.failed.Add(1)
	if r.Err != nil {
		e.syncErrorsMu.Lock()
		e.syncErrors = append(e.syncErrors, r.Err)
		e.syncErrorsMu.Unlock()
	}
}

// logFailureSummary logs an aggregated summary of sync errors from the
// current pass. Groups errors by message prefix (first 80 chars) and logs
// one WARN per group with count + sample paths when count > 10, or per-item
// WARN otherwise. Mirrors the scanner aggregation pattern in
// recordSkippedItems (R-6.6.12). Resets syncErrors after logging.
func (e *Engine) logFailureSummary() {
	e.syncErrorsMu.Lock()
	errs := e.syncErrors
	e.syncErrors = nil
	e.syncErrorsMu.Unlock()

	if len(errs) == 0 {
		return
	}

	// Group by error message for aggregation. Use the first errorGroupKeyLen
	// chars of the error message as the group key — detailed enough to
	// distinguish issue types without creating too many groups.
	const errorGroupKeyLen = 80
	type group struct {
		msgs  []string
		count int
	}
	groups := make(map[string]*group)
	for _, err := range errs {
		msg := err.Error()
		key := msg
		if len(key) > errorGroupKeyLen {
			key = key[:errorGroupKeyLen]
		}
		g, ok := groups[key]
		if !ok {
			g = &group{}
			groups[key] = g
		}
		g.count++
		// Keep first 3 unique messages as samples.
		const sampleCount = 3
		if len(g.msgs) < sampleCount {
			g.msgs = append(g.msgs, msg)
		}
	}

	const aggregateThreshold = 10
	for key, g := range groups {
		if g.count > aggregateThreshold {
			e.logger.Warn("sync failures (aggregated)",
				slog.String("error_prefix", key),
				slog.Int("count", g.count),
				slog.Any("samples", g.msgs),
			)
		} else {
			for _, msg := range g.msgs {
				e.logger.Warn("sync failure",
					slog.String("error", msg),
				)
			}
		}
	}
}

// clearFailureOnSuccess removes the sync_failures row for a successfully
// completed action. The engine owns failure lifecycle — store_baseline's
// CommitOutcome handles only baseline/remote_state updates (D-6).
func (e *Engine) clearFailureOnSuccess(ctx context.Context, r *synctypes.WorkerResult) {
	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	if clearErr := e.baseline.ClearSyncFailure(ctx, r.Path, driveID); clearErr != nil {
		e.logger.Warn("failed to clear sync failure on success",
			slog.String("path", r.Path),
			slog.String("error", clearErr.Error()),
		)
	}
}

// recordFailure writes a failure to sync_failures with the given delay
// function for computing next_retry_at. For transient failures, pass
// retry.Reconcile.Delay; for actionable/fatal, pass nil (no retry).
func (e *Engine) recordFailure(ctx context.Context, r *synctypes.WorkerResult, delayFn func(int) time.Duration) {
	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	// The engine's routing already classifies each result — delayFn is non-nil
	// for transient failures (retryable) and nil for actionable/fatal ones.
	category := synctypes.CategoryTransient
	if delayFn == nil {
		category = synctypes.CategoryActionable
	}

	issueType := issueTypeForHTTPStatus(r.HTTPStatus, r.Err)
	scopeKey := deriveScopeKey(r)

	if recErr := e.baseline.RecordFailure(ctx, &synctypes.SyncFailureParams{
		Path:       r.Path,
		DriveID:    driveID,
		Direction:  direction,
		Category:   category,
		IssueType:  issueType,
		ErrMsg:     r.ErrMsg,
		HTTPStatus: r.HTTPStatus,
		ScopeKey:   scopeKey,
	}, delayFn); recErr != nil {
		e.logger.Warn("failed to record failure",
			slog.String("path", r.Path),
			slog.String("error", recErr.Error()),
		)

		return
	}

	// Per-item failure detail at DEBUG. Bulk sync logs individual items at
	// DEBUG and aggregates at WARN in logFailureSummary (R-6.6.10).
	e.logger.Debug("sync failure recorded",
		slog.String("path", r.Path),
		slog.String("action", r.ActionType.String()),
		slog.Int("http_status", r.HTTPStatus),
		slog.String("error", r.ErrMsg),
		slog.String("scope_key", scopeKey.String()),
	)
}

// deriveScopeKey returns the scope key for a failed worker result.
// deriveScopeKey maps a worker result to its typed scope key. Delegates to
// synctypes.ScopeKeyForStatus — single source of truth for HTTP status → scope key
// mapping. Returns the zero-value synctypes.ScopeKey for non-scope statuses.
func deriveScopeKey(r *synctypes.WorkerResult) synctypes.ScopeKey {
	return synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
}

// issueTypeForHTTPStatus maps an HTTP status code and error to a sync
// failure issue type. Used by recordFailure to populate the issue_type
// column. Returns empty string for generic/unknown failures.
func issueTypeForHTTPStatus(httpStatus int, err error) string {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		return synctypes.IssueRateLimited
	case httpStatus == http.StatusInsufficientStorage:
		return synctypes.IssueQuotaExceeded
	case httpStatus == http.StatusForbidden:
		return synctypes.IssuePermissionDenied
	case httpStatus == http.StatusBadRequest && isOutagePattern(err):
		return synctypes.IssueServiceOutage
	case httpStatus >= http.StatusInternalServerError:
		return synctypes.IssueServiceOutage
	case httpStatus == http.StatusRequestTimeout:
		return "request_timeout"
	case httpStatus == http.StatusPreconditionFailed:
		return "transient_conflict"
	case httpStatus == http.StatusNotFound:
		return "transient_not_found"
	case httpStatus == http.StatusLocked:
		return "resource_locked"
	case errors.Is(err, driveops.ErrDiskFull):
		return synctypes.IssueDiskFull
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return synctypes.IssueFileTooLargeForSpace
	case errors.Is(err, os.ErrPermission):
		return synctypes.IssueLocalPermissionDenied
	default:
		return ""
	}
}

// nowFunc returns the current time from the engine's injectable clock.
// Always set by NewEngine; tests overwrite with a controllable clock.
func (e *Engine) nowFunc() time.Time {
	return e.nowFn()
}

// resultStats returns the engine-owned counters and error list.
func (e *Engine) resultStats() (succeeded, failed int, errs []error) {
	e.syncErrorsMu.Lock()
	errs = make([]error, len(e.syncErrors))
	copy(errs, e.syncErrors)
	e.syncErrorsMu.Unlock()
	return int(e.succeeded.Load()), int(e.failed.Load()), errs
}

// resetResultStats resets the engine-owned counters for a new pass.
func (e *Engine) resetResultStats() {
	e.succeeded.Store(0)
	e.failed.Store(0)
	e.syncErrorsMu.Lock()
	e.syncErrors = nil
	e.syncErrorsMu.Unlock()
}

// directionFromAction maps a synctypes.ActionType to a typed Direction enum.
// All ActionType values are explicitly covered — no default case, so the
// exhaustive linter catches any new ActionType values added in the future.
func directionFromAction(at synctypes.ActionType) synctypes.Direction {
	switch at {
	case synctypes.ActionUpload:
		return synctypes.DirectionUpload
	case synctypes.ActionDownload, synctypes.ActionFolderCreate, synctypes.ActionConflict:
		return synctypes.DirectionDownload
	case synctypes.ActionLocalDelete, synctypes.ActionRemoteDelete:
		return synctypes.DirectionDelete
	case synctypes.ActionLocalMove, synctypes.ActionRemoteMove,
		synctypes.ActionUpdateSynced, synctypes.ActionCleanup:
		// Metadata ops default to download direction for failure recording.
		return synctypes.DirectionDownload
	}
	// Unreachable — all ActionType values are covered above.
	return synctypes.DirectionDownload
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all scope blocks. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the drain loop's select
// watches the old timer's channel after replacement. Called after scope blocks
// are created, trials dispatched, or trial results processed (R-2.10.5).
func (e *Engine) armTrialTimer() {
	if e.watch == nil {
		return
	}

	e.watch.trialMu.Lock()
	defer e.watch.trialMu.Unlock()

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}

	earliest, ok := e.watch.scopeGate.EarliestTrialAt()
	if !ok {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		delay = 1 * time.Millisecond // fire immediately
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// drainWorkerResults calls AllDueTrials, so even if a second AfterFunc
	// fires while a signal is pending, all due scopes are still processed
	// on the next drain iteration.
	e.watch.trialTimer = time.AfterFunc(delay, func() {
		select {
		case e.trialCh <- struct{}{}:
		default:
		}
	})
}

// trialTimerChan returns the persistent trial notification channel.
// time.AfterFunc sends to this channel when a trial timer fires.
// The channel is always non-nil after NewEngine.
func (e *Engine) trialTimerChan() <-chan struct{} {
	return e.trialCh
}

// stopTrialTimer stops and clears the trial timer. Called on shutdown.
func (e *Engine) stopTrialTimer() {
	if e.watch == nil {
		return
	}

	e.watch.trialMu.Lock()
	defer e.watch.trialMu.Unlock()

	if e.watch.trialTimer != nil {
		e.watch.trialTimer.Stop()
		e.watch.trialTimer = nil
	}
}

// drainWorkerResults reads from the worker result channel and processes
// results using failure-aware dependent dispatch. Uses the actor-with-outbox
// pattern (§3.5): ready dependents go to an in-memory outbox, and the select
// interleaves outbox draining with result processing to prevent deadlock
// when Complete returns many dependents and readyCh is full.
//
// The trial timer fires scope trial dispatch when trials become due (R-2.10.5).
// In one-shot mode (scopeGate == nil), trial dispatch is a no-op.
func (e *Engine) drainWorkerResults(ctx context.Context, results <-chan synctypes.WorkerResult, bl *synctypes.Baseline) {
	defer e.stopTrialTimer()

	var outbox []*synctypes.TrackedAction

	for {
		// Actor-with-outbox: if outbox has items, include a send case for
		// readyCh. The nil channel pattern disables the case when outbox is
		// empty — Go's select on a nil channel blocks, effectively removing
		// it from the select.
		var outCh chan<- *synctypes.TrackedAction
		var outVal *synctypes.TrackedAction
		if len(outbox) > 0 {
			outCh = e.readyCh
			outVal = outbox[0]
		}

		select {
		case outCh <- outVal:
			// Drained one item from outbox to readyCh.
			outbox = outbox[1:]

		case r, ok := <-results:
			if !ok {
				return
			}

			dispatched := e.processWorkerResult(ctx, &r, bl)
			outbox = append(outbox, dispatched...)

		case <-e.trialTimerChan():
			e.handleTrialTimer(ctx)

		case <-e.retryTimerChan():
			e.handleRetryTimer(ctx)

		case <-ctx.Done():
			return
		}
	}
}

// handleTrialTimer dispatches due scope trials via ScopeGate.
// In one-shot mode (scopeGate == nil), this is a no-op.
func (e *Engine) handleTrialTimer(ctx context.Context) {
	if e.watch != nil {
		e.runTrialDispatch(ctx)
	}
}

// handleRetryTimer runs a retrier sweep for due sync_failures. Only active
// when depGraph is initialized (watch mode with new architecture).
func (e *Engine) handleRetryTimer(ctx context.Context) {
	if e.depGraph != nil {
		e.runRetrierSweep(ctx)
	}
}

// startObservers launches remote and local observer goroutines that feed
// events into the buffer. Returns an error channel for observer failures and
// the number of observers started. The events channel is closed automatically
// when all observers exit, allowing the bridge goroutine to drain cleanly.
func (e *Engine) startObservers(
	ctx context.Context, bl *synctypes.Baseline, mode synctypes.SyncMode, buf *syncobserve.Buffer, opts synctypes.WatchOpts,
) (<-chan error, int, <-chan []synctypes.SkippedItem) {
	events := make(chan synctypes.ChangeEvent, watchEventBuf)
	errs := make(chan error, 2)

	var obsWg stdsync.WaitGroup

	// Bridge goroutine: reads from shared events channel, adds to buffer.
	// Exits when events is closed (all observers done) or ctx canceled.
	go func() {
		for {
			select {
			case ev, ok := <-events:
				if !ok {
					return
				}

				buf.Add(&ev)
			case <-ctx.Done():
				return
			}
		}
	}()

	count := 0

	// Remote observer (skip for upload-only mode).
	if mode != synctypes.SyncUploadOnly {
		remoteObs := syncobserve.NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)
		remoteObs.SetObsWriter(e.baseline)
		e.watch.remoteObs = remoteObs

		savedToken, tokenErr := e.baseline.GetDeltaToken(ctx, e.driveID.String(), "")
		if tokenErr != nil {
			e.logger.Warn("failed to get delta token for watch",
				slog.String("error", tokenErr.Error()),
			)
		}

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()
			errs <- remoteObs.Watch(ctx, savedToken, events, e.resolvePollInterval(opts))
		}()
	}

	// Channel for forwarding SkippedItems from safety scans to the engine.
	// Buffered(2) — at most 2 safety scans could overlap before draining.
	skippedCh := make(chan []synctypes.SkippedItem, 2)

	// Local observer (skip for download-only mode).
	if mode != synctypes.SyncDownloadOnly {
		localObs := syncobserve.NewLocalObserver(bl, e.logger, e.checkWorkers)
		localObs.SetSafetyScanInterval(opts.SafetyScanInterval)
		localObs.SetSkippedChannel(skippedCh)

		if e.localWatcherFactory != nil {
			localObs.SetWatcherFactory(e.localWatcherFactory)
		}

		e.watch.localObs = localObs

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()

			watchErr := localObs.Watch(ctx, e.syncRoot, events)
			if errors.Is(watchErr, synctypes.ErrWatchLimitExhausted) {
				e.logger.Warn("inotify watch limit exhausted, falling back to periodic full scan",
					slog.Duration("poll_interval", e.resolvePollInterval(opts)),
				)

				e.runPeriodicFullScan(ctx, localObs, e.syncRoot, events, e.resolvePollInterval(opts))
				errs <- nil // clean exit after context cancel

				return
			}

			errs <- watchErr
		}()
	}

	// Close events channel when all observers exit so the bridge goroutine
	// drains remaining events and exits cleanly.
	go func() {
		obsWg.Wait()
		close(events)
	}()

	return errs, count, skippedCh
}

// runPeriodicFullScan runs periodic full filesystem scans as a fallback when
// inotify watch limits are exhausted. Blocks until the context is canceled.
// Each scan's events are forwarded to the events channel via trySend.
func (e *Engine) runPeriodicFullScan(
	ctx context.Context, obs *syncobserve.LocalObserver, syncRoot string,
	events chan<- synctypes.ChangeEvent, interval time.Duration,
) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	e.logger.Info("periodic full scan fallback started",
		slog.Duration("interval", interval),
	)

	for {
		select {
		case <-ticker.C:
			// Jitter: sleep 0-10% of interval to prevent thundering-herd
			// when multiple drives fire periodic scans simultaneously.
			if jitter := interval / periodicScanJitterDivisor; jitter > 0 {
				time.Sleep(rand.N(jitter)) //nolint:gosec // non-cryptographic jitter for I/O scheduling
			}

			result, err := obs.FullScan(ctx, syncRoot)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				e.logger.Warn("periodic full scan failed",
					slog.String("error", err.Error()),
				)

				continue
			}

			// Forward events only — skipped items are logged at DEBUG.
			// The primary scan and safety scan handle recording to sync_failures.
			for i := range result.Events {
				obs.TrySend(ctx, events, &result.Events[i])
			}

			if len(result.Skipped) > 0 {
				e.logger.Debug("periodic scan: skipped items",
					slog.Int("count", len(result.Skipped)))
			}
		case <-ctx.Done():
			return
		}
	}
}

// processBatch plans and dispatches a batch of path changes. On planner
// error (e.g. big-delete protection), the batch is skipped and the loop
// continues. In-flight actions for overlapping paths are canceled and
// replaced (B-122 deduplication).
func (e *Engine) processBatch(
	ctx context.Context, batch []synctypes.PathChanges, bl *synctypes.Baseline,
	mode synctypes.SyncMode, safety *synctypes.SafetyConfig,
) {
	e.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)

	e.periodicPermRecheck(ctx, bl)

	// R-2.10.10: use scanner output as proof-of-accessibility to clear
	// permission denials for paths observed in this batch.
	e.permHandler.clearScannerResolvedPermissions(ctx, pathSetFromBatch(batch))

	denied := e.permHandler.DeniedPrefixes()
	plan, err := e.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, synctypes.ErrBigDeleteTriggered) {
			e.logger.Warn("big-delete protection triggered, skipping batch",
				slog.Int("paths", len(batch)),
			)

			return
		}

		e.logger.Error("planner error, skipping batch",
			slog.String("error", err.Error()),
		)

		return
	}

	if len(plan.Actions) == 0 {
		e.logger.Debug("empty plan for batch, nothing to do")
		return
	}

	// Rolling-window big-delete protection: count planned deletes and
	// filter them out if the counter trips. Non-delete actions continue
	// flowing. The planner-level check is disabled in watch mode
	// (threshold=MaxInt32) — this counter replaces it.
	if e.watch != nil && e.watch.deleteCounter != nil {
		plan = e.applyDeleteCounter(ctx, plan)
		if len(plan.Actions) == 0 {
			return
		}
	}

	e.deduplicateInFlight(plan)
	e.dispatchBatchActions(ctx, plan)
}

// periodicPermRecheck runs permission rechecks at most once per 60 seconds.
// Throttled to avoid API hammering (R-2.10.9).
func (e *Engine) periodicPermRecheck(ctx context.Context, bl *synctypes.Baseline) {
	const permRecheckInterval = 60 * time.Second

	now := time.Now()
	if now.Sub(e.watch.lastPermRecheck) < permRecheckInterval {
		return
	}

	e.watch.lastPermRecheck = now

	// recheckPermissions calls the Graph API — skip during outage or
	// throttle to avoid wasting API calls (R-2.10.30). Local permission
	// rechecks (filesystem-only) proceed regardless.
	if e.permHandler.HasPermChecker() && !e.isObservationSuppressed() {
		shortcuts, err := e.baseline.ListShortcuts(ctx)
		if err == nil {
			e.permHandler.recheckPermissions(ctx, bl, shortcuts)
		}
	}

	e.permHandler.recheckLocalPermissions(ctx)
}

// deduplicateInFlight cancels in-flight actions for paths that appear in the
// plan. B-122: newer observation supersedes in-progress action.
func (e *Engine) deduplicateInFlight(plan *synctypes.ActionPlan) {
	for i := range plan.Actions {
		if e.depGraph.HasInFlight(plan.Actions[i].Path) {
			e.logger.Info("canceling in-flight action for updated path",
				slog.String("path", plan.Actions[i].Path),
			)

			e.depGraph.CancelByPath(plan.Actions[i].Path)
		}
	}
}

// dispatchBatchActions adds plan actions to the DepGraph with monotonic IDs,
// then admits ready actions through the scope gate.
func (e *Engine) dispatchBatchActions(ctx context.Context, plan *synctypes.ActionPlan) {
	// Invariant: Planner always builds Deps with len(Actions).
	if len(plan.Actions) != len(plan.Deps) {
		e.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return
	}

	// Allocate monotonic action IDs for this batch. Using a global atomic
	// counter prevents ID collisions across batches — loop indices (int64(i))
	// would collide when multiple batches are processed.
	batchBaseID := e.watch.nextActionID.Add(int64(len(plan.Actions))) - int64(len(plan.Actions))

	// Map from plan index → action ID for dependency resolution.
	actionIDs := make([]int64, len(plan.Actions))
	for i := range plan.Actions {
		actionIDs[i] = batchBaseID + int64(i)
	}

	// Add actions to DepGraph and collect immediately-ready ones. Dispatch
	// transitions (setDispatch) are deferred to admitAndDispatch, which runs
	// AFTER scope gate checks — setDispatch on a scope-blocked action would
	// be incorrect (section 2.2: no dispatch before admission).
	var ready []*synctypes.TrackedAction

	for i := range plan.Actions {
		id := actionIDs[i]

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, actionIDs[depIdx])
		}

		if ta := e.depGraph.Add(&plan.Actions[i], id, depIDs); ta != nil {
			ready = append(ready, ta)
		}
	}

	// Admit ready actions through the scope gate and send to workers.
	// admitAndDispatch handles trial interception, scope blocking, and
	// setDispatch for admitted actions.
	if len(ready) > 0 {
		e.admitAndDispatch(ctx, ready)
	}

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
	)
}

// setDispatch writes the dispatch state transition for an action before it
// enters the tracker. Only applies to downloads and local deletes (the action
// types that have remote_state lifecycle).
func (e *Engine) setDispatch(ctx context.Context, action *synctypes.Action) {
	if err := e.baseline.SetDispatchStatus(ctx, action.DriveID.String(), action.ItemID, action.Type); err != nil {
		e.logger.Warn("failed to set dispatch status",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}

// resolvePollInterval returns the configured poll interval or the default.
func (e *Engine) resolvePollInterval(opts synctypes.WatchOpts) time.Duration {
	if opts.PollInterval > 0 {
		return opts.PollInterval
	}

	return defaultPollInterval
}

// resolveDebounce returns the configured debounce or the default.
func (e *Engine) resolveDebounce(opts synctypes.WatchOpts) time.Duration {
	if opts.Debounce > 0 {
		return opts.Debounce
	}

	return defaultDebounce
}

// isDeleteAction returns true if the action type is a local or remote delete.
func isDeleteAction(t synctypes.ActionType) bool {
	return t == synctypes.ActionLocalDelete || t == synctypes.ActionRemoteDelete
}

// applyDeleteCounter counts planned deletes in the plan, feeds them to the
// rolling counter, and — if the counter is held — filters delete actions out
// of the plan and records them as actionable issues. Returns the (possibly
// filtered) plan. When all actions are filtered, returns a plan with empty
// Actions/Deps.
func (e *Engine) applyDeleteCounter(ctx context.Context, plan *synctypes.ActionPlan) *synctypes.ActionPlan {
	// Count planned deletes.
	deleteCount := 0
	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			deleteCount++
		}
	}

	if deleteCount == 0 {
		return plan
	}

	// Feed to the rolling counter. tripped=true means this call caused
	// the first transition from not-held → held.
	tripped := e.watch.deleteCounter.Add(deleteCount)
	if tripped {
		e.logger.Warn("big-delete protection triggered in watch mode",
			slog.Int("delete_count", e.watch.deleteCounter.Count()),
			slog.Int("threshold", e.watch.deleteCounter.Threshold()),
		)
	}

	if !e.watch.deleteCounter.IsHeld() {
		return plan
	}

	// Filter: separate deletes from non-deletes and rebuild the plan.
	// Dependency indices must be remapped to the new action positions.
	kept := make([]synctypes.Action, 0, len(plan.Actions))
	keptDeps := make([][]int, 0, len(plan.Deps))
	oldToNew := make(map[int]int, len(plan.Actions))

	var heldDeletes []synctypes.Action

	for i := range plan.Actions {
		if isDeleteAction(plan.Actions[i].Type) {
			heldDeletes = append(heldDeletes, plan.Actions[i])
			continue
		}

		oldToNew[i] = len(kept)
		kept = append(kept, plan.Actions[i])
		keptDeps = append(keptDeps, nil) // placeholder, remap below
	}

	// Remap dependency indices for kept actions. Drop deps pointing to
	// filtered-out (delete) actions — the non-delete action can proceed
	// independently since the delete won't run.
	for newIdx := range kept {
		// Find the original index by scanning oldToNew (small N, fast enough).
		var origIdx int
		for oi, ni := range oldToNew {
			if ni == newIdx {
				origIdx = oi
				break
			}
		}

		for _, depOld := range plan.Deps[origIdx] {
			if depNew, ok := oldToNew[depOld]; ok {
				keptDeps[newIdx] = append(keptDeps[newIdx], depNew)
			}
		}
	}

	plan.Actions = kept
	plan.Deps = keptDeps

	// Record held deletes as actionable issues for user visibility.
	e.recordHeldDeletes(ctx, heldDeletes)

	return plan
}

// recordHeldDeletes writes held delete actions to sync_failures as actionable
// issues with type big_delete_held. Uses UpsertActionableFailures for batch
// upsert — idempotent when the same deletes are re-observed.
func (e *Engine) recordHeldDeletes(ctx context.Context, actions []synctypes.Action) {
	if len(actions) == 0 {
		return
	}

	failures := make([]synctypes.ActionableFailure, len(actions))
	for i := range actions {
		failures[i] = synctypes.ActionableFailure{
			Path:      actions[i].Path,
			DriveID:   actions[i].DriveID,
			Direction: synctypes.DirectionDelete,
			IssueType: synctypes.IssueBigDeleteHeld,
			Error:     fmt.Sprintf("held by big-delete protection (threshold: %d)", e.bigDeleteThreshold),
		}
	}

	if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
		e.logger.Error("failed to record held deletes",
			slog.Int("count", len(failures)),
			slog.String("error", err.Error()),
		)
	}

	e.logger.Info("held delete actions recorded as issues",
		slog.Int("count", len(failures)),
	)
}

// externalDBChanged checks whether another process (e.g., the CLI) wrote to
// the database since the last check. Uses PRAGMA data_version — changes every
// time another connection commits a write. The engine's own writes don't
// change it. Returns true if the version advanced.
func (e *Engine) externalDBChanged(ctx context.Context) bool {
	dv, err := e.baseline.DataVersion(ctx)
	if err != nil {
		e.logger.Warn("failed to check data_version",
			slog.String("error", err.Error()),
		)

		return false
	}

	if dv == e.watch.lastDataVersion {
		return false
	}

	e.watch.lastDataVersion = dv

	return true
}

// handleExternalChanges reacts to external DB modifications detected via
// PRAGMA data_version. Currently handles big-delete clearance: if the
// counter is held but all big_delete_held rows have been cleared (via
// `issues clear`), releases the counter so deletes resume on the next
// observation cycle.
// handleRecheckTick processes a recheck timer tick: detects external DB
// changes (e.g., `issues clear`) and logs a watch summary.
func (e *Engine) handleRecheckTick(ctx context.Context) {
	if e.externalDBChanged(ctx) {
		e.handleExternalChanges(ctx)
	}

	e.logWatchSummary(ctx)
}

func (e *Engine) handleExternalChanges(ctx context.Context) {
	// Big-delete clearance: check if user approved held deletes.
	if e.watch != nil && e.watch.deleteCounter != nil && e.watch.deleteCounter.IsHeld() {
		rows, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueBigDeleteHeld)
		if err != nil {
			e.logger.Warn("failed to check big-delete-held entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			e.watch.deleteCounter.Release()
			e.logger.Info("big-delete protection cleared by user")
		}
	}

	// Permission clearance: if user cleared perm:dir failures via CLI,
	// release the corresponding in-memory scope blocks.
	e.clearResolvedPermissionScopes(ctx)
}

// clearResolvedPermissionScopes checks if any perm:dir scope blocks have
// had their sync_failures cleared (by user action via CLI), and releases
// the corresponding scope blocks.
func (e *Engine) clearResolvedPermissionScopes(ctx context.Context) {
	if e.watch == nil {
		return
	}

	scopeKeys := e.watch.scopeGate.ScopeBlockKeys()
	if len(scopeKeys) == 0 {
		return
	}

	issues, err := e.baseline.ListSyncFailuresByIssueType(ctx, synctypes.IssueLocalPermissionDenied)
	if err != nil {
		e.logger.Warn("failed to check permission failures",
			slog.String("error", err.Error()),
		)

		return
	}

	// Build set of still-active scope keys from DB.
	activeScopes := make(map[synctypes.ScopeKey]bool, len(issues))
	for i := range issues {
		if issues[i].ScopeKey.IsPermDir() {
			activeScopes[issues[i].ScopeKey] = true
		}
	}

	// Release any scope blocks whose failures were cleared.
	for _, key := range scopeKeys {
		if key.IsPermDir() && !activeScopes[key] {
			e.onScopeClear(ctx, key)

			e.logger.Info("permission scope block cleared by user",
				slog.String("scope", key.String()),
			)
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of actionable issues
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (e *Engine) logWatchSummary(ctx context.Context) {
	issues, err := e.baseline.ListActionableFailures(ctx)
	if err != nil || len(issues) == 0 {
		if e.watch.lastSummaryTotal != 0 {
			e.watch.lastSummaryTotal = 0
		}

		return
	}

	// Only log if count changed since last summary.
	if len(issues) == e.watch.lastSummaryTotal {
		return
	}

	e.watch.lastSummaryTotal = len(issues)

	// Group by issue_type, emit one-liner.
	counts := make(map[string]int)
	for i := range issues {
		counts[issues[i].IssueType]++
	}

	parts := make([]string, 0, len(counts))
	for typ, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, typ))
	}

	sort.Strings(parts)

	e.logger.Warn("actionable issues",
		slog.Int("total", len(issues)),
		slog.String("breakdown", strings.Join(parts, ", ")),
	)
}

// recordSkippedItems records observation-time rejections (invalid names,
// path too long, file too large) as actionable failures in sync_failures.
// Groups items by issue type and uses UpsertActionableFailures for efficient
// batch upserts. Aggregated logging: >10 same-type items → 1 WARN with
// count + sample paths; ≤10 → per-file WARN.
func (e *Engine) recordSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	if len(skipped) == 0 {
		return
	}

	// Group by issue type for batch upsert and aggregated logging.
	byReason := make(map[string][]synctypes.SkippedItem)
	for i := range skipped {
		byReason[skipped[i].Reason] = append(byReason[skipped[i].Reason], skipped[i])
	}

	for reason, items := range byReason {
		// Aggregated logging.
		const aggregateThreshold = 10
		if len(items) > aggregateThreshold {
			// Log summary with sample paths.
			const sampleCount = 3
			samples := make([]string, 0, sampleCount)
			for i := range items {
				if i >= sampleCount {
					break
				}
				samples = append(samples, items[i].Path)
			}

			e.logger.Warn("observation filter: skipped files",
				slog.String("issue_type", reason),
				slog.Int("count", len(items)),
				slog.Any("sample_paths", samples),
			)
		} else {
			for i := range items {
				e.logger.Warn("observation filter: skipped file",
					slog.String("path", items[i].Path),
					slog.String("issue_type", reason),
					slog.String("detail", items[i].Detail),
				)
			}
		}

		// Build synctypes.ActionableFailure slice for batch upsert.
		failures := make([]synctypes.ActionableFailure, len(items))
		for i := range items {
			failures[i] = synctypes.ActionableFailure{
				Path:      items[i].Path,
				DriveID:   e.driveID,
				Direction: synctypes.DirectionUpload,
				IssueType: reason,
				Error:     items[i].Detail,
				FileSize:  items[i].FileSize,
			}
		}

		if err := e.baseline.UpsertActionableFailures(ctx, failures); err != nil {
			e.logger.Error("failed to record skipped items",
				slog.String("issue_type", reason),
				slog.Int("count", len(failures)),
				slog.String("error", err.Error()),
			)
		}
	}
}

// clearResolvedSkippedItems removes sync_failures entries for scanner-detectable
// issue types that are no longer present in the current scan. For example, if a
// user renames an invalid file to a valid name, the old failure is auto-cleared.
func (e *Engine) clearResolvedSkippedItems(ctx context.Context, skipped []synctypes.SkippedItem) {
	// Collect current paths per scanner-detectable issue type.
	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	// For each scanner-detectable issue type, clear entries not in the current scan.
	// If no items of that type were found, pass empty slice (clears all of that type).
	scannerIssueTypes := []string{
		synctypes.IssueInvalidFilename, synctypes.IssuePathTooLong,
		synctypes.IssueFileTooLarge, synctypes.IssueCaseCollision,
	}
	for _, issueType := range scannerIssueTypes {
		paths := currentByType[issueType] // nil if no items — that's fine (clears all)
		if err := e.baseline.ClearResolvedActionableFailures(ctx, issueType, paths); err != nil {
			e.logger.Error("failed to clear resolved failures",
				slog.String("issue_type", issueType),
				slog.String("error", err.Error()),
			)
		}
	}
}

// minReconcileInterval is the minimum allowed reconcile interval. A full
// enumeration of 100K items costs ~500 API calls; anything under 15 minutes
// risks rate-limit exhaustion.
const minReconcileInterval = 15 * time.Minute

// resolveReconcileInterval returns the configured reconcile interval or the
// default. Negative values disable periodic reconciliation. Values below
// minReconcileInterval are clamped up.
func (e *Engine) resolveReconcileInterval(opts synctypes.WatchOpts) time.Duration {
	if opts.ReconcileInterval < 0 {
		return 0 // disabled
	}

	if opts.ReconcileInterval > 0 {
		if opts.ReconcileInterval < minReconcileInterval {
			e.logger.Warn("reconcile interval below minimum, clamping",
				slog.Duration("requested", opts.ReconcileInterval),
				slog.Duration("minimum", minReconcileInterval),
			)

			return minReconcileInterval
		}

		return opts.ReconcileInterval
	}

	return defaultReconcileInterval
}

// newReconcileTicker creates a ticker for periodic reconciliation. Returns
// nil if the interval is 0 (disabled).
func (e *Engine) newReconcileTicker(interval time.Duration) *time.Ticker {
	if interval <= 0 {
		return nil
	}

	return time.NewTicker(interval)
}

// initReconcileTicker creates the periodic full-reconciliation timer and
// returns its channel plus a stop function. If reconciliation is disabled,
// both the channel and stop function are nil.
func (e *Engine) initReconcileTicker(opts synctypes.WatchOpts) (<-chan time.Time, func()) {
	interval := e.resolveReconcileInterval(opts)
	ticker := e.newReconcileTicker(interval)

	if ticker == nil {
		return nil, nil
	}

	e.logger.Info("periodic full reconciliation enabled",
		slog.Duration("interval", interval),
	)

	return ticker.C, ticker.Stop
}

// runFullReconciliationAsync spawns a goroutine for full delta enumeration +
// orphan detection. Non-blocking — the watch loop continues processing events
// while reconciliation runs. Events are fed into the watch buffer so they flow
// through the normal pipeline (FlushDebounced → processBatch).
//
// Concurrency safety:
//   - reconcileRunning atomic.Bool prevents overlapping reconciliations
//   - SQLite WAL mode serializes CommitObservation + CommitOutcome
//   - Buffer.Add is mutex-protected (buffer.go:41)
//   - Planner is idempotent on duplicate events
//   - DepGraph.HasInFlight + CancelByPath prevent duplicate dispatch
func (e *Engine) runFullReconciliationAsync(ctx context.Context, bl *synctypes.Baseline) {
	if !e.watch.reconcileRunning.CompareAndSwap(false, true) {
		e.logger.Info("full reconciliation skipped — previous still running")
		return
	}

	go func() {
		defer e.watch.reconcileRunning.Store(false)

		start := time.Now()
		e.logger.Info("periodic full reconciliation starting")

		events, deltaToken, err := e.observeRemoteFull(ctx, bl)
		if err != nil {
			// Suppress error logging during shutdown — context cancellation
			// is expected when the daemon is stopping.
			if ctx.Err() == nil {
				e.logger.Error("full reconciliation failed",
					slog.String("error", err.Error()),
				)
			}

			return
		}

		// Commit observations and delta token.
		observed := changeEventsToObservedItems(e.logger, events)
		if commitErr := e.baseline.CommitObservation(
			ctx, observed, deltaToken, e.driveID,
		); commitErr != nil {
			e.logger.Error("failed to commit full reconciliation observations",
				slog.String("error", commitErr.Error()),
			)

			return
		}

		if e.watch.afterReconcileCommit != nil {
			e.watch.afterReconcileCommit()
		}

		// Observations are durably committed. If we're shutting down, skip
		// feeding events to the buffer — the watch loop is also stopping and
		// won't process them. Next startup will re-observe idempotently.
		if ctx.Err() != nil {
			e.logger.Info("full reconciliation: observations committed, stopping for shutdown")
			return
		}

		// Filter out shortcut events and process shortcut scopes.
		events = filterOutShortcuts(events)

		shortcutEvents, scErr := e.reconcileShortcutScopes(ctx, bl)
		if scErr != nil {
			e.logger.Warn("shortcut reconciliation failed during full reconciliation",
				slog.String("error", scErr.Error()),
			)
		}

		events = append(events, shortcutEvents...)

		if len(events) == 0 {
			e.logger.Info("periodic full reconciliation complete: no changes",
				slog.Duration("duration", time.Since(start)),
			)
			return
		}

		// Feed events into the watch buffer — they flow through
		// FlushDebounced → processBatch in the watch loop. This avoids
		// calling processBatch directly from this goroutine, which would
		// race with the watch loop's own processBatch calls.
		for i := range events {
			e.watch.buf.Add(&events[i])
		}

		// Refresh watch shortcuts — reconcileShortcutScopes may have
		// added or removed shortcuts.
		if refreshed, refreshErr := e.baseline.ListShortcuts(ctx); refreshErr == nil {
			e.setShortcuts(refreshed)
		}

		e.logger.Info("periodic full reconciliation complete",
			slog.Int("events", len(events)),
			slog.Duration("duration", time.Since(start)),
		)
	}()
}
