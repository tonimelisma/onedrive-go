package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync" // used by WaitGroup
	"time"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

var errUnknownActionType = errors.New("sync: unknown action type in worker dispatch")

// minWorkers is the floor for total worker count.
const minWorkers = 4

// WorkerPool spawns goroutines that pull TrackedActions from the dispatch
// channel, execute them, persist success outcomes, and send results back to
// the engine.
// Workers are pure executors — they NEVER call depGraph.Complete(). The engine
// owns all completion decisions (R-6.8.9).
//
// Workers read from dispatchCh and wait on completeCh, which may be backed by
// DepGraph or any other dispatch source.
type WorkerPool struct {
	cfg        *ExecutorConfig
	dispatchCh <-chan *synctypes.TrackedAction
	completeCh <-chan struct{}
	baseline   synctypes.OutcomeWriter
	logger     *slog.Logger

	// results reports per-action outcomes back to the engine. The engine
	// reads from this channel, classifies results, and calls depGraph.Complete.
	// Failed items are recorded in sync_failures for retry.
	results chan synctypes.WorkerResult

	cancel    context.CancelFunc
	wg        stdsync.WaitGroup
	closeOnce stdsync.Once
}

// NewWorkerPool creates a pool without starting any workers. planSize
// determines the result channel buffer (use the number of actions in the
// plan for one-shot mode, or a generous buffer for watch mode).
//
// dispatchCh provides actions ready for execution. completeCh signals when all
// work is complete (workers exit when completeCh closes or ctx is canceled).
func NewWorkerPool(
	cfg *ExecutorConfig,
	dispatchCh <-chan *synctypes.TrackedAction,
	completeCh <-chan struct{},
	baseline synctypes.OutcomeWriter,
	logger *slog.Logger,
	planSize int,
) *WorkerPool {
	if planSize < 1 {
		planSize = 1
	}

	return &WorkerPool{
		cfg:        cfg,
		dispatchCh: dispatchCh,
		completeCh: completeCh,
		baseline:   baseline,
		logger:     logger,
		// Buffer sizing contract: one-shot mode uses planSize (equal to
		// the number of actions, so workers never block). Watch mode uses
		// watchResultBuf (4096) with a drain goroutine reading results
		// concurrently, so blocking is unlikely under normal load.
		results: make(chan synctypes.WorkerResult, planSize),
	}
}

// Start spawns a flat pool of goroutines, all reading from the single dispatch
// channel. total is the desired concurrency (typically cfg.TransferWorkers).
// Minimum 4 workers.
func (wp *WorkerPool) Start(ctx context.Context, total int) {
	if total < minWorkers {
		total = minWorkers
	}

	ctx, wp.cancel = context.WithCancel(ctx)

	for range total {
		wp.wg.Add(1)

		go wp.worker(ctx)
	}

	go func() {
		wp.wg.Wait()
		wp.closeResults()
	}()

	wp.logger.Info("worker pool started",
		slog.Int("workers", total),
	)
}

// Wait blocks until the completion signal fires (all actions complete).
func (wp *WorkerPool) Wait() {
	<-wp.completeCh
}

// Stop cancels all in-flight work, waits for goroutines to exit, and closes
// the results channel so the engine-owned result loop can terminate.
func (wp *WorkerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel()
	}

	wp.wg.Wait()
	wp.closeResults()
}

// worker is the main loop for a single goroutine. It reads from dispatchCh
// until the context is canceled or all actions are complete.
func (wp *WorkerPool) worker(ctx context.Context) {
	defer wp.wg.Done()

	for {
		select {
		case <-ctx.Done():
			return
		case <-wp.completeCh:
			return
		case ta, ok := <-wp.dispatchCh:
			if !ok {
				return
			}
			if ta == nil {
				continue
			}

			wp.safeExecuteAction(ctx, ta)
		}
	}
}

// safeExecuteAction wraps executeAction with panic recovery so a single
// action panic doesn't crash the entire program. The engine receives the
// panic as a failed WorkerResult and decides how to handle it.
func (wp *WorkerPool) safeExecuteAction(ctx context.Context, ta *synctypes.TrackedAction) {
	defer func() {
		if r := recover(); r != nil {
			wp.logger.Error("worker: panic in action execution",
				slog.Int64("id", ta.ID),
				slog.String("path", ta.Action.Path),
				slog.Any("panic", r),
			)
			panicErr := fmt.Errorf("panic: %v", r)
			wp.sendResult(ctx, ta, nil, panicErr)
			// NO depGraph.Complete() — engine owns completion decisions.
		}
	}()

	wp.executeAction(ctx, ta)
}

