package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
)

type fatalObserverError struct {
	err error
}

func (e fatalObserverError) Error() string {
	return e.err.Error()
}

func (e fatalObserverError) Unwrap() error {
	return e.err
}

func newFatalObserverError(err error) error {
	if err == nil {
		return nil
	}

	return fatalObserverError{err: err}
}

func isFatalObserverError(err error) bool {
	var fatal fatalObserverError
	return errors.As(err, &fatal)
}

func buildPrimaryWatchBatch(
	engine *Engine,
	primaryEvents []ChangeEvent,
	newToken string,
) remoteObservationBatch {
	projected := projectRemoteObservations(engine.logger, primaryEvents)

	return remoteObservationBatch{
		source:   remoteObservationBatchPrimaryWatch,
		observed: projected.observed,
		emitted:  append([]ChangeEvent(nil), projected.emitted...),
		pending:  primaryCursorCommit(newToken, engine, false, len(projected.emitted)),
		findings: newRemoteObservationFindingsBatch(),
		applyAck: make(chan error, 1),
	}
}

func buildSharedRootWatchBatch(
	engine *Engine,
	result *remoteFetchResult,
) remoteObservationBatch {
	if result == nil {
		return remoteObservationBatch{
			source:   remoteObservationBatchSharedRoot,
			findings: newRemoteObservationFindingsBatch(),
			applyAck: make(chan error, 1),
		}
	}

	projected := projectRemoteObservations(engine.logger, result.events)

	return remoteObservationBatch{
		source:   remoteObservationBatchSharedRoot,
		observed: projected.observed,
		emitted:  append([]ChangeEvent(nil), projected.emitted...),
		pending:  clonePendingPrimaryCursorCommit(result.pending),
		findings: result.findings,
		applyAck: make(chan error, 1),
	}
}

func buildFullRemoteRefreshBatch(
	engine *Engine,
	result remoteFetchResult,
) remoteRefreshResult {
	projected := projectRemoteObservations(engine.logger, result.events)

	return remoteObservationBatch{
		source:                remoteObservationBatchFullRefresh,
		observed:              projected.observed,
		emitted:               append([]ChangeEvent(nil), projected.emitted...),
		pending:               clonePendingPrimaryCursorCommit(result.pending),
		findings:              result.findings,
		armFullRefreshTimer:   true,
		markFullRefreshIfIdle: len(projected.emitted) == 0,
	}
}

func clonePendingPrimaryCursorCommit(pending *pendingPrimaryCursorCommit) *pendingPrimaryCursorCommit {
	if pending == nil {
		return nil
	}

	clone := *pending
	return &clone
}

func (batch *remoteObservationBatch) waitApplied(ctx context.Context) error {
	if batch == nil || batch.applyAck == nil {
		return nil
	}

	select {
	case err := <-batch.applyAck:
		return err
	case <-ctx.Done():
		return fmt.Errorf("waiting for remote observation batch apply: %w", ctx.Err())
	}
}

func (batch *remoteObservationBatch) finishApplied(err error) {
	if batch == nil || batch.applyAck == nil {
		return
	}

	select {
	case batch.applyAck <- err:
	default:
	}
}

func (rt *watchRuntime) applyRemoteObservationBatch(
	ctx context.Context,
	batch *remoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	commitToken := ""
	pending := clonePendingPrimaryCursorCommit(batch.pending)
	if batch.source == remoteObservationBatchPrimaryWatch && pending != nil && !pending.markFullRemoteRefresh {
		commitToken = pending.token
		pending = nil
	}

	if len(batch.observed) > 0 || commitToken != "" {
		if err := rt.commitObservedItems(ctx, batch.observed, commitToken); err != nil {
			return fmt.Errorf("commit remote observations: %w", err)
		}
	}

	if pending != nil {
		if err := rt.commitPendingPrimaryCursor(ctx, pending); err != nil {
			return err
		}
	}

	findings := batch.findings
	rt.reconcileObservationFindingsBatch(
		ctx,
		rt,
		&findings,
		batchObservationFailureMessage(batch.source),
	)

	if batch.armFullRefreshTimer {
		if err := rt.armFullRefreshTimer(ctx); err != nil {
			return fmt.Errorf("arm full remote refresh timer: %w", err)
		}
	}

	return nil
}

