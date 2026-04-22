package sync

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (r *oneShotRunner) observeLiveCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	pendingCursorCommit, err := r.observeCurrentTruth(ctx, nil, bl, false, fullReconcile)
	if err != nil {
		return nil, err
	}
	observed, err := r.loadObservedCurrentState(ctx, pendingCursorCommit)
	if err != nil {
		return nil, err
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return observed, nil
}

// observeRemote fetches delta changes from the Graph API. Automatically
// retries with an empty token if ErrDeltaExpired is returned (full resync).
func (flow *engineFlow) observeRemote(ctx context.Context, bl *Baseline) ([]ChangeEvent, string, error) {
	eng := flow.engine
	state, err := eng.baseline.ReadObservationState(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("sync: reading observation state: %w", err)
	}
	savedToken := state.Cursor

	obs := NewRemoteObserver(eng.fetcher, bl, eng.driveID, eng.logger)

	events, token, err := obs.FullDelta(ctx, savedToken)
	if err != nil {
		if !errors.Is(err, ErrDeltaExpired) {
			return nil, "", fmt.Errorf("sync: observing remote delta: %w", err)
		}

		// Delta token expired — retry with empty token for full resync.
		eng.logger.Warn("delta token expired, performing full resync")

		events, token, err = obs.FullDelta(ctx, "")
		if err != nil {
			return nil, "", fmt.Errorf("sync: full resync after delta expiry: %w", err)
		}
	}

	return events, token, nil
}

// observeLocal scans the local filesystem for changes and collects skipped
// items (invalid names, path too long, file too large) for failure recording.
// The observer also receives platform-derived naming rules from the engine so
// SharePoint-specific validation stays aligned across one-shot, watch, and
// retry/trial observation paths.
func (flow *engineFlow) observeLocal(
	ctx context.Context,
	bl *Baseline,
) (ScanResult, error) {
	eng := flow.engine

	obs := NewLocalObserver(bl, eng.logger, eng.checkWorkers)
	obs.SetFilterConfig(eng.localFilter)
	obs.SetObservationRules(eng.localRules)

	result, err := obs.FullScan(ctx, eng.syncTree)
	if err != nil {
		return ScanResult{}, fmt.Errorf("sync: local scan: %w", err)
	}

	return result, nil
}

// observeCurrentTruth runs the entrypoint-owned remote/local observation flow
// and returns the deferred remote cursor commit the runtime will publish after
// the current plan is accepted.
func (flow *engineFlow) observeCurrentTruth(
	ctx context.Context,
	watch *watchRuntime,
	bl *Baseline,
	dryRun, fullReconcile bool,
) (*pendingPrimaryCursorCommit, error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	remoteEvents, pendingCursorCommit, err := flow.observeRemoteChanges(
		ctx, bl, dryRun, plan,
	)
	if err != nil {
		return nil, err
	}
	_ = remoteEvents

	_ = watch
	localResult, err := flow.observeLocalChanges(ctx, bl)
	if err != nil {
		return nil, err
	}
	if commitLocalErr := flow.commitObservedLocalSnapshot(ctx, dryRun, localResult); commitLocalErr != nil {
		return nil, commitLocalErr
	}

	return pendingCursorCommit, nil
}

func (flow *engineFlow) observeRemoteChanges(
	ctx context.Context,
	bl *Baseline,
	dryRun bool,
	plan primaryRootObservationPlan,
) ([]ChangeEvent, *pendingPrimaryCursorCommit, error) {
	fetchResult, err := flow.fetchRemoteChanges(ctx, bl, plan)
	if err != nil {
		return nil, nil, err
	}

	// Dry-run previews must never advance remote observation cursors.
	if dryRun {
		fetchResult.pending = nil
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)
	if err := flow.commitObservedRemoteChanges(
		ctx,
		dryRun,
		projectedRemote.observed,
	); err != nil {
		return nil, nil, err
	}
	if !dryRun && flow.engine.beforeRemoteObservationFindingsReconcile != nil {
		flow.engine.beforeRemoteObservationFindingsReconcile()
	}
	if !dryRun {
		if err := flow.applyObservationFindingsBatch(
			ctx,
			&fetchResult.findings,
			"failed to reconcile remote observation findings",
		); err != nil {
			return nil, nil, err
		}
	}

	return projectedRemote.emitted, fetchResult.pending, nil
}

func (flow *engineFlow) fetchRemoteChanges(
	ctx context.Context,
	bl *Baseline,
	plan primaryRootObservationPlan,
) (remoteFetchResult, error) {
	return flow.executePrimaryRootObservation(ctx, bl, plan)
}

func (flow *engineFlow) commitObservedRemoteChanges(
	ctx context.Context,
	dryRun bool,
	observed []ObservedItem,
) error {
	if dryRun {
		return nil
	}

	if len(observed) == 0 {
		return nil
	}

	if err := flow.commitObservedItems(ctx, observed, ""); err != nil {
		return err
	}

	return nil
}

func (flow *engineFlow) observeLocalChanges(
	ctx context.Context,
	bl *Baseline,
) (ScanResult, error) {
	localResult, err := flow.observeLocal(ctx, bl)
	if err != nil {
		return ScanResult{}, err
	}

	if err := flow.reconcileSkippedObservationFindings(ctx, localResult.Skipped); err != nil {
		return ScanResult{}, err
	}

	return localResult, nil
}

func (flow *engineFlow) commitObservedLocalSnapshot(
	ctx context.Context,
	dryRun bool,
	localResult ScanResult,
) error {
	if dryRun {
		return nil
	}

	observedAt := flow.engine.nowFunc().UnixNano()
	rows := buildLocalStateRows(localResult)
	if err := flow.engine.baseline.ReplaceLocalState(ctx, rows); err != nil {
		return fmt.Errorf("sync: replacing local_state snapshot: %w", err)
	}
	mode := localRefreshModeWatchHealthy
	state, err := flow.engine.baseline.ReadObservationState(ctx)
	if err == nil && state != nil {
		mode = state.LocalRefreshMode
	}
	if err := flow.engine.baseline.MarkFullLocalRefresh(
		ctx,
		flow.engine.driveID,
		time.Unix(0, observedAt),
		mode,
	); err != nil {
		return fmt.Errorf("sync: marking full local refresh: %w", err)
	}

	return nil
}
