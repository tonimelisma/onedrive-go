package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func (rt *watchRuntime) handleWatchLocalObservationBatch(
	ctx context.Context,
	batch *syncobserve.LocalObservation,
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
	batch *syncobserve.LocalObservation,
) (bool, error) {
	switch batch.Kind {
	case syncobserve.LocalTruthSuspect:
		reason := batch.RecoveryReason
		if reason == "" {
			reason = syncdomain.LocalTruthRecoveryWatcherError
		}
		if err := rt.deps.store.MarkLocalTruthSuspect(ctx, reason); err != nil {
			return false, err
		}
		rt.deps.perf.RecordLocalObservationSuspect(reason)
		return true, nil
	case syncobserve.LocalSnapshot:
		return rt.applyLocalSnapshot(ctx, batch)
	case syncobserve.LocalPatch:
		return rt.applyLocalPatch(ctx, batch)
	default:
		return false, fmt.Errorf("sync: applying local observation: unknown kind %d", batch.Kind)
	}
}

// applyLocalSnapshot commits a watch-mode full scan exactly as the replan
// commits its own (commitLocalScan): rows, findings and crossed mounts in one
// transaction. It replans when the rows (or the truth-complete marker,
// R-2.8.17) or the persisted finding set changed, and a persistent finding
// every safety scan re-reports does not (R-2.8.21). The observer's mount flags
// are then set from what the store holds, so a disk mounted during the watch
// is recognized when it is unmounted.
func (rt *watchRuntime) applyLocalSnapshot(ctx context.Context, batch *syncobserve.LocalObservation) (bool, error) {
	// A scan observed before a shutdown is still committed: detached from
	// the cancellation, under the stop's one deadline (runStoppedStoreAccess).
	// A commit that deadline cuts short is said once and costs nothing but
	// the scan: the next sync scans the tree again, so the watch ends as the
	// stop asked, not with a new error.
	var (
		result syncstore.LocalSnapshotCommitResult
		err    error
	)
	if ctx.Err() != nil {
		err = rt.runStoppedStoreAccess(ctx, func(commitCtx context.Context) error {
			var commitErr error
			result, commitErr = rt.commitLocalScan(commitCtx, batch.Rows, batch.Findings, batch.CrossedMounts)
			return commitErr
		})
		if errors.Is(err, syncdomain.ErrStoreAccessTimedOut) {
			rt.deps.logger.Warn("The stop's time ran out before the last local scan was recorded; the next sync scans again",
				slog.Int("rows", len(batch.Rows)),
				slog.Any("error", err),
			)
			return false, nil
		}
	} else {
		result, err = rt.commitLocalScan(ctx, batch.Rows, batch.Findings, batch.CrossedMounts)
	}
	if err != nil && isWatchShutdownError(ctx, err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	rt.deps.perf.RecordLocalObservationFullSnapshotReplacement()
	if rt.resources.localObs != nil {
		rt.resources.localObs.ReplaceMountBoundaryFlags(result.MountBoundaries)
	}

	return result.RowsChanged || result.FindingsChanged, nil
}

func (rt *watchRuntime) applyLocalPatch(ctx context.Context, batch *syncobserve.LocalObservation) (bool, error) {
	changed := batch.Dirty ||
		len(batch.Rows) > 0 ||
		len(batch.DeletedPrefixes) > 0
	if !changed {
		return false, nil
	}

	if err := rt.deps.store.ApplyLocalStatePatch(
		ctx,
		batch.Rows,
		batch.DeletedPrefixes,
	); err != nil {
		return false, err
	}
	rt.deps.perf.RecordLocalObservationScopedCommit(
		len(batch.Rows),
		len(batch.DeletedPrefixes),
	)

	return true, nil
}
