package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync" // used by WaitGroup
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

var errUnknownActionType = errors.New("sync: unknown action type in worker dispatch")

// minWorkers is the floor for total worker count.
const minWorkers = 4

// WorkerPool spawns goroutines that pull TrackedActions from the DepTracker's
// ready channel, execute them, persist success outcomes, and send results
// back to the engine. Workers are pure executors — they NEVER call
// tracker.Complete(). The engine owns all completion decisions (R-6.8.9).
type WorkerPool struct {
	cfg      *ExecutorConfig
	tracker  *DepTracker
	baseline *SyncStore
	logger   *slog.Logger

	// results reports per-action outcomes back to the engine. The engine
	// reads from this channel, classifies results, and calls Complete on
	// the tracker. Failed items are recorded in sync_failures for retry.
	results chan WorkerResult

	cancel context.CancelFunc
	wg     stdsync.WaitGroup
}

// WorkerResult reports the outcome of a single action execution. The engine
// reads these from the Results channel, classifies them, and calls
// tracker.Complete. Failed items are recorded in sync_failures for retry
// by the FailureRetrier.
type WorkerResult struct {
	Path       string
	DriveID    driveid.ID
	ActionType ActionType
	Success    bool
	ErrMsg     string
	HTTPStatus int // from graph.GraphError, 0 if not a Graph API error

	// Err is the full error for classification (context.Canceled, os.ErrPermission, etc.).
	// The engine uses errors.Is to distinguish shutdown from genuine failures.
	Err error

	// RetryAfter is the server-mandated wait duration from the Retry-After
	// header on 429/503 responses. Zero when absent. Used by scope blocks
	// for initial trial timing (R-2.10.7, R-2.10.8).
	RetryAfter time.Duration

	// TargetDriveID is the actual drive ID targeted by this action. For
	// own-drive actions, equals DriveID. For shortcut actions, equals the
	// sharer's drive. Flows through the pipeline without lookup (R-6.8.12).
	TargetDriveID driveid.ID

	// ShortcutKey identifies the shortcut scope. Format: "remoteDrive:remoteItem".
	// Empty for own-drive actions. Used by updateScope for 507 scope keys (R-2.10.16).
	ShortcutKey string

	// IsTrial is true if this was a scope trial action (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies the scope being tested by this trial.
	TrialScopeKey ScopeKey

	// ActionID is the TrackedAction.ID for the engine to call Complete on
	// the tracker.
	ActionID int64
}

// NewWorkerPool creates a pool without starting any workers. planSize
// determines the result channel buffer (use the number of actions in the
// plan for one-shot mode, or a generous buffer for watch mode).
func NewWorkerPool(
	cfg *ExecutorConfig,
	tracker *DepTracker,
	baseline *SyncStore,
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
// action panic doesn't crash the entire program. The engine receives the
// panic as a failed WorkerResult and decides how to handle it.
func (wp *WorkerPool) safeExecuteAction(ctx context.Context, ta *TrackedAction) {
	defer func() {
		if r := recover(); r != nil {
			wp.logger.Error("worker: panic in action execution",
				slog.Int64("id", ta.ID),
				slog.String("path", ta.Action.Path),
				slog.Any("panic", r),
			)
			panicErr := fmt.Errorf("panic: %v", r)
			wp.sendResult(ctx, ta, nil, panicErr)
			// NO tracker.Complete() — engine owns completion decisions.
		}
	}()

	wp.executeAction(ctx, ta)
}

// executeAction runs a single tracked action: execute, persist success
// outcomes, and send the result to the engine. Workers are pure executors —
// they NEVER call tracker.Complete().
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
	// NO tracker.Complete() — engine owns completion decisions.
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
// reads from this channel, classifies each result, and calls
// tracker.Complete. Failed items go to sync_failures for reconciler retry.
func (wp *WorkerPool) Results() <-chan WorkerResult {
	return wp.results
}

// sendResult reports a per-action outcome to the results channel. Populates
// the WorkerResult from the TrackedAction and any error. When outcome is
// non-nil, uses its Success/Error fields; otherwise treats as failure with
// the provided error.
//
// If the context is canceled before the result is sent (engine shutdown),
// the WorkerResult is silently dropped. The engine handles shutdown via
// context cancellation on the drain goroutine (resultShutdown classification).
func (wp *WorkerPool) sendResult(ctx context.Context, ta *TrackedAction, outcome *Outcome, actionErr error) {
	r := WorkerResult{
		Path:          ta.Action.Path,
		DriveID:       ta.Action.DriveID,
		ActionType:    ta.Action.Type,
		Err:           actionErr,
		HTTPStatus:    extractHTTPStatus(actionErr),
		RetryAfter:    extractRetryAfter(actionErr),
		TargetDriveID: ta.Action.TargetDriveID(),
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
			r.HTTPStatus = extractHTTPStatus(outcome.Error)
			r.RetryAfter = extractRetryAfter(outcome.Error)
		}
	} else if actionErr != nil {
		r.ErrMsg = actionErr.Error()
	}

	select {
	case wp.results <- r:
	case <-ctx.Done():
	}
}

// extractHTTPStatus unwraps a graph.GraphError from err and returns its
// StatusCode. Returns 0 if err is nil or not a GraphError.
func extractHTTPStatus(err error) int {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.StatusCode
	}

	return 0
}

// extractRetryAfter unwraps a graph.GraphError from err and returns its
// RetryAfter duration. Returns 0 if err is nil or not a GraphError.
func extractRetryAfter(err error) time.Duration {
	if err == nil {
		return 0
	}

	var ge *graph.GraphError
	if errors.As(err, &ge) {
		return ge.RetryAfter
	}

	return 0
}
