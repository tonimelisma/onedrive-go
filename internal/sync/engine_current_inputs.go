package sync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

func (flow *engineFlow) loadObservedCurrentState(
	ctx context.Context,
	pendingCursorCommit *pendingPrimaryCursorCommit,
) (*observedCurrentState, error) {
	inputs, err := flow.loadCurrentActionPlanInputs(ctx, flow.engine.baseline, flow.engine.driveID)
	if err != nil {
		return nil, err
	}

	return &observedCurrentState{
		inputs:              inputs,
		observedPaths:       len(inputs.localRows) + len(inputs.remoteRows),
		pendingCursorCommit: pendingCursorCommit,
	}, nil
}

func (flow *engineFlow) loadCurrentActionPlanInputs(
	ctx context.Context,
	store *SyncStore,
	defaultDriveID driveid.ID,
) (currentActionPlanInputs, error) {
	tx, err := beginPerfTx(ctx, store.db)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: beginning current action planner read transaction: %w", err)
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			flow.engine.logger.Debug("current action planner read transaction rollback failed",
				slog.String("error", rollbackErr.Error()),
			)
		}
	}()

	return flow.loadCurrentActionPlanInputsTx(ctx, store, tx, defaultDriveID)
}

func (flow *engineFlow) loadCurrentActionPlanInputsTx(
	ctx context.Context,
	store *SyncStore,
	tx sqlTxRunner,
	defaultDriveID driveid.ID,
) (currentActionPlanInputs, error) {
	comparisons, err := queryComparisonStateWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: querying comparison state: %w", err)
	}
	reconciliations, err := queryReconciliationStateWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: querying reconciliation state: %w", err)
	}
	localRows, err := listLocalStateRows(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing local_state rows: %w", err)
	}
	observationState, err := store.readObservationStateTx(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: reading observation state for remote_state: %w", err)
	}
	configuredDriveID := observationState.ConfiguredDriveID
	if configuredDriveID.IsZero() {
		configuredDriveID = defaultDriveID
	}
	remoteRows, err := queryRemoteStateRowsWithRunner(
		ctx,
		tx,
		`SELECT `+sqlSelectRemoteStateCols+` FROM remote_state`,
		configuredDriveID,
	)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing remote_state rows: %w", err)
	}
	observationIssues, err := queryObservationIssueRowsWithRunner(ctx, tx)
	if err != nil {
		return currentActionPlanInputs{}, fmt.Errorf("sync: listing observation issues: %w", err)
	}
	return currentActionPlanInputs{
		comparisons:       comparisons,
		reconciliations:   reconciliations,
		localRows:         localRows,
		remoteRows:        remoteRows,
		observationIssues: observationIssues,
	}, nil
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

	localResult, err := flow.observeLocalChanges(ctx, nil, bl)
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
