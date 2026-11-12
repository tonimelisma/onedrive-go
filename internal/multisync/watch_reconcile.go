package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// reconcileWatchRunners brings the runner set in line with the configured
// drives and the cached parent snapshots. It returns an error only when a
// runner it had to stop missed its bound (ErrSyncStuck); a set it
// could not compute is warned about and leaves the runners as they are.
func (o *Orchestrator) reconcileWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) error {
	decisions, err := o.buildRuntimeWorkSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults, childWork)
	if err != nil {
		o.warnOnChange(reconcileWarningKey(""),
			"Couldn't update shared-folder shortcuts; the shortcuts already syncing keep syncing", err)
		return nil
	}
	o.clearWarnOnChange(reconcileWarningKey(""))

	stopped, started, startResults, err := o.applyWatchMountSet(ctx, runners, decisions, mode, opts, runnerEvents, childWork)
	if err != nil {
		return err
	}
	o.emitStartWarning(startResults, startAnnounceFull)
	o.logReconcileOutcome("", started, stopped, len(runners), startResults)

	return nil
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

// applyWatchMountSet stops the runners decisions no longer runs as they are,
// cleans up removed shortcuts, and starts what is missing. A runner that
// misses its stop bound ends the pass before anything starts, with an error
// wrapping ErrSyncStuck.
func (o *Orchestrator) applyWatchMountSet(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	decisions *runtimeWorkSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (int, int, []MountStartupResult, error) {
	runnable := runnableMountMap(decisions.Mounts)
	restartingParents := parentWatchRunnersNeedingStop(runners, runnable)
	removeChildRunnablesForParents(runnable, restartingParents)
	removeCleanupWorkForParents(decisions, restartingParents)
	stopped, err := o.stopInactiveWatchRunners(ctx, runners, runnable, childWork, restartingParents)
	if err != nil {
		o.setControlMountIDs(sortedRunnerMountIDs(runners))
		return stopped, 0, nil, err
	}
	parentArtifactAckers := watchParentAckers(runners)
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

	return stopped, started, startResults, nil
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

// reconcileWatchRunnersForParent applies one parent's new child-work snapshot
// to that parent's shortcut children. Like reconcileWatchRunners, it returns
// an error only when a child it had to stop missed its bound.
func (o *Orchestrator) reconcileWatchRunnersForParent(
	ctx context.Context,
	parentID mountID,
	snapshot syncengine.ShortcutChildWorkSnapshot,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) error {
	if parentID == "" {
		return o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
	}
	parents, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
	if err != nil {
		o.warnParentReconcileFailure(parentID, err)
		return nil
	}
	var parent *mountSpec
	for i := range parents {
		if parents[i] != nil && parents[i].id() == parentID {
			parent = parents[i]
			break
		}
	}
	if parent == nil {
		stopped, stopErr := o.stopChildWatchRunnersForParent(ctx, runners, parentID)
		o.setControlMountIDs(sortedRunnerMountIDs(runners))
		if stopErr != nil {
			return stopErr
		}
		childWork.forget(parentID)
		o.logger.Debug("stopped shortcuts of a drive that is no longer selected",
			slog.String("parent_mount_id", parentID.String()),
			slog.Int("stopped", stopped),
		)
		return nil
	}
	publications := map[mountID]syncengine.ShortcutChildWorkSnapshot{
		parentID: syncengine.NormalizeShortcutChildWorkSnapshot(parentID.String(), snapshot),
	}
	decisions, err := o.buildRuntimeWorkFromParentSnapshots([]*mountSpec{parent}, publications)
	if err != nil {
		o.warnParentReconcileFailure(parentID, err)
		return nil
	}
	o.clearWarnOnChange(reconcileWarningKey(parentID))
	stopped, started, startResults, err := o.applyWatchMountSetForParent(
		ctx,
		runners,
		parentID,
		decisions,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	if err != nil {
		return err
	}
	o.emitStartWarning(startResults, startAnnouncePartial)
	o.logReconcileOutcome(parentID, started, stopped, len(runners), startResults)

	return nil
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
) (int, int, []MountStartupResult, error) {
	childMounts := filterMountSpecsByProjection(decisions.Mounts, MountProjectionChild)
	runnable := runnableMountMap(childMounts)
	stopped, err := o.stopInactiveChildWatchRunnersForParent(ctx, runners, parentID, runnable)
	if err != nil {
		o.setControlMountIDs(sortedRunnerMountIDs(runners))
		return stopped, 0, nil, err
	}
	parentArtifactAckers := watchParentAckers(runners)
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
	return stopped, started, startResults, nil
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

		wr, err := o.startNextWatchRunner(ctx, mount, mode, opts, runnerEvents, childWork)
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
