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
	"sort"
	"strings"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// forceSafetyMax is the maximum threshold used when --force is set,
// effectively disabling big-delete protection.
const forceSafetyMax = math.MaxInt32

// periodicScanJitterDivisor controls the jitter window for periodic full
// scans. With a divisor of 10, each tick sleeps 0-10% of the interval to
// prevent thundering-herd I/O spikes in multi-drive mode.
const periodicScanJitterDivisor = 10

// EngineConfig holds the options for NewEngine. Uses a struct because
// seven fields is too many for positional parameters.
type EngineConfig struct {
	DBPath             string              // path to the SQLite state database
	SyncRoot           string              // absolute path to the local sync directory
	DataDir            string              // application data directory for session files (optional)
	DriveID            driveid.ID          // normalized drive identifier
	Fetcher            DeltaFetcher        // satisfied by *graph.Client
	Items              ItemClient          // satisfied by *graph.Client
	Downloads          driveops.Downloader // satisfied by *graph.Client
	Uploads            driveops.Uploader   // satisfied by *graph.Client
	DriveVerifier      DriveVerifier       // optional: verified at startup (B-074); nil skips check
	FolderDelta        FolderDeltaFetcher  // optional: folder-scoped delta for shortcut observation (6.4b)
	RecursiveLister    RecursiveLister     // optional: recursive listing for shortcut observation (6.4b)
	PermChecker        PermissionChecker   // optional: permission checking for shared folders (6.4c)
	Logger             *slog.Logger
	UseLocalTrash      bool  // move deleted local files to OS trash instead of permanent delete
	TransferWorkers    int   // goroutine count for the worker pool (0 → minWorkers)
	CheckWorkers       int   // goroutine limit for parallel file hashing (0 → 4)
	BigDeleteThreshold int   // max delete actions before big-delete protection triggers (0 → defaultBigDeleteThreshold)
	MinFreeSpace       int64 // minimum free disk space (bytes) before downloads; 0 disables (R-6.4.7)
}

// RunOpts holds per-pass options for RunOnce.
type RunOpts struct {
	DryRun        bool
	Force         bool
	FullReconcile bool // when true, runs a full delta enumeration + orphan detection
}

// SyncReport summarizes the result of a single sync pass.
type SyncReport struct {
	Mode     SyncMode
	DryRun   bool
	Duration time.Duration

	// Plan counts (always populated, even for dry-run).
	FolderCreates int
	Moves         int
	Downloads     int
	Uploads       int
	LocalDeletes  int
	RemoteDeletes int
	Conflicts     int
	SyncedUpdates int
	Cleanups      int

	// Execution results (zero for dry-run).
	Succeeded int
	Failed    int
	Errors    []error
}

// Engine orchestrates a complete sync pass: observe → plan → execute → commit.
// Single-drive only; multi-drive orchestration is handled by the Orchestrator.
type Engine struct {
	baseline           *SyncStore
	planner            *Planner
	execCfg            *ExecutorConfig
	fetcher            DeltaFetcher
	driveVerifier      DriveVerifier      // optional (B-074)
	folderDelta        FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister    RecursiveLister    // optional: for shortcut observation (6.4b)
	permChecker        PermissionChecker  // optional: for shared folder permission checks (6.4c)
	permCache          *permissionCache   // per-pass in-memory cache of folder→canWrite
	syncRoot           string
	driveID            driveid.ID
	logger             *slog.Logger
	remoteObs          *RemoteObserver        // stored during RunWatch for delta token reads
	localObs           *LocalObserver         // stored during RunWatch for drop counter reads
	sessionStore       *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers    int                    // goroutine count for the worker pool
	checkWorkers       int                    // goroutine limit for parallel file hashing
	bigDeleteThreshold int                    // from config; 0 means use default

	// Watch-mode big-delete protection: rolling counter + external change detection.
	deleteCounter   *deleteCounter // nil outside RunWatch
	lastDataVersion int64          // tracks PRAGMA data_version for CLI→daemon notification

	// watchShortcuts holds the latest shortcuts for use by the drain
	// goroutine in watch mode. Updated by the watch goroutine after
	// observation; read by drainWorkerResults for 403 handling.
	watchShortcuts   []Shortcut
	watchShortcutsMu stdsync.RWMutex

	// scopeState tracks scope-level failure detection (sliding windows)
	// and informs scope block decisions. Created per sync pass.
	scopeState *ScopeState

	// tracker is a reference to the active DepTracker, needed by
	// processWorkerResult to call Complete. Set during executePlan
	// (one-shot) or RunWatch (watch mode).
	tracker *DepTracker

	// retrier re-injects failed items from sync_failures into the pipeline.
	// nil in one-shot mode — failed items are retried on the next `onedrive sync`.
	retrier *FailureRetrier

	// trialTimer fires when the next scope trial is due (R-2.10.5).
	// Uses time.AfterFunc to send to trialCh — a persistent channel that
	// the drain loop always watches. This avoids a race where armTrialTimer
	// (called from onHeld on a different goroutine) replaces the timer and
	// the drain loop's select watches the old timer's channel.
	// Protected by trialMu. Nil when no trials are pending.
	trialTimer *time.Timer
	trialMu    stdsync.Mutex
	trialCh    chan struct{} // persistent, buffered(1); trial timer signals here

	// lastPermRecheck tracks the last time recheckPermissions was called
	// in watch mode, to throttle API calls to at most once per 60 seconds (R-2.10.9).
	lastPermRecheck time.Time

	// lastSummaryTotal caches the last actionable issue count to avoid
	// logging duplicate watch mode summaries on every recheck tick.
	lastSummaryTotal int

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
	localWatcherFactory func() (FsWatcher, error)
}

// NewEngine creates an Engine, initializing the SyncStore (which opens
// the SQLite database and runs migrations). Returns an error if DB init fails
// or if DriveID is zero (indicates a config/login issue).
func NewEngine(cfg *EngineConfig) (*Engine, error) {
	if cfg.DriveID.IsZero() {
		return nil, fmt.Errorf("sync: engine requires non-zero drive ID")
	}

	bm, err := NewSyncStore(cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	execCfg := NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, cfg.SyncRoot, cfg.DriveID, cfg.Logger)

	if cfg.UseLocalTrash {
		execCfg.trashFunc = defaultTrashFunc
	}

	// Construct sessionStore and TransferManager together so the TM is
	// immutable after creation (no post-hoc field mutation). Disk space
	// checking is configured via WithDiskCheck so every download (sync
	// and CLI) gets automatic protection (R-6.2.6).
	var sessionStore *driveops.SessionStore
	if cfg.DataDir != "" {
		sessionStore = driveops.NewSessionStore(cfg.DataDir, cfg.Logger)
	}

	execCfg.transferMgr = driveops.NewTransferManager(cfg.Downloads, cfg.Uploads, sessionStore, cfg.Logger,
		driveops.WithDiskCheck(cfg.MinFreeSpace, driveops.DiskAvailable),
	)

	// Default threshold if not set by config.
	bdThreshold := cfg.BigDeleteThreshold
	if bdThreshold == 0 {
		bdThreshold = defaultBigDeleteThreshold
	}

	return &Engine{
		baseline:           bm,
		planner:            NewPlanner(cfg.Logger),
		execCfg:            execCfg,
		fetcher:            cfg.Fetcher,
		driveVerifier:      cfg.DriveVerifier,
		folderDelta:        cfg.FolderDelta,
		recursiveLister:    cfg.RecursiveLister,
		permChecker:        cfg.PermChecker,
		permCache:          newPermissionCache(),
		sessionStore:       sessionStore,
		syncRoot:           cfg.SyncRoot,
		driveID:            cfg.DriveID,
		logger:             cfg.Logger,
		transferWorkers:    cfg.TransferWorkers,
		checkWorkers:       cfg.CheckWorkers,
		bigDeleteThreshold: bdThreshold,
		nowFn:              time.Now,
		trialCh:            make(chan struct{}, 1),
	}, nil
}

