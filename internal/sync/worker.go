package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync"
	"sync/atomic"
)

var errUnknownActionType = errors.New("sync: unknown action type in worker dispatch")

const (
	// minWorkers is the floor for total worker count.
	minWorkers = 4
	// maxRecordedErrors caps the diagnostic error slice to bound memory in
	// long-running watch mode. The failed atomic counter remains accurate
	// regardless of this cap (B-205).
	maxRecordedErrors = 1000
)

// WorkerPool spawns goroutines that pull TrackedActions from the DepTracker's
// ready channel, execute them, commit outcomes, and signal completion back
// to the tracker for dependent dispatch.
type WorkerPool struct {
	cfg      *ExecutorConfig
	tracker  *DepTracker
	baseline *BaselineManager
	logger   *slog.Logger

	succeeded     atomic.Int32
	failed        atomic.Int32
	errors        []error
	errorsMu      stdsync.Mutex
	droppedErrors atomic.Int64

	// results reports per-action outcomes back to the engine for in-memory
	// cycle result tracking.
	results chan WorkerResult

	cancel context.CancelFunc
	wg     stdsync.WaitGroup
}

// WorkerResult reports the outcome of a single action execution. The engine
// reads these from the Results channel for failure suppression and delta
// token commit decisions.
type WorkerResult struct {
	ID      int64
	CycleID string
	Path    string
	Success bool
	ErrMsg  string
}

// NewWorkerPool creates a pool without starting any workers. planSize
// determines the result channel buffer (use the number of actions in the
// plan for one-shot mode, or a generous buffer for watch mode).
func NewWorkerPool(
	cfg *ExecutorConfig,
	tracker *DepTracker,
	baseline *BaselineManager,
	logger *slog.Logger,
	planSize int,
) *WorkerPool {
	if planSize < 1 {
		planSize = 1
	}

	return &WorkerPool{
		cfg:      cfg,
		tracker:  tracker,
		baseline: baseline,
		logger:   logger,
		// Buffer sizing contract: one-shot mode uses planSize (equal to
		// the number of actions, so workers never block). Watch mode uses
		// watchResultBuf (4096) with a drain goroutine reading results
		// concurrently, so blocking is unlikely under normal load.
		results: make(chan WorkerResult, planSize),
	}
}

// Start spawns a flat pool of goroutines, all reading from the tracker's
// single ready channel. total is the desired concurrency (typically
// cfg.TransferWorkers). Minimum 4 workers.
func (wp *WorkerPool) Start(ctx context.Context, total int) {
	if total < minWorkers {
		total = minWorkers
	}

	ctx, wp.cancel = context.WithCancel(ctx)

	for range total {
		wp.wg.Add(1)

		go wp.worker(ctx)
	}

	wp.logger.Info("worker pool started",
		slog.Int("workers", total),
	)
}

// Wait blocks until all tracked actions are complete (tracker.Done signal).
func (wp *WorkerPool) Wait() {
	<-wp.tracker.Done()
}

// Stop cancels all in-flight work, waits for goroutines to exit, and closes
// the results channel so consumers (drainWorkerResults) can terminate.
func (wp *WorkerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel()
	}

	wp.wg.Wait()
	close(wp.results)
}

// Stats returns execution counters and any errors collected during execution.
func (wp *WorkerPool) Stats() (succeeded, failed int, errors []error) {
	wp.errorsMu.Lock()
	errs := make([]error, len(wp.errors))
	copy(errs, wp.errors)
	wp.errorsMu.Unlock()

	return int(wp.succeeded.Load()), int(wp.failed.Load()), errs
}

// worker is the main loop for a single goroutine. It reads from the
// tracker's single ready channel until the context is canceled or all
// actions are done.
func (wp *WorkerPool) worker(ctx context.Context) {
	defer wp.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wp.tracker.Done():
			return
		case ta := <-wp.tracker.Ready():
			if ta == nil {
				continue
			}

			wp.safeExecuteAction(ctx, ta)
		}
	}
}

