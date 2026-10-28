package sync

import "context"

func (rt *watchRuntime) handleWatchLocalObservationBatch(
	ctx context.Context,
	batch *localObservationBatch,
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

func (rt *watchRuntime) applyLocalObservationBatch(
	ctx context.Context,
	batch *localObservationBatch,
) (bool, error) {
	if batch.markSuspect {
		reason := batch.recoveryReason
		if reason == "" {
			reason = localTruthRecoveryWatcherError
		}
		if err := rt.deps.store.MarkLocalTruthSuspect(ctx, reason); err != nil {
			return false, err
		}
		rt.engine.collector().RecordLocalObservationSuspect(reason)
		return true, nil
	}

	if batch.fullSnapshot {
		// The store writes only the rows that differ, so an idle safety
		// scan costs no row writes (D-081a).
		changed, err := rt.deps.store.ReplaceLocalStateReportingChange(ctx, batch.rows)
		if err != nil {
			return false, err
		}
		rt.engine.collector().RecordLocalObservationFullSnapshotReplacement()
		return batch.dirty || changed, nil
	}

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
