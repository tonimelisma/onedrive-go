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

	if err := rt.applyShortcutObservationBatch(ctx, batch); err != nil {
		return fmt.Errorf("apply shortcut observation batch: %w", err)
	}

	commitToken := ""
	progress := *batch
	if batch.source == remoteObservationBatchPrimaryWatch && !batch.markFullRemoteRefresh {
		commitToken = batch.cursorToken
		progress.cursorToken = ""
	}

	if len(batch.observed) > 0 || commitToken != "" {
		if err := rt.commitObservedItems(ctx, batch.observed, commitToken); err != nil {
			return fmt.Errorf("commit remote observations: %w", err)
		}
	}

	// Watch mode must rearm the live refresh timer in the same control path that
	// shortens the persisted deadline, otherwise mount-root enumerate fallback
	// can leave the process sleeping on an outdated timer.
	armFullRefreshTimer := batch.armFullRefreshTimer
	if deferredProgress := progress.deferredProgress(); deferredProgress != nil {
		deadlineChanged, err := rt.commitPendingRemoteObservation(ctx, deferredProgress)
		if err != nil {
			return err
		}
		armFullRefreshTimer = armFullRefreshTimer || deadlineChanged
	}

	findings := batch.findings
	if err := rt.applyObservationFindingsBatch(
		ctx,
		&findings,
		batchObservationFailureMessage(batch.source),
		batchObservationDebugNote(batch.source),
	); err != nil {
		return err
	}

	if armFullRefreshTimer {
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
	case remoteObservationBatchMountRoot:
		return "failed to reconcile mount-root remote observation findings"
	case remoteObservationBatchFullRefresh:
		return "failed to reconcile full remote refresh observation findings"
	default:
		return "failed to reconcile remote observation findings"
	}
}

func batchObservationDebugNote(source remoteObservationBatchSource) string {
	switch source {
	case remoteObservationBatchPrimaryWatch:
		return engineDebugNotePrimaryWatch
	case remoteObservationBatchMountRoot:
		return engineDebugNoteMountRootWatch
	case remoteObservationBatchFullRefresh:
		return engineDebugNoteFullRefresh
	default:
		return ""
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

	if batch.source == remoteObservationBatchFullRefresh {
		rt.engine.emitDebugEvent(engineDebugEvent{Type: engineDebugEventRemoteRefreshCommitted})
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
	case remoteObservationBatchPrimaryWatch, remoteObservationBatchMountRoot:
		batch.finishApplied(err)
		if ctx.Err() != nil {
			return err
		}

		return newFatalObserverError(fmt.Errorf("apply %s batch: %w", batch.source, err))
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

	if rt.remoteBatchHasPlannerVisibleEffects(batch) {
		rt.dirtyBuf.MarkDirty()
	}
}

func (rt *watchRuntime) remoteBatchHasPlannerVisibleEffects(batch *remoteObservationBatch) bool {
	if rt == nil || rt.engine == nil || batch == nil || len(batch.emitted) == 0 {
		return false
	}
	visibility := NewContentFilter(rt.engine.contentFilter)
	for i := range batch.emitted {
		event := batch.emitted[i]
		if visibility.Visible(event.Path, event.ItemType) {
			return true
		}
		if event.OldPath != "" && visibility.Visible(event.OldPath, event.ItemType) {
			return true
		}
	}
	return false
}
