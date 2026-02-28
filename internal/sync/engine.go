package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"runtime"
	stdsync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// forceSafetyMax is the maximum threshold used when --force is set,
// effectively disabling big-delete protection.
const forceSafetyMax = math.MaxInt32

// EngineConfig holds the options for NewEngine. Uses a struct because
// seven fields is too many for positional parameters.
type EngineConfig struct {
	DBPath        string        // path to the SQLite state database
	SyncRoot      string        // absolute path to the local sync directory
	DataDir       string        // application data directory for session files (optional)
	DriveID       driveid.ID    // normalized drive identifier
	Fetcher       DeltaFetcher  // satisfied by *graph.Client
	Items         ItemClient    // satisfied by *graph.Client
	Downloads     Downloader    // satisfied by *graph.Client
	Uploads       Uploader      // satisfied by *graph.Client
	DriveVerifier DriveVerifier // optional: verified at startup (B-074); nil skips check
	Logger        *slog.Logger
	UseLocalTrash bool // move deleted local files to OS trash instead of permanent delete
}

// RunOpts holds per-cycle options for RunOnce.
type RunOpts struct {
	DryRun bool
	Force  bool
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
// Single-drive only; multi-drive orchestration is deferred to Phase 5.
type Engine struct {
	baseline      *BaselineManager
	planner       *Planner
	execCfg       *ExecutorConfig
	fetcher       DeltaFetcher
	driveVerifier DriveVerifier   // optional (B-074)
	failures      *failureTracker // watch mode only (B-123)
	syncRoot      string
	driveID       driveid.ID
	logger        *slog.Logger
	remoteObs     *RemoteObserver // stored during RunWatch for delta token reads
	localObs      *LocalObserver  // stored during RunWatch for drop counter reads

	// In-memory per-cycle failure counts. Fed by drainWorkerResults, read by
	// watchCycleCompletion for delta token commit decisions.
	cycleFailuresMu stdsync.Mutex
	cycleFailures   map[string]int
}

// NewEngine creates an Engine, initializing the BaselineManager (which opens
// the SQLite database and runs migrations). Returns an error if DB init fails.
func NewEngine(cfg *EngineConfig) (*Engine, error) {
	bm, err := NewBaselineManager(cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	execCfg := NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, cfg.SyncRoot, cfg.DriveID, cfg.Logger)

	if cfg.UseLocalTrash {
		execCfg.trashFunc = defaultTrashFunc
	}

	if cfg.DataDir != "" {
		execCfg.sessionStore = NewSessionStore(cfg.DataDir, cfg.Logger)
		execCfg.transferMgr.sessionStore = execCfg.sessionStore
	}

	return &Engine{
		baseline:      bm,
		planner:       NewPlanner(cfg.Logger),
		execCfg:       execCfg,
		fetcher:       cfg.Fetcher,
		driveVerifier: cfg.DriveVerifier,
		syncRoot:      cfg.SyncRoot,
		driveID:       cfg.DriveID,
		logger:        cfg.Logger,
		cycleFailures: make(map[string]int),
	}, nil
}

// Close releases resources held by the engine (database connection).
func (e *Engine) Close() error {
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

	// Step 1: Load baseline.
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return nil, fmt.Errorf("sync: loading baseline: %w", err)
	}

	// Step 2: Observe remote changes.
	var remoteEvents []ChangeEvent
	var deltaToken string

	if mode != SyncUploadOnly {
		remoteEvents, deltaToken, err = e.observeRemote(ctx, bl)
		if err != nil {
			return nil, err
		}
	}

	// Step 3: Observe local changes.
	var localEvents []ChangeEvent

	if mode != SyncDownloadOnly {
		localEvents, err = e.observeLocal(ctx, bl)
		if err != nil {
			return nil, err
		}
	}

	// Step 4: Buffer and flush.
	buf := NewBuffer(e.logger)
	buf.AddAll(remoteEvents)
	buf.AddAll(localEvents)

	changes := buf.FlushImmediate()

	// Step 5: Early return if no changes.
	if len(changes) == 0 {
		e.logger.Info("sync cycle complete: no changes detected",
			slog.Duration("duration", time.Since(start)),
		)

		return &SyncReport{
			Mode:     mode,
			DryRun:   opts.DryRun,
			Duration: time.Since(start),
		}, nil
	}

	// Step 6: Plan actions.
	safety := e.resolveSafetyConfig(opts)

	plan, err := e.planner.Plan(changes, bl, mode, safety)
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

	// Execute plan: run workers, then commit delta token if all succeeded.
	e.executePlan(ctx, plan, deltaToken, report)

	report.Duration = time.Since(start)

	e.logger.Info("sync cycle complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
	)

	// Post-sync housekeeping: report stale .partial files (async to avoid
	// blocking sync completion) and clean old sessions.
	go reportStalePartials(e.syncRoot, stalePartialThreshold, e.logger)

	if e.execCfg.sessionStore != nil {
		if n, cleanErr := e.execCfg.sessionStore.CleanStale(staleSessionAge); cleanErr != nil {
			e.logger.Warn("stale session cleanup failed", slog.String("error", cleanErr.Error()))
		} else if n > 0 {
			e.logger.Info("cleaned stale upload sessions", slog.Int("count", n))
		}
	}

	return report, nil
}

