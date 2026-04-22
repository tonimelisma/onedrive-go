package sync

import (
	"context"
	"errors"
	"fmt"
)

func (r *oneShotRunner) observeDryRunCurrentState(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (*observedCurrentState, error) {
	observeStart := r.engine.nowFunc()
	planResult, err := r.buildDryRunCurrentActionPlan(ctx, bl, fullReconcile)
	if err != nil {
		return nil, err
	}
	observed := observedCurrentState{
		inputs:        planResult.currentActionPlanInputs,
		observedPaths: planResult.observedPaths,
	}
	r.engine.collector().RecordObserve(observed.observedPaths, r.engine.since(observeStart))

	return &observed, nil
}

func (flow *engineFlow) buildDryRunCurrentActionPlan(
	ctx context.Context,
	bl *Baseline,
	fullReconcile bool,
) (result *dryRunPlanInput, err error) {
	plan := flow.buildPrimaryRootObservationPlan(fullReconcile)
	fetchResult, err := flow.fetchRemoteChanges(ctx, bl, plan)
	if err != nil {
		return nil, err
	}

	projectedRemote := projectRemoteObservations(flow.engine.logger, fetchResult.events)

	localResult, err := flow.observeAndReconcileLocalTruth(ctx, bl)
	if err != nil {
		return nil, err
	}

	scratchStore, cleanup, err := flow.engine.baseline.createScratchPlanningStore(ctx, bl)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cleanupErr := cleanup(context.WithoutCancel(ctx)); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	commitErr := scratchStore.CommitObservation(ctx, projectedRemote.observed, "", flow.engine.driveID)
	if commitErr != nil {
		return nil, fmt.Errorf("sync: committing dry-run remote snapshot to scratch store: %w", commitErr)
	}
	if reconcileErr := scratchStore.ReconcileObservationFindings(ctx, &fetchResult.findings, flow.engine.nowFunc()); reconcileErr != nil {
		return nil, fmt.Errorf("sync: reconciling dry-run remote observation findings in scratch store: %w", reconcileErr)
	}

	localRows := buildLocalStateRows(localResult)
	replaceErr := scratchStore.ReplaceLocalState(ctx, localRows)
	if replaceErr != nil {
		return nil, fmt.Errorf("sync: replacing dry-run local snapshot in scratch store: %w", replaceErr)
	}

	inputs, err := flow.loadCurrentActionPlanInputs(ctx, scratchStore, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &dryRunPlanInput{
		currentActionPlanInputs: inputs,
		observedPaths:           len(inputs.localRows) + len(inputs.remoteRows),
	}, nil
}
