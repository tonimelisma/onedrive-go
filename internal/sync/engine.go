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
	DBPath          string              // path to the SQLite state database
	SyncRoot        string              // absolute path to the local sync directory
	DataDir         string              // application data directory for session files (optional)
	DriveID         driveid.ID          // normalized drive identifier
	Fetcher         DeltaFetcher        // satisfied by *graph.Client
	Items           ItemClient          // satisfied by *graph.Client
	Downloads       driveops.Downloader // satisfied by *graph.Client
	Uploads         driveops.Uploader   // satisfied by *graph.Client
	DriveVerifier   DriveVerifier       // optional: verified at startup (B-074); nil skips check
	FolderDelta     FolderDeltaFetcher  // optional: folder-scoped delta for shortcut observation (6.4b)
	RecursiveLister RecursiveLister     // optional: recursive listing for shortcut observation (6.4b)
	PermChecker     PermissionChecker   // optional: permission checking for shared folders (6.4c)
	Logger          *slog.Logger
	UseLocalTrash   bool // move deleted local files to OS trash instead of permanent delete
	TransferWorkers int  // goroutine count for the worker pool (0 → minWorkers)
	CheckWorkers    int  // goroutine limit for parallel file hashing (0 → 4)
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
	baseline        *SyncStore
	planner         *Planner
	execCfg         *ExecutorConfig
	fetcher         DeltaFetcher
	driveVerifier   DriveVerifier      // optional (B-074)
	folderDelta     FolderDeltaFetcher // optional: for shortcut observation (6.4b)
	recursiveLister RecursiveLister    // optional: for shortcut observation (6.4b)
	permChecker     PermissionChecker  // optional: for shared folder permission checks (6.4c)
	permCache       *permissionCache   // per-pass in-memory cache of folder→canWrite
	syncRoot        string
	driveID         driveid.ID
	logger          *slog.Logger
	remoteObs       *RemoteObserver        // stored during RunWatch for delta token reads
	localObs        *LocalObserver         // stored during RunWatch for drop counter reads
	sessionStore    *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers int                    // goroutine count for the worker pool
	checkWorkers    int                    // goroutine limit for parallel file hashing

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

	// Engine-owned result counters. Workers are pure executors — the engine
	// classifies results and owns all final disposition counts (R-6.8.9).
	succeeded    atomic.Int32
	failed       atomic.Int32
	syncErrors   []error
	syncErrorsMu stdsync.Mutex

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
	// immutable after creation (no post-hoc field mutation).
	var sessionStore *driveops.SessionStore
	if cfg.DataDir != "" {
		sessionStore = driveops.NewSessionStore(cfg.DataDir, cfg.Logger)
	}

	execCfg.transferMgr = driveops.NewTransferManager(cfg.Downloads, cfg.Uploads, sessionStore, cfg.Logger)

	return &Engine{
		baseline:        bm,
		planner:         NewPlanner(cfg.Logger),
		execCfg:         execCfg,
		fetcher:         cfg.Fetcher,
		driveVerifier:   cfg.DriveVerifier,
		folderDelta:     cfg.FolderDelta,
		recursiveLister: cfg.RecursiveLister,
		permChecker:     cfg.PermChecker,
		permCache:       newPermissionCache(),
		sessionStore:    sessionStore,
		syncRoot:        cfg.SyncRoot,
		driveID:         cfg.DriveID,
		logger:          cfg.Logger,
		transferWorkers: cfg.TransferWorkers,
		checkWorkers:    cfg.CheckWorkers,
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
	if err := e.baseline.ResetInProgressStates(ctx, e.syncRoot); err != nil {
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
	shortcutEvents, err := e.processShortcuts(ctx, remoteEvents, bl, dryRun)
	if err != nil {
		e.logger.Warn("shortcut processing failed, continuing without shortcut content",
			slog.String("error", err.Error()),
		)
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
// When Force is true, thresholds are set to max values (effectively disabled).
func (e *Engine) resolveSafetyConfig(opts RunOpts) *SafetyConfig {
	if opts.Force {
		return &SafetyConfig{
			BigDeleteMinItems:   0,
			BigDeleteMaxCount:   forceSafetyMax,
			BigDeleteMaxPercent: float64(forceSafetyMax),
		}
	}

	return DefaultSafetyConfig()
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
	_, err := e.RunOnce(ctx, mode, RunOpts{Force: opts.Force})
	if err != nil {
		return fmt.Errorf("sync: initial sync failed: %w", err)
	}

	// Step 2: Load baseline (cached, mutated in-place under RWMutex) and
	// shortcuts (stored in synchronized field, updated when shortcuts change).
	bl, err := e.loadWatchState(ctx)
	if err != nil {
		return err
	}

	// Step 3: Create persistent tracker and worker pool.
	tracker := NewPersistentDepTracker(e.logger)
	e.tracker = tracker
	e.scopeState = NewScopeState(e.nowFunc, e.logger)

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, watchResultBuf)
	pool.Start(ctx, e.transferWorkers)

	defer func() {
		inFlight := tracker.InFlightCount()
		if inFlight > 0 {
			e.logger.Info("graceful shutdown: draining in-flight actions",
				slog.Int("in_flight", inFlight),
			)
		}

		pool.Stop()

		e.logger.Info("watch mode stopped")
	}()

	// Drain worker results in a background goroutine. The engine classifies
	// results and calls tracker.Complete (R-6.8.9).
	go e.drainWorkerResults(ctx, pool.Results(), bl)

	// Step 4: Create buffer and debounced output.
	buf := NewBuffer(e.logger)
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	// Step 4b: Start the failure retrier — sole retry mechanism (R-6.8.10).
	// Re-injects due sync_failures via buffer → planner → tracker.
	e.retrier = NewFailureRetrier(e.baseline, buf, tracker, e.logger)
	go e.retrier.Run(ctx)

	// Step 5: Start observer goroutines.
	errs, activeObservers := e.startObservers(ctx, bl, mode, buf, opts)

	// Step 6: Main select loop.
	safety := e.resolveWatchSafetyConfig(opts)

	// Periodic full reconciliation timer (safety net for missed delta deletions).
	reconcileInterval := e.resolveReconcileInterval(opts)
	reconcileTicker := e.newReconcileTicker(reconcileInterval)
	var reconcileC <-chan time.Time
	if reconcileTicker != nil {
		reconcileC = reconcileTicker.C
		defer reconcileTicker.Stop()

		e.logger.Info("periodic full reconciliation enabled",
			slog.Duration("interval", reconcileInterval),
		)
	}

	for {
		select {
		case batch, ok := <-ready:
			if !ok {
				return nil
			}

			e.processBatch(ctx, batch, bl, mode, safety, tracker)

		case <-reconcileC:
			e.runFullReconciliation(ctx, bl, mode, safety, tracker)

		case obsErr := <-errs:
			// Observers return nil on clean context cancellation and non-nil
			// on genuine failures (e.g., nosync guard, watcher creation error).
			// RemoteObserver retries indefinitely and only exits on ctx cancel.
			if obsErr != nil {
				e.logger.Warn("observer error",
					slog.String("error", obsErr.Error()),
				)
			}

			activeObservers--
			if activeObservers == 0 {
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
func classifyResult(r *WorkerResult) (resultClass, string) {
	if r.Success {
		return resultSuccess, ""
	}

	// Shutdown: context canceled or deadline exceeded — graceful drain.
	// NOT a failure — just a canceled operation. Don't record in sync_failures.
	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return resultShutdown, ""
	}

	switch {
	case r.HTTPStatus == http.StatusUnauthorized:
		return resultFatal, ""

	case r.HTTPStatus == http.StatusForbidden:
		return resultSkip, ""

	case r.HTTPStatus == http.StatusTooManyRequests:
		return resultScopeBlock, scopeKeyThrottleAccount

	case r.HTTPStatus == http.StatusInsufficientStorage:
		return resultScopeBlock, scopeKeyForQuota(r)

	case r.HTTPStatus == http.StatusBadRequest && isOutagePattern(r.Err):
		// Known outage pattern (e.g., "ObjectHandle is Invalid") — transient,
		// feeds scope detection. Distinguished from phantom drive 400s
		// (R-6.7.11) which are handled by drive filtering.
		return resultRequeue, ""

	case r.HTTPStatus >= 500:
		return resultRequeue, ""

	case r.HTTPStatus == http.StatusRequestTimeout ||
		r.HTTPStatus == http.StatusPreconditionFailed ||
		r.HTTPStatus == http.StatusNotFound ||
		r.HTTPStatus == http.StatusLocked:
		return resultRequeue, ""

	case errors.Is(r.Err, os.ErrPermission):
		return resultSkip, ""

	default:
		return resultSkip, ""
	}
}

// scopeKeyForQuota returns the scope key for a 507 quota error based on
// the target drive context (R-2.10.1, R-2.10.17).
func scopeKeyForQuota(r *WorkerResult) string {
	if r.ShortcutKey != "" {
		return scopeKeyQuotaShortcut + r.ShortcutKey
	}
	return scopeKeyQuotaOwn
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
func (e *Engine) processWorkerResult(ctx context.Context, r *WorkerResult, bl *Baseline, shortcuts []Shortcut) {
	// Handle trial results — check if a scope trial succeeded or failed.
	if r.IsTrial && r.TrialScopeKey != "" {
		e.handleTrialResult(r)
	}

	class, _ := classifyResult(r)

	switch class {
	case resultSuccess:
		e.tracker.Complete(r.ActionID)
		if e.scopeState != nil {
			e.scopeState.RecordSuccess(r)
		}
		e.succeeded.Add(1)

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
		e.recordError(r)
		if e.retrier != nil {
			e.retrier.Kick()
		}

	case resultSkip:
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

// handleTrialResult processes the result of a scope trial action. On
// success, releases the scope. On failure, extends the trial interval
// with backoff.
func (e *Engine) handleTrialResult(r *WorkerResult) {
	class, _ := classifyResult(r)
	if class == resultSuccess {
		e.tracker.ReleaseScope(r.TrialScopeKey)
		return
	}

	// Trial failed — extend the interval with 2× backoff.
	e.tracker.ExtendTrial(r.TrialScopeKey, e.nowFunc().Add(r.RetryAfter*2+time.Second))
}

// feedScopeDetection feeds a worker result into scope detection sliding
// windows. If a threshold is crossed, creates a scope block. Handles both
// regular failures and 400 outage patterns.
func (e *Engine) feedScopeDetection(r *WorkerResult) {
	if e.scopeState == nil {
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
// affected actions.
func (e *Engine) applyScopeBlock(sr ScopeUpdateResult) {
	now := e.nowFunc()
	block := &ScopeBlock{
		Key:           sr.ScopeKey,
		IssueType:     sr.IssueType,
		BlockedAt:     now,
		TrialInterval: sr.TrialInterval,
		NextTrialAt:   now.Add(sr.TrialInterval),
	}
	e.tracker.HoldScope(sr.ScopeKey, block)
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

	if recErr := e.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       r.Path,
		DriveID:    driveID,
		Direction:  direction,
		Category:   category,
		ErrMsg:     r.ErrMsg,
		HTTPStatus: r.HTTPStatus,
	}, delayFn); recErr != nil {
		e.logger.Warn("failed to record failure",
			slog.String("path", r.Path),
			slog.String("error", recErr.Error()),
		)
	}
}

// nowFunc returns the current time. Uses the engine's clock if available,
// otherwise falls back to time.Now.
func (e *Engine) nowFunc() time.Time {
	return time.Now()
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

// drainWorkerResults reads from the worker result channel and persists
// failures to remote_state for durable retry tracking. Used by watch mode.
// Baseline is passed explicitly. Shortcuts are read from the synchronized
// watchShortcuts field on each result to pick up newly discovered shortcuts.
func (e *Engine) drainWorkerResults(ctx context.Context, results <-chan WorkerResult, bl *Baseline) {
	for {
		select {
		case r, ok := <-results:
			if !ok {
				return
			}

			e.processWorkerResult(ctx, &r, bl, e.getWatchShortcuts())

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
) (<-chan error, int) {
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

	// Local observer (skip for download-only mode).
	if mode != SyncDownloadOnly {
		localObs := NewLocalObserver(bl, e.logger, e.checkWorkers)
		localObs.safetyScanInterval = opts.SafetyScanInterval

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

	return errs, count
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
// When Force is set, big-delete protection is disabled.
func (e *Engine) resolveWatchSafetyConfig(opts WatchOpts) *SafetyConfig {
	if opts.Force {
		return &SafetyConfig{
			BigDeleteMinItems:   0,
			BigDeleteMaxCount:   forceSafetyMax,
			BigDeleteMaxPercent: float64(forceSafetyMax),
		}
	}

	return DefaultSafetyConfig()
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
	for _, issueType := range []string{IssueInvalidFilename, IssuePathTooLong, IssueFileTooLarge} {
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
