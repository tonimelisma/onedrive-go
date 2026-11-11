package multisync

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	// watchRunnerStopSlack is how much longer than one detached store wait
	// of a canceled engine a stop may take before it counts as stuck: the
	// rest of the engine's drain and the runner's deferred cleanup, each
	// milliseconds in a healthy engine.
	watchRunnerStopSlack = 15 * time.Second
	// watchRunnerStopTimeout bounds the wait for one canceled runner to
	// return, measured from its cancel. What a canceled engine still waits
	// for is its detached store writes (the commit of work that finished as
	// the cancel landed, its retry-work clear, the findings it had observed),
	// each bounded by syncengine.OutcomeCommitTimeout and normally done in
	// milliseconds. The bound covers one such write running to its limit
	// plus the slack; a store slow enough to need two is wedged, and ending
	// the watch is then the right outcome. A bound at or below one write's
	// limit would call a slow clean stop stuck.
	watchRunnerStopTimeout = syncengine.OutcomeCommitTimeout + watchRunnerStopSlack
)

// nextWatchRunnerGeneration names the next watch runner. Only the RunWatch
// goroutine starts watch runners, so the counter needs no lock.
func (o *Orchestrator) nextWatchRunnerGeneration() uint64 {
	o.lastRunnerGeneration++
	return o.lastRunnerGeneration
}

// retireWatchRunner is the one way a watch runner stops, whatever asked for
// it: a reload, a drive removed or paused, a shortcut child retired or
// replaced, a runner that exited on its own, or the end of the watch. It
// cancels the runner, waits for its goroutine, closes its engine and removes
// it from runners, and both waits are bounded.
//
// A runner that misses a bound fails closed. It is marked stuck and stays in
// runners, so nothing is started under its mount ID, and its engine, which
// may still be writing its store under the sync-folder lock, is never closed
// under it. Every goroutine's stack is logged once at Error, and the returned
// error wraps ErrSyncStuck: the caller ends the watch with it. The
// stuck runner's goroutine, or an engine close that ran over, is left to the
// process exit that follows. Only the RunWatch goroutine calls this.
func (o *Orchestrator) retireWatchRunner(ctx context.Context, runners map[mountID]*watchRunner, id mountID) error {
	wr := runners[id]
	if wr == nil {
		return nil
	}
	if wr.stuckErr != nil {
		return wr.stuckErr
	}

	o.requestWatchRunnerStop(wr)
	if !o.waitWithin(wr.done, watchRunnerStopTimeout-o.clockNow().Sub(wr.stopRequestedAt)) {
		return o.markWatchRunnerStuck(wr, "waited "+humanDelay(watchRunnerStopTimeout))
	}
	if !o.closeEngineWithin(ctx, wr.mount, wr.engine) {
		return o.markWatchRunnerStuck(wr, engineCloseStuckDetail())
	}

	delete(runners, id)
	return nil
}

// requestWatchRunnerStop cancels wr once. The stop bound runs from that
// cancel, so runners canceled together share one deadline and a watch that
// ends waits for all of them at most once.
func (o *Orchestrator) requestWatchRunnerStop(wr *watchRunner) {
	if !wr.stopRequestedAt.IsZero() {
		return
	}
	wr.stopRequestedAt = o.clockNow()
	wr.cancel()
}

// markWatchRunnerStuck records that wr missed a bound and returns the error
// the watch ends with (reportStuckSync).
func (o *Orchestrator) markWatchRunnerStuck(wr *watchRunner, detail string) error {
	wr.stuckErr = o.reportStuckSync(wr.mount, detail)
	return wr.stuckErr
}

// retireAllWatchRunners stops every runner when the watch ends. All of them
// are canceled first, so they stop in parallel under one shared bound; each
// is then retired in stop order, shortcut children before the drives that
// own them. A runner already marked stuck is left alone: the error that
// marked it is already the watch's result. The result joins the errors of
// runners that got stuck here.
func (o *Orchestrator) retireAllWatchRunners(ctx context.Context, runners map[mountID]*watchRunner) error {
	order := stopOrderForWatchRunners(runners)
	for _, id := range order {
		if wr := runners[id]; wr != nil && wr.stuckErr == nil {
			o.requestWatchRunnerStop(wr)
		}
	}

	var stuck []error
	for _, id := range order {
		if wr := runners[id]; wr == nil || wr.stuckErr != nil {
			continue
		}
		if err := o.retireWatchRunner(ctx, runners, id); err != nil {
			stuck = append(stuck, err)
		}
	}

	return errors.Join(stuck...)
}

