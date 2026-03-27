package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// engineLoopConfig describes one invocation of the engine-owned result/admission
// loop. Nil channels disable the corresponding select cases, which lets the same
// loop drive one-shot execution, watch bootstrap, and steady-state watch mode
// without inventing a second control subsystem.
type engineLoopConfig struct {
	bl     *synctypes.Baseline
	safety *synctypes.SafetyConfig
	mode   synctypes.SyncMode

	initialOutbox []*synctypes.TrackedAction

	readyBatches <-chan []synctypes.PathChanges
	results      <-chan synctypes.WorkerResult
	skippedCh    <-chan []synctypes.SkippedItem
	recheckC     <-chan time.Time
	reconcileC   <-chan time.Time
	observerErrs <-chan error
	heartbeatC   <-chan time.Time

	stopWhenEmpty        bool
	stopWhenResultsClose bool
	returnContextErr     bool

	onObserverError func(error) error
	onHeartbeat     func()
}

// runEngineLoop is the single internal control loop for result processing and
// dependent admission. Watch mode uses it directly, bootstrap uses it until the
// graph drains, and one-shot mode runs it over worker results only.
func (e *Engine) runEngineLoop(ctx context.Context, cfg *engineLoopConfig) error {
	outbox := append([]*synctypes.TrackedAction(nil), cfg.initialOutbox...)

	var emptyCh <-chan struct{}
	if cfg.stopWhenEmpty {
		emptyCh = e.depGraph.WaitForEmpty()
	}

	for {
		var (
			done bool
			err  error
		)

		if len(outbox) == 0 {
			outbox, done, err = e.runEngineLoopIdle(ctx, cfg, emptyCh)
		} else {
			outbox, done, err = e.runEngineLoopWithOutbox(ctx, cfg, emptyCh, outbox)
		}
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (e *Engine) runEngineLoopIdle(
	ctx context.Context,
	cfg *engineLoopConfig,
	emptyCh <-chan struct{},
) ([]*synctypes.TrackedAction, bool, error) {
	select {
	case batch, ok := <-cfg.readyBatches:
		return e.loopReadyBatch(ctx, cfg, nil, batch, ok)
	case r, ok := <-cfg.results:
		return e.loopWorkerResult(ctx, cfg, nil, &r, ok)
	case skipped, ok := <-cfg.skippedCh:
		return e.loopSkipped(ctx, cfg, nil, skipped, ok)
	case <-cfg.recheckC:
		e.handleRecheckTick(ctx)
		return nil, false, nil
	case <-cfg.reconcileC:
		e.runFullReconciliationAsync(ctx, cfg.bl)
		return nil, false, nil
	case obsErr, ok := <-cfg.observerErrs:
		return e.loopObserverError(cfg, nil, obsErr, ok)
	case <-cfg.heartbeatC:
		return e.loopHeartbeat(cfg, nil)
	case <-e.trialTimerChan():
		e.handleTrialTimer(ctx)
		return nil, false, nil
	case <-e.retryTimerChan():
		e.handleRetryTimer(ctx)
		return nil, false, nil
	case <-emptyCh:
		return nil, true, nil
	case <-ctx.Done():
		return e.loopContextDone(ctx, cfg, nil)
	}
}

func (e *Engine) runEngineLoopWithOutbox(
	ctx context.Context,
	cfg *engineLoopConfig,
	emptyCh <-chan struct{},
	outbox []*synctypes.TrackedAction,
) ([]*synctypes.TrackedAction, bool, error) {
	select {
	case e.readyCh <- outbox[0]:
		return outbox[1:], false, nil
	case batch, ok := <-cfg.readyBatches:
		return e.loopReadyBatch(ctx, cfg, outbox, batch, ok)
	case r, ok := <-cfg.results:
		return e.loopWorkerResult(ctx, cfg, outbox, &r, ok)
	case skipped, ok := <-cfg.skippedCh:
		return e.loopSkipped(ctx, cfg, outbox, skipped, ok)
	case <-cfg.recheckC:
		e.handleRecheckTick(ctx)
		return outbox, false, nil
	case <-cfg.reconcileC:
		e.runFullReconciliationAsync(ctx, cfg.bl)
		return outbox, false, nil
	case obsErr, ok := <-cfg.observerErrs:
		return e.loopObserverError(cfg, outbox, obsErr, ok)
	case <-cfg.heartbeatC:
		return e.loopHeartbeat(cfg, outbox)
	case <-e.trialTimerChan():
		e.handleTrialTimer(ctx)
		return outbox, false, nil
	case <-e.retryTimerChan():
		e.handleRetryTimer(ctx)
		return outbox, false, nil
	case <-emptyCh:
		return outbox, true, nil
	case <-ctx.Done():
		return e.loopContextDone(ctx, cfg, outbox)
	}
}

func (e *Engine) loopReadyBatch(
	ctx context.Context,
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
	batch []synctypes.PathChanges,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		return outbox, true, nil
	}

	return append(outbox, e.processBatch(ctx, batch, cfg.bl, cfg.mode, cfg.safety)...), false, nil
}

func (e *Engine) loopWorkerResult(
	ctx context.Context,
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
	r *synctypes.WorkerResult,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		if cfg.stopWhenResultsClose || ctx.Err() != nil {
			return outbox, true, nil
		}
		return outbox, false, fmt.Errorf("sync: worker results channel closed unexpectedly")
	}

	return append(outbox, e.processWorkerResult(ctx, r, cfg.bl)...), false, nil
}

