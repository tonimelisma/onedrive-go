package sync

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// nanosecondsPerMillisecond converts nanoseconds to milliseconds for log output.
const nanosecondsPerMillisecond = 1_000_000

// GraphClient combines the Graph API interfaces needed by the sync engine.
// Satisfied by *graph.Client — no adapter needed.
type GraphClient interface {
	DeltaFetcher
	ItemClient
	TransferClient
}

// SyncOptions holds per-invocation flags from CLI that override config.
type SyncOptions struct {
	Force  bool // override big-delete protection (S5)
	DryRun bool // preview actions without executing
}

// Engine orchestrates the full sync pipeline: delta → scan → reconcile →
// safety → execute → cleanup. Components are built in the constructor and
// reused across RunOnce invocations. The store is injected (caller owns its
// lifecycle); the TransferManager is owned by the engine.
type Engine struct {
	store       Store
	delta       *DeltaProcessor
	scanner     *Scanner
	reconciler  *Reconciler
	safety      *SafetyChecker
	executor    *Executor
	transferMgr *TransferManager

	// running tracks in-flight RunOnce calls so Close can wait for them to
	// finish before tearing down the TransferManager (use-after-close safety).
	running gosync.WaitGroup

	driveID                string
	syncRoot               string
	tombstoneRetentionDays int
	logger                 *slog.Logger
}

// NewEngine constructs an Engine from a store, graph client, and resolved drive config.
// The store is not owned by the engine — the caller must close it.
// The engine builds all internal components (delta, scanner, filter, reconciler,
// safety, executor, transfer manager) and wires them together.
func NewEngine(
	store Store,
	graphClient GraphClient,
	resolved *config.ResolvedDrive,
	logger *slog.Logger,
) (*Engine, error) {
	if logger == nil {
		logger = slog.Default()
	}

	logger.Info("engine: initializing",
		"drive_id", resolved.DriveID,
		"sync_root", resolved.SyncDir,
	)

	// Build filter engine from resolved config.
	filterCfg := NewFilterConfig(resolved)

	filterEngine, err := NewFilterEngine(&filterCfg, resolved.SyncDir, logger)
	if err != nil {
		return nil, fmt.Errorf("engine: create filter: %w", err)
	}

	safetyCfg := NewSafetyConfig(resolved)

	e := &Engine{
		store:                  store,
		delta:                  NewDeltaProcessor(graphClient, store, logger),
		scanner:                NewScanner(store, filterEngine, resolved.SkipSymlinks, logger),
		reconciler:             NewReconciler(store, logger),
		safety:                 NewSafetyChecker(store, safetyCfg, resolved.SyncDir, logger),
		executor:               NewExecutor(store, graphClient, graphClient, resolved.SyncDir, safetyCfg, &resolved.TransfersConfig, logger),
		driveID:                resolved.DriveID,
		syncRoot:               resolved.SyncDir,
		tombstoneRetentionDays: resolved.TombstoneRetentionDays,
		logger:                 logger,
	}

	tm, err := NewTransferManager(e.executor, &resolved.TransfersConfig, logger)
	if err != nil {
		return nil, fmt.Errorf("engine: create transfer manager: %w", err)
	}

	e.transferMgr = tm
	e.executor.SetTransferManager(tm)

	logger.Info("engine: initialized")

	return e, nil
}

// RunOnce executes a single sync cycle: fetch remote changes, scan local files,
// reconcile differences, check safety invariants, and execute the action plan.
// Returns a SyncReport summarizing what happened. Errors from delta or scan
// abort the cycle — running reconciliation on stale state violates safety invariants.
func (e *Engine) RunOnce(ctx context.Context, mode SyncMode, opts SyncOptions) (*SyncReport, error) {
	e.running.Add(1)
	defer e.running.Done()

	startedAt := NowNano()

	e.logger.Info("engine: sync cycle started",
		"mode", mode,
		"drive_id", e.driveID,
		"sync_root", e.syncRoot,
		"force", opts.Force,
		"dry_run", opts.DryRun,
	)

	// Phase 1: Delta fetch (skip for upload-only mode).
	if err := e.runDelta(ctx, mode); err != nil {
		return nil, fmt.Errorf("engine: delta fetch: %w", err)
	}

	// Phase 2: Local scan (skip for download-only mode).
	if err := e.runScan(ctx, mode); err != nil {
		return nil, fmt.Errorf("engine: local scan: %w", err)
	}

	// Phase 3: Reconcile.
	plan, err := e.reconciler.Reconcile(ctx, mode)
	if err != nil {
		return nil, fmt.Errorf("engine: reconcile: %w", err)
	}

	e.logger.Info("engine: reconciliation complete", "total_actions", plan.TotalActions())

	// Inject drive ID into scanner-originated actions (B-050).
	// Scanner items have empty DriveID because the scanner is a local-filesystem
	// component. The engine is the correct injection point.
	e.populateDriveID(plan)

	// Phase 4: Safety check.
	plan, err = e.safety.Check(ctx, plan, opts.Force, opts.DryRun)
	if err != nil {
		return nil, fmt.Errorf("engine: safety check: %w", err)
	}

	// Phase 5: Execute (or build dry-run preview).
	report, err := e.runExecute(ctx, plan, opts.DryRun)
	if err != nil {
		return nil, fmt.Errorf("engine: execute: %w", err)
	}

	// Phase 6: Tombstone cleanup (skip in dry-run, best-effort).
	if !opts.DryRun {
		e.cleanupTombstones(ctx)
	}

	// Augment report with engine-level metadata.
	report.StartedAt = startedAt
	report.CompletedAt = NowNano()
	report.Mode = mode
	report.DryRun = opts.DryRun

	durationMs := (report.CompletedAt - report.StartedAt) / nanosecondsPerMillisecond

	e.logger.Info("engine: sync cycle complete",
		"duration_ms", durationMs,
		"downloaded", report.Downloaded,
		"uploaded", report.Uploaded,
		"local_deleted", report.LocalDeleted,
		"remote_deleted", report.RemoteDeleted,
		"conflicts", report.Conflicts,
		"errors", len(report.Errors),
	)

	return report, nil
}

