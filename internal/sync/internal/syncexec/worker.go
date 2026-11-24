package syncexec

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	stdsync "sync" // used by WaitGroup
	"time"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

var (
	ErrUnknownActionType         = errors.New("syncexec: unknown action type in worker dispatch")
	errPublicationOnlyActionType = errors.New("syncexec: publication-only action routed to worker")
)

// minWorkers is the floor for total worker count.
const minWorkers = 4

// PoolWorkerCount is how many workers a pool runs for a requested count: the
// floor applies unless the pool is serialized to one worker (D-090). The
// engine's slow start doubles toward this same number (R-6.8.25).
func PoolWorkerCount(requested int, serialize bool) int {
	if serialize {
		return 1
	}

	return max(requested, minWorkers)
}

// WorkerPool spawns goroutines that pull trackedActions from the dispatch
// channel, execute them, persist success outcomes, and send completions back
// to the engine.
// Workers never call DepGraph.Complete(); the engine owns all completion
// decisions (R-6.8.9).
type WorkerPool struct {
	cfg           *ExecutorConfig
	dispatchCh    <-chan *syncdomain.TrackedAction
	baseline      ExecutorStore
	logger        *slog.Logger
	perfCollector *perf.Collector

	// completions reports per-action outcomes back to the engine. The engine
	// reads from this channel, classifies completions, and calls DepGraph.Complete.
	// Failed items are persisted through the engine-owned retry/block-scope
	// authorities; observation-owned issues are written by observation passes.
	completions chan syncdomain.ActionCompletion

	// serialize runs exactly one worker, below the minWorkers floor, so a
	// test's dispatch order is also the execution order (D-090).
	serialize bool
	// clock starts the pool's goroutines.
	clock syncdomain.SyncClock

	cancel    context.CancelCauseFunc
	wg        stdsync.WaitGroup
	closeOnce stdsync.Once
}

// WorkerPoolInputs is everything NewWorkerPool builds a pool from.
type WorkerPoolInputs struct {
	// Config is the executor configuration every worker's executor shares.
	Config *ExecutorConfig
	// Dispatch provides actions ready for execution. Workers exit only when
	// the owning context is canceled or the channel is closed.
	Dispatch <-chan *syncdomain.TrackedAction
	Store    ExecutorStore
	Logger   *slog.Logger
	// CompletionBuffer sizes the completions channel: the number of actions
	// in the plan for one-shot mode, so workers never block, or a generous
	// buffer for watch mode, whose loop reads completions as they arrive.
	// Below one is one.
	CompletionBuffer int
	// Perf is the collector the workers report to; nil reports nowhere.
	Perf *perf.Collector
	// Serialize runs exactly one worker, below the minWorkers floor, so a
	// test's dispatch order is also the execution order (D-090). The engine
	// passes its test seam through; production's is false.
	Serialize bool
	// Clock starts the workers' goroutines: the engine's clock, so a test
	// that owns it owns when they run. Nil is the real clock.
	Clock syncdomain.SyncClock
}

// NewWorkerPool creates a pool without starting any workers.
func NewWorkerPool(in WorkerPoolInputs) *WorkerPool {
	clock := in.Clock
	if clock == nil {
		clock = syncdomain.RealClock{}
	}

	return &WorkerPool{
		clock:         clock,
		cfg:           in.Config,
		dispatchCh:    in.Dispatch,
		baseline:      in.Store,
		logger:        in.Logger,
		perfCollector: in.Perf,
		serialize:     in.Serialize,
		completions:   make(chan syncdomain.ActionCompletion, max(in.CompletionBuffer, 1)),
	}
}

// Start spawns a flat pool of goroutines, all reading from the single dispatch
// channel. total is the desired concurrency (typically cfg.TransferWorkers).
// Minimum 4 workers.
func (wp *WorkerPool) Start(ctx context.Context, total int) {
	total = PoolWorkerCount(total, wp.serialize)

	ctx, wp.cancel = context.WithCancelCause(ctx)

	for range total {
		wp.wg.Add(1)

		wp.clock.Go(func() { wp.worker(ctx) })
	}

	wp.clock.Go(wp.closeCompletionsWhenWorkersExit)

	wp.logger.Info("worker pool started",
		slog.Int("workers", total),
	)
}

