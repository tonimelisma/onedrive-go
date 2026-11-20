package sync

import (
	"context"
	"fmt"
)

func (rt *watchRuntime) handleWatchLocalObservationBatch(
	ctx context.Context,
	batch *localObservation,
) error {
	if batch == nil {
		return nil
	}

	dirty, err := rt.applyLocalObservationBatch(ctx, batch)
	if err != nil {
		return err
	}
	if dirty && rt.resources.dirtyBuf != nil {
		rt.resources.dirtyBuf.MarkDirty()
	}

	return nil
}

// applyLocalObservationBatch commits one local observation and reports
// whether the runtime must replan.
func (rt *watchRuntime) applyLocalObservationBatch(
	ctx context.Context,
	batch *localObservation,
) (bool, error) {
	switch batch.kind {
	case localTruthSuspect:
		reason := batch.recoveryReason
		if reason == "" {
			reason = localTruthRecoveryWatcherError
		}
		if err := rt.deps.store.MarkLocalTruthSuspect(ctx, reason); err != nil {
			return false, err
		}
		rt.engine.collector().RecordLocalObservationSuspect(reason)
		return true, nil
	case localSnapshot:
		return rt.applyLocalSnapshot(ctx, batch)
	case localPatch:
		return rt.applyLocalPatch(ctx, batch)
	default:
		return false, fmt.Errorf("sync: applying local observation: unknown kind %d", batch.kind)
	}
}

// applyLocalSnapshot commits a watch-mode full scan exactly as the replan
// commits its own (commitLocalScan): rows, findings and crossed mounts in one
// transaction. It replans when the rows (or the truth-complete marker,
// R-2.8.17) or the persisted finding set changed, and a persistent finding
// every safety scan re-reports does not (R-2.8.21). The observer's mount flags
// are then set from what the store holds, so a disk mounted during the watch
// is recognized when it is unmounted.
func (rt *watchRuntime) applyLocalSnapshot(ctx context.Context, batch *localObservation) (bool, error) {
	// A scan observed before a shutdown is still committed: detached from
	// the cancellation, under the bound every detached store write has.
	commitCtx := ctx
	if ctx.Err() != nil {
		detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeCommitTimeout)
		defer cancel()
		commitCtx = detached
	}

	result, err := rt.commitLocalScan(commitCtx, batch.rows, batch.findings, batch.crossedMounts)
	if err != nil && isWatchShutdownError(ctx, err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rt.engine.collector().RecordLocalObservationFullSnapshotReplacement()
	if rt.resources.localObs != nil {
		rt.resources.localObs.SetMountBoundaryFlags(result.mountBoundaries)
	}

	return result.rowsChanged || result.findingsChanged, nil
}

func (rt *watchRuntime) applyLocalPatch(ctx context.Context, batch *localObservation) (bool, error) {
	changed := batch.dirty ||
		len(batch.rows) > 0 ||
		len(batch.deletedPaths) > 0 ||
		len(batch.deletedPrefixes) > 0
	if !changed {
		return false, nil
	}

	if err := rt.deps.store.applyLocalStatePatch(
		ctx,
		batch.rows,
		batch.deletedPaths,
		batch.deletedPrefixes,
	); err != nil {
		return false, err
	}
	rt.engine.collector().RecordLocalObservationScopedCommit(
		len(batch.rows),
		len(batch.deletedPaths),
		len(batch.deletedPrefixes),
	)

	return true, nil
}
