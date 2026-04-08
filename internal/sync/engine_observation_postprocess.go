package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (flow *engineFlow) processCommittedPrimaryBatch(
	ctx context.Context,
	bl *synctypes.Baseline,
	primaryEvents []synctypes.ChangeEvent,
	snapshot syncscope.Snapshot,
	generation int64,
	dryRun bool,
	fullReconcile bool,
) ([]synctypes.ChangeEvent, error) {
	visiblePrimary := filterOutShortcuts(append([]synctypes.ChangeEvent(nil), primaryEvents...))
	if dryRun {
		return visiblePrimary, nil
	}

	mutations, err := flow.shortcutCoordinator().applyShortcutBatchMutations(ctx, primaryEvents, fullReconcile)
	if err != nil {
		return nil, err
	}
	visiblePrimary = mutations.VisiblePrimary

	if flow.scopeController().isObservationSuppressed(flow.watch) {
		flow.engine.logger.Debug("suppressing shortcut observation — global scope block active")
		return visiblePrimary, nil
	}

	suppressedShortcutTargets := flow.scopeController().suppressedShortcutTargets(flow.watch)
	shortcutEvents, err := flow.shortcutCoordinator().observeShortcutFollowUp(
		ctx,
		mutations.Shortcuts,
		bl,
		fullReconcile,
		nil,
		suppressedShortcutTargets,
	)
	if err != nil {
		return nil, err
	}

	filteredShortcuts := applyRemoteScope(flow.engine.logger, snapshot, generation, shortcutEvents).emitted
	return append(visiblePrimary, filteredShortcuts...), nil
}

func (rt *watchRuntime) processCommittedScopedWatchBatch(
	ctx context.Context,
	bl *synctypes.Baseline,
	result remoteFetchResult,
	fullReconcile bool,
) ([]synctypes.ChangeEvent, bool) {
	scopeSnapshot := rt.currentScopeSnapshot()
	scopeGeneration := rt.currentScopeGeneration()
	scoped := applyRemoteScope(rt.engine.logger, scopeSnapshot, scopeGeneration, result.events)

	if len(scoped.observed) > 0 {
		if err := rt.commitObservedItems(ctx, scoped.observed, ""); err != nil {
			rt.logCommittedScopedBatchFailure("commit observations", err, len(scoped.observed))
			return nil, false
		}
	}

	if err := rt.commitDeferredDeltaTokens(ctx, result.deferred); err != nil {
		rt.logCommittedScopedBatchFailure("commit delta tokens", err, 0)
		return nil, false
	}

	finalEvents, err := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		scoped.emitted,
		scopeSnapshot,
		scopeGeneration,
		false,
		fullReconcile,
	)
	if err != nil {
		rt.logCommittedPrimaryBatchFailure(err, fullReconcile)
		return filterOutShortcuts(scoped.emitted), true
	}

	return finalEvents, true
}

func (rt *watchRuntime) logCommittedScopedBatchFailure(step string, err error, eventCount int) {
	attrs := []any{slog.String("error", err.Error())}
	if eventCount > 0 {
		attrs = append(attrs, slog.Int("events", eventCount))
	}

	rt.engine.logger.Error(fmt.Sprintf("failed to %s for scoped watch batch", step), attrs...)
}

func (rt *watchRuntime) logCommittedPrimaryBatchFailure(err error, fullReconcile bool) {
	message := "shortcut processing failed during scoped watch batch"
	if fullReconcile {
		message = "shortcut reconciliation failed during full reconciliation"
	}

	rt.engine.logger.Warn(message, slog.String("error", err.Error()))
}
