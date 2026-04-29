package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// runWatchUntilQuiescent drives the bootstrap watch loop until all work due
// now has drained through the shared runtime. Future-held retry/scope work may
// remain unresolved in the graph, so bootstrap quiescence is engine-owned
// rather than defined by graph emptiness.
func (rt *watchRuntime) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*TrackedAction,
) error {
	ticker := rt.engine.newTicker(quiescenceLogInterval)
	defer stopTicker(ticker)

	rt.enterBootstrap()
	rt.replaceOutbox(initialOutbox)

	return rt.runWatchLoop(ctx, p, tickerChan(ticker))
}

// runWatchLoop owns steady-state watch execution. The same goroutine handles
// observed replans, action completions, retry/trial timers, reconcile
// completions, and outbox draining.
func (rt *watchRuntime) runWatchLoop(
	ctx context.Context,
	p *watchPipeline,
	bootstrapLogC ...<-chan time.Time,
) error {
	if len(bootstrapLogC) == 0 {
		rt.replaceOutbox(nil)
	}

	var logC <-chan time.Time
	if len(bootstrapLogC) > 0 {
		logC = bootstrapLogC[0]
	}

	for {
		done, err := rt.runWatchLoopStep(ctx, p, logC)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (rt *watchRuntime) runWatchLoopStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	rt.beginWatchDrainIfCanceled(ctx, p)

	switch rt.phase() {
	case watchRuntimePhaseDraining:
		return rt.runDrainStep(ctx, p)
	case watchRuntimePhaseBootstrap:
		return rt.runBootstrapLoopStep(ctx, p, logC)
	case watchRuntimePhaseRunning:
		return rt.runRunningLoopStep(ctx, p)
	default:
		return false, fmt.Errorf("sync: unknown watch runtime phase %q", rt.phase())
	}
}

func (rt *watchRuntime) beginWatchDrainIfCanceled(ctx context.Context, p *watchPipeline) {
	if ctx.Err() != nil && !rt.isDraining() {
		rt.beginWatchDrain(ctx, p)
	}
}

func (rt *watchRuntime) runBootstrapLoopStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	if rt.isBootstrapQuiescent() {
		rt.enterRunning()
		return true, nil
	}

	return rt.runNonDrainingWatchStep(ctx, p, logC)
}

func (rt *watchRuntime) runRunningLoopStep(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	replanned, err := rt.runPendingWatchReplan(ctx, p)
	if err != nil {
		return false, err
	}
	if replanned {
		return false, nil
	}

	return rt.runNonDrainingWatchStep(ctx, p, nil)
}

func (rt *watchRuntime) runPendingWatchReplan(
	ctx context.Context,
	p *watchPipeline,
) (bool, error) {
	if !rt.canPrepareNow() {
		return false, nil
	}

	batch, ok := rt.takePendingReplan()
	if !ok {
		return false, nil
	}

	return true, rt.runSteadyStateReplan(ctx, p, batch)
}

//nolint:gocyclo // The watch select owns one event fan-in point; splitting cases would obscure ownership.
func (rt *watchRuntime) runNonDrainingWatchStep(
	ctx context.Context,
	p *watchPipeline,
	logC <-chan time.Time,
) (bool, error) {
	dispatchCh, nextAction := rt.dispatchChannelForOutbox()

	select {
	case dispatchCh <- nextAction:
		rt.handleWatchDispatch(nextAction)
		return false, nil
	case batch, ok := <-p.replanReady:
		return rt.handleWatchReplanSignal(ctx, p, batch, ok)
	case completion, ok := <-p.completions:
		return rt.handleWatchCompletionSignal(ctx, p, &completion, ok)
	case change, ok := <-rt.localEvents:
		return rt.handleWatchLocalChangeSignal(&change, ok)
	case event, ok := <-rt.protectedRootEvents:
		return rt.handleWatchProtectedRootEventSignal(ctx, &event, ok)
	case batch, ok := <-rt.remoteBatches:
		return rt.handleWatchRemoteBatchSignal(ctx, &batch, ok)
	case skipped, ok := <-rt.skippedItems:
		return rt.handleWatchSkippedSignal(ctx, skipped, ok)
	case <-rt.refreshCh:
		rt.runFullRemoteRefreshAsync(ctx, p.bl)
		return false, nil
	case result, ok := <-rt.refreshResults:
		return rt.handleWatchRefreshResultSignal(ctx, &result, ok)
	case <-p.maintenanceC:
		rt.handleMaintenanceTick(ctx)
		return false, nil
	case observerErr, ok := <-rt.observerErrs:
		return rt.handleWatchObserverErrorSignal(ctx, p, observerErr, ok)
	case <-rt.trialTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, true)
	case <-rt.retryTimerChan():
		return false, rt.handleWatchHeldReleaseSignal(ctx, p, false)
	case <-logC:
		rt.logBootstrapWait()
		return false, nil
	case <-ctx.Done():
		rt.beginWatchDrain(ctx, p)
		return false, nil
	}
}

