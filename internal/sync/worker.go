package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	stdsync "sync" // used by WaitGroup

	"github.com/tonimelisma/onedrive-go/internal/perf"
)

var (
	errUnknownActionType         = errors.New("sync: unknown action type in worker dispatch")
	errPublicationOnlyActionType = errors.New("sync: publication-only action routed to worker")
)

// minWorkers is the floor for total worker count.
const minWorkers = 4

// workerPool spawns goroutines that pull TrackedActions from the dispatch
// channel, execute them, persist success outcomes, and send completions back
// to the engine.
// Workers are pure executors — they NEVER call depGraph.Complete(). The engine
// owns all completion decisions (R-6.8.9).
type workerPool struct {
	cfg           *executorConfig
	dispatchCh    <-chan *trackedAction
	baseline      *SyncStore
	logger        *slog.Logger
	perfCollector *perf.Collector

	// completions reports per-action outcomes back to the engine. The engine
	// reads from this channel, classifies completions, and calls depGraph.Complete.
	// Failed items are persisted through the engine-owned retry/block-scope
	// authorities; observation-owned issues are written by observation passes.
	completions chan actionCompletion

	cancel    context.CancelFunc
	wg        stdsync.WaitGroup
	closeOnce stdsync.Once
}

// newWorkerPool creates a pool without starting any workers. planSize
// determines the result channel buffer (use the number of actions in the
// plan for one-shot mode, or a generous buffer for watch mode).
//
// dispatchCh provides actions ready for execution. Workers exit only when the
// owning context is canceled or the dispatch channel is closed.
func newWorkerPool(
	cfg *executorConfig,
	dispatchCh <-chan *trackedAction,
	baseline *SyncStore,
	logger *slog.Logger,
	planSize int,
) *workerPool {
	if planSize < 1 {
		planSize = 1
	}

	return &workerPool{
		cfg:           cfg,
		dispatchCh:    dispatchCh,
		baseline:      baseline,
		logger:        logger,
		perfCollector: nil,
		// Buffer sizing contract: one-shot mode uses planSize (equal to
		// the number of actions, so workers never block). Watch mode passes
		// watchCompletionBuf with a drain goroutine reading completions
		// concurrently, so blocking is unlikely under normal load.
		completions: make(chan actionCompletion, planSize),
	}
}

// Start spawns a flat pool of goroutines, all reading from the single dispatch
// channel. total is the desired concurrency (typically cfg.TransferWorkers).
// Minimum 4 workers.
func (wp *workerPool) Start(ctx context.Context, total int) {
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
		wp.closeCompletions()
	}()

	wp.logger.Info("worker pool started",
		slog.Int("workers", total),
	)
}

// Stop cancels all in-flight work, waits for goroutines to exit, and closes
// the completions channel so the engine-owned completion loop can terminate.
func (wp *workerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel()
	}

	wp.wg.Wait()
	wp.closeCompletions()
}

