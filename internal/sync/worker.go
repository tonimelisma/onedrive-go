package sync

import (
	"context"
	"errors"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
)

var errUnknownActionType = errors.New("sync: unknown action type in worker dispatch")

const (
	// minWorkers is the floor for total worker count.
	minWorkers = 4
	// laneDivisor is used to compute reserved workers per lane (total/laneDivisor).
	laneDivisor = 8
	// minReserved is the minimum reserved workers per lane.
	minReserved = 2
)

// WorkerPool spawns goroutines that pull TrackedActions from the DepTracker's
// lane channels, execute them, commit outcomes, and signal completion back
// to the tracker for dependent dispatch.
type WorkerPool struct {
	cfg      *ExecutorConfig
	tracker  *DepTracker
	baseline *BaselineManager
	ledger   *Ledger
	logger   *slog.Logger

	succeeded atomic.Int32
	failed    atomic.Int32
	errors    []error
	errorsMu  stdsync.Mutex

	cancel context.CancelFunc
	wg     stdsync.WaitGroup
}

// NewWorkerPool creates a pool without starting any workers.
func NewWorkerPool(
	cfg *ExecutorConfig,
	tracker *DepTracker,
	baseline *BaselineManager,
	ledger *Ledger,
	logger *slog.Logger,
) *WorkerPool {
	return &WorkerPool{
		cfg:      cfg,
		tracker:  tracker,
		baseline: baseline,
		ledger:   ledger,
		logger:   logger,
	}
}

// Start spawns workers divided across interactive (reserved), bulk (reserved),
// and shared lanes. total is the desired concurrency (typically numCPU or
// a user-configured cap). Minimum 4 workers to guarantee at least 1 per lane
// type plus shared.
func (wp *WorkerPool) Start(ctx context.Context, total int) {
	if total < minWorkers {
		total = minWorkers
	}

	ctx, wp.cancel = context.WithCancel(ctx)

	reservedInteractive := max(minReserved, total/laneDivisor)
	reservedBulk := max(minReserved, total/laneDivisor)
	shared := total - reservedInteractive - reservedBulk

	if shared < 1 {
		shared = 1
	}

	// Reserved interactive workers: only read from interactive channel.
	for range reservedInteractive {
		wp.wg.Add(1)

		go wp.worker(ctx, wp.tracker.Interactive(), nil)
	}

	// Reserved bulk workers: only read from bulk channel.
	for range reservedBulk {
		wp.wg.Add(1)

		go wp.worker(ctx, nil, wp.tracker.Bulk())
	}

	// Shared workers: prefer interactive, fall back to bulk.
	for range shared {
		wp.wg.Add(1)

		go wp.worker(ctx, wp.tracker.Interactive(), wp.tracker.Bulk())
	}

	wp.logger.Info("worker pool started",
		slog.Int("interactive", reservedInteractive),
		slog.Int("bulk", reservedBulk),
		slog.Int("shared", shared),
	)
}

// Wait blocks until all tracked actions are complete (tracker.Done signal).
func (wp *WorkerPool) Wait() {
	<-wp.tracker.Done()
}

// Stop cancels all in-flight work and waits for goroutines to exit.
func (wp *WorkerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel()
	}

	wp.wg.Wait()
}

// Stats returns execution counters and any errors collected during execution.
func (wp *WorkerPool) Stats() (succeeded, failed int, errors []error) {
	wp.errorsMu.Lock()
	errs := make([]error, len(wp.errors))
	copy(errs, wp.errors)
	wp.errorsMu.Unlock()

	return int(wp.succeeded.Load()), int(wp.failed.Load()), errs
}

// worker is the main loop for a single goroutine. It reads from primary
// and/or secondary channels depending on the lane assignment.
func (wp *WorkerPool) worker(ctx context.Context, primary, secondary <-chan *TrackedAction) {
	defer wp.wg.Done()

	for {
		var ta *TrackedAction

		select {
		case <-ctx.Done():
			return
		case <-wp.tracker.Done():
			return
		case ta = <-primary:
		case ta = <-secondary:
		}

		if ta == nil {
			continue
		}

		wp.executeAction(ctx, ta)
	}
}