// closeCompletionsWhenWorkersExit closes the completions channel once every
// worker has returned, so the engine-owned completion loop ends. Waiting and
// a close through sync.Once cannot panic, so it needs no boundary.
func (wp *WorkerPool) closeCompletionsWhenWorkersExit() {
	wp.wg.Wait()
	wp.closeCompletions()
}

// Stop cancels all in-flight work, waits for goroutines to exit, and closes
// the completions channel so the engine-owned completion loop can terminate.
func (wp *WorkerPool) Stop() {
	if wp.cancel != nil {
		wp.cancel(ErrWorkerPoolStopped)
	}

	wp.wg.Wait()
	wp.closeCompletions()
}

// worker is the main loop for a single goroutine. It reads from dispatchCh
// until the context is canceled or all actions are complete. Its panic
// boundary is safeExecuteAction, around each action; the loop itself only
// receives.
func (wp *WorkerPool) worker(ctx context.Context) {
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
func (wp *WorkerPool) safeExecuteAction(ctx context.Context, ta *syncdomain.TrackedAction) {
	defer func() {
		if r := recover(); r != nil {
			// Logged with the stack here because the goroutine that could
			// answer "where" is unwound by the time anyone reads the failed
			// completion, and the recovered value names only "what".
			panicErr := panicguard.Recovered(wp.logger, "while syncing "+ta.Action.Path, r,
				slog.Int64("action_id", ta.ID))
			wp.sendResult(ctx, ta, nil, panicErr)
			// NO DepGraph.Complete() — engine owns completion decisions.
		}
	}()

	wp.executeAction(ctx, ta)
}

// executeAction runs a single tracked action: execute, persist success
// outcomes, and send the result to the engine. Workers never call
// DepGraph.Complete(); the engine owns all completion decisions.
func (wp *WorkerPool) executeAction(ctx context.Context, ta *syncdomain.TrackedAction) {
	// actionCtx is canceled when executeAction returns, which stops what the
	// action started under it.
	actionCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(ErrActionReturned)

	if freshnessErr := wp.validateActionFreshness(actionCtx, ta); freshnessErr != nil {
		wp.sendResult(ctx, ta, nil, freshnessErr)
		return
	}

	// Load baseline (cached after first call).
	bl, loadErr := wp.baseline.Load(actionCtx)
	if loadErr != nil {
		// Debug: the error is returned as this action's failure and
		// reported once by the engine and console.
		wp.logger.Debug("worker: baseline load failed",
			slog.Any("error", loadErr),
		)
		wp.sendResult(ctx, ta, nil, loadErr)
		return
	}

	// Execute the action.
	exec := NewExecution(wp.cfg, bl)
	outcome := wp.dispatchAction(actionCtx, exec, ta)

	// Persist success outcomes immediately via the store-owned mutation input.
	// The transfer already happened, so the commit is detached from both the
	// action and the pool context: a shutdown (Ctrl-C) landing between
	// transfer completion and commit must not drop the record of work that
	// is already done. OutcomeCommitTimeout bounds the detached commit so a
	// wedged store cannot hold shutdown hostage.
	if outcome.Success {
		mutation := syncdomain.MutationFromActionOutcome(&outcome)
		if mutation != nil {
			if commitErr := wp.commitOutcome(ctx, mutation); commitErr != nil {
				// Debug: returned as the action's failure (and a shutdown
				// cancellation is not a failure the user needs to see).
				wp.logger.Debug("worker: commit outcome failed",
					slog.Int64("id", ta.ID),
					slog.Any("error", commitErr),
				)
				wp.sendResult(ctx, ta, nil, commitErr)
				return
			}
		}
	}

	wp.sendResult(ctx, ta, &outcome, outcome.Error)
	// NO DepGraph.Complete() — engine owns completion decisions.
}

// commitOutcome persists a completed action's baseline mutation detached
// from the caller's cancellation, bounded by the pool's outcomeCommitBound.
func (wp *WorkerPool) commitOutcome(ctx context.Context, mutation *syncdomain.BaselineMutation) error {
	return RunDetachedStoreAccess(ctx, wp.outcomeCommitBound(), func(commitCtx context.Context) error {
		return wp.baseline.CommitMutation(commitCtx, mutation)
	})
}

// RunDetachedStoreAccess runs access on a context that keeps ctx's values but
// not its cancellation, bounded by bound: a shutdown cannot drop the record of
// work that already happened, and a store that stopped answering cannot hold
// the shutdown. A failure the bound ended carries ErrStoreAccessTimedOut beside
// the store's own error, because database/sql returns the context's error and
// not its cause. The sentinel is joined last: the sync console prints the
// innermost error of the last error a chain joins, so a user reads that the
// sync database did not answer, not the deadline the store gave up at.
//
// Once the run ctx belongs to has ended (WithRunStop), the access ends at the
// stop's one deadline when that comes first, so a worker's commit or an
// executor write that starts after a cancel shares the stop's bound instead
// of taking a whole bound of its own after it.
func RunDetachedStoreAccess(ctx context.Context, bound time.Duration, access func(context.Context) error) error {
	deadline := syncdomain.WallNow().Add(bound)
	if stop, ended := runStopDeadline(ctx); ended && stop.Before(deadline) {
		deadline = stop
	}

	return RunDetachedStoreAccessUntil(ctx, deadline, access)
}

// runStopKey marks a context that belongs to a run whose stop has one
// deadline (WithRunStop).
type runStopKey struct{}

// runStop is what WithRunStop hands down about the run's own context, whose
// end is the stop: whether it ended, a callback on its end, and the stop's one
// deadline, read once it ended. It holds functions over that context rather
// than the context itself, so a context derived from a detached one cannot
// take the run's cancellation back by mistake.
type runStop struct {
	ended    func() bool
	onEnd    func(func()) func() bool
	deadline func() time.Time
	logger   *slog.Logger
}

// WithRunStop returns ctx carrying the stop of the run ctx is: once ctx has
// ended, every detached store access under it, its own or a context derived
// from it (the workers', a completion's), ends at deadline() when that comes
// before its own bound. deadline is called from any goroutine once ctx ended,
// so it reads only what is safe to read there. A panic in a callback the
// run's end starts is logged through logger and ends that callback only.
func WithRunStop(ctx context.Context, deadline func() time.Time, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, runStopKey{}, runStop{
		ended: func() bool { return ctx.Err() != nil },
		onEnd: func(f func()) func() bool {
			return context.AfterFunc(ctx, func() {
				defer recoverRunStopCallback(logger)
				f()
			})
		},
		deadline: deadline,
		logger:   logger,
	})
}