// worker is the main loop for a single goroutine. It reads from dispatchCh
// until the context is canceled or all actions are complete.
func (wp *workerPool) worker(ctx context.Context) {
	defer wp.wg.Done()

	for {
		select {
		case <-ctx.Done():
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
// panic as a failed ActionCompletion and decides how to handle it.
func (wp *workerPool) safeExecuteAction(ctx context.Context, ta *trackedAction) {
	defer func() {
		if r := recover(); r != nil {
			// The stack is captured here because the goroutine that could
			// answer "where" is unwound by the time anyone reads the failed
			// completion, and the recovered value names only "what".
			wp.logger.Error("worker: panic in action execution",
				slog.Int64("id", ta.ID),
				slog.String("path", ta.Action.Path),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
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
func (wp *workerPool) executeAction(ctx context.Context, ta *trackedAction) {
	// Per-action cancellable context.
	actionCtx, cancel := context.WithCancel(ctx)
	ta.Cancel = cancel

	defer cancel()

	if freshnessErr := wp.validateActionFreshness(actionCtx, ta); freshnessErr != nil {
		wp.sendResult(ctx, ta, nil, freshnessErr)
		return
	}

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
	exec := newExecution(wp.cfg, bl)
	outcome := wp.dispatchAction(actionCtx, exec, ta)

	// Persist success outcomes immediately via the store-owned mutation input.
	// Uses pool-level ctx because the action already completed — its outcome
	// should be persisted even if the owning runtime canceled actionCtx.
	if outcome.Success {
		mutation := mutationFromActionOutcome(&outcome)
		if mutation != nil {
			if commitErr := wp.baseline.CommitMutation(ctx, mutation); commitErr != nil {
				wp.logger.Error("worker: commit outcome failed",
					slog.Int64("id", ta.ID),
					slog.String("error", commitErr.Error()),
				)
				wp.sendResult(ctx, ta, nil, commitErr)
				return
			}
		}
	}

	wp.sendResult(ctx, ta, &outcome, outcome.Error)
	// NO depGraph.Complete() — engine owns completion decisions.
}

func (wp *workerPool) validateActionFreshness(ctx context.Context, ta *trackedAction) error {
	if ta == nil {
		return nil
	}

	decision, err := evaluateActionFreshnessFromStore(ctx, wp.baseline, &ta.Action)
	if err != nil {
		return err
	}
	if decision.Fresh {
		return nil
	}

	wp.recordWorkerStartSuperseded(decision)
	return fmt.Errorf("%w: %s", errActionPreconditionChanged, decision.Reason)
}

func (wp *workerPool) recordWorkerStartSuperseded(decision actionFreshnessDecision) {
	switch decision.Source {
	case actionFreshnessSourceLocalTruth:
		wp.collector().RecordSuperseded(perf.SupersededSourceWorkerStartLocalTruth, 1)
	case actionFreshnessSourceRemoteTruth:
		wp.collector().RecordSuperseded(perf.SupersededSourceWorkerStartRemoteTruth, 1)
	case actionFreshnessSourceUnknown:
		return
	}
}

// dispatchAction routes a tracked action to the appropriate executor method.
func (wp *workerPool) dispatchAction(
	ctx context.Context, exec *executor, ta *trackedAction,
) actionOutcome {
	action := &ta.Action

	switch action.Type {
	case ActionFolderCreate:
		return exec.ExecuteFolderCreate(ctx, action)
	case ActionConflictCopy:
		return exec.ExecuteConflictCopy(ctx, action)
	case ActionLocalMove, ActionRemoteMove:
		return exec.ExecuteMove(ctx, action)
	case ActionDownload:
		return exec.ExecuteDownload(ctx, action)
	case ActionUpload:
		return exec.ExecuteUpload(ctx, action)
	case ActionLocalDelete:
		return exec.ExecuteLocalDelete(ctx, action)
	case ActionRemoteDelete:
		return exec.ExecuteRemoteDelete(ctx, action)
	case ActionBaselineUpdate, ActionCleanup:
		return actionOutcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   errPublicationOnlyActionType,
		}
	default:
		return actionOutcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   errUnknownActionType,
		}
	}
}

// Completions returns a read-only channel of per-action completions. The
// engine reads from this channel, classifies each completion, and calls
// depGraph.Complete. Failed items become retry_work, block scopes, or other
// engine-owned durable control state for held release and scope lifecycle.
func (wp *workerPool) Completions() <-chan actionCompletion {
	return wp.completions
}

// sendResult reports a per-action outcome to the completions channel. Populates
// the ActionCompletion from the TrackedAction and any error. When outcome is
// non-nil, uses its Success/Error fields; otherwise treats as failure with
// the provided error.
//
// If the context is canceled before the completion is sent (engine shutdown),
// the ActionCompletion is silently dropped. The engine handles shutdown via
// context cancellation on the result-processing loop (resultShutdown classification).
func (wp *workerPool) sendResult(ctx context.Context, ta *trackedAction, outcome *actionOutcome, actionErr error) {
	r := actionCompletionFromTrackedAction(ta, outcome, actionErr)
	wp.recordLivePreconditionSuperseded(&r, outcome)

	select {
	case wp.completions <- r:
	case <-ctx.Done():
	}
}

func (wp *workerPool) recordLivePreconditionSuperseded(r *actionCompletion, outcome *actionOutcome) {
	if outcome == nil || r == nil || !errors.Is(r.Err, errActionPreconditionChanged) {
		return
	}

	switch r.FailureCapability {
	case permissionCapabilityLocalRead, permissionCapabilityLocalWrite:
		wp.collector().RecordSuperseded(perf.SupersededSourceLiveLocalPrecondition, 1)
	case permissionCapabilityRemoteRead, permissionCapabilityRemoteWrite:
		wp.collector().RecordSuperseded(perf.SupersededSourceLiveRemotePrecondition, 1)
	case permissionCapabilityUnknown:
	}
}

func (wp *workerPool) collector() *perf.Collector {
	if wp == nil {
		return nil
	}

	return wp.perfCollector
}

func (wp *workerPool) closeCompletions() {
	wp.closeOnce.Do(func() {
		close(wp.completions)
	})
}