// executePlan populates the dependency tracker, runs the worker pool,
// and commits the delta token after completion.
func (e *Engine) executePlan(
	ctx context.Context, plan *ActionPlan, deltaToken string, report *SyncReport,
) {
	// Guard: changes existed but all classified to no-op actions, producing
	// an empty plan. Commit the delta token and return — no work to do.
	if len(plan.Actions) == 0 {
		if commitErr := e.baseline.CommitDeltaToken(ctx, deltaToken, e.driveID.String()); commitErr != nil {
			e.logger.Error("failed to commit delta token", slog.String("error", commitErr.Error()))
		}

		return
	}

	tracker := NewDepTracker(len(plan.Actions), len(plan.Actions), e.logger)

	for i := range plan.Actions {
		id := int64(i)

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, int64(depIdx))
		}

		tracker.Add(&plan.Actions[i], id, depIDs, plan.CycleID)
	}

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, len(plan.Actions))
	pool.Start(ctx, runtime.NumCPU())
	pool.Wait()
	pool.Stop()

	report.Succeeded, report.Failed, report.Errors = pool.Stats()

	// Only advance the delta token when the entire cycle succeeded. If any
	// action failed, the token stays at the previous value so the next sync
	// re-observes the items that failed.
	if report.Failed == 0 {
		if commitErr := e.baseline.CommitDeltaToken(ctx, deltaToken, e.driveID.String()); commitErr != nil {
			e.logger.Error("failed to commit delta token", slog.String("error", commitErr.Error()))
		}
	} else {
		e.logger.Warn("skipping delta token commit due to failed actions",
			slog.Int("failed", report.Failed),
		)
	}
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
	savedToken, err := e.baseline.GetDeltaToken(ctx, e.driveID.String())
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
	obs := NewLocalObserver(bl, e.logger)

	events, err := obs.FullScan(ctx, e.syncRoot)
	if err != nil {
		return nil, fmt.Errorf("sync: local scan: %w", err)
	}

	return events, nil
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
// Watch mode (Phase 5.2)
// ---------------------------------------------------------------------------

// Default watch intervals.
const (
	defaultPollInterval = 5 * time.Minute
	defaultDebounce     = 2 * time.Second
	watchEventBuf       = 256
	// watchResultBuf is the buffer size for the worker result channel in watch
	// mode. Large enough for typical batches without blocking workers.
	watchResultBuf = 4096
	// stalePartialThreshold is the age after which .partial files are reported
	// as stale at the end of a sync cycle.
	stalePartialThreshold = 48 * time.Hour
)