func (rt *watchRuntime) handleWatchDispatch(nextAction *TrackedAction) {
	rt.markRunning(nextAction)
	rt.consumeOutboxHead()
}

func (rt *watchRuntime) handleWatchReplanSignal(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
	ok bool,
) (bool, error) {
	if !ok {
		return true, nil
	}

	return false, rt.handleWatchReplanReady(ctx, p, batch)
}

func (rt *watchRuntime) handleWatchCompletionSignal(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
	ok bool,
) (bool, error) {
	if !ok {
		return false, rt.handleWatchCompletionsClosed(ctx, p)
	}

	return false, rt.handleWatchActionCompletion(ctx, p, completion)
}

func (rt *watchRuntime) handleWatchLocalChangeSignal(change *ChangeEvent, ok bool) (bool, error) {
	if !ok {
		rt.localEvents = nil
		return false, nil
	}

	rt.handleWatchLocalChange(change)
	return false, nil
}

func (rt *watchRuntime) handleWatchProtectedRootEventSignal(
	ctx context.Context,
	event *ProtectedRootEvent,
	ok bool,
) (bool, error) {
	if !ok {
		rt.protectedRootEvents = nil
		return false, nil
	}
	if event == nil {
		return false, nil
	}
	bl, err := rt.engine.baseline.Load(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: load baseline for protected root event: %w", err)
	}
	if _, refreshErr := rt.refreshAndCommitLocalCurrentState(ctx, bl, false); refreshErr != nil {
		return false, fmt.Errorf("sync: refresh local_state for protected root event: %w", refreshErr)
	}
	changed, err := rt.engine.reconcileShortcutRootLocalState(ctx)
	if err != nil {
		return false, err
	}
	if !changed || rt.engine.shortcutChildWorkSink == nil {
		return false, nil
	}
	roots, err := rt.engine.baseline.listShortcutRoots(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: read shortcut roots after protected root event: %w", err)
	}
	return false, rt.engine.shortcutChildWorkSink(ctx, shortcutChildWorkSnapshotFromRootsWithParentRoot(
		rt.engine.shortcutNamespaceID,
		rt.engine.syncRoot,
		rt.engine.contentFilter,
		roots,
	))
}

func (rt *watchRuntime) handleWatchRemoteBatchSignal(
	ctx context.Context,
	batch *remoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		rt.remoteBatches = nil
		return false, nil
	}

	return false, rt.handleRemoteObservationBatch(ctx, batch)
}

func (rt *watchRuntime) handleWatchSkippedSignal(
	ctx context.Context,
	skipped []SkippedItem,
	ok bool,
) (bool, error) {
	if !ok {
		rt.skippedItems = nil
		return false, nil
	}

	reconcileCtx := ctx
	if ctx.Err() != nil {
		reconcileCtx = context.WithoutCancel(ctx)
	}

	err := rt.reconcileSkippedObservationFindings(reconcileCtx, skipped)
	if err != nil && isWatchShutdownError(ctx, err) {
		return false, nil
	}

	return false, err
}

func (rt *watchRuntime) handleWatchRefreshResultSignal(
	ctx context.Context,
	result *remoteObservationBatch,
	ok bool,
) (bool, error) {
	if !ok {
		rt.refreshResults = nil
		return false, nil
	}

	return false, rt.applyRemoteRefreshResult(ctx, result)
}

func (rt *watchRuntime) handleWatchObserverErrorSignal(
	ctx context.Context,
	p *watchPipeline,
	observerErr error,
	ok bool,
) (bool, error) {
	if !ok {
		rt.observerErrs = nil
		return false, nil
	}

	return false, rt.handleWatchObserverError(ctx, p, observerErr)
}

func (rt *watchRuntime) handleWatchHeldReleaseSignal(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	if rt.phase() == watchRuntimePhaseBootstrap {
		return rt.releaseHeldFrontier(ctx, p, trial)
	}

	return rt.handleWatchHeldRelease(ctx, p, trial)
}

func (rt *watchRuntime) appendReadyFrontier(ready []*TrackedAction) error {
	nextOutbox := append(rt.currentOutbox(), ready...)
	rt.replaceOutbox(nextOutbox)
	rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventReadyFrontierAppended})
	return nil
}