// executeAction runs a single tracked action: execute, persist success
// outcomes, and send the result to the engine. Workers are pure executors —
// they NEVER call depGraph.Complete().
func (wp *WorkerPool) executeAction(ctx context.Context, ta *synctypes.TrackedAction) {
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
		wp.sendResult(ctx, ta, nil, loadErr)
		return
	}

	// Execute the action.
	exec := NewExecution(wp.cfg, bl)
	outcome := wp.dispatchAction(actionCtx, exec, ta)

	// Persist success outcomes immediately. CommitOutcome is a no-op for
	// failures (store_baseline.go:186), so we only call it on success.
	// Uses pool-level ctx because the action already completed — its outcome
	// should be persisted even if CancelByPath canceled actionCtx.
	if outcome.Success {
		if commitErr := wp.baseline.CommitOutcome(ctx, &outcome); commitErr != nil {
			wp.logger.Error("worker: commit outcome failed",
				slog.Int64("id", ta.ID),
				slog.String("error", commitErr.Error()),
			)
			wp.sendResult(ctx, ta, nil, commitErr)
			return
		}
	}

	wp.sendResult(ctx, ta, &outcome, outcome.Error)
	// NO depGraph.Complete() — engine owns completion decisions.
}

// dispatchAction routes a tracked action to the appropriate executor method.
func (wp *WorkerPool) dispatchAction(
	ctx context.Context, exec *Executor, ta *synctypes.TrackedAction,
) synctypes.Outcome {
	action := &ta.Action

	switch action.Type {
	case synctypes.ActionFolderCreate:
		return exec.ExecuteFolderCreate(ctx, action)
	case synctypes.ActionLocalMove, synctypes.ActionRemoteMove:
		return exec.ExecuteMove(ctx, action)
	case synctypes.ActionDownload:
		return exec.ExecuteDownload(ctx, action)
	case synctypes.ActionUpload:
		return exec.ExecuteUpload(ctx, action)
	case synctypes.ActionLocalDelete:
		return exec.ExecuteLocalDelete(ctx, action)
	case synctypes.ActionRemoteDelete:
		return exec.ExecuteRemoteDelete(ctx, action)
	case synctypes.ActionConflict:
		return exec.ExecuteConflict(ctx, action)
	case synctypes.ActionUpdateSynced:
		return exec.ExecuteSyncedUpdate(action)
	case synctypes.ActionCleanup:
		return exec.ExecuteCleanup(action)
	default:
		return synctypes.Outcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   errUnknownActionType,
		}
	}
}

// Results returns a read-only channel of per-action results. The engine
// reads from this channel, classifies each result, and calls
// depGraph.Complete. Failed items go to sync_failures for the engine retry sweep.
func (wp *WorkerPool) Results() <-chan synctypes.WorkerResult {
	return wp.results
}

// sendResult reports a per-action outcome to the results channel. Populates
// the WorkerResult from the TrackedAction and any error. When outcome is
// non-nil, uses its Success/Error fields; otherwise treats as failure with
// the provided error.
//
// If the context is canceled before the result is sent (engine shutdown),
// the WorkerResult is silently dropped. The engine handles shutdown via
// context cancellation on the result-processing loop (resultShutdown classification).
func (wp *WorkerPool) sendResult(ctx context.Context, ta *synctypes.TrackedAction, outcome *synctypes.Outcome, actionErr error) {
	driveID := ta.Action.DriveID
	if outcome != nil && !outcome.DriveID.IsZero() {
		driveID = outcome.DriveID
	} else if driveID.IsZero() && !ta.Action.TargetDriveID.IsZero() {
		// Shortcut-targeted actions may defer drive resolution to execution time.
		// When no concrete action drive was planned, retain the intended target
		// drive so failure persistence and success cleanup address the same row.
		driveID = ta.Action.TargetDriveID
	}

	r := synctypes.WorkerResult{
		Path:          ta.Action.Path,
		ItemID:        ta.Action.ItemID,
		DriveID:       driveID,
		ActionType:    ta.Action.Type,
		Err:           actionErr,
		HTTPStatus:    ExtractHTTPStatus(actionErr),
		RetryAfter:    ExtractRetryAfter(actionErr),
		TargetDriveID: ta.Action.TargetDriveID,
		ShortcutKey:   ta.Action.ShortcutKey(),
		IsTrial:       ta.IsTrial,
		TrialScopeKey: ta.TrialScopeKey,
		ActionID:      ta.ID,
	}

	if outcome != nil {
		r.Success = outcome.Success
		if outcome.Error != nil {
			r.ErrMsg = outcome.Error.Error()
			r.Err = outcome.Error
			r.HTTPStatus = ExtractHTTPStatus(outcome.Error)
			r.RetryAfter = ExtractRetryAfter(outcome.Error)
		}
	} else if actionErr != nil {
		r.ErrMsg = actionErr.Error()
	}

	select {
	case wp.results <- r:
	case <-ctx.Done():
	}
}

func (wp *WorkerPool) closeResults() {
	wp.closeOnce.Do(func() {
		close(wp.results)
	})
}

// ExtractHTTPStatus unwraps a graph.GraphError from err and returns its
// StatusCode. Returns 0 if err is nil or not a GraphError.
func ExtractHTTPStatus(err error) int {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.StatusCode
	}

	return 0
}

// ExtractRetryAfter unwraps a graph.GraphError from err and returns its
// RetryAfter duration. Returns 0 if err is nil or not a GraphError.
func ExtractRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.RetryAfter
	}

	return 0
}