// WatchOpts holds per-session options for RunWatch.
type WatchOpts struct {
	Force              bool
	PollInterval       time.Duration // remote delta polling interval (0 → 5m)
	Debounce           time.Duration // buffer debounce window (0 → 2s)
	SafetyScanInterval time.Duration // local safety scan interval (0 → 5m) (B-099)
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

	// Step 2: Load baseline. bl is loaded once and reused across all batches.
	// This is safe because Baseline.Load() returns a cached object that
	// CommitOutcome() updates in-place under RWMutex — each processBatch
	// call sees the latest state.
	bl, err := e.baseline.Load(ctx)
	if err != nil {
		return fmt.Errorf("sync: loading baseline for watch: %w", err)
	}

	// Step 3: Create persistent tracker, failure tracker, and worker pool.
	e.failures = newFailureTracker(e.logger)
	tracker := NewPersistentDepTracker(e.logger)
	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.logger, watchResultBuf)
	pool.Start(ctx, runtime.NumCPU())

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

	// Drain worker results in a background goroutine for failure tracking.
	go e.drainWorkerResults(ctx, pool.Results())

	// Step 4: Create buffer and debounced output.
	buf := NewBuffer(e.logger)
	ready := buf.FlushDebounced(ctx, e.resolveDebounce(opts))

	// Step 5: Start observer goroutines.
	errs, activeObservers := e.startObservers(ctx, bl, mode, buf, opts)

	// Step 6: Main select loop.
	safety := e.resolveWatchSafetyConfig(opts)

	for {
		select {
		case batch, ok := <-ready:
			if !ok {
				return nil
			}

			e.processBatch(ctx, batch, bl, mode, safety, tracker)

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

// drainWorkerResults reads from the worker result channel and feeds
// successes/failures into the failure tracker for B-123 suppression.
// Also feeds per-cycle failure counts into the cycleFailures map
// for delta token commit decisions.
func (e *Engine) drainWorkerResults(ctx context.Context, results <-chan WorkerResult) {
	for {
		select {
		case r, ok := <-results:
			if !ok {
				return
			}

			if e.failures == nil {
				continue
			}

			if r.Success {
				e.failures.recordSuccess(r.Path)
			} else {
				e.failures.recordFailure(r.Path, r.ErrMsg)
			}

			// Track per-cycle failures for delta token commit decision.
			if !r.Success {
				e.cycleFailuresMu.Lock()
				e.cycleFailures[r.CycleID]++
				e.cycleFailuresMu.Unlock()
			}

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
		e.remoteObs = remoteObs

		savedToken, tokenErr := e.baseline.GetDeltaToken(ctx, e.driveID.String())
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
		localObs := NewLocalObserver(bl, e.logger)
		localObs.safetyScanInterval = opts.SafetyScanInterval
		e.localObs = localObs

		obsWg.Add(1)
		count++

		go func() {
			defer obsWg.Done()
			errs <- localObs.Watch(ctx, e.syncRoot, events)
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

	plan, err := e.planner.Plan(batch, bl, mode, safety)
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

	// B-123: Build set of suppressed indices for paths that fail repeatedly.
	suppressed := make(map[int]bool)

	if e.failures != nil {
		for i := range plan.Actions {
			if e.failures.shouldSkip(plan.Actions[i].Path) {
				e.logger.Warn("skipping repeatedly-failing path",
					slog.String("path", plan.Actions[i].Path),
				)

				suppressed[i] = true
			}
		}
	}

	if len(suppressed) == len(plan.Actions) {
		e.logger.Debug("all actions suppressed due to repeated failures")
		// Early return before cycleFailures init and watchCycleCompletion spawn.
		// No actions dispatched → no results to drain → no cycle to complete.
		return
	}

	// Initialize per-cycle failure counter.
	e.cycleFailuresMu.Lock()
	e.cycleFailures[plan.CycleID] = 0
	e.cycleFailuresMu.Unlock()

	// Populate tracker with non-suppressed actions using sequential IDs.
	for i := range plan.Actions {
		if suppressed[i] {
			continue
		}

		id := int64(i)

		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			if suppressed[depIdx] {
				continue // suppressed dep won't be tracked; skip to avoid phantom dependency
			}

			depIDs = append(depIDs, int64(depIdx))
		}

		tracker.Add(&plan.Actions[i], id, depIDs, plan.CycleID)
	}

	// Spawn cycle completion watcher (B-121: per-cycle delta token tracking).
	go e.watchCycleCompletion(ctx, tracker, plan.CycleID)

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)-len(suppressed)),
		slog.String("cycle_id", plan.CycleID),
	)
}

// watchCycleCompletion waits for all actions in a cycle to complete, then
// commits the delta token if no failures occurred. This ensures the token
// is only advanced for fully-successful cycles (B-121).
func (e *Engine) watchCycleCompletion(ctx context.Context, tracker *DepTracker, cycleID string) {
	select {
	case <-tracker.CycleDone(cycleID):
	case <-ctx.Done():
		return
	}

	// Check in-memory failure count for this cycle.
	e.cycleFailuresMu.Lock()
	failed := e.cycleFailures[cycleID]
	delete(e.cycleFailures, cycleID)
	e.cycleFailuresMu.Unlock()

	if failed > 0 {
		e.logger.Warn("skipping delta token commit for cycle with failures",
			slog.String("cycle_id", cycleID),
			slog.Int("failed", failed),
		)

		tracker.CleanupCycle(cycleID)

		return
	}

	// Commit the delta token for this successful cycle.
	// Read the latest delta token, not the one from batch arrival time.
	// This is safe because delta tokens are monotonically advancing —
	// a later token always subsumes earlier ones. Using the latest token
	// avoids re-processing changes that arrived while this cycle executed.
	if e.remoteObs != nil {
		token := e.remoteObs.CurrentDeltaToken()
		if commitErr := e.baseline.CommitDeltaToken(ctx, token, e.driveID.String()); commitErr != nil {
			e.logger.Error("failed to commit delta token for watch cycle",
				slog.String("cycle_id", cycleID),
				slog.String("error", commitErr.Error()),
			)
		}
	}

	// Log and reset dropped local events for this cycle. ResetDroppedEvents
	// atomically reads and zeros the counter, preventing double-counting
	// across cycles (B-190).
	if e.localObs != nil {
		if dropped := e.localObs.ResetDroppedEvents(); dropped > 0 {
			e.logger.Warn("local observer dropped events due to channel backpressure",
				slog.String("cycle_id", cycleID),
				slog.Int64("dropped_this_cycle", dropped),
			)
		}
	}

	tracker.CleanupCycle(cycleID)
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
