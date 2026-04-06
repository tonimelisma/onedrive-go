package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (rt *watchRuntime) watchPrimaryScopedRemote(
	ctx context.Context,
	bl *synctypes.Baseline,
	events chan<- synctypes.ChangeEvent,
	interval time.Duration,
	scopes []primaryObservationScope,
) error {
	if interval < syncobserve.MinPollInterval {
		interval = syncobserve.MinPollInterval
	}

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(interval)

	for {
		result, err := rt.observePrimaryScopedWatchPoll(ctx, bl, scopes)
		if err != nil {
			stop, handleErr := rt.handlePrimaryScopedPollError(ctx, bo, err)
			if stop || handleErr != nil {
				return handleErr
			}
			continue
		}

		scoped := applyRemoteScope(rt.engine.logger, rt.currentScopeSnapshot(), rt.currentScopeGeneration(), result.events)
		if !rt.commitPrimaryScopedWatchBatch(ctx, scoped.observed, result.deferred) {
			continue
		}

		if len(scoped.emitted) == 0 {
			bo.Reset()
			stop, sleepErr := rt.sleepPrimaryScopedWatch(ctx, interval, "interval")
			if stop || sleepErr != nil {
				return sleepErr
			}
			continue
		}

		if stop := rt.dispatchPrimaryScopedWatchEvents(ctx, events, scoped.emitted); stop {
			return nil
		}

		bo.Reset()
		stop, sleepErr := rt.sleepPrimaryScopedWatch(ctx, interval, "interval")
		if stop || sleepErr != nil {
			return sleepErr
		}
	}
}

func (rt *watchRuntime) observePrimaryScopedWatchPoll(
	ctx context.Context,
	bl *synctypes.Baseline,
	scopes []primaryObservationScope,
) (remoteFetchResult, error) {
	result := remoteFetchResult{
		events:       make([]synctypes.ChangeEvent, 0),
		deferred:     make([]deferredDeltaToken, 0),
		fullPrefixes: make([]string, 0),
	}

	for i := range scopes {
		scopeResult, err := rt.observeSinglePrimaryScope(ctx, bl, scopes[i], false)
		if err != nil {
			return remoteFetchResult{}, err
		}

		result.events = append(result.events, scopeResult.events...)
		result.deferred = append(result.deferred, scopeResult.deferred...)
		result.fullPrefixes = append(result.fullPrefixes, scopeResult.fullPrefixes...)
	}

	return result, nil
}

func (rt *watchRuntime) handlePrimaryScopedPollError(
	ctx context.Context,
	bo *retry.Backoff,
	err error,
) (bool, error) {
	if primaryScopedWatchStopped(ctx, err) {
		return true, nil
	}

	delay := bo.Next()
	rt.engine.logger.Warn("scoped sync_paths watch poll failed, backing off",
		slog.String("error", err.Error()),
		slog.Duration("backoff", delay),
		slog.String("drive_id", rt.engine.driveID.String()),
	)

	return rt.sleepPrimaryScopedWatch(ctx, delay, "backoff")
}

func (rt *watchRuntime) dispatchPrimaryScopedWatchEvents(
	ctx context.Context,
	events chan<- synctypes.ChangeEvent,
	polledEvents []synctypes.ChangeEvent,
) bool {
	for i := range polledEvents {
		select {
		case events <- polledEvents[i]:
		case <-ctx.Done():
			return true
		}
	}

	return false
}

func (rt *watchRuntime) sleepPrimaryScopedWatch(
	ctx context.Context,
	delay time.Duration,
	label string,
) (bool, error) {
	sleepErr := syncobserve.TimeSleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}
	if primaryScopedWatchStopped(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("scoped sync_paths watch %s sleep: %w", label, sleepErr)
}

func primaryScopedWatchStopped(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func (rt *watchRuntime) commitPrimaryScopedWatchBatch(
	ctx context.Context,
	observed []synctypes.ObservedItem,
	tokens []deferredDeltaToken,
) bool {
	if len(observed) > 0 {
		if err := rt.commitObservedItems(ctx, observed, ""); err != nil {
			rt.engine.logger.Error("failed to commit scoped sync_paths observations in watch",
				slog.String("error", err.Error()),
				slog.Int("events", len(observed)),
			)
			return false
		}
	}

	if err := rt.commitDeferredDeltaTokens(ctx, tokens); err != nil {
		rt.engine.logger.Error("failed to commit scoped sync_paths delta tokens in watch",
			slog.String("error", err.Error()),
		)
		return false
	}

	return true
}
