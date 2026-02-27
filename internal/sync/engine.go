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
	DBPath        string       // path to the SQLite state database
	SyncRoot      string       // absolute path to the local sync directory
	DriveID       driveid.ID   // normalized drive identifier
	Fetcher       DeltaFetcher // satisfied by *graph.Client
	Items         ItemClient   // satisfied by *graph.Client
	Downloads     Downloader   // satisfied by *graph.Client
	Uploads       Uploader     // satisfied by *graph.Client
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
	baseline  *BaselineManager
	ledger    *Ledger
	planner   *Planner
	execCfg   *ExecutorConfig
	fetcher   DeltaFetcher
	syncRoot  string
	driveID   driveid.ID
	logger    *slog.Logger
	remoteObs *RemoteObserver // stored during RunWatch for delta token reads
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

	ledger := NewLedger(bm.DB(), cfg.Logger)

	return &Engine{
		baseline: bm,
		ledger:   ledger,
		planner:  NewPlanner(cfg.Logger),
		execCfg:  execCfg,
		fetcher:  cfg.Fetcher,
		syncRoot: cfg.SyncRoot,
		driveID:  cfg.DriveID,
		logger:   cfg.Logger,
	}, nil
}

// Close releases resources held by the engine (database connection).
func (e *Engine) Close() error {
	return e.baseline.Close()
}

// RunOnce executes a single sync cycle:
//  1. Load baseline
//  2. Observe remote (skip if upload-only)
//  3. Observe local (skip if download-only)
//  4. Buffer and flush changes
//  5. Early return if no changes
//  6. Plan actions (flat list + dependency edges)
//  7. Return early if dry-run
//  8. Write actions to ledger, build tracker, start worker pool
//  9. Wait for completion, commit delta token
func (e *Engine) RunOnce(ctx context.Context, mode SyncMode, opts RunOpts) (*SyncReport, error) {
	start := time.Now()

	e.logger.Info("sync cycle starting",
		slog.String("mode", mode.String()),
		slog.Bool("dry_run", opts.DryRun),
		slog.Bool("force", opts.Force),
	)

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

	// Steps 8-9: Execute plan and commit delta token.
	if execErr := e.executePlan(ctx, plan, deltaToken, report); execErr != nil {
		return report, execErr
	}

	report.Duration = time.Since(start)

	e.logger.Info("sync cycle complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
	)

	return report, nil
}

// executePlan writes actions to the ledger, populates the dependency tracker,
// runs the worker pool, and commits the delta token after completion.
func (e *Engine) executePlan(
	ctx context.Context, plan *ActionPlan, deltaToken string, report *SyncReport,
) error {
	// Guard: changes existed but all classified to no-op actions, producing
	// an empty plan. Commit the delta token and return — no work to do.
	if len(plan.Actions) == 0 {
		if commitErr := e.baseline.CommitDeltaToken(ctx, deltaToken, e.driveID.String()); commitErr != nil {
			e.logger.Error("failed to commit delta token", slog.String("error", commitErr.Error()))
		}

		return nil
	}

	ids, writeErr := e.ledger.WriteActions(ctx, plan.Actions, plan.Deps, plan.CycleID)
	if writeErr != nil {
		return fmt.Errorf("sync: writing actions to ledger: %w", writeErr)
	}

	tracker := NewDepTracker(len(plan.Actions), len(plan.Actions), e.logger)

	for i := range plan.Actions {
		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, ids[depIdx])
		}

		tracker.Add(&plan.Actions[i], ids[i], depIDs, plan.CycleID)
	}

	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.ledger, e.logger)
	pool.Start(ctx, runtime.NumCPU())
	pool.Wait()
	pool.Stop()

	report.Succeeded, report.Failed, report.Errors = pool.Stats()

	// Only advance the delta token when the entire cycle succeeded. If any
	// action failed, the token stays at the previous value so the next sync
	// re-observes the items that failed. This matches the spec requirement
	// that the token is committed only when all actions reach "done"
	// (concurrent-execution.md §13.1). When multi-cycle overlap lands
	// (Phase 5.3), this guard should be refined to per-cycle tracking.
	if report.Failed == 0 {
		if commitErr := e.baseline.CommitDeltaToken(ctx, deltaToken, e.driveID.String()); commitErr != nil {
			e.logger.Error("failed to commit delta token", slog.String("error", commitErr.Error()))
		}
	} else {
		e.logger.Warn("skipping delta token commit due to failed actions",
			slog.Int("failed", report.Failed),
		)
	}

	return nil
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
// resolution and commits the result to the baseline. Uses CommitOutcome with
// ledgerID=0 (no ledger action for manual conflict resolution).
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

	return e.baseline.CommitOutcome(ctx, &outcome, 0)
}

// ---------------------------------------------------------------------------
// Watch mode (Phase 5.2)
// ---------------------------------------------------------------------------

// Default watch intervals.
const (
	defaultPollInterval = 5 * time.Minute
	defaultDebounce     = 2 * time.Second
	watchEventBuf       = 256
)

// WatchOpts holds per-session options for RunWatch.
type WatchOpts struct {
	Force        bool
	PollInterval time.Duration // remote delta polling interval (0 → 5m)
	Debounce     time.Duration // buffer debounce window (0 → 2s)
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

	// Step 3: Create persistent tracker and worker pool.
	tracker := NewPersistentDepTracker(e.logger)
	pool := NewWorkerPool(e.execCfg, tracker, e.baseline, e.ledger, e.logger)
	pool.Start(ctx, runtime.NumCPU())

	defer func() {
		pool.Stop()
		e.logger.Info("watch mode stopped")
	}()

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

	// Write actions to ledger.
	ids, writeErr := e.ledger.WriteActions(ctx, plan.Actions, plan.Deps, plan.CycleID)
	if writeErr != nil {
		e.logger.Error("failed to write watch batch to ledger",
			slog.String("error", writeErr.Error()),
		)

		return
	}

	// Populate tracker with this cycle's actions.
	for i := range plan.Actions {
		var depIDs []int64
		for _, depIdx := range plan.Deps[i] {
			depIDs = append(depIDs, ids[depIdx])
		}

		tracker.Add(&plan.Actions[i], ids[i], depIDs, plan.CycleID)
	}

	// Spawn cycle completion watcher (B-121: per-cycle delta token tracking).
	go e.watchCycleCompletion(ctx, tracker, plan.CycleID)

	e.logger.Info("watch batch dispatched",
		slog.Int("actions", len(plan.Actions)),
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

	// Check for failures in this cycle.
	failed, err := e.ledger.CountFailedForCycle(ctx, cycleID)
	if err != nil {
		e.logger.Error("failed to count failed actions for cycle",
			slog.String("cycle_id", cycleID),
			slog.String("error", err.Error()),
		)

		tracker.CleanupCycle(cycleID)

		return
	}

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
