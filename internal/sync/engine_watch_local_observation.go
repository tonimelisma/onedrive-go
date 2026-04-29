package sync

import (
	"context"
	"slices"
	"strings"
)

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
	if dirty && rt.dirtyBuf != nil {
		rt.dirtyBuf.MarkDirty()
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
			reason = LocalTruthRecoveryWatcherError
		}
		if err := rt.engine.baseline.MarkLocalTruthSuspect(ctx, reason); err != nil {
			return false, err
		}
		return true, nil
	}

	if batch.fullSnapshot {
		currentRows, err := rt.engine.baseline.ListLocalState(ctx)
		if err != nil {
			return false, err
		}
		changed := batch.dirty || !sameLocalStateRows(currentRows, batch.rows)
		if err := rt.engine.baseline.ReplaceLocalState(ctx, batch.rows); err != nil {
			return false, err
		}
		return changed, nil
	}

	changed := batch.dirty ||
		len(batch.rows) > 0 ||
		len(batch.deletedPaths) > 0 ||
		len(batch.deletedPrefixes) > 0
	if !changed {
		return false, nil
	}

	if err := rt.engine.baseline.applyLocalStatePatch(
		ctx,
		batch.rows,
		batch.deletedPaths,
		batch.deletedPrefixes,
	); err != nil {
		return false, err
	}

	return true, nil
}

func sameLocalStateRows(a []LocalStateRow, b []LocalStateRow) bool {
	if len(a) != len(b) {
		return false
	}

	left := append([]LocalStateRow(nil), a...)
	right := append([]LocalStateRow(nil), b...)
	slices.SortFunc(left, compareLocalStateRowsByPath)
	slices.SortFunc(right, compareLocalStateRowsByPath)
	return slices.Equal(left, right)
}

func compareLocalStateRowsByPath(a LocalStateRow, b LocalStateRow) int {
	return strings.Compare(a.Path, b.Path)
}