// recoverRunStopCallback is the panic boundary of a goroutine the run's end
// starts: the panic is a bug, logged with its stack, and the callback ends.
func recoverRunStopCallback(logger *slog.Logger) {
	if r := recover(); r != nil {
		//nolint:errcheck // Recovered logs the panic at Error; the callback has no caller to return it to
		_ = panicguard.Recovered(logger, "in the run stop's deadline watch", r)
	}
}

// RunHasEnded reports that ctx belongs to a run (WithRunStop) whose context
// has ended, even when ctx itself is detached from that end.
func RunHasEnded(ctx context.Context) bool {
	stop, ok := ctx.Value(runStopKey{}).(runStop)
	return ok && stop.ended()
}

// runStopDeadline is the stop's one deadline of the run ctx belongs to, and
// whether that run's context has ended.
func runStopDeadline(ctx context.Context) (time.Time, bool) {
	stop, ok := ctx.Value(runStopKey{}).(runStop)
	if !ok || !stop.ended() {
		return time.Time{}, false
	}

	return stop.deadline(), true
}

// DetachedAtStopContext is the context a run applies a completion on while
// the run goes on: it keeps ctx's values but not its cancellation, so a
// cancel that lands while the completion is being applied does not cut its
// records short; from the moment the run's context ends (WithRunStop) it ends
// at the stop's one deadline, with ErrStoreAccessTimedOut as its cause. A
// ctx that carries no run stop is returned as it is. The returned cancel
// releases the watch on the run's end; the caller defers it.
func DetachedAtStopContext(ctx context.Context) (context.Context, context.CancelFunc) {
	stop, ok := ctx.Value(runStopKey{}).(runStop)
	if !ok {
		return ctx, func() {}
	}

	// The stop's deadline is armed when the run's context ends, so this
	// detach has no deadline before then: a live completion is not bounded,
	// as the run's own context bounds nothing either.
	detached, cancelCause := context.WithCancelCause(context.WithoutCancel(ctx))
	var (
		// mu guards cancelDeadline, which the callback on the run's end sets
		// and the returned cancel calls.
		mu             stdsync.Mutex
		cancelDeadline context.CancelFunc
	)
	stopWatching := stop.onEnd(func() {
		mu.Lock()
		defer mu.Unlock()
		if detached.Err() != nil {
			return
		}
		// The deadline is a context deadline on detached itself, so its end
		// carries the store-timeout cause to the context the completion
		// runs on.
		bounded, cancel := context.WithDeadlineCause(detached, stop.deadline(), syncdomain.ErrStoreAccessTimedOut)
		cancelDeadline = cancel
		context.AfterFunc(bounded, func() {
			defer recoverRunStopCallback(stop.logger)
			cancelCause(context.Cause(bounded))
		})
	})

	return detached, func() {
		stopWatching()
		cancelCause(context.Canceled)
		mu.Lock()
		defer mu.Unlock()
		if cancelDeadline != nil {
			cancelDeadline()
		}
	}
}