// executeAction runs a single tracked action: claim, execute, commit, complete.
func (wp *WorkerPool) executeAction(ctx context.Context, ta *TrackedAction) {
	// Per-action cancellable context.
	actionCtx, cancel := context.WithCancel(ctx)
	ta.Cancel = cancel

	defer cancel()

	// Claim in the ledger.
	if claimErr := wp.ledger.Claim(actionCtx, ta.LedgerID); claimErr != nil {
		wp.logger.Warn("worker: claim failed",
			slog.Int64("ledger_id", ta.LedgerID),
			slog.String("error", claimErr.Error()),
		)
		wp.recordFailure(claimErr)
		wp.tracker.Complete(ta.LedgerID)

		return
	}

	// Load baseline (cached after first call).
	bl, loadErr := wp.baseline.Load(actionCtx)
	if loadErr != nil {
		wp.logger.Error("worker: baseline load failed",
			slog.String("error", loadErr.Error()),
		)
		wp.recordFailure(loadErr)
		wp.failAndComplete(actionCtx, ta, loadErr.Error())

		return
	}

	// Execute the action.
	exec := NewExecution(wp.cfg, bl)
	outcome := wp.dispatchAction(actionCtx, exec, ta)

	// Commit outcome (baseline + ledger in one transaction).
	if commitErr := wp.baseline.CommitOutcome(actionCtx, &outcome, ta.LedgerID); commitErr != nil {
		wp.logger.Error("worker: commit outcome failed",
			slog.Int64("ledger_id", ta.LedgerID),
			slog.String("error", commitErr.Error()),
		)
		wp.recordFailure(commitErr)
		wp.tracker.Complete(ta.LedgerID)

		return
	}

	if outcome.Success {
		wp.succeeded.Add(1)
	} else {
		wp.recordFailure(outcome.Error)
	}

	// Signal completion to dispatch dependents.
	wp.tracker.Complete(ta.LedgerID)
}

// dispatchAction routes a tracked action to the appropriate executor method.
func (wp *WorkerPool) dispatchAction(
	ctx context.Context, exec *Executor, ta *TrackedAction,
) Outcome {
	action := &ta.Action

	switch action.Type {
	case ActionFolderCreate:
		return exec.executeFolderCreate(ctx, action)
	case ActionLocalMove, ActionRemoteMove:
		return exec.executeMove(ctx, action)
	case ActionDownload:
		return exec.executeDownload(ctx, action)
	case ActionUpload:
		return exec.executeUpload(ctx, action)
	case ActionLocalDelete:
		return exec.executeLocalDelete(ctx, action)
	case ActionRemoteDelete:
		return exec.executeRemoteDelete(ctx, action)
	case ActionConflict:
		return exec.executeConflict(ctx, action)
	case ActionUpdateSynced:
		return exec.executeSyncedUpdate(action)
	case ActionCleanup:
		return exec.executeCleanup(action)
	default:
		return Outcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   errUnknownActionType,
		}
	}
}

// recordFailure atomically appends an error to the pool's error list.
func (wp *WorkerPool) recordFailure(err error) {
	if err == nil {
		return
	}

	wp.failed.Add(1)
	wp.errorsMu.Lock()
	wp.errors = append(wp.errors, err)
	wp.errorsMu.Unlock()
}

// failAndComplete marks a ledger action as failed and signals tracker completion.
func (wp *WorkerPool) failAndComplete(ctx context.Context, ta *TrackedAction, errMsg string) {
	if failErr := wp.ledger.Fail(ctx, ta.LedgerID, errMsg); failErr != nil {
		wp.logger.Warn("worker: ledger fail recording failed",
			slog.Int64("ledger_id", ta.LedgerID),
			slog.String("error", failErr.Error()),
		)
	}

	wp.tracker.Complete(ta.LedgerID)
}