func (o *Orchestrator) stopInactiveWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
	childWork *parentChildWorkSnapshots,
	restartingParents map[mountID]struct{},
) (int, error) {
	stopped := 0
	for _, id := range stopOrderForWatchRunners(runners) {
		wr := runners[id]
		if wr == nil || wr.mount == nil {
			continue
		}
		parentRestarting := false
		if wr.mount.projectionKind() == MountProjectionChild {
			_, parentRestarting = restartingParents[wr.mount.childParentMountID()]
		}
		if next, ok := runnable[id]; ok && !parentRestarting {
			if mountSpecsEquivalentForWatchRestart(wr.mount, next) {
				wr.mount = next
				continue
			}
		}

		if err := o.retireWatchRunner(ctx, runners, id); err != nil {
			return stopped, err
		}
		o.logger.Info("Stopped syncing drive",
			slog.String("drive", wr.mount.label()),
			slog.String("mount_id", id.String()),
		)

		if _, stillSelected := runnable[id]; !stillSelected {
			o.forgetRunnerRestart(id)
		}
		if wr.mount.projectionKind() == MountProjectionStandalone {
			childWork.forget(id)
		}
		stopped++
	}

	return stopped, nil
}

func removeChildRunnablesForParents(
	runnable map[mountID]*mountSpec,
	parentIDs map[mountID]struct{},
) {
	if len(parentIDs) == 0 {
		return
	}
	for id, mount := range runnable {
		if mount == nil || mount.projectionKind() != MountProjectionChild {
			continue
		}
		if _, ok := parentIDs[mount.childParentMountID()]; ok {
			delete(runnable, id)
		}
	}
}

func removeCleanupWorkForParents(
	decisions *runtimeWorkSet,
	parentIDs map[mountID]struct{},
) {
	if decisions == nil || len(parentIDs) == 0 || len(decisions.CleanupChildren) == 0 {
		return
	}
	kept := decisions.CleanupChildren[:0]
	for _, cleanup := range decisions.CleanupChildren {
		if _, ok := parentIDs[mountID(cleanup.namespaceID)]; ok {
			continue
		}
		kept = append(kept, cleanup)
	}
	decisions.CleanupChildren = kept
}

func parentWatchRunnersNeedingStop(
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
) map[mountID]struct{} {
	parents := make(map[mountID]struct{})
	for id, wr := range runners {
		if wr == nil || wr.mount == nil || wr.mount.projectionKind() != MountProjectionStandalone {
			continue
		}
		next, ok := runnable[id]
		if !ok || !mountSpecsEquivalentForWatchRestart(wr.mount, next) {
			parents[id] = struct{}{}
		}
	}
	return parents
}

func stopOrderForWatchRunners(runners map[mountID]*watchRunner) []mountID {
	ids := make([]mountID, 0, len(runners))
	for id := range runners {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := runners[ids[i]]
		right := runners[ids[j]]
		leftChild := left != nil && left.mount != nil && left.mount.projectionKind() == MountProjectionChild
		rightChild := right != nil && right.mount != nil && right.mount.projectionKind() == MountProjectionChild
		if leftChild != rightChild {
			return leftChild
		}
		return ids[i].String() < ids[j].String()
	})
	return ids
}

func (o *Orchestrator) stopChildWatchRunnersForParent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	parentID mountID,
) (int, error) {
	stopped := 0
	for _, id := range stopOrderForWatchRunners(runners) {
		wr := runners[id]
		if wr == nil || wr.mount == nil || wr.mount.projectionKind() != MountProjectionChild {
			continue
		}
		if wr.mount.childParentMountID() != parentID {
			continue
		}
		if err := o.retireWatchRunner(ctx, runners, id); err != nil {
			return stopped, err
		}
		o.logger.Info("Stopped syncing shortcut because its drive stopped",
			slog.String("shortcut", wr.mount.label()),
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		stopped++
	}
	return stopped, nil
}

func (o *Orchestrator) stopInactiveChildWatchRunnersForParent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	parentID mountID,
	runnable map[mountID]*mountSpec,
) (int, error) {
	stopped := 0
	for _, id := range stopOrderForWatchRunners(runners) {
		wr := runners[id]
		if wr == nil || wr.mount == nil || wr.mount.projectionKind() != MountProjectionChild {
			continue
		}
		if wr.mount.childParentMountID() != parentID {
			continue
		}
		if next, ok := runnable[id]; ok {
			if mountSpecsEquivalentForWatchRestart(wr.mount, next) {
				wr.mount = next
				continue
			}
		}

		if err := o.retireWatchRunner(ctx, runners, id); err != nil {
			return stopped, err
		}
		o.logger.Info("Stopped syncing shortcut after it changed on OneDrive",
			slog.String("shortcut", wr.mount.label()),
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		stopped++
	}
	return stopped, nil
}