// Close releases engine-owned resources. The store is NOT closed — the caller owns it.
// Close waits for any in-flight RunOnce calls to finish before tearing down the
// TransferManager, preventing use-after-close races.
func (e *Engine) Close() {
	e.running.Wait()

	if e.transferMgr != nil {
		e.transferMgr.Close()
	}

	e.logger.Info("engine: closed")
}

// runDelta fetches remote changes unless mode is upload-only.
func (e *Engine) runDelta(ctx context.Context, mode SyncMode) error {
	if mode == SyncUploadOnly {
		e.logger.Info("engine: skipping delta fetch (upload-only mode)")
		return nil
	}

	return e.delta.FetchAndApply(ctx, e.driveID)
}

// runScan walks the local filesystem unless mode is download-only.
func (e *Engine) runScan(ctx context.Context, mode SyncMode) error {
	if mode == SyncDownloadOnly {
		e.logger.Info("engine: skipping local scan (download-only mode)")
		return nil
	}

	return e.scanner.Scan(ctx, e.syncRoot)
}

// runExecute dispatches the action plan or builds a dry-run preview.
func (e *Engine) runExecute(ctx context.Context, plan *ActionPlan, dryRun bool) (*SyncReport, error) {
	if dryRun {
		e.logger.Info("engine: dry-run mode — skipping execution")
		return buildDryRunReport(plan), nil
	}

	return e.executor.Execute(ctx, plan)
}

// cleanupTombstones removes expired tombstone records. Best-effort: errors are
// logged as warnings but do not fail the sync cycle.
func (e *Engine) cleanupTombstones(ctx context.Context) {
	cleaned, err := e.store.CleanupTombstones(ctx, e.tombstoneRetentionDays)
	if err != nil {
		e.logger.Warn("engine: tombstone cleanup failed", "error", err)
		return
	}

	if cleaned > 0 {
		e.logger.Info("engine: tombstones cleaned", "count", cleaned)
	}
}

// populateDriveID sets the engine's drive ID on actions and items where
// DriveID is empty. Scanner-created items lack drive identity because the
// scanner is a local-filesystem component. The engine injects it here,
// after reconciliation but before safety checks and execution (B-050).
func (e *Engine) populateDriveID(plan *ActionPlan) {
	allSlices := []*[]Action{
		&plan.FolderCreates, &plan.Moves,
		&plan.Downloads, &plan.Uploads,
		&plan.LocalDeletes, &plan.RemoteDeletes,
		&plan.Conflicts, &plan.SyncedUpdates, &plan.Cleanups,
	}
	for _, slice := range allSlices {
		for i := range *slice {
			a := &(*slice)[i]
			if a.DriveID == "" {
				a.DriveID = e.driveID
			}

			// Do NOT set a.Item.DriveID here. The item's DriveID must
			// reflect the actual DB key so the executor can capture the
			// pre-mutation key and delete the stale scanner row (B-050).
			// The executor injects action.DriveID into the item after
			// capturing the old key.
		}
	}
}

// buildDryRunReport creates a preview report from action plan counts
// without executing any operations.
func buildDryRunReport(plan *ActionPlan) *SyncReport {
	return &SyncReport{
		FoldersCreated: len(plan.FolderCreates),
		Moved:          len(plan.Moves),
		Downloaded:     len(plan.Downloads),
		Uploaded:       len(plan.Uploads),
		LocalDeleted:   len(plan.LocalDeletes),
		RemoteDeleted:  len(plan.RemoteDeletes),
		Conflicts:      len(plan.Conflicts),
		SyncedUpdates:  len(plan.SyncedUpdates),
		Cleanups:       len(plan.Cleanups),
	}
}