// RunDetachedStoreAccessUntil is RunDetachedStoreAccess under a deadline the
// caller already holds: a stopping run's one deadline, which every store wait
// after the run's context ended shares. It is the one way sync product code
// detaches a store wait from a cancellation; a source-reading test refuses a
// hand-made detach anywhere else.
func RunDetachedStoreAccessUntil(ctx context.Context, deadline time.Time, access func(context.Context) error) error {
	accessCtx, cancel := DetachedStoreContext(ctx, deadline)
	defer cancel()

	return StoreAccessError(accessCtx, access(accessCtx))
}

// detachedStoreAccessKey marks a context DetachedStoreContext made.
type detachedStoreAccessKey struct{}

// DetachedStoreContext is the context RunDetachedStoreAccess runs its access
// on: it keeps ctx's values but not its cancellation and ends at deadline with
// ErrStoreAccessTimedOut as its cause. A caller that runs several accesses
// under one bound (a stopping run's drain) gives each the same deadline. A
// ctx that runs under such a context, itself or one derived from it, is
// returned as it is, any cancel of its own included, so an access detached
// inside a detached one keeps the outer deadline: nested accesses add up to
// one bound, never to one each.
func DetachedStoreContext(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc) {
	if detached, ok := ctx.Value(detachedStoreAccessKey{}).(bool); ok && detached {
		return ctx, func() {}
	}

	accessCtx, cancel := context.WithDeadlineCause(context.WithoutCancel(ctx), deadline, syncdomain.ErrStoreAccessTimedOut)

	return context.WithValue(accessCtx, detachedStoreAccessKey{}, true), cancel
}

// StoreAccessError is err from an access that ran on accessCtx, with
// ErrStoreAccessTimedOut joined once when accessCtx's bound ended it.
func StoreAccessError(accessCtx context.Context, err error) error {
	if err == nil || errors.Is(err, syncdomain.ErrStoreAccessTimedOut) ||
		!errors.Is(context.Cause(accessCtx), syncdomain.ErrStoreAccessTimedOut) {
		return err
	}

	return fmt.Errorf("%w: %w", err, syncdomain.ErrStoreAccessTimedOut)
}

func (wp *WorkerPool) validateActionFreshness(ctx context.Context, ta *syncdomain.TrackedAction) error {
	if ta == nil {
		return nil
	}

	decision, err := EvaluateActionFreshnessFromStore(ctx, wp.baseline, &ta.Action)
	if err != nil {
		return err
	}
	if decision.Fresh {
		return nil
	}

	wp.recordWorkerStartSuperseded(decision)
	return fmt.Errorf("%w: %s", syncdomain.ErrActionPreconditionChanged, decision.Reason)
}