// Close releases resources held by the engine. Nil-safe for observer
// references set during RunWatch, cleans stale upload sessions, and
// closes the database connection last. Safe to call more than once.
func (e *Engine) Close() error {
	// Nil out observer references to prevent dangling reads after Close.
	e.remoteObs = nil
	e.localObs = nil

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
// API (B-074). Returns nil if no DriveVerifier is configured (optional check).
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
//  8. Build tracker, start worker pool
//  9. Wait for completion, commit delta token
func (e *Engine) RunOnce(ctx context.Context, mode SyncMode, opts RunOpts) (*SyncReport, error) {
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
	// Also creates sync_failures entries so the FailureRetrier can rediscover
	// items that were mid-execution when the crash occurred.
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
	if e.permChecker != nil && scErr == nil {
		e.recheckPermissions(ctx, bl, shortcuts)
	}

	// Recheck local permission denials — clear scope blocks for
	// directories that have become accessible since the last pass (R-2.10.13).
	e.recheckLocalPermissions(ctx)

	// Steps 2-4: Observe remote + local, buffer, and flush.
	changes, err := e.observeChanges(ctx, bl, mode, opts.DryRun, opts.FullReconcile)
	if err != nil {
		return nil, err
	}

	// Step 5: Early return if no changes.
	if len(changes) == 0 {
		e.logger.Info("sync pass complete: no changes detected",
			slog.Duration("duration", time.Since(start)),
		)

		report := &SyncReport{
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
	safety := e.resolveSafetyConfig(opts)
	denied := e.permCache.deniedPrefixes()

	plan, err := e.planner.Plan(changes, bl, mode, safety, denied)
	if err != nil {
		return nil, err
	}

	// Step 7: Build report from plan counts.
	counts := countByType(plan.Actions)
	report := buildReportFromCounts(counts, mode, opts)

	if opts.DryRun {
		report.Duration = time.Since(start)

		e.logger.Info("dry-run complete: no changes applied",
			slog.Duration("duration", report.Duration),
		)

		return report, nil
	}

	// Store shortcuts so the drain goroutine can access them via getWatchShortcuts.
	e.setWatchShortcuts(shortcuts)

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

// executePlan populates the dependency tracker and runs the worker pool.
// The engine processes results concurrently while workers run, classifying
// each result and calling tracker.Complete (R-6.8.9).
func (e *Engine) executePlan(
	ctx context.Context, plan *ActionPlan, report *SyncReport,
	bl *Baseline,
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
	// Scope detection is watch-mode only. In one-shot, feedScopeDetection
	// is nil-guarded (e.scopeState == nil → no-op).

	tracker := NewDepTracker(len(plan.Actions), e.logger)
	tracker.onHeld = func() { e.armTrialTimer() }
	e.tracker = tracker

	for i := range plan.Actions {
		id := int64(i)

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		// Dispatch state transition: pending/failed → in-progress.
		e.setDispatch(ctx, &plan.Actions[i])

		tracker.Add(&plan.Actions[i], id, depIDs)
	}

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, len(plan.Actions))
	pool.Start(ctx, e.transferWorkers)

	// Process results concurrently — engine classifies and calls Complete.
	// The drain goroutine reads from the results channel while workers run.
	go e.drainWorkerResults(ctx, pool.Results(), bl)

	pool.Wait() // blocks until tracker.Done() (all actions at terminal state)
	pool.Stop() // cancels workers, closes results → drain goroutine exits

	// End-of-pass failure summary — aggregates failures by issue type so
	// bulk sync produces WARN summaries instead of per-item noise (R-6.6.12).
	e.logFailureSummary()

	report.Succeeded, report.Failed, report.Errors = e.resultStats()
}

// buildReportFromCounts populates a SyncReport with plan counts.
func buildReportFromCounts(counts map[ActionType]int, mode SyncMode, opts RunOpts) *SyncReport {
	return &SyncReport{
		Mode:          mode,
		DryRun:        opts.DryRun,
		FolderCreates: counts[ActionFolderCreate],
		Moves:         counts[ActionLocalMove] + counts[ActionRemoteMove],
		Downloads:     counts[ActionDownload],
		Uploads:       counts[ActionUpload],
		LocalDeletes:  counts[ActionLocalDelete],
		RemoteDeletes: counts[ActionRemoteDelete],
		Conflicts:     counts[ActionConflict],
		SyncedUpdates: counts[ActionUpdateSynced],
		Cleanups:      counts[ActionCleanup],
	}
}

// observeRemote fetches delta changes from the Graph API. Automatically
// retries with an empty token if ErrDeltaExpired is returned (full resync).
func (e *Engine) observeRemote(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	savedToken, err := e.baseline.GetDeltaToken(ctx, e.driveID.String(), "")
	if err != nil {
		return nil, "", fmt.Errorf("sync: getting delta token: %w", err)
	}

	obs := NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, ErrDeltaExpired) {
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
func (e *Engine) observeLocal(ctx context.Context, bl *Baseline) (ScanResult, error) {
	obs := NewLocalObserver(bl, e.logger, e.checkWorkers)

	result, err := obs.FullScan(ctx, e.syncRoot)
	if err != nil {
		return ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// observeChanges runs remote and local observers based on mode, buffers their
// events, and returns the flushed change set. Delta token is committed
// atomically with observations in observeAndCommitRemote (skipped for dry-run
// so that a subsequent real sync sees the same delta changes).
//
// When fullReconcile is true, runs a fresh delta with empty token (enumerates
// ALL remote items) and detects orphans — baseline entries not in the full
// enumeration, representing missed delta deletions.
func (e *Engine) observeChanges(
	ctx context.Context, bl *Baseline, mode SyncMode, dryRun, fullReconcile bool,
) ([]PathChanges, error) {
	var remoteEvents []ChangeEvent

	var err error

	if mode != SyncUploadOnly {
		if fullReconcile {
			e.logger.Info("full reconciliation: enumerating all remote items")
			remoteEvents, err = e.observeAndCommitRemoteFull(ctx, bl)
		} else if dryRun {
			// Dry-run: observe without committing delta token or observations.
			// A subsequent real sync must see the same remote changes.
			remoteEvents, _, err = e.observeRemote(ctx, bl)
		} else {
			remoteEvents, err = e.observeAndCommitRemote(ctx, bl)
		}

		if err != nil {
			return nil, err
		}
	}

	// Process shortcuts: register new ones, remove deleted ones, observe content.
	// During throttle:account or service scope blocks, suppress shortcut
	// observation to avoid wasting API calls (R-2.10.30).
	var shortcutEvents []ChangeEvent

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

	// Filter out ChangeShortcut events from primary events — they were consumed
	// by processShortcuts and should not enter the planner as regular events.
	remoteEvents = filterOutShortcuts(remoteEvents)

	var localResult ScanResult

	if mode != SyncDownloadOnly {
		localResult, err = e.observeLocal(ctx, bl)
		if err != nil {
			return nil, err
		}

		// Record observation-time issues (invalid names, path too long, file too large).
		e.recordSkippedItems(ctx, localResult.Skipped)
		e.clearResolvedSkippedItems(ctx, localResult.Skipped)

		// R-2.10.10: If the scanner observed paths that were previously blocked
		// by local permission denials, clear the failures (scanner success = proof
		// of accessibility).
		e.clearScannerResolvedPermissions(ctx, pathSetFromEvents(localResult.Events))
	}

	buf := NewBuffer(e.logger)
	buf.AddAll(remoteEvents)
	buf.AddAll(shortcutEvents)
	buf.AddAll(localResult.Events)

	return buf.FlushImmediate(), nil
}

// observeAndCommitRemote wraps observeRemote to persist observations
// and delta token atomically via CommitObservation.
//
// When delta returns 0 events, the token is NOT advanced. The old token
// still covers the same window — replaying it costs nothing (O(1)). But if
// a deletion was still propagating to the Graph change log, advancing would
// permanently skip it. Deletions are delivered exactly once in a narrow
// window (ci_issues.md §20). This narrows but does not close the miss
// window — events can also be excluded from non-empty batches.
func (e *Engine) observeAndCommitRemote(ctx context.Context, bl *Baseline) ([]ChangeEvent, error) {
	events, deltaToken, err := e.observeRemote(ctx, bl)
	if err != nil {
		return nil, err
	}

	// Skip token advancement when no events were returned. The old token
	// replays the same empty window at zero cost, but avoids advancing
	// past deletions still propagating through the Graph change log.
	if len(events) == 0 {
		e.logger.Debug("delta returned 0 events, skipping token advancement")
		return events, nil
	}

	observed := changeEventsToObservedItems(events)
	if commitErr := e.baseline.CommitObservation(ctx, observed, deltaToken, e.driveID); commitErr != nil {
		return nil, fmt.Errorf("sync: committing observations: %w", commitErr)
	}

	return events, nil
}

// observeRemoteFull runs a fresh delta with empty token (enumerates ALL remote
// items) and compares against the baseline to find orphans: items in baseline
// but not in the full enumeration = deleted remotely but missed by incremental
// delta. Returns all events (creates/modifies from the full enumeration +
// synthesized deletes for orphans) and the new delta token.
func (e *Engine) observeRemoteFull(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	obs := NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)

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
// and delta token atomically via CommitObservation.
func (e *Engine) observeAndCommitRemoteFull(ctx context.Context, bl *Baseline) ([]ChangeEvent, error) {
	events, deltaToken, err := e.observeRemoteFull(ctx, bl)
	if err != nil {
		return nil, err
	}

	observed := changeEventsToObservedItems(events)
	if commitErr := e.baseline.CommitObservation(ctx, observed, deltaToken, e.driveID); commitErr != nil {
		return nil, fmt.Errorf("sync: committing full reconciliation: %w", commitErr)
	}

	return events, nil
}

// changeEventsToObservedItems converts remote ChangeEvents into ObservedItems
// for CommitObservation. Filters out local-source events and events with
// empty ItemIDs (defensive guard against malformed API responses).
func changeEventsToObservedItems(events []ChangeEvent) []ObservedItem {
	var items []ObservedItem

	for i := range events {
		if events[i].Source != SourceRemote {
			continue
		}

		if events[i].ItemID == "" {
			slog.Warn("changeEventsToObservedItems: skipping event with empty ItemID",
				slog.String("path", events[i].Path),
			)

			continue
		}

		items = append(items, ObservedItem{
			DriveID:   events[i].DriveID,
			ItemID:    events[i].ItemID,
			ParentID:  events[i].ParentID,
			Path:      events[i].Path,
			ItemType:  events[i].ItemType.String(),
			Hash:      events[i].Hash,
			Size:      events[i].Size,
			Mtime:     events[i].Mtime,
			ETag:      events[i].ETag,
			IsDeleted: events[i].IsDeleted,
		})
	}

	return items
}

// resolveSafetyConfig returns the appropriate SafetyConfig based on RunOpts.
// When Force is true, the threshold is set to MaxInt32 (effectively disabled).
// Otherwise, uses the engine's configured threshold from config.
func (e *Engine) resolveSafetyConfig(opts RunOpts) *SafetyConfig {
	if opts.Force {
		return &SafetyConfig{
			BigDeleteThreshold: forceSafetyMax,
		}
	}

	return &SafetyConfig{
		BigDeleteThreshold: e.bigDeleteThreshold,
	}
}

// ListConflicts returns all unresolved conflicts from the database.
func (e *Engine) ListConflicts(ctx context.Context) ([]ConflictRecord, error) {
	return e.baseline.ListConflicts(ctx)
}

// ListAllConflicts returns all conflicts (resolved and unresolved) from the
// database. Used by 'conflicts --history'.
func (e *Engine) ListAllConflicts(ctx context.Context) ([]ConflictRecord, error) {
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
	case ResolutionKeepBoth:
		// DB-only — executor already saved both copies during sync.
		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	case ResolutionKeepLocal:
		if err := e.resolveKeepLocal(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, ResolutionKeepLocal, err)
		}

		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	case ResolutionKeepRemote:
		if err := e.resolveKeepRemote(ctx, c); err != nil {
			return fmt.Errorf("sync: resolving conflict %s (%s): %w", c.ID, ResolutionKeepRemote, err)
		}

		return e.baseline.ResolveConflict(ctx, c.ID, resolution)

	default:
		return fmt.Errorf("sync: unknown resolution strategy %q", resolution)
	}
}

// resolveKeepLocal uploads the local file to overwrite the remote version.
func (e *Engine) resolveKeepLocal(ctx context.Context, c *ConflictRecord) error {
	return e.resolveTransfer(ctx, c, ActionUpload)
}

// resolveKeepRemote downloads the remote file to overwrite the local version.
func (e *Engine) resolveKeepRemote(ctx context.Context, c *ConflictRecord) error {
	return e.resolveTransfer(ctx, c, ActionDownload)
}

// resolveTransfer executes a single transfer (upload or download) for conflict
// resolution and commits the result to the baseline.
//
// Hash verification is intentionally skipped here (B-153): conflict resolution
// is a user-initiated override ("keep mine" / "keep theirs") where the intent
// is to force one side to match the other, not to verify content integrity.
// The executor's per-action functions already verify hashes for normal syncs.
func (e *Engine) resolveTransfer(ctx context.Context, c *ConflictRecord, actionType ActionType) error {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline for resolve: %w", err)
	}

	exec := NewExecution(e.execCfg, bl)

	action := &Action{
		Type:    actionType,
		DriveID: c.DriveID,
		ItemID:  c.ItemID,
		Path:    c.Path,
		View:    &PathView{Path: c.Path},
	}

	var outcome Outcome
	if actionType == ActionUpload {
		outcome = exec.executeUpload(ctx, action)
	} else {
		outcome = exec.executeDownload(ctx, action)
	}

	if !outcome.Success {
		return fmt.Errorf("transfer failed: %w", outcome.Error)
	}

	return e.baseline.CommitOutcome(ctx, &outcome)
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

// WatchOpts holds per-session options for RunWatch.
type WatchOpts struct {
	Force              bool
	PollInterval       time.Duration // remote delta polling interval (0 → 5m)
	Debounce           time.Duration // buffer debounce window (0 → 2s)
	SafetyScanInterval time.Duration // local safety scan interval (0 → 5m) (B-099)
	ReconcileInterval  time.Duration // periodic full reconciliation (0 → 24h, negative = disabled)
}

// setWatchShortcuts updates the shortcuts used by the drain goroutine.
// Called by the watch goroutine after observation when shortcuts may have changed.
func (e *Engine) setWatchShortcuts(shortcuts []Shortcut) {
	e.watchShortcutsMu.Lock()
	e.watchShortcuts = shortcuts
	e.watchShortcutsMu.Unlock()
}

// getWatchShortcuts returns the latest shortcuts for drain goroutine use.
func (e *Engine) getWatchShortcuts() []Shortcut {
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
		e.deleteCounter = newDeleteCounter(e.bigDeleteThreshold, deleteCounterWindow, time.Now)
	}

	if err := e.baseline.ClearResolvedActionableFailures(ctx, IssueBigDeleteHeld, nil); err != nil {
		e.logger.Warn("failed to clear stale big-delete-held entries",
			slog.String("error", err.Error()),
		)
	}

	if dv, dvErr := e.baseline.DataVersion(ctx); dvErr == nil {
		e.lastDataVersion = dv
	}
}

// loadWatchState loads the baseline and shortcuts for the watch session.
// Both are loaded once after the initial sync. Baseline is live-mutated
// under RWMutex; shortcuts are updated via setWatchShortcuts when they change.
func (e *Engine) loadWatchState(ctx context.Context) (*Baseline, error) {
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

	e.setWatchShortcuts(shortcuts)

	return bl, nil
}

// RunWatch runs a continuous sync loop: initial one-shot sync, then
// watches for remote and local changes, processing them in batches.
// Blocks until the context is canceled, returning nil on clean shutdown.
func (e *Engine) RunWatch(ctx context.Context, mode SyncMode, opts WatchOpts) error {
	e.logger.Info("watch mode starting",
		slog.String("mode", mode.String()),
		slog.Bool("force", opts.Force),
		slog.Duration("poll_interval", e.resolvePollInterval(opts)),
		slog.Duration("debounce", e.resolveDebounce(opts)),
	)

	// Step 1: Run initial one-shot sync to establish baseline.
	if _, err := e.RunOnce(ctx, mode, RunOpts{Force: opts.Force}); err != nil {
		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Steps 2–5: Set up the watch pipeline (baseline, tracker, pool,
	// buffer, retrier, observers, tickers).
	pipe, err := e.initWatchPipeline(ctx, mode, opts)
	if err != nil {
		return err
	}
	defer pipe.cleanup()

	return e.runWatchLoop(ctx, pipe)
}

// watchPipeline holds all handles needed by the watch select loop.
// Created by initWatchPipeline; cleaned up by its cleanup method.
type watchPipeline struct {
	bl         *Baseline
	tracker    *DepTracker
	safety     *SafetyConfig
	ready      <-chan []PathChanges
	errs       <-chan error
	skippedCh  <-chan []SkippedItem
	reconcileC <-chan time.Time
	recheckC   <-chan time.Time
	activeObs  int
	mode       SyncMode
	cleanup    func()
}

// initWatchPipeline sets up all watch-mode subsystems: baseline, tracker,
// worker pool, buffer, retrier, observers, and tickers. Returns a
// watchPipeline with a cleanup function that stops everything in order.
func (e *Engine) initWatchPipeline(
	ctx context.Context, mode SyncMode, opts WatchOpts,
) (*watchPipeline, error) {
	// Load baseline and shortcuts.
	bl, err := e.loadWatchState(ctx)
	if err != nil {
		return nil, err
	}

	e.initDeleteProtection(ctx, opts.Force)

	// Tracker and worker pool.
	tracker := NewPersistentDepTracker(e.logger)
	tracker.onHeld = func() { e.armTrialTimer() }
	e.tracker, e.scopeState = tracker, NewScopeState(e.nowFunc, e.logger)

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, watchResultBuf)
	pool.Start(ctx, e.transferWorkers)

	go e.drainWorkerResults(ctx, pool.Results(), bl)

	// Buffer and retrier.
	buf := NewBuffer(e.logger)
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	e.retrier = NewFailureRetrier(e.baseline, buf, tracker, e.logger)
	go e.retrier.Run(ctx)

	// Observers.
	errs, activeObs, skippedCh := e.startObservers(ctx, bl, mode, buf, opts)

	// Tickers.
	reconcileC, stopReconcile := e.initReconcileTicker(opts)
	recheckTicker := time.NewTicker(recheckInterval)

	return &watchPipeline{
		bl:         bl,
		tracker:    tracker,
		safety:     e.resolveWatchSafetyConfig(opts),
		ready:      ready,
		errs:       errs,
		skippedCh:  skippedCh,
		reconcileC: reconcileC,
		recheckC:   recheckTicker.C,
		activeObs:  activeObs,
		mode:       mode,
		cleanup: func() {
			recheckTicker.Stop()
			if stopReconcile != nil {
				stopReconcile()
			}

			inFlight := tracker.InFlightCount()
			if inFlight > 0 {
				e.logger.Info("graceful shutdown: draining in-flight actions",
					slog.Int("in_flight", inFlight),
				)
			}

			pool.Stop()
			e.logger.Info("watch mode stopped")
		},
	}, nil
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

			e.processBatch(ctx, batch, p.bl, p.mode, p.safety, p.tracker)

		case skipped := <-p.skippedCh:
			e.recordSkippedItems(ctx, skipped)
			e.clearResolvedSkippedItems(ctx, skipped)

		case <-p.recheckC:
			e.handleRecheckTick(ctx)

		case <-p.reconcileC:
			e.runFullReconciliation(ctx, p.bl, p.mode, p.safety, p.tracker)

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

// resultClass categorizes a WorkerResult for routing by processWorkerResult.
type resultClass int

const (
	resultSuccess    resultClass = iota // action succeeded
	resultRequeue                       // transient failure — re-queue with backoff
	resultScopeBlock                    // scope-level failure (429, 507, 5xx pattern)
	resultSkip                          // non-retryable — record and move on
	resultShutdown                      // context canceled — discard silently
	resultFatal                         // abort sync pass (401 unrecoverable auth)
)

// classifyResult is a pure function that maps a WorkerResult to a result
// class and optional scope key. No side effects — classification is
// separate from routing ("functions do one thing").
//
//nolint:gocyclo // classification table — each HTTP status code is a distinct case
func classifyResult(r *WorkerResult) (resultClass, ScopeKey) {
	if r.Success {
		return resultSuccess, ScopeKey{}
	}

	// Shutdown: context canceled or deadline exceeded — graceful drain.
	// NOT a failure — just a canceled operation. Don't record in sync_failures.
	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return resultShutdown, ScopeKey{}
	}

	switch {
	case r.HTTPStatus == http.StatusUnauthorized:
		return resultFatal, ScopeKey{}

	case r.HTTPStatus == http.StatusForbidden:
		return resultSkip, ScopeKey{}

	case r.HTTPStatus == http.StatusTooManyRequests:
		return resultScopeBlock, SKThrottleAccount

	case r.HTTPStatus == http.StatusInsufficientStorage:
		return resultScopeBlock, ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)

	case r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err):
		// Known outage pattern (e.g., "ObjectHandle is Invalid") — transient,
		// feeds scope detection. Distinguished from phantom drive 400s
		// (R-6.7.11) which are handled by drive filtering.
		return resultRequeue, ScopeKey{}

	case r.HTTPStatus >= 500:
		return resultRequeue, ScopeKey{}

	case r.HTTPStatus == http.StatusRequestTimeout ||
		r.HTTPStatus == http.StatusPreconditionFailed ||
		r.HTTPStatus == http.StatusNotFound ||
		r.HTTPStatus == http.StatusLocked:
		return resultRequeue, ScopeKey{}

	case errors.Is(r.Err, driveops.ErrDiskFull):
		// Deterministic signal — immediate scope block, no sliding window (R-2.10.43).
		return resultScopeBlock, SKDiskLocal

	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		// Per-file failure, no scope escalation — smaller files may fit (R-2.10.44).
		return resultSkip, ScopeKey{}

	case errors.Is(r.Err, os.ErrPermission):
		return resultSkip, ScopeKey{}

	default:
		return resultSkip, ScopeKey{}
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

// processWorkerResult handles a single worker result: classifies via
// classifyResult, records failure in sync_failures, and calls
// tracker.Complete. The engine owns all completion decisions — workers
// execute and report, the engine classifies and routes (R-6.8.9).
//
// No ReQueue — failed items are persisted in sync_failures with
// next_retry_at. The FailureRetrier re-injects them via buffer → planner →
// tracker when due (R-6.8.10).
//
// Shared by both one-shot (executePlan drain goroutine) and watch
// (drainWorkerResults) paths.
//
// Trial results are handled entirely by processTrialResult with an early
// return — they never enter the normal switch. This prevents future logic
// added to the normal switch cases from accidentally applying to failed
// trials (Group A separation).
func (e *Engine) processWorkerResult(ctx context.Context, r *WorkerResult, bl *Baseline, shortcuts []Shortcut) {
	// Trial results are fully self-contained — early return prevents
	// fallthrough into the normal switch (Group A: trial path separation).
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		e.processTrialResult(ctx, r)
		return
	}

	class, _ := classifyResult(r)

	switch class {
	case resultSuccess:
		e.tracker.Complete(r.ActionID)
		if e.scopeState != nil {
			e.scopeState.RecordSuccess(r)
		}
		e.succeeded.Add(1)
		e.defensiveClearFailure(ctx, r)

	case resultRequeue:
		// Transient failure: record with backoff, complete, kick reconciler.
		e.recordFailure(ctx, r, retry.Reconcile.Delay)
		e.tracker.Complete(r.ActionID)
		e.recordError(r)
		e.feedScopeDetection(r)
		if e.retrier != nil {
			e.retrier.Kick()
		}

	case resultScopeBlock:
		// Scope-level failure (429, 507): record with backoff, detect scope.
		e.recordFailure(ctx, r, retry.Reconcile.Delay)
		e.feedScopeDetection(r)
		e.tracker.Complete(r.ActionID)
		e.armTrialTimer() // catch dependents that just entered held
		e.recordError(r)
		if e.retrier != nil {
			e.retrier.Kick()
		}

	case resultSkip:
		// Local permission errors get special handling — walk up to find the
		// denied directory and create a scope block (R-2.10.12).
		if errors.Is(r.Err, os.ErrPermission) {
			e.handleLocalPermission(ctx, r)
			e.tracker.Complete(r.ActionID)
			e.recordError(r)

			break
		}

		if r.HTTPStatus == http.StatusForbidden && e.permChecker != nil {
			e.handle403(ctx, bl, r.Path, shortcuts)
		}
		// Non-retryable: record with nil delayFn (no next_retry_at).
		e.recordFailure(ctx, r, nil)
		e.tracker.Complete(r.ActionID)
		e.recordError(r)

	case resultShutdown:
		// Context canceled — graceful drain. Don't record failure.
		e.tracker.Complete(r.ActionID)

	case resultFatal:
		// Fatal (e.g., 401): record with nil delayFn, no retry.
		e.recordFailure(ctx, r, nil)
		e.tracker.Complete(r.ActionID)
		e.recordError(r)
	}
}

// processTrialResult handles the result of a scope trial action entirely.
// On success: releases the scope, triggers thundering herd, completes the
// action. On failure: extends the trial interval with per-scope-type caps.
// On shutdown: just completes. Trial results NEVER enter the normal
// processWorkerResult switch — this is the full handler (Group A).
//
// Scope detection is intentionally NOT called — the scope is already blocked,
// and re-detecting would overwrite the doubled interval (A2 bug prevention).
func (e *Engine) processTrialResult(ctx context.Context, r *WorkerResult) {
	class, _ := classifyResult(r)

	if class == resultSuccess {
		e.tracker.ReleaseScope(r.TrialScopeKey)
		e.resetScopeRetryTimes(ctx, r.TrialScopeKey)
		e.armTrialTimer()
		e.tracker.Complete(r.ActionID)
		if e.scopeState != nil {
			e.scopeState.RecordSuccess(r)
		}
		e.succeeded.Add(1)

		e.logger.Info("scope block cleared — actions released",
			slog.String("scope_key", r.TrialScopeKey.String()),
		)

		return
	}

	if class == resultShutdown {
		e.tracker.Complete(r.ActionID)
		return
	}

	// Trial failure: extend interval. Scope detection is NOT called — the scope
	// is already blocked, and re-detecting would overwrite the doubled interval
	// with a fresh initial interval from applyScopeBlock (A2 bug prevention).
	e.extendTrialInterval(r.TrialScopeKey, r.RetryAfter)

	var delayFn func(int) time.Duration
	if class == resultRequeue || class == resultScopeBlock {
		delayFn = retry.Reconcile.Delay
	}

	e.recordFailure(ctx, r, delayFn)
	e.tracker.Complete(r.ActionID)
	e.recordError(r)

	if e.retrier != nil {
		e.retrier.Kick()
	}
}

// extendTrialInterval extends the trial interval for the given scope key.
// Delegates to computeTrialInterval for the actual computation — the same
// function used by applyScopeBlock for initial intervals, ensuring a single
// code path for the Retry-After-vs-backoff policy.
func (e *Engine) extendTrialInterval(scopeKey ScopeKey, retryAfter time.Duration) {
	block, ok := e.tracker.GetScopeBlock(scopeKey)
	if !ok {
		return // scope was released between dispatch and result
	}

	newInterval := computeTrialInterval(retryAfter, block.TrialInterval)

	e.logger.Debug("trial failed — extending interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
	)

	e.tracker.ExtendTrialInterval(scopeKey, e.nowFunc().Add(newInterval), newInterval)
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
		if doubled > defaultMaxTrialInterval {
			return defaultMaxTrialInterval
		}
		return doubled
	}
	return defaultInitialTrialInterval
}

// isObservationSuppressed returns true if a global scope block
// (throttle:account or service) is active, meaning shortcut observation
// polling should be skipped to avoid wasting API calls (R-2.10.30).
func (e *Engine) isObservationSuppressed() bool {
	if e.tracker == nil {
		return false
	}

	_, throttled := e.tracker.GetScopeBlock(SKThrottleAccount)
	_, serviceDown := e.tracker.GetScopeBlock(SKService)

	return throttled || serviceDown
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Handles both
// regular failures and 400 outage patterns. Called directly from the normal
// processWorkerResult switch — never called for trial results (the scope is
// already blocked, and re-detecting would overwrite the doubled interval).
func (e *Engine) feedScopeDetection(r *WorkerResult) {
	if e.scopeState == nil {
		return
	}

	// Local errors (HTTPStatus==0) must not feed scope detection windows.
	// Only remote API errors should increment service/quota counters (R-6.7.27).
	if r.HTTPStatus == 0 {
		return
	}

	sr := e.scopeState.UpdateScope(r)
	if sr.Block {
		e.applyScopeBlock(sr)
	}
	// Also check outage pattern for 400s.
	if r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err) {
		osr := e.scopeState.UpdateScopeOutagePattern(r.Path)
		if osr.Block {
			e.applyScopeBlock(osr)
		}
	}
}

// applyScopeBlock creates a ScopeBlock and tells the tracker to hold
// affected actions. Uses computeTrialInterval for the initial interval,
// ensuring the same Retry-After-vs-backoff policy as extendTrialInterval.
// Logs a WARN because a scope block is a degraded-but-recoverable state
// (R-6.6.10).
func (e *Engine) applyScopeBlock(sr ScopeUpdateResult) {
	now := e.nowFunc()
	interval := computeTrialInterval(sr.RetryAfter, 0)
	block := &ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}
	e.tracker.HoldScope(sr.ScopeKey, block)

	e.logger.Warn("scope block active — actions held",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("issue_type", sr.IssueType),
		slog.Duration("trial_interval", interval),
	)

	e.armTrialTimer() // arm so the first trial fires at NextTrialAt (R-2.10.5)
}

// recordError increments the failed counter and appends the error to the
// diagnostic error list.
func (e *Engine) recordError(r *WorkerResult) {
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

// defensiveClearFailure removes any stale sync_failures row for a
// successfully completed action. CommitOutcome (worker.go) already deletes
// the row inside its transaction, but this guards against edge cases where
// the worker path was bypassed or interrupted.
func (e *Engine) defensiveClearFailure(ctx context.Context, r *WorkerResult) {
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
func (e *Engine) recordFailure(ctx context.Context, r *WorkerResult, delayFn func(int) time.Duration) {
	direction := directionFromAction(r.ActionType)

	driveID := r.DriveID
	if driveID.String() == "" {
		driveID = e.driveID
	}

	// The engine's routing already classifies each result — delayFn is non-nil
	// for transient failures (retryable) and nil for actionable/fatal ones.
	category := strTransient
	if delayFn == nil {
		category = strActionable
	}

	issueType := issueTypeForHTTPStatus(r.HTTPStatus, r.Err)
	scopeKey := deriveScopeKey(r)

	if recErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
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
// ScopeKeyForStatus — single source of truth for HTTP status → scope key
// mapping. Returns the zero-value ScopeKey for non-scope statuses.
func deriveScopeKey(r *WorkerResult) ScopeKey {
	return ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
}

// issueTypeForHTTPStatus maps an HTTP status code and error to a sync
// failure issue type. Used by recordFailure to populate the issue_type
// column. Returns empty string for generic/unknown failures.
func issueTypeForHTTPStatus(httpStatus int, err error) string {
	switch {
	case httpStatus == http.StatusTooManyRequests:
		return IssueRateLimited
	case httpStatus == http.StatusInsufficientStorage:
		return IssueQuotaExceeded
	case httpStatus == http.StatusForbidden:
		return IssuePermissionDenied
	case httpStatus == http.StatusBadRequest && isOutagePattern(err):
		return IssueServiceOutage
	case httpStatus >= http.StatusInternalServerError:
		return IssueServiceOutage
	case httpStatus == http.StatusRequestTimeout:
		return "request_timeout"
	case httpStatus == http.StatusPreconditionFailed:
		return "transient_conflict"
	case httpStatus == http.StatusNotFound:
		return "transient_not_found"
	case httpStatus == http.StatusLocked:
		return "resource_locked"
	case errors.Is(err, driveops.ErrDiskFull):
		return IssueDiskFull
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return IssueFileTooLargeForSpace
	case errors.Is(err, os.ErrPermission):
		return IssueLocalPermissionDenied
	default:
		return ""
	}
}

// resetScopeRetryTimes resets next_retry_at for all sync_failures matching
// the given scope key. This is the "thundering herd" — when a scope trial
// succeeds, all items with future backoff for that scope become immediately
// retriable. Kicks the retrier so it picks them up promptly.
func (e *Engine) resetScopeRetryTimes(ctx context.Context, scopeKey ScopeKey) {
	if err := e.baseline.ResetRetryTimesForScope(ctx, scopeKey, e.nowFunc()); err != nil {
		e.logger.Warn("failed to reset retry times for scope",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)

		return
	}

	if e.retrier != nil {
		e.retrier.Kick()
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

// directionFromAction maps an ActionType to a sync_failures direction string.
func directionFromAction(at ActionType) string {
	switch at { //nolint:exhaustive // only failure-producing actions need mapping
	case ActionUpload:
		return strUpload
	case ActionLocalDelete, ActionRemoteDelete:
		return strDelete
	default:
		return strDownload
	}
}

// armTrialTimer sets (or resets) the trial timer to fire at the earliest
// NextTrialAt across all scope blocks. Uses time.AfterFunc to send to the
// persistent trialCh channel, avoiding a race where the drain loop's select
// watches the old timer's channel after replacement. Called after scope blocks
// are created, trials dispatched, trial results processed, or when onHeld
// signals that an action entered a held queue (R-2.10.5).
func (e *Engine) armTrialTimer() {
	e.trialMu.Lock()
	defer e.trialMu.Unlock()

	if e.trialTimer != nil {
		e.trialTimer.Stop()
		e.trialTimer = nil
	}

	earliest, ok := e.tracker.EarliestTrialAt()
	if !ok {
		return
	}

	delay := time.Until(earliest)
	if delay <= 0 {
		delay = 1 * time.Millisecond // fire immediately
	}

	// Non-blocking send to the buffered(1) channel. If a signal is already
	// pending, the new one is coalesced (dropped). This is self-healing:
	// drainWorkerResults calls NextDueTrial in a loop, so even if a second
	// AfterFunc fires while a signal is pending, all due scopes are still
	// processed on the next drain iteration.
	e.trialTimer = time.AfterFunc(delay, func() {
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
	e.trialMu.Lock()
	defer e.trialMu.Unlock()

	if e.trialTimer != nil {
		e.trialTimer.Stop()
		e.trialTimer = nil
	}
}

// drainWorkerResults reads from the worker result channel and persists
// failures to remote_state for durable retry tracking. Used by watch mode.
// Baseline is passed explicitly. Shortcuts are read from the synchronized
// watchShortcuts field on each result to pick up newly discovered shortcuts.
// The trial timer fires DispatchTrial when scope trials become due (R-2.10.5).
func (e *Engine) drainWorkerResults(ctx context.Context, results <-chan WorkerResult, bl *Baseline) {
	defer e.stopTrialTimer()

	for {
		select {
		case r, ok := <-results:
			if !ok {
				return
			}

			e.processWorkerResult(ctx, &r, bl, e.getWatchShortcuts())

		case <-e.trialTimerChan():
			now := e.nowFunc()
			for {
				key, _, ok := e.tracker.NextDueTrial(now)
				if !ok {
					break
				}
				e.tracker.DispatchTrial(key)
			}
			e.armTrialTimer()

		case <-ctx.Done():
			return
		}
	}
}

// startObservers launches remote and local observer goroutines that feed
// events into the buffer. Returns an error channel for observer failures and
// the number of observers started. The events channel is closed automatically
// when all observers exit, allowing the bridge goroutine to drain cleanly.
func (e *Engine) startObservers(
	ctx context.Context, bl *Baseline, mode SyncMode, buf *Buffer, opts WatchOpts,
) (<-chan error, int, <-chan []SkippedItem) {
	events := make(chan ChangeEvent, watchEventBuf)
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
	if mode != SyncUploadOnly {
		remoteObs := NewRemoteObserver(e.fetcher, bl, e.driveID, e.logger)
		remoteObs.obsWriter = e.baseline
		e.remoteObs = remoteObs

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
	skippedCh := make(chan []SkippedItem, 2)

	// Local observer (skip for download-only mode).
	if mode != SyncDownloadOnly {
		localObs := NewLocalObserver(bl, e.logger, e.checkWorkers)
		localObs.safetyScanInterval = opts.SafetyScanInterval
		localObs.SetSkippedChannel(skippedCh)

		if e.localWatcherFactory != nil {
			localObs.watcherFactory = e.localWatcherFactory
		}

		e.localObs = localObs

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()

			watchErr := localObs.Watch(ctx, e.syncRoot, events)
			if errors.Is(watchErr, ErrWatchLimitExhausted) {
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
	ctx context.Context, obs *LocalObserver, syncRoot string,
	events chan<- ChangeEvent, interval time.Duration,
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
				obs.trySend(ctx, events, &result.Events[i])
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
	ctx context.Context, batch []PathChanges, bl *Baseline,
	mode SyncMode, safety *SafetyConfig, tracker *DepTracker,
) {
	e.logger.Info("processing watch batch",
		slog.Int("paths", len(batch)),
	)

	// Periodically recheck permissions in watch mode (R-2.10.9).
	// Throttled to at most once per 60 seconds to avoid API hammering.
	const permRecheckInterval = 60 * time.Second

	now := time.Now()
	if now.Sub(e.lastPermRecheck) >= permRecheckInterval {
		e.lastPermRecheck = now

		// recheckPermissions calls the Graph API — skip during outage or
		// throttle to avoid wasting API calls (R-2.10.30). Local permission
		// rechecks (filesystem-only) proceed regardless.
		if e.permChecker != nil && !e.isObservationSuppressed() {
			shortcuts, err := e.baseline.ListShortcuts(ctx)
			if err == nil {
				e.recheckPermissions(ctx, bl, shortcuts)
			}
		}

		e.recheckLocalPermissions(ctx)
	}

	// R-2.10.10: use scanner output as proof-of-accessibility to clear
	// permission denials for paths observed in this batch.
	e.clearScannerResolvedPermissions(ctx, pathSetFromBatch(batch))

	denied := e.permCache.deniedPrefixes()
	plan, err := e.planner.Plan(batch, bl, mode, safety, denied)
	if err != nil {
		if errors.Is(err, ErrBigDeleteTriggered) {
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
	if e.deleteCounter != nil {
		plan = e.applyDeleteCounter(ctx, plan)
		if len(plan.Actions) == 0 {
			return
		}
	}

	// B-122: Cancel in-flight actions for paths that appear in this batch.
	for i := range plan.Actions {
		if tracker.HasInFlight(plan.Actions[i].Path) {
			e.logger.Info("canceling in-flight action for updated path",
				slog.String("path", plan.Actions[i].Path),
			)

			tracker.CancelByPath(plan.Actions[i].Path)
		}
	}

	// Invariant: Planner always builds Deps with len(Actions).
	// Log-only on violation: processBatch has no SyncReport to populate (unlike
	// executePlan), and the Error log is the correct signal for this impossible
	// condition. The batch is safely dropped — the next delta poll re-observes.
	if len(plan.Actions) != len(plan.Deps) {
		e.logger.Error("plan invariant violation: Actions/Deps length mismatch",
			slog.Int("actions", len(plan.Actions)),
			slog.Int("deps", len(plan.Deps)),
		)

		return
	}

	// Populate tracker with actions. Dispatch transitions set the in-progress
	// status on remote_state before the worker picks up the action.
	for i := range plan.Actions {
		id := int64(i)

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		e.setDispatch(ctx, &plan.Actions[i])

		tracker.Add(&plan.Actions[i], id, depIDs)
	}

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
	)
}

// setDispatch writes the dispatch state transition for an action before it
// enters the tracker. Only applies to downloads and local deletes (the action
// types that have remote_state lifecycle).
func (e *Engine) setDispatch(ctx context.Context, action *Action) {
	if err := e.baseline.SetDispatchStatus(ctx, action.DriveID.String(), action.ItemID, action.Type); err != nil {
		e.logger.Warn("failed to set dispatch status",
			slog.String("path", action.Path),
			slog.String("error", err.Error()),
		)
	}
}

// resolvePollInterval returns the configured poll interval or the default.
func (e *Engine) resolvePollInterval(opts WatchOpts) time.Duration {
	if opts.PollInterval > 0 {
		return opts.PollInterval
	}

	return defaultPollInterval
}

// resolveDebounce returns the configured debounce or the default.
func (e *Engine) resolveDebounce(opts WatchOpts) time.Duration {
	if opts.Debounce > 0 {
		return opts.Debounce
	}

	return defaultDebounce
}

// resolveWatchSafetyConfig returns the safety config for watch mode.
// In watch mode, the planner's big-delete check is disabled (threshold=MaxInt32)
// because the rolling deleteCounter handles protection instead.
// When Force is also set, both are disabled.
func (e *Engine) resolveWatchSafetyConfig(_ WatchOpts) *SafetyConfig {
	// Watch mode always disables the planner-level check — the rolling
	// deleteCounter in processBatch handles big-delete protection instead.
	return &SafetyConfig{
		BigDeleteThreshold: forceSafetyMax,
	}
}

// isDeleteAction returns true if the action type is a local or remote delete.
func isDeleteAction(t ActionType) bool {
	return t == ActionLocalDelete || t == ActionRemoteDelete
}

// applyDeleteCounter counts planned deletes in the plan, feeds them to the
// rolling counter, and — if the counter is held — filters delete actions out
// of the plan and records them as actionable issues. Returns the (possibly
// filtered) plan. When all actions are filtered, returns a plan with empty
// Actions/Deps.
func (e *Engine) applyDeleteCounter(ctx context.Context, plan *ActionPlan) *ActionPlan {
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
	tripped := e.deleteCounter.Add(deleteCount)
	if tripped {
		e.logger.Warn("big-delete protection triggered in watch mode",
			slog.Int("delete_count", e.deleteCounter.Count()),
			slog.Int("threshold", e.deleteCounter.Threshold()),
		)
	}

	if !e.deleteCounter.IsHeld() {
		return plan
	}

	// Filter: separate deletes from non-deletes and rebuild the plan.
	// Dependency indices must be remapped to the new action positions.
	kept := make([]Action, 0, len(plan.Actions))
	keptDeps := make([][]int, 0, len(plan.Deps))
	oldToNew := make(map[int]int, len(plan.Actions))

	var heldDeletes []Action

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
func (e *Engine) recordHeldDeletes(ctx context.Context, actions []Action) {
	if len(actions) == 0 {
		return
	}

	failures := make([]ActionableFailure, len(actions))
	for i := range actions {
		failures[i] = ActionableFailure{
			Path:      actions[i].Path,
			DriveID:   actions[i].DriveID,
			Direction: strDelete,
			IssueType: IssueBigDeleteHeld,
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

	if dv == e.lastDataVersion {
		return false
	}

	e.lastDataVersion = dv

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
	if e.deleteCounter != nil && e.deleteCounter.IsHeld() {
		rows, err := e.baseline.ListSyncFailuresByIssueType(ctx, IssueBigDeleteHeld)
		if err != nil {
			e.logger.Warn("failed to check big-delete-held entries",
				slog.String("error", err.Error()),
			)

			return
		}

		if len(rows) == 0 {
			e.deleteCounter.Release()
			e.logger.Info("big-delete protection cleared by user")
		}
	}

	// Permission clearance: if user cleared perm:dir failures via CLI,
	// release the corresponding in-memory scope blocks.
	if e.tracker != nil {
		issues, err := e.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
		if err != nil {
			e.logger.Warn("failed to check permission failures",
				slog.String("error", err.Error()),
			)

			return
		}

		// Build set of still-active scope keys from DB.
		activeScopes := make(map[ScopeKey]bool, len(issues))
		for i := range issues {
			if issues[i].ScopeKey.IsPermDir() {
				activeScopes[issues[i].ScopeKey] = true
			}
		}

		// Release any tracker scope blocks whose failures were cleared.
		for _, key := range e.tracker.ScopeBlockKeys() {
			if key.IsPermDir() && !activeScopes[key] {
				e.tracker.ReleaseScope(key)
				e.logger.Info("permission scope block cleared by user",
					slog.String("scope", key.String()),
				)
			}
		}
	}
}

// logWatchSummary logs a periodic one-liner summary of actionable issues
// in watch mode. Only logs when the count changes since the last summary
// to avoid noisy repeated output.
func (e *Engine) logWatchSummary(ctx context.Context) {
	issues, err := e.baseline.ListActionableFailures(ctx)
	if err != nil || len(issues) == 0 {
		if e.lastSummaryTotal != 0 {
			e.lastSummaryTotal = 0
		}

		return
	}

	// Only log if count changed since last summary.
	if len(issues) == e.lastSummaryTotal {
		return
	}

	e.lastSummaryTotal = len(issues)

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
func (e *Engine) recordSkippedItems(ctx context.Context, skipped []SkippedItem) {
	if len(skipped) == 0 {
		return
	}

	// Group by issue type for batch upsert and aggregated logging.
	byReason := make(map[string][]SkippedItem)
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

		// Build ActionableFailure slice for batch upsert.
		failures := make([]ActionableFailure, len(items))
		for i := range items {
			failures[i] = ActionableFailure{
				Path:      items[i].Path,
				DriveID:   e.driveID,
				Direction: "upload",
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
func (e *Engine) clearResolvedSkippedItems(ctx context.Context, skipped []SkippedItem) {
	// Collect current paths per scanner-detectable issue type.
	currentByType := make(map[string][]string)
	for i := range skipped {
		currentByType[skipped[i].Reason] = append(currentByType[skipped[i].Reason], skipped[i].Path)
	}

	// For each scanner-detectable issue type, clear entries not in the current scan.
	// If no items of that type were found, pass empty slice (clears all of that type).
	for _, issueType := range []string{IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge, IssueCaseCollision} {
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
func (e *Engine) resolveReconcileInterval(opts WatchOpts) time.Duration {
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
func (e *Engine) initReconcileTicker(opts WatchOpts) (<-chan time.Time, func()) {
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

// runFullReconciliation performs a full delta enumeration + orphan detection,
// then feeds the resulting events through the normal planner + executor
// pipeline. Called periodically in watch mode to recover from missed delta
// deletions.
func (e *Engine) runFullReconciliation(
	ctx context.Context, bl *Baseline, mode SyncMode, safety *SafetyConfig, tracker *DepTracker,
) {
	e.logger.Info("periodic full reconciliation starting")

	events, deltaToken, err := e.observeRemoteFull(ctx, bl)
	if err != nil {
		e.logger.Error("full reconciliation failed",
			slog.String("error", err.Error()),
		)

		return
	}

	// Commit observations and delta token.
	observed := changeEventsToObservedItems(events)
	if commitErr := e.baseline.CommitObservation(ctx, observed, deltaToken, e.driveID); commitErr != nil {
		e.logger.Error("failed to commit full reconciliation observations",
			slog.String("error", commitErr.Error()),
		)

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

	// Buffer and flush through the normal pipeline.
	buf := NewBuffer(e.logger)
	buf.AddAll(events)
	batch := buf.FlushImmediate()

	if len(batch) == 0 {
		e.logger.Info("periodic full reconciliation complete: no changes")
		return
	}

	e.processBatch(ctx, batch, bl, mode, safety, tracker)

	// Refresh watch shortcuts after reconciliation — reconcileShortcutScopes
	// may have added or removed shortcuts.
	if refreshed, refreshErr := e.baseline.ListShortcuts(ctx); refreshErr == nil {
		e.setWatchShortcuts(refreshed)
	}

	e.logger.Info("periodic full reconciliation complete",
		slog.Int("paths", len(batch)),
	)
}