func batchObservationFailureMessage(source remoteObservationBatchSource) string {
	switch source {
	case remoteObservationBatchPrimaryWatch:
		return "failed to reconcile primary remote observation findings"
	case remoteObservationBatchSharedRoot:
		return "failed to reconcile shared-root remote observation findings"
	case remoteObservationBatchFullRefresh:
		return "failed to reconcile full remote refresh observation findings"
	default:
		return "failed to reconcile remote observation findings"
	}
}

func (rt *watchRuntime) handleRemoteObservationBatch(
	ctx context.Context,
	batch *remoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	if batch.source == remoteObservationBatchFullRefresh {
		defer func() {
			rt.refreshActive = false
		}()
	}

	if err := rt.applyRemoteObservationBatch(ctx, batch); err != nil {
		return rt.handleRemoteObservationBatchApplyFailure(ctx, batch, err)
	}

	if batch.source == remoteObservationBatchFullRefresh && rt.afterRefreshCommit != nil {
		rt.afterRefreshCommit()
	}

	if batch.source == remoteObservationBatchFullRefresh && ctx.Err() != nil {
		if rt.dirtyBuf != nil {
			rt.dirtyBuf.MarkFullRefresh()
		}
		batch.finishApplied(nil)
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshApplied})
		return nil
	}

	rt.markDirtyFromRemoteBatch(batch)
	batch.finishApplied(nil)

	if batch.source == remoteObservationBatchFullRefresh {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshApplied})
	}

	return nil
}

func (rt *watchRuntime) handleRemoteObservationBatchApplyFailure(
	ctx context.Context,
	batch *remoteObservationBatch,
	err error,
) error {
	if batch == nil {
		return err
	}

	switch batch.source {
	case remoteObservationBatchPrimaryWatch:
		batch.finishApplied(err)
		if ctx.Err() != nil {
			return err
		}

		return newFatalObserverError(fmt.Errorf("apply primary watch batch: %w", err))
	case remoteObservationBatchSharedRoot:
		rt.logCommittedSharedRootBatchFailure("apply watch batch", err, len(batch.emitted))
		batch.finishApplied(nil)
		return nil
	case remoteObservationBatchFullRefresh:
		rt.engine.logger.Error("failed to apply full remote refresh batch",
			slog.String("error", err.Error()),
		)
		if rt.dirtyBuf != nil {
			rt.dirtyBuf.MarkFullRefresh()
		}
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshApplied})
		batch.finishApplied(nil)
		return nil
	default:
		batch.finishApplied(err)
		return err
	}
}

func (rt *watchRuntime) markDirtyFromRemoteBatch(batch *remoteObservationBatch) {
	if rt == nil || rt.dirtyBuf == nil || batch == nil {
		return
	}

	if batch.markFullRefreshIfIdle {
		rt.dirtyBuf.MarkFullRefresh()
	}

	for i := range batch.emitted {
		if batch.emitted[i].Path != "" {
			rt.dirtyBuf.MarkPath(batch.emitted[i].Path)
		}
		if batch.emitted[i].OldPath != "" {
			rt.dirtyBuf.MarkPath(batch.emitted[i].OldPath)
		}
	}
}

func (rt *watchRuntime) logCommittedSharedRootBatchFailure(step string, err error, eventCount int) {
	attrs := []any{slog.String("error", err.Error())}
	if eventCount > 0 {
		attrs = append(attrs, slog.Int("events", eventCount))
	}

	rt.engine.logger.Error(fmt.Sprintf("failed to %s for shared-root watch batch", step), attrs...)
}