// safeExecuteAction wraps executeAction with panic recovery so a single
// action panic doesn't crash the entire program.
func (wp *WorkerPool) safeExecuteAction(ctx context.Context, ta *TrackedAction) {
	defer func() {
		if r := recover(); r != nil {
			wp.logger.Error("worker: panic in action execution",
				slog.Int64("id", ta.ID),
				slog.String("path", ta.Action.Path),
				slog.Any("panic", r),
			)
			wp.recordFailure(fmt.Errorf("panic: %v", r))
			wp.sendResult(ctx, ta, false, fmt.Sprintf("panic: %v", r))
			wp.tracker.Complete(ta.ID)
		}
	}()

	wp.executeAction(ctx, ta)
}

// executeAction runs a single tracked action: execute, commit, complete.
func (wp *WorkerPool) executeAction(ctx context.Context, ta *TrackedAction) {
	// Per-action cancellable context.
	actionCtx, cancel := context.WithCancel(ctx)
	ta.Cancel = cancel

	defer cancel()

	// Load baseline (cached after first call).
	bl, loadErr := wp.baseline.Load(actionCtx)
	if loadErr != nil {
		wp.logger.Error("worker: baseline load failed",
			slog.String("error", loadErr.Error()),
		)
		wp.recordFailure(loadErr)
		wp.sendResult(ctx, ta, false, loadErr.Error())
		wp.tracker.Complete(ta.ID)

		return
	}

	// Execute the action.
	exec := NewExecution(wp.cfg, bl)
	outcome := wp.dispatchAction(actionCtx, exec, ta)

	// Commit outcome to baseline. Uses pool-level ctx because the action
	// already completed — its outcome should be persisted even if
	// CancelByPath canceled actionCtx after dispatch returned.
	if commitErr := wp.baseline.CommitOutcome(ctx, &outcome); commitErr != nil {
		wp.logger.Error("worker: commit outcome failed",
			slog.Int64("id", ta.ID),
			slog.String("error", commitErr.Error()),
		)
		wp.recordFailure(commitErr)
		wp.sendResult(ctx, ta, false, commitErr.Error())
		wp.tracker.Complete(ta.ID)

		return
	}

	if outcome.Success {
		wp.succeeded.Add(1)
		wp.sendResult(ctx, ta, true, "")
	} else {
		wp.recordFailure(outcome.Error)
		wp.sendResult(ctx, ta, false, outcome.Error.Error())
	}

	// Signal completion to dispatch dependents.
	wp.tracker.Complete(ta.ID)
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

// Results returns a read-only channel of per-action results. The engine
// reads from this channel for in-memory cycle result tracking (failure
// suppression, delta token commit decisions).
func (wp *WorkerPool) Results() <-chan WorkerResult {
	return wp.results
}

// recordFailure atomically increments the failed counter and appends an error
// to the diagnostic error list. The list is capped at maxRecordedErrors to
// bound memory in long-running watch mode (B-205). Overflow errors are counted
// via droppedErrors; the failed counter remains accurate regardless.
func (wp *WorkerPool) recordFailure(err error) {
	if err == nil {
		return
	}

	wp.failed.Add(1)
	wp.errorsMu.Lock()

	if len(wp.errors) >= maxRecordedErrors {
		wp.droppedErrors.Add(1)
	} else {
		wp.errors = append(wp.errors, err)
	}

	wp.errorsMu.Unlock()
}

// DroppedErrors returns the number of errors that were not recorded because
// the diagnostic error slice was full (B-205).
func (wp *WorkerPool) DroppedErrors() int64 {
	return wp.droppedErrors.Load()
}

// sendResult reports a per-action outcome to the results channel. Blocks until
// the result is sent or the context is canceled. In one-shot mode the channel
// is sized to planSize so this never blocks. In watch mode the channel is 4096
// deep and a drain goroutine reads concurrently (see Engine.drainWorkerResults).
//
// If the context is canceled before the result is sent (e.g., during engine
// shutdown), the WorkerResult is silently dropped. This is benign: callers
// always call recordFailure() before sendResult, so the failed counter and
// diagnostic error list remain accurate regardless (B-206).
func (wp *WorkerPool) sendResult(ctx context.Context, ta *TrackedAction, success bool, errMsg string) {
	r := WorkerResult{
		ID:      ta.ID,
		CycleID: ta.CycleID,
		Path:    ta.Action.Path,
		Success: success,
		ErrMsg:  errMsg,
	}

	select {
	case wp.results <- r:
	case <-ctx.Done():
	}
}