func (wp *WorkerPool) recordWorkerStartSuperseded(decision actionFreshnessDecision) {
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
func (wp *WorkerPool) dispatchAction(
	ctx context.Context, exec *Executor, ta *syncdomain.TrackedAction,
) syncdomain.ActionOutcome {
	action := &ta.Action

	switch action.Type {
	case syncdomain.ActionFolderCreate:
		return exec.ExecuteFolderCreate(ctx, action)
	case syncdomain.ActionConflictCopy:
		return exec.ExecuteConflictCopy(ctx, action)
	case syncdomain.ActionLocalMove, syncdomain.ActionRemoteMove:
		return exec.ExecuteMove(ctx, action)
	case syncdomain.ActionDownload:
		return exec.ExecuteDownload(ctx, action)
	case syncdomain.ActionUpload:
		return exec.ExecuteUpload(ctx, action)
	case syncdomain.ActionLocalDelete:
		return exec.ExecuteLocalDelete(ctx, action)
	case syncdomain.ActionRemoteDelete:
		return exec.ExecuteRemoteDelete(ctx, action)
	case syncdomain.ActionBaselineUpdate, syncdomain.ActionCleanup:
		return syncdomain.ActionOutcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   errPublicationOnlyActionType,
		}
	default:
		// The plan refuses an action outside the enum before it is admitted
		// (validatePlannedActions); dispatch fails one rather than run it.
		return syncdomain.ActionOutcome{
			Action:  action.Type,
			Path:    action.Path,
			Success: false,
			Error:   fmt.Errorf("%w: %s", ErrUnknownActionType, syncdomain.OutsideTheEnum(action.Type)),
		}
	}
}

// Completions returns a read-only channel of per-action completions. The
// engine reads from this channel, classifies each completion, and calls
// DepGraph.Complete. Failed items become retry_work, block scopes, or other
// engine-owned durable control state for held release and scope lifecycle.
func (wp *WorkerPool) Completions() <-chan syncdomain.ActionCompletion {
	return wp.completions
}

// sendResult reports a per-action outcome to the completions channel. Populates
// the ActionCompletion from the TrackedAction and any error. When outcome is
// non-nil, uses its Success/Error fields; otherwise treats as failure with
// the provided error. ctx is the context the workers were started under,
// which ends only with the run or once nothing is left to classify: whether
// it has ended is the completion's RunEnded, the one place a worker's
// completion learns it.
//
// A completion is the result of work that already happened, which a stopping
// run still records, so the send prefers delivery: it gives up only once that
// context has ended and the channel has no room, when no one may be left to
// read it, and says the drop at Debug. One select of both would pick at random
// once the context has ended, and drop completions the stopping run's drain
// still reads.
func (wp *WorkerPool) sendResult(ctx context.Context, ta *syncdomain.TrackedAction, outcome *syncdomain.ActionOutcome, actionErr error) {
	r := syncdomain.ActionCompletionFromTrackedAction(ta, outcome, actionErr)
	r.RunEnded = syncdomain.RunEnded(ctx, r.Err)
	wp.recordLivePreconditionSuperseded(&r, outcome)

	select {
	case wp.completions <- r:
		return
	default:
	}

	select {
	case wp.completions <- r:
	case <-ctx.Done():
		wp.logger.Debug("worker: completion dropped after the run ended; the next sync checks it again",
			slog.Int64("action_id", r.ActionID),
			slog.String("path", r.Path),
		)
	}
}

func (wp *WorkerPool) recordLivePreconditionSuperseded(r *syncdomain.ActionCompletion, outcome *syncdomain.ActionOutcome) {
	if outcome == nil || r == nil || !errors.Is(r.Err, syncdomain.ErrActionPreconditionChanged) {
		return
	}

	switch r.FailureCapability {
	case syncdomain.PermissionCapabilityLocalRead, syncdomain.PermissionCapabilityLocalWrite:
		wp.collector().RecordSuperseded(perf.SupersededSourceLiveLocalPrecondition, 1)
	case syncdomain.PermissionCapabilityRemoteRead, syncdomain.PermissionCapabilityRemoteWrite:
		wp.collector().RecordSuperseded(perf.SupersededSourceLiveRemotePrecondition, 1)
	case syncdomain.PermissionCapabilityUnknown:
	}
}

func (wp *WorkerPool) collector() *perf.Collector {
	if wp == nil {
		return nil
	}

	return wp.perfCollector
}

func (wp *WorkerPool) closeCompletions() {
	wp.closeOnce.Do(func() {
		close(wp.completions)
	})
}

// The causes of the worker contexts' cancels. Neither reaches an action while
// its run goes on: ErrActionReturned lands after the action returned, and
// ErrWorkerPoolStopped ends the context the workers were started under, so an
// action it interrupts completes with RunEnded set, as a shutdown.
const (
	ErrActionReturned    syncdomain.CancelCauseError = "syncexec: action returned"
	ErrWorkerPoolStopped syncdomain.CancelCauseError = "syncexec: worker pool stopped"
)
