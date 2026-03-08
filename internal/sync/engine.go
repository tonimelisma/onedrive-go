package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

// RunOpts holds per-cycle options for RunOnce.
type RunOpts struct {
	DryRun        bool
	Force         bool
	FullReconcile bool // when true, runs a full delta enumeration + orphan detection
}

// SyncReport summarizes the result of a single sync cycle.
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

// Engine orchestrates a complete sync cycle: observe → plan → execute → commit.
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
	permCache       *permissionCache   // per-cycle in-memory cache of folder→canWrite
	syncRoot        string
	driveID         driveid.ID
	logger          *slog.Logger
	remoteObs       *RemoteObserver        // stored during RunWatch for delta token reads
	localObs        *LocalObserver         // stored during RunWatch for drop counter reads
	sessionStore    *driveops.SessionStore // for CleanStale() housekeeping
	transferWorkers int                    // goroutine count for the worker pool
	checkWorkers    int                    // goroutine limit for parallel file hashing

	// retrier retries failed items with exponential backoff in watch mode.
	// nil in one-shot mode.
	retrier *FailureRetrier

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

// RunOnce executes a single sync cycle:
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

	e.logger.Info("sync cycle starting",
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
	// for folders that have become writable since the last cycle.
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
		e.logger.Info("sync cycle complete: no changes detected",
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

	// Step 6b: Pre-upload validation — reject permanently invalid uploads.
	plan = e.filterInvalidUploads(ctx, plan)

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

	// Execute plan: run workers, drain results (failures, 403s, upload issues).
	e.executePlan(ctx, plan, report, bl, shortcuts)

	report.Duration = time.Since(start)

	e.logger.Info("sync cycle complete",
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

// postSyncHousekeeping runs non-critical cleanup after a sync cycle:
// .partial deletion and session file cleanup. Synchronous — completes
// before RunOnce returns to guarantee cleanup on process exit.
func (e *Engine) postSyncHousekeeping() {
	driveops.CleanTransferArtifacts(e.syncRoot, e.sessionStore, e.logger)
}

// executePlan populates the dependency tracker and runs the worker pool.
// In one-shot mode, drains the result channel synchronously after pool
// shutdown to process failures, 403 permission checks, and upload issues.
func (e *Engine) executePlan(
	ctx context.Context, plan *ActionPlan, report *SyncReport,
	bl *Baseline, shortcuts []Shortcut,
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

	tracker := NewDepTracker(len(plan.Actions), e.logger)

	for i := range plan.Actions {
		id := int64(i)

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		// Dispatch state transition: pending/failed → in-progress.
		e.setDispatch(ctx, &plan.Actions[i])

		// One-shot mode: no per-cycle tracking needed (empty cycleID).
		tracker.Add(&plan.Actions[i], id, depIDs, "")
	}

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, len(plan.Actions))
	pool.Start(ctx, e.transferWorkers)
	pool.Wait()
	pool.Stop()

	// Drain results synchronously — process failures, 403s, upload issues.
	// After Stop(), the results channel is closed; range terminates cleanly.
	for r := range pool.Results() {
		e.processWorkerResult(ctx, r, bl, shortcuts)
	}

	report.Succeeded, report.Failed, report.Errors = pool.Stats()
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

// observeLocal scans the local filesystem for changes.
func (e *Engine) observeLocal(ctx context.Context, bl *Baseline) ([]ChangeEvent, error) {
	obs := NewLocalObserver(bl, e.logger, e.checkWorkers)

	events, err := obs.FullScan(ctx, e.syncRoot)
	if err != nil {
		return nil, fmt.Errorf("sync: local scan: %w", err)
	}

	return events, nil
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

	var localEvents []ChangeEvent

	if mode != SyncDownloadOnly {
		localEvents, err = e.observeLocal(ctx, bl)
		if err != nil {
			return nil, err
		}
	}

	buf := NewBuffer(e.logger)
	buf.AddAll(remoteEvents)
	buf.AddAll(shortcutEvents)
	buf.AddAll(localEvents)

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

// loadWatchState loads the baseline and shortcuts for the watch session.
// Both are loaded once after the initial sync and reused throughout.
func (e *Engine) loadWatchState(ctx context.Context) (*Baseline, []Shortcut, error) {
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	shortcuts, scErr := e.baseline.ListShortcuts(ctx)
	if scErr != nil {
		e.logger.Warn("failed to load shortcuts for watch mode",
			slog.String("error", scErr.Error()),
		)
	}

	return bl, shortcuts, nil
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
	// shortcuts (frozen after initial sync) for use throughout the watch session.
	bl, watchShortcuts, err := e.loadWatchState(ctx)
	if err != nil {
		return err
	}

	// Step 3: Create persistent tracker and worker pool.
	tracker := NewPersistentDepTracker(e.logger)
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

		if dropped := pool.DroppedErrors(); dropped > 0 {
			e.logger.Warn("diagnostic error buffer overflowed",
				slog.Int64("dropped_errors", dropped),
				slog.Int("max_recorded", maxRecordedErrors),
			)
		}

		e.logger.Info("watch mode stopped")
	}()

	// Drain worker results in a background goroutine for failure tracking.
	// Baseline and shortcuts are passed explicitly — no store queries during execution.
	go e.drainWorkerResults(ctx, pool.Results(), bl, watchShortcuts)

	// Step 4: Create buffer and debounced output.
	buf := NewBuffer(e.logger)
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	// Step 4b: Start failure retrier for automatic retry of failed items.
	// Created after buf so it can re-inject synthesized events.
	e.retrier = NewFailureRetrier(DefaultFailureRetrierConfig(), e.baseline, e.baseline, e.baseline, buf, tracker, e.logger)
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

// processWorkerResult handles a single worker result: records failures in
// remote_state, checks permissions on 403s, records upload issues in
// local_issues, and kicks the retrier. Shared by both one-shot (RunOnce)
// and watch (drainWorkerResults) paths.
func (e *Engine) processWorkerResult(ctx context.Context, r WorkerResult, bl *Baseline, shortcuts []Shortcut) {
	if !r.Success {
		// Check permissions FIRST for 403s — if read-only, skip remote_state
		// recording entirely. Permission-denied items should not enter the
		// retry/escalation pipeline.
		if r.HTTPStatus == http.StatusForbidden && e.permChecker != nil {
			if e.handle403(ctx, bl, r.Path, shortcuts) {
				return
			}
		}

		// Non-permission failures: record in remote_state for retry.
		if recErr := e.baseline.RecordFailure(ctx, r.Path, r.ErrMsg, r.HTTPStatus); recErr != nil {
			e.logger.Warn("failed to record failure in remote_state",
				slog.String("path", r.Path),
				slog.String("error", recErr.Error()),
			)
		}

		// Also record upload failures in local_issues for user visibility.
		if r.ActionType == ActionUpload {
			if issueErr := e.baseline.RecordLocalIssue(ctx, r.Path, "upload_failed", r.ErrMsg, r.HTTPStatus, 0, ""); issueErr != nil {
				e.logger.Warn("failed to record upload issue in local_issues",
					slog.String("path", r.Path),
					slog.String("error", issueErr.Error()),
				)
			}
		}
	}

	// Kick retrier after every result (success or failure) so it
	// can re-evaluate retry timers and dispatch newly retriable items.
	if e.retrier != nil {
		e.retrier.Kick()
	}
}

// drainWorkerResults reads from the worker result channel and persists
// failures to remote_state for durable retry tracking. Used by watch mode.
// Receives baseline and shortcuts as explicit parameters.
func (e *Engine) drainWorkerResults(ctx context.Context, results <-chan WorkerResult, bl *Baseline, shortcuts []Shortcut) {
	for {
		select {
		case r, ok := <-results:
			if !ok {
				return
			}

			e.processWorkerResult(ctx, r, bl, shortcuts)

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

			scanEvents, err := obs.FullScan(ctx, syncRoot)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				e.logger.Warn("periodic full scan failed",
					slog.String("error", err.Error()),
				)

				continue
			}

			for i := range scanEvents {
				obs.trySend(ctx, events, &scanEvents[i])
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

	// Pre-upload validation for watch-mode batches.
	plan = e.filterInvalidUploads(ctx, plan)

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

		tracker.Add(&plan.Actions[i], id, depIDs, "")
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

// filterInvalidUploads runs pre-upload validation on the plan, recording
// permanent issues for rejected uploads and returning a filtered plan.
func (e *Engine) filterInvalidUploads(ctx context.Context, plan *ActionPlan) *ActionPlan {
	keep, failures := validateUploadActions(plan.Actions)
	if len(failures) == 0 {
		return plan
	}

	recordErrors := 0

	for _, f := range failures {
		e.logger.Warn("pre-upload validation failed",
			slog.String("path", f.Path),
			slog.String("issue_type", f.IssueType),
			slog.String("error", f.Error),
		)

		var fileSize int64
		if plan.Actions[f.Index].View != nil && plan.Actions[f.Index].View.Local != nil {
			fileSize = plan.Actions[f.Index].View.Local.Size
		}

		if recErr := e.baseline.RecordLocalIssue(ctx, f.Path, f.IssueType, f.Error, 0, fileSize, ""); recErr != nil {
			e.logger.Error("failed to record local issue",
				slog.String("path", f.Path),
				slog.String("error", recErr.Error()),
			)

			recordErrors++
		}
	}

	if recordErrors > 0 {
		e.logger.Error("local issue recording failures",
			slog.Int("failed", recordErrors),
			slog.Int("total", len(failures)),
		)
	}

	return removeActionsByIndex(plan, keep)
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

	e.logger.Info("periodic full reconciliation complete",
		slog.Int("paths", len(batch)),
	)
}
