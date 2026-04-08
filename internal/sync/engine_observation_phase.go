package sync

import (
	"context"
	"fmt"
	"log/slog"
	stdsync "sync"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (flow *engineFlow) executeObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	phase ObservationPhasePlan,
	fullReconcile bool,
) (remoteFetchResult, error) {
	if err := validateObservationPhasePlan(phase); err != nil {
		return remoteFetchResult{}, fmt.Errorf("execute observation phase: %w", err)
	}
	if phase.Driver == "" {
		return remoteFetchResult{}, nil
	}

	result, err := flow.observeObservationPhase(ctx, bl, phase, fullReconcile)
	if err != nil {
		return remoteFetchResult{}, err
	}

	return flow.finalizeObservationPhase(ctx, phase, result)
}

func (flow *engineFlow) observeObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	phase ObservationPhasePlan,
	fullReconcile bool,
) (remoteFetchResult, error) {
	var (
		result remoteFetchResult
		err    error
	)

	switch phase.DispatchPolicy {
	case observationPhaseDispatchPolicySingleBatch:
		result, err = flow.executeSingleBatchObservationPhase(ctx, bl, phase, fullReconcile)
	case observationPhaseDispatchPolicySequentialTargets:
		result, err = flow.executeSequentialTargetObservationPhase(ctx, bl, phase, fullReconcile)
	case observationPhaseDispatchPolicyParallelTargets:
		result, err = flow.executeParallelTargetObservationPhase(ctx, bl, phase, fullReconcile)
	default:
		return remoteFetchResult{}, fmt.Errorf("observe observation phase: unknown dispatch policy %q", phase.DispatchPolicy)
	}
	if err != nil {
		return remoteFetchResult{}, err
	}

	return result, nil
}

func (flow *engineFlow) executeSingleBatchObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	phase ObservationPhasePlan,
	fullReconcile bool,
) (remoteFetchResult, error) {
	switch phase.Driver {
	case observationPhaseDriverRootDelta:
		return flow.executeRootDeltaObservationPhase(ctx, bl, fullReconcile)
	case observationPhaseDriverScopedRoot:
		events, token, err := flow.observeScopedRemote(ctx, bl, fullReconcile, phase.FallbackPolicy)
		return remoteFetchResult{
			events:   events,
			deferred: deferredTokensForPrimary(token, flow.engine, fullReconcile, len(events)),
		}, err
	case observationPhaseDriverScopedTarget:
		return remoteFetchResult{}, fmt.Errorf(
			"execute observation phase: driver %q requires target dispatch",
			phase.Driver,
		)
	default:
		return remoteFetchResult{}, fmt.Errorf("execute observation phase: unknown driver %q", phase.Driver)
	}
}

func (flow *engineFlow) executeRootDeltaObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	fullReconcile bool,
) (remoteFetchResult, error) {
	if fullReconcile {
		events, token, err := flow.observeRemoteFull(ctx, bl)
		return remoteFetchResult{
			events:   events,
			deferred: deferredTokensForPrimary(token, flow.engine, true, len(events)),
		}, err
	}

	events, token, err := flow.observeRemote(ctx, bl)
	return remoteFetchResult{
		events:   events,
		deferred: deferredTokensForPrimary(token, flow.engine, false, len(events)),
	}, err
}

func (flow *engineFlow) executeSequentialTargetObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	phase ObservationPhasePlan,
	fullReconcile bool,
) (remoteFetchResult, error) {
	if phase.Driver != observationPhaseDriverScopedTarget {
		return remoteFetchResult{}, fmt.Errorf(
			"execute observation phase: sequential dispatch requires scoped target driver, got %q",
			phase.Driver,
		)
	}

	return flow.observePlannedPrimaryScopes(ctx, bl, phase, fullReconcile)
}

func (flow *engineFlow) executeParallelTargetObservationPhase(
	ctx context.Context,
	bl *synctypes.Baseline,
	phase ObservationPhasePlan,
	fullReconcile bool,
) (remoteFetchResult, error) {
	if phase.Driver != observationPhaseDriverScopedTarget {
		return remoteFetchResult{}, fmt.Errorf("execute observation phase: parallel dispatch requires scoped target driver, got %q", phase.Driver)
	}
	if len(phase.Targets) == 0 {
		return remoteFetchResult{}, nil
	}

	results := make([]remoteFetchResult, len(phase.Targets))

	var mu stdsync.Mutex

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(maxShortcutConcurrency)

	for i := range phase.Targets {
		target := phase.Targets[i]
		g.Go(func() error {
			phaseResult, execErr := flow.observePlannedTarget(gCtx, bl, phase, target, fullReconcile)
			if execErr != nil {
				if phase.ErrorPolicy == observationPhaseErrorPolicyIsolateTarget {
					flow.logIsolatedTargetObservationFailure(target, execErr)
					return nil
				}

				return fmt.Errorf("observe target %s: %w", observationTargetLogID(target), execErr)
			}

			mu.Lock()
			results[i] = phaseResult
			mu.Unlock()
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return remoteFetchResult{}, fmt.Errorf("observe parallel target phase: %w", err)
	}

	merged := remoteFetchResult{
		events:       make([]synctypes.ChangeEvent, 0),
		deferred:     make([]deferredDeltaToken, 0),
		fullPrefixes: make([]string, 0),
	}
	for i := range results {
		merged.events = append(merged.events, results[i].events...)
		merged.deferred = append(merged.deferred, results[i].deferred...)
		merged.fullPrefixes = append(merged.fullPrefixes, results[i].fullPrefixes...)
	}

	return merged, nil
}

func (flow *engineFlow) finalizeObservationPhase(
	ctx context.Context,
	phase ObservationPhasePlan,
	result remoteFetchResult,
) (remoteFetchResult, error) {
	switch phase.TokenCommitPolicy {
	case observationPhaseTokenCommitPolicyAfterPlannerAccepts:
		return result, nil
	case observationPhaseTokenCommitPolicyAfterPhaseSuccess:
		if len(result.deferred) == 0 {
			return result, nil
		}

		if err := flow.commitDeferredDeltaTokens(ctx, result.deferred); err != nil {
			flow.engine.logger.Warn("failed to commit phase delta tokens",
				slog.String("driver", string(phase.Driver)),
				slog.String("error", err.Error()),
			)
		}

		result.deferred = nil
		return result, nil
	default:
		return remoteFetchResult{}, fmt.Errorf("execute observation phase: unknown token commit policy %q", phase.TokenCommitPolicy)
	}
}

func (flow *engineFlow) logIsolatedTargetObservationFailure(target plannedObservationTarget, err error) {
	attrs := []any{
		slog.String("path", target.localPath),
		slog.String("drive_id", target.driveID.String()),
		slog.String("error", err.Error()),
	}
	if target.shortcut != nil {
		attrs = append(attrs, slog.String("item_id", target.shortcut.ItemID))
	}

	flow.engine.logger.Warn("target observation failed, skipping", attrs...)
}

func observationTargetLogID(target plannedObservationTarget) string {
	if target.shortcut != nil {
		return target.shortcut.ItemID
	}

	if target.localPath != "" {
		return target.localPath
	}

	return target.scopeID
}
