package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func (o *Orchestrator) reconcileWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) {
	decisions, err := o.buildRuntimeWorkSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults, childWork)
	if err != nil {
		o.warnOnChange(reconcileWarningKey(""),
			"Couldn't update shared-folder shortcuts; the shortcuts already syncing keep syncing", err)
		return
	}
	o.clearWarnOnChange(reconcileWarningKey(""))

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, decisions, mode, opts, runnerEvents, childWork)
	o.emitStartWarning(startResults, startAnnounceFull)
	o.logReconcileOutcome("", started, stopped, len(runners), startResults)
}

func reconcileWarningKey(parentID mountID) string {
	return "reconcile:" + parentID.String()
}

// logReconcileOutcome stays at DEBUG for the common pass that changed
// nothing, which runs every reconcile tick.
func (o *Orchestrator) logReconcileOutcome(
	parentID mountID,
	started int,
	stopped int,
	active int,
	startResults []MountStartupResult,
) {
	attrs := []any{
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", active),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	}
	if parentID != "" {
		attrs = append(attrs, slog.String("parent_mount_id", parentID.String()))
	}
	if started == 0 && stopped == 0 {
		o.logger.Debug("drive set unchanged", attrs...)
		return
	}
	o.logger.Info("Updated the set of drives and shortcuts being synced", attrs...)
}

func (o *Orchestrator) applyWatchMountSet(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	decisions *runtimeWorkSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (int, int, []MountStartupResult) {
	runnable := runnableMountMap(decisions.Mounts)
	restartingParents := parentWatchRunnersNeedingStop(runners, runnable)
	removeChildRunnablesForParents(runnable, restartingParents)
	removeCleanupWorkForParents(decisions, restartingParents)
	stopped := o.stopInactiveWatchRunners(ctx, runners, runnable, childWork, restartingParents)
	parentArtifactAckers := watchParentArtifactCleanupAckers(runners)
	o.reportShortcutCleanup(o.purgeShortcutChildArtifactsForDecisions(ctx, decisions, parentArtifactAckers, childWork))
	started, startResults := o.startReloadWatchRunners(
		ctx,
		runners,
		runnable,
		decisions.Skipped,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	o.setControlMountIDs(sortedRunnerMountIDs(runners))

	return stopped, started, startResults
}

// reportShortcutCleanup warns about a failed removed-shortcut cleanup once
// per distinct failure. Cleanup is retried on every pass, and the failure is
// also kept in the status diagnostics, so repeating it every pass adds nothing.
func (o *Orchestrator) reportShortcutCleanup(err error) {
	const key = "shortcut-cleanup"
	if err == nil {
		o.clearWarnOnChange(key)
		return
	}
	o.warnOnChange(key, "Couldn't clean up local state for removed shortcuts; will retry", err)
}

func (o *Orchestrator) reconcileWatchRunnersForParent(
	ctx context.Context,
	parentID mountID,
	snapshot syncengine.ShortcutChildWorkSnapshot,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) {
	if parentID == "" {
		o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		return
	}
	parents, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
	if err != nil {
		o.warnParentReconcileFailure(parentID, err)
		return
	}
	var parent *mountSpec
	for i := range parents {
		if parents[i] != nil && parents[i].id() == parentID {
			parent = parents[i]
			break
		}
	}
	if parent == nil {
		stopped := o.stopChildWatchRunnersForParent(ctx, runners, parentID)
		childWork.forget(parentID)
		o.setControlMountIDs(sortedRunnerMountIDs(runners))
		o.logger.Debug("stopped shortcuts of a drive that is no longer selected",
			slog.String("parent_mount_id", parentID.String()),
			slog.Int("stopped", stopped),
		)
		return
	}
	publications := map[mountID]syncengine.ShortcutChildWorkSnapshot{
		parentID: syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot),
	}
	decisions, err := o.buildRuntimeWorkFromParentSnapshots([]*mountSpec{parent}, publications)
	if err != nil {
		o.warnParentReconcileFailure(parentID, err)
		return
	}
	o.clearWarnOnChange(reconcileWarningKey(parentID))
	stopped, started, startResults := o.applyWatchMountSetForParent(
		ctx,
		runners,
		parentID,
		decisions,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	o.emitStartWarning(startResults, startAnnouncePartial)
	o.logReconcileOutcome(parentID, started, stopped, len(runners), startResults)
}

func (o *Orchestrator) warnParentReconcileFailure(parentID mountID, err error) {
	o.warnOnChange(reconcileWarningKey(parentID),
		fmt.Sprintf("Couldn't update shared-folder shortcuts for %s; the shortcuts already syncing keep syncing",
			o.standaloneDisplayName(parentID)),
		err,
	)
}

// standaloneDisplayName names a configured drive by its mount ID for messages,
// falling back to the ID when the drive is not (or no longer) configured.
func (o *Orchestrator) standaloneDisplayName(id mountID) string {
	for i := range o.cfg.StandaloneMounts {
		mount := &o.cfg.StandaloneMounts[i]
		if mount.CanonicalID.String() == id.String() && mount.DisplayName != "" {
			return mount.DisplayName
		}
	}
	return id.String()
}

func (o *Orchestrator) applyWatchMountSetForParent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	parentID mountID,
	decisions *runtimeWorkSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (int, int, []MountStartupResult) {
	childMounts := filterMountSpecsByProjection(decisions.Mounts, MountProjectionChild)
	runnable := runnableMountMap(childMounts)
	stopped := o.stopInactiveChildWatchRunnersForParent(ctx, runners, parentID, runnable)
	parentArtifactAckers := watchParentArtifactCleanupAckers(runners)
	o.reportShortcutCleanup(o.purgeShortcutChildArtifactsForDecisions(ctx, decisions, parentArtifactAckers, childWork))
	started, startResults := o.startReloadWatchRunners(
		ctx,
		runners,
		runnable,
		decisions.Skipped,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	o.setControlMountIDs(sortedRunnerMountIDs(runners))
	return stopped, started, startResults
}

func runnableMountMap(mounts []*mountSpec) map[mountID]*mountSpec {
	active := make(map[mountID]*mountSpec)
	for i := range mounts {
		if mounts[i].paused() {
			continue
		}
		active[mounts[i].id()] = mounts[i]
	}
	return active
}

func (o *Orchestrator) startReloadWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
	initialStartup []MountStartupResult,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (int, []MountStartupResult) {
	started := 0
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for _, id := range sortedRunnableMountIDs(runnable) {
		mount := runnable[id]
		if _, ok := runners[id]; ok {
			continue
		}
		if o.runnerRestartPending(id) {
			o.logger.Debug("waiting before restarting drive",
				slog.String("mount_id", id.String()),
			)
			continue
		}

		o.attachParentChildWorkSink(mount, childWork, runnerEvents, nil)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, runnerEvents)
		if err != nil {
			// Reported through the startup result, once per distinct
			// failure, by emitStartWarning.
			o.logger.Debug("watch runner did not start",
				slog.String("mount_id", id.String()),
				slog.Any("error", err),
			)
			startResults = append(startResults, mountStartupResultForMount(mount, err))
			continue
		}

		startResults = append(startResults, MountStartupResult{
			SelectionIndex: mount.selectionIndex(),
			Identity:       mount.identity(),
			DisplayName:    mount.displayName(),
			Status:         MountStartupRunnable,
		})
		runners[id] = wr
		started++
	}

	return started, startResults
}

func sortedRunnableMountIDs(runnable map[mountID]*mountSpec) []mountID {
	ids := make([]mountID, 0, len(runnable))
	for id := range runnable {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := runnable[ids[i]]
		right := runnable[ids[j]]
		leftChild := left != nil && left.projectionKind() == MountProjectionChild
		rightChild := right != nil && right.projectionKind() == MountProjectionChild
		if leftChild != rightChild {
			return !leftChild
		}
		return ids[i].String() < ids[j].String()
	})
	return ids
}