func (rt *watchRuntime) releaseHeldFrontier(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	var (
		released []*TrackedAction
		err      error
	)
	if trial {
		released, err = rt.releaseDueHeldTrialsNow(ctx, p.bl)
	} else {
		released, err = rt.releaseDueHeldRetriesNow(ctx, p.bl)
	}
	if err != nil {
		rt.completeOutboxAsShutdown(released)
		return err
	}

	return rt.appendReadyFrontier(released)
}

func (rt *watchRuntime) handleObserverExit(_ *watchPipeline, shuttingDown bool) error {
	rt.mustAssertObserverExitPhase(rt, shuttingDown, "handle observer exit")

	rt.activeObservers--
	if rt.activeObservers > 0 {
		return nil
	}

	if shuttingDown {
		rt.engine.logger.Info("all observers exited during shutdown")
		return nil
	}

	rt.engine.logger.Error("all observers have exited, stopping watch mode")
	return fmt.Errorf("sync: all observers exited")
}

func (rt *watchRuntime) logObserverError(obsErr error) {
	if obsErr == nil {
		return
	}

	rt.engine.logger.Warn("observer error",
		slog.String("error", obsErr.Error()),
	)
}

func (rt *watchRuntime) dispatchChannelForOutbox() (chan<- *TrackedAction, *TrackedAction) {
	outbox := rt.currentOutbox()
	nextAction := firstOutbox(outbox)
	if nextAction == nil {
		return nil, nil
	}

	if rt.isDraining() {
		rt.mustAssertDispatchAdmissionSealed(rt, outbox, "dispatch channel for outbox")
		return nil, nil
	}

	return rt.dispatchCh, nextAction
}

func firstOutbox(outbox []*TrackedAction) *TrackedAction {
	if len(outbox) == 0 {
		return nil
	}

	return outbox[0]
}

func (rt *watchRuntime) handleWatchReplanReady(
	ctx context.Context,
	p *watchPipeline,
	batch dirtyBatch,
) error {
	if !rt.canPrepareNow() {
		rt.queuePendingReplan(batch)
		return nil
	}

	return rt.runSteadyStateReplan(ctx, p, batch)
}

func (rt *watchRuntime) handleWatchActionCompletion(
	ctx context.Context,
	p *watchPipeline,
	completion *ActionCompletion,
) error {
	ready, err := rt.applyRuntimeCompletionStage(ctx, rt, completion, p.bl)
	if err != nil {
		rt.completeOutboxAsShutdown(ready)
		return err
	}

	return rt.appendReadyFrontier(ready)
}

func (rt *watchRuntime) handleWatchCompletionsClosed(
	ctx context.Context,
	p *watchPipeline,
) error {
	select {
	case <-ctx.Done():
		p.completions = nil
		rt.beginWatchDrain(ctx, p)
		return nil
	default:
	}

	return fmt.Errorf("sync: action completions channel closed unexpectedly")
}

func (rt *watchRuntime) handleWatchLocalChange(change *ChangeEvent) {
	if change == nil || rt.dirtyBuf == nil {
		return
	}
	if change.Path != "" || change.OldPath != "" {
		rt.dirtyBuf.MarkDirty()
	}
}

func (rt *watchRuntime) handleWatchObserverError(
	ctx context.Context,
	p *watchPipeline,
	observerErr error,
) error {
	if isFatalObserverError(observerErr) {
		return observerErr
	}

	rt.logObserverError(observerErr)
	if err := rt.handleObserverExit(p, ctx.Err() != nil); err != nil {
		return err
	}
	if rt.activeObservers == 0 {
		rt.observerErrs = nil
	}

	return nil
}

func (rt *watchRuntime) handleWatchHeldRelease(
	ctx context.Context,
	p *watchPipeline,
	trial bool,
) error {
	if rt.hasPendingReplan() {
		return nil
	}

	return rt.releaseHeldFrontier(ctx, p, trial)
}

func (rt *watchRuntime) logBootstrapWait() {
	rt.engine.logger.Info("bootstrap: waiting for in-flight actions",
		slog.Int("in_flight", rt.depGraph.InFlightCount()),
		slog.Int("running", rt.runningCount),
		slog.Int("held", len(rt.heldByKey)),
	)
}

func (rt *watchRuntime) isBootstrapQuiescent() bool {
	return len(rt.currentOutbox()) == 0 &&
		rt.runningCount == 0 &&
		!rt.hasDueHeldWork(rt.engine.nowFunc())
}
