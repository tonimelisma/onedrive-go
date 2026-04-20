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

func (flow *engineFlow) processCommittedPrimaryBatch(
	ctx context.Context,
	bl *Baseline,
	primaryEvents []ChangeEvent,
	dryRun bool,
	fullReconcile bool,
) []ChangeEvent {
	_ = ctx
	_ = bl
	_ = dryRun
	_ = fullReconcile

	return append([]ChangeEvent(nil), primaryEvents...)
}

func (rt *watchRuntime) processCommittedSharedRootWatchBatch(
	ctx context.Context,
	bl *Baseline,
	result *remoteFetchResult,
) ([]ChangeEvent, bool) {
	if result == nil {
		return nil, false
	}
	projected := projectRemoteObservations(rt.engine.logger, result.events)

	if len(projected.observed) > 0 {
		if err := rt.commitObservedItems(ctx, projected.observed, ""); err != nil {
			rt.logCommittedSharedRootBatchFailure("commit observations", err, len(projected.observed))
			return nil, false
		}
	}

	if err := rt.commitPendingPrimaryCursor(ctx, result.pending); err != nil {
		rt.logCommittedSharedRootBatchFailure("commit primary cursor", err, 0)
		return nil, false
	}
	rt.reconcileObservationFindingsBatch(
		ctx,
		rt,
		&result.findings,
		"failed to reconcile shared-root remote observation findings",
	)

	finalEvents := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		projected.emitted,
		false,
		false,
	)

	return finalEvents, true
}

func (rt *watchRuntime) processCommittedPrimaryWatchBatch(
	ctx context.Context,
	bl *Baseline,
	primaryEvents []ChangeEvent,
	newToken string,
) ([]ChangeEvent, error) {
	projected := projectRemoteObservations(rt.engine.logger, primaryEvents)

	if err := rt.commitObservedItems(ctx, projected.observed, newToken); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}

		return nil, newFatalObserverError(fmt.Errorf("commit primary watch observations: %w", err))
	}
	batch := remoteObservationManagedBatch()
	rt.reconcileObservationFindingsBatch(
		ctx,
		rt,
		&batch,
		"failed to reconcile primary remote observation findings",
	)

	finalEvents := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		projected.emitted,
		false,
		false,
	)

	return finalEvents, nil
}

func (rt *watchRuntime) logCommittedSharedRootBatchFailure(step string, err error, eventCount int) {
	attrs := []any{slog.String("error", err.Error())}
	if eventCount > 0 {
		attrs = append(attrs, slog.Int("events", eventCount))
	}

	rt.engine.logger.Error(fmt.Sprintf("failed to %s for shared-root watch batch", step), attrs...)
}