func (e *Engine) loopSkipped(
	ctx context.Context,
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
	skipped []synctypes.SkippedItem,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		cfg.skippedCh = nil
		return outbox, false, nil
	}

	e.recordSkippedItems(ctx, skipped)
	e.clearResolvedSkippedItems(ctx, skipped)
	return outbox, false, nil
}

func (e *Engine) loopObserverError(
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
	obsErr error,
	ok bool,
) ([]*synctypes.TrackedAction, bool, error) {
	if !ok {
		cfg.observerErrs = nil
		return outbox, false, nil
	}
	if cfg.onObserverError == nil {
		return outbox, false, nil
	}

	return outbox, false, cfg.onObserverError(obsErr)
}

func (e *Engine) loopHeartbeat(
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
) ([]*synctypes.TrackedAction, bool, error) {
	if cfg.onHeartbeat != nil {
		cfg.onHeartbeat()
	}

	return outbox, false, nil
}

func (e *Engine) loopContextDone(
	ctx context.Context,
	cfg *engineLoopConfig,
	outbox []*synctypes.TrackedAction,
) ([]*synctypes.TrackedAction, bool, error) {
	if cfg.returnContextErr {
		return outbox, false, ctx.Err()
	}

	return outbox, true, nil
}

// runWatchUntilQuiescent drives the engine loop until the dependency graph
// empties. Used by bootstrapSync so the initial sync executes through the same
// single-owner result/admission loop as steady-state watch mode.
func (e *Engine) runWatchUntilQuiescent(
	ctx context.Context,
	p *watchPipeline,
	initialOutbox []*synctypes.TrackedAction,
) error {
	ticker := time.NewTicker(quiescenceLogInterval)
	defer ticker.Stop()

	return e.runEngineLoop(ctx, &engineLoopConfig{
		bl:               p.bl,
		safety:           p.safety,
		mode:             p.mode,
		initialOutbox:    initialOutbox,
		readyBatches:     p.ready,
		results:          p.results,
		heartbeatC:       ticker.C,
		stopWhenEmpty:    true,
		returnContextErr: true,
		onHeartbeat: func() {
			e.logger.Info("bootstrap: waiting for in-flight actions",
				slog.Int("in_flight", e.depGraph.InFlightCount()),
			)
		},
	})
}

// waitForQuiescence is the compatibility wrapper for callers and tests that
// only need to wait for the dependency graph to drain, without a full watch
// pipeline. It reuses the same single-owner loop used by bootstrap.
func (e *Engine) waitForQuiescence(ctx context.Context) error {
	return e.runWatchUntilQuiescent(ctx, &watchPipeline{}, nil)
}

// runWatchLoop runs the steady-state watch-mode select loop. The same goroutine
// owns batch intake, result processing, retry/trial timers, and worker outbox
// draining.
func (e *Engine) runWatchLoop(ctx context.Context, p *watchPipeline) error {
	return e.runEngineLoop(ctx, &engineLoopConfig{
		bl:           p.bl,
		safety:       p.safety,
		mode:         p.mode,
		readyBatches: p.ready,
		results:      p.results,
		skippedCh:    p.skippedCh,
		recheckC:     p.recheckC,
		reconcileC:   p.reconcileC,
		observerErrs: p.errs,
		onObserverError: func(obsErr error) error {
			return e.handleObserverExit(p, obsErr)
		},
	})
}

func (e *Engine) handleObserverExit(p *watchPipeline, obsErr error) error {
	if obsErr != nil {
		e.logger.Warn("observer error",
			slog.String("error", obsErr.Error()),
		)
	}

	p.activeObs--
	if p.activeObs > 0 {
		return nil
	}

	e.logger.Error("all observers have exited, stopping watch mode")
	return fmt.Errorf("sync: all observers exited")
}
