package multisync

import (
	"context"
	"log/slog"
	"sort"
)

func (o *Orchestrator) stopInactiveWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
	childWork *parentChildWorkSnapshots,
	restartingParents map[mountID]struct{},
) int {
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

		o.logger.Info("Stopped syncing drive",
			slog.String("drive", wr.mount.label()),
			slog.String("mount_id", id.String()),
		)

		wr.cancel()
		<-wr.done
		o.closeEngine(ctx, wr.mount, wr.engine)

		delete(runners, id)
		if _, stillSelected := runnable[id]; !stillSelected {
			o.forgetRunnerRestart(id)
		}
		if wr.mount.projectionKind() == MountProjectionStandalone {
			childWork.forget(id)
		}
		stopped++
	}

	return stopped
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
) int {
	stopped := 0
	for _, id := range stopOrderForWatchRunners(runners) {
		wr := runners[id]
		if wr == nil || wr.mount == nil || wr.mount.projectionKind() != MountProjectionChild {
			continue
		}
		if wr.mount.childParentMountID() != parentID {
			continue
		}
		o.logger.Info("Stopped syncing shortcut because its drive stopped",
			slog.String("shortcut", wr.mount.label()),
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		wr.cancel()
		<-wr.done
		o.closeEngine(ctx, wr.mount, wr.engine)
		delete(runners, id)
		stopped++
	}
	return stopped
}

func (o *Orchestrator) stopInactiveChildWatchRunnersForParent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	parentID mountID,
	runnable map[mountID]*mountSpec,
) int {
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

		o.logger.Info("Stopped syncing shortcut after it changed on OneDrive",
			slog.String("shortcut", wr.mount.label()),
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		wr.cancel()
		<-wr.done
		o.closeEngine(ctx, wr.mount, wr.engine)
		delete(runners, id)
		stopped++
	}
	return stopped
}
