package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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

func (rt *watchRuntime) applyRemoteObservationBatch(
	ctx context.Context,
	batch *syncobserve.RemoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}

	if err := rt.applyShortcutObservationBatch(ctx, batch); err != nil {
		return fmt.Errorf("apply shortcut observation batch: %w", err)
	}

	commitToken := ""
	progress := *batch
	if batch.Source == syncobserve.RemoteObservationBatchPrimaryWatch && !batch.MarkFullRemoteRefresh {
		commitToken = batch.CursorToken
		progress.CursorToken = ""
	}

	if len(batch.Observed) > 0 || commitToken != "" {
		if err := rt.commitObservedItems(ctx, batch.Observed, commitToken); err != nil {
			return fmt.Errorf("commit remote observations: %w", err)
		}
	}

	// Watch mode must rearm the live refresh timer in the same control path that
	// shortens the persisted deadline, otherwise mount-root enumerate fallback
	// can leave the process sleeping on an outdated timer.
	armFullRefreshTimer := batch.ArmFullRefreshTimer
	if deferredProgress := progress.DeferredProgress(); deferredProgress != nil {
		deadlineChanged, err := rt.commitPendingRemoteObservation(ctx, deferredProgress)
		if err != nil {
			return err
		}
		armFullRefreshTimer = armFullRefreshTimer || deadlineChanged
	}

	findings := batch.Findings
	if err := rt.applyObservationFindingsBatch(
		ctx,
		&findings,
		batchObservationFailureMessage(batch.Source),
		batchObservationDebugNote(batch.Source),
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

// recordSkippedOneDriveItems prefixes a failure to persist the skipped-item
// findings of a remote observation batch.
const recordSkippedOneDriveItems = "record skipped OneDrive items"

func batchObservationFailureMessage(source syncobserve.RemoteObservationBatchSource) string {
	switch source {
	case syncobserve.RemoteObservationBatchPrimaryWatch:
		return recordSkippedOneDriveItems
	case syncobserve.RemoteObservationBatchMountRoot:
		return "record skipped shared folder items"
	case syncobserve.RemoteObservationBatchFullRefresh:
		return "record skipped items from the full OneDrive check"
	default:
		return recordSkippedOneDriveItems
	}
}

func batchObservationDebugNote(source syncobserve.RemoteObservationBatchSource) string {
	switch source {
	case syncobserve.RemoteObservationBatchPrimaryWatch:
		return engineDebugNotePrimaryWatch
	case syncobserve.RemoteObservationBatchMountRoot:
		return engineDebugNoteMountRootWatch
	case syncobserve.RemoteObservationBatchFullRefresh:
		return engineDebugNoteFullRefresh
	default:
		return ""
	}
}

func (rt *watchRuntime) handleRemoteObservationBatch(
	ctx context.Context,
	batch *syncobserve.RemoteObservationBatch,
) error {
	if batch == nil {
		return nil
	}
	if batch.PollOnly {
		// A poll that returned no items: nothing to commit and nothing to
		// replan, only the time it began, which may settle create intents.
		rt.dropGoneCreateIntentsAfterPoll(ctx, batch.PollStartedAt)
		batch.FinishApplied(nil)
		return nil
	}

	if batch.Source == syncobserve.RemoteObservationBatchFullRefresh {
		defer func() {
			rt.resources.refreshActive = false
		}()
	}

	if err := rt.applyRemoteObservationBatch(ctx, batch); err != nil {
		return rt.handleRemoteObservationBatchApplyFailure(ctx, batch, err)
	}
	// Committed: every create the poll saw has been claimed by now.
	rt.dropGoneCreateIntentsAfterPoll(ctx, batch.PollStartedAt)

	if batch.Source == syncobserve.RemoteObservationBatchFullRefresh {
		rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventRemoteRefreshCommitted})
	}

	if batch.Source == syncobserve.RemoteObservationBatchFullRefresh && ctx.Err() != nil {
		if rt.resources.dirtyBuf != nil {
			rt.resources.dirtyBuf.MarkFullRefresh()
		}
		batch.FinishApplied(nil)
		rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventRemoteRefreshApplied})
		return nil
	}

	rt.markDirtyFromRemoteBatch(batch)
	batch.FinishApplied(nil)

	if batch.Source == syncobserve.RemoteObservationBatchFullRefresh {
		rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventRemoteRefreshApplied})
	}

	return nil
}

func (rt *watchRuntime) handleRemoteObservationBatchApplyFailure(
	ctx context.Context,
	batch *syncobserve.RemoteObservationBatch,
	err error,
) error {
	if batch == nil {
		return err
	}

	switch batch.Source {
	case syncobserve.RemoteObservationBatchPrimaryWatch, syncobserve.RemoteObservationBatchMountRoot:
		batch.FinishApplied(err)
		if ctx.Err() != nil {
			return err
		}

		return newFatalObserverError(fmt.Errorf("apply %s batch: %w", batch.Source, err))
	case syncobserve.RemoteObservationBatchFullRefresh:
		// The full refresh is re-armed below and retried, so this is a
		// Warn, and a Ctrl-C landing during its commit is not reported.
		if ctx.Err() == nil {
			rt.deps.logger.Warn("Could not save the result of the full OneDrive check; will retry",
				slog.Any("error", err),
			)
		}
		if rt.resources.dirtyBuf != nil {
			rt.resources.dirtyBuf.MarkFullRefresh()
		}
		rt.deps.emit(syncdomain.EngineDebugEvent{Type: syncdomain.EngineDebugEventRemoteRefreshApplied})
		batch.FinishApplied(nil)
		return nil
	default:
		batch.FinishApplied(err)
		return err
	}
}

func (rt *watchRuntime) markDirtyFromRemoteBatch(batch *syncobserve.RemoteObservationBatch) {
	if rt == nil || rt.resources.dirtyBuf == nil || batch == nil {
		return
	}

	if batch.MarkFullRefreshIfIdle {
		rt.resources.dirtyBuf.MarkFullRefresh()
	}

	if rt.remoteBatchHasPlannerVisibleEffects(batch) {
		rt.resources.dirtyBuf.MarkDirty()
	}
}

func (rt *watchRuntime) remoteBatchHasPlannerVisibleEffects(batch *syncobserve.RemoteObservationBatch) bool {
	if rt == nil || rt.engine == nil || batch == nil || len(batch.Changes) == 0 {
		return false
	}

	return remoteChangesHavePlannerVisibleEffects(rt.engine.contentFilter, batch.Changes)
}

func remoteChangesHavePlannerVisibleEffects(filter syncdomain.ContentFilterConfig, changes []syncobserve.RemoteChange) bool {
	for i := range changes {
		if remoteChangeHasPlannerVisibleEffect(filter, &changes[i]) {
			return true
		}
	}
	return false
}

func remoteChangeHasPlannerVisibleEffect(filter syncdomain.ContentFilterConfig, change *syncobserve.RemoteChange) bool {
	visibility := syncdomain.NewContentFilter(filter)
	if visibility.Visible(change.Path, change.ItemType) {
		return true
	}
	return change.OldPath != "" && visibility.Visible(change.OldPath, change.ItemType)
}
