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

func (rt *watchRuntime) processCommittedScopedWatchBatch(
	ctx context.Context,
	bl *Baseline,
	result remoteFetchResult,
) ([]ChangeEvent, bool) {
	projected := projectRemoteObservations(rt.engine.logger, result.events)

	if len(projected.observed) > 0 {
		if err := rt.commitObservedItems(ctx, projected.observed, ""); err != nil {
			rt.logCommittedScopedBatchFailure("commit observations", err, len(projected.observed))
			return nil, false
		}
	}

	if err := rt.commitDeferredDeltaTokens(ctx, result.deferred); err != nil {
		rt.logCommittedScopedBatchFailure("commit delta tokens", err, 0)
		return nil, false
	}

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

	finalEvents := rt.processCommittedPrimaryBatch(
		ctx,
		bl,
		projected.emitted,
		false,
		false,
	)

	return finalEvents, nil
}

func (rt *watchRuntime) logCommittedScopedBatchFailure(step string, err error, eventCount int) {
	attrs := []any{slog.String("error", err.Error())}
	if eventCount > 0 {
		attrs = append(attrs, slog.Int("events", eventCount))
	}

	rt.engine.logger.Error(fmt.Sprintf("failed to %s for scoped watch batch", step), attrs...)
}
