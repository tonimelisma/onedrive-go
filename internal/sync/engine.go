package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// forceSafetyMax is the maximum threshold used when --force is set,
// effectively disabling big-delete protection.
const forceSafetyMax = math.MaxInt32

// EngineConfig holds the options for NewEngine. Uses a struct because
// seven fields is too many for positional parameters.
type EngineConfig struct {
	DBPath    string       // path to the SQLite state database
	SyncRoot  string       // absolute path to the local sync directory
	DriveID   driveid.ID   // normalized drive identifier
	Fetcher   DeltaFetcher // satisfied by *graph.Client
	Items     ItemClient   // satisfied by *graph.Client
	Downloads Downloader   // satisfied by *graph.Client
	Uploads   Uploader     // satisfied by *graph.Client
	Logger    *slog.Logger
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
	baseline *BaselineManager
	planner  *Planner
	execCfg  *ExecutorConfig
	fetcher  DeltaFetcher
	syncRoot string
	driveID  driveid.ID
	logger   *slog.Logger
}

// NewEngine creates an Engine, initializing the BaselineManager (which opens
// the SQLite database and runs migrations). Returns an error if DB init fails.
func NewEngine(cfg *EngineConfig) (*Engine, error) {
	bm, err := NewBaselineManager(cfg.DBPath, cfg.Logger)
	if err != nil {
		return nil, fmt.Errorf("sync: creating engine: %w", err)
	}

	execCfg := NewExecutorConfig(cfg.Items, cfg.Downloads, cfg.Uploads, cfg.SyncRoot, cfg.DriveID, cfg.Logger)

	return &Engine{
		baseline: bm,
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

// RunOnce executes a single sync cycle with 9 steps:
//  1. Load baseline
//  2. Observe remote (skip if upload-only)
//  3. Observe local (skip if download-only)
//  4. Buffer and flush changes
//  5. Early return if no changes
//  6. Plan actions
//  7. Return early if dry-run
//  8. Execute plan
//  9. Commit outcomes and delta token
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
	report := buildReport(plan, mode, opts)

	if opts.DryRun {
		report.Duration = time.Since(start)

		e.logger.Info("dry-run complete: no changes applied",
			slog.Duration("duration", report.Duration),
		)

		return report, nil
	}

	// Steps 8-9: Execute and commit.
	if err := e.executeAndCommit(ctx, plan, bl, deltaToken, report); err != nil {
		report.Duration = time.Since(start)
		return report, err
	}

	report.Duration = time.Since(start)

	e.logger.Info("sync cycle complete",
		slog.Duration("duration", report.Duration),
		slog.Int("succeeded", report.Succeeded),
		slog.Int("failed", report.Failed),
	)

	return report, nil
}

// buildReport populates a SyncReport with plan counts.
func buildReport(plan *ActionPlan, mode SyncMode, opts RunOpts) *SyncReport {
	return &SyncReport{
		Mode:          mode,
		DryRun:        opts.DryRun,
		FolderCreates: len(plan.FolderCreates),
		Moves:         len(plan.Moves),
		Downloads:     len(plan.Downloads),
		Uploads:       len(plan.Uploads),
		LocalDeletes:  len(plan.LocalDeletes),
		RemoteDeletes: len(plan.RemoteDeletes),
		Conflicts:     len(plan.Conflicts),
		SyncedUpdates: len(plan.SyncedUpdates),
		Cleanups:      len(plan.Cleanups),
	}
}

// executeAndCommit runs the executor and commits outcomes atomically.
// Partial outcomes are committed even on executor fatal error.
func (e *Engine) executeAndCommit(
	ctx context.Context, plan *ActionPlan, bl *Baseline, deltaToken string, report *SyncReport,
) error {
	exec := NewExecution(e.execCfg, bl)
	outcomes, execErr := exec.Execute(ctx, plan)
	report.Succeeded, report.Failed, report.Errors = classifyOutcomes(outcomes)

	if len(outcomes) > 0 {
		if commitErr := e.baseline.Commit(ctx, outcomes, deltaToken, e.driveID.String()); commitErr != nil {
			if execErr != nil {
				return fmt.Errorf("sync: commit failed after execution error: commit=%w, exec=%w", commitErr, execErr)
			}

			return fmt.Errorf("sync: committing outcomes: %w", commitErr)
		}
	}

	if execErr != nil {
		return fmt.Errorf("sync: execution error (partial outcomes committed): %w", execErr)
	}

	return nil
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
// resolution and commits the result to the baseline. Creates a fresh executor
// per call — no lazy initialization needed.
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

	return e.baseline.Commit(ctx, []Outcome{outcome}, "", e.driveID.String())
}

// classifyOutcomes counts successes and failures in an Outcome slice,
// collecting non-nil errors from failed outcomes.
func classifyOutcomes(outcomes []Outcome) (succeeded, failed int, errs []error) {
	for i := range outcomes {
		if outcomes[i].Success {
			succeeded++
		} else {
			failed++

			if outcomes[i].Error != nil {
				errs = append(errs, outcomes[i].Error)
			}
		}
	}

	return succeeded, failed, errs
}
