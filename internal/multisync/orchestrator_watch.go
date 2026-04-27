package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	watchRunnerEventBuffer = 64
)

// watchRunner holds per-mount state for a running watch-mode engine.
type watchRunner struct {
	mount     *mountSpec
	engine    engineRunner
	parentAck syncengine.ShortcutChildAckHandle
	cancel    context.CancelFunc
	done      chan struct{} // closed exactly once by the goroutine started in startWatchRunner
}

type watchRunnerEvent struct {
	mountID                  mountID
	report                   *syncengine.Report
	err                      error
	parentPublicationChanged bool
}

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	commands := make(chan controlCommand)
	runnerEvents := make(chan watchRunnerEvent, watchRunnerEventBuffer)
	runners, control, err := o.startWatchRuntime(ctx, mode, opts, commands, runnerEvents)
	if err != nil {
		if control != nil {
			o.closeControlSocket(ctx, control)
		}
		return err
	}
	if control != nil {
		defer o.closeControlSocket(ctx, control)
	}

	defer func() {
		for id, wr := range runners {
			wr.cancel()
			<-wr.done

			if closeErr := wr.engine.Close(ctx); closeErr != nil {
				o.logger.Warn("engine close error on shutdown",
					slog.String("mount_id", id.String()),
					slog.String("error", closeErr.Error()),
				)
			}
		}

		o.logger.Info("orchestrator RunWatch stopped")
	}()

	reconcileTickCh, stopReconcileTicks := o.reconcileTicks(reconcileWatchInterval(opts.PollInterval))
	defer stopReconcileTicks()

	for {
		select {
		case cmd := <-commands:
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners, runnerEvents) {
				return nil
			}
		case event := <-runnerEvents:
			o.handleWatchRunnerEvent(ctx, event, mode, opts, runners, runnerEvents)
		case <-reconcileTickCh:
			o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents)
		case <-ctx.Done():
			return nil
		}
	}
}

func (o *Orchestrator) startWatchRuntime(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	commands chan controlCommand,
	runnerEvents chan<- watchRunnerEvent,
) (map[mountID]*watchRunner, *controlSocketServer, error) {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return nil, nil, &WatchStartupError{
				Summary: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return nil, nil, fmt.Errorf("sync: no standalone mounts configured")
	}

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands)
	if err != nil {
		return nil, nil, err
	}

	decisions, err := o.buildRunnerDecisionSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return nil, control, fmt.Errorf("sync: building mount specs: %w", err)
	}
	o.setControlMountIDs(mountIDsForSpecs(decisions.Mounts))

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("mounts", len(decisions.Mounts)),
		slog.String("mode", mode.String()),
	)

	runners, startResults := o.startInitialWatchRunners(
		ctx,
		mode,
		decisions.Mounts,
		decisions.Skipped,
		opts,
		runnerEvents,
	)
	startSummary := summarizeStartupResults(startResults)
	if err := validateInitialWatchStart(runners, startSummary); err != nil {
		return nil, control, err
	}
	if startSummary.SelectedCount() > 0 {
		o.emitStartWarning(startSummary)
	}

	return runners, control, nil
}

func (o *Orchestrator) startInitialWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	initialStartup []MountStartupResult,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
) (map[mountID]*watchRunner, []MountStartupResult) {
	runners := make(map[mountID]*watchRunner)
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for i := range mounts {
		mount := mounts[i]
		if mount.paused {
			o.logger.Info("skipping paused mount",
				slog.String("mount_id", mount.mountID.String()),
			)
			startResults = append(startResults, MountStartupResult{
				SelectionIndex: mount.selectionIndex,
				Identity:       mount.identity(),
				DisplayName:    mount.displayName,
				Status:         MountStartupPaused,
			})

			continue
		}

		o.attachParentRunnerPublicationSink(mount, runnerEvents, nil)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, runnerEvents)
		if err != nil {
			o.logger.Error("failed to start watch runner",
				slog.String("mount_id", mount.mountID.String()),
				slog.String("error", err.Error()),
			)
			startResults = append(startResults, mountStartupResultForMount(mount, err))

			continue
		}

		startResults = append(startResults, MountStartupResult{
			SelectionIndex: mount.selectionIndex,
			Identity:       mount.identity(),
			DisplayName:    mount.displayName,
			Status:         MountStartupRunnable,
		})
		runners[mount.mountID] = wr
	}
	o.setControlMountIDs(sortedRunnerMountIDs(runners))

	return runners, startResults
}

func validateInitialWatchStart(
	runners map[mountID]*watchRunner,
	startSummary StartupSelectionSummary,
) error {
	if len(runners) > 0 {
		return nil
	}

	return &WatchStartupError{Summary: startSummary}
}

func (o *Orchestrator) emitStartWarning(summary StartupSelectionSummary) {
	failures := summary.SkippedResults()
	if len(failures) == 0 || o == nil || o.cfg == nil || o.cfg.StartWarning == nil {
		return
	}

	o.cfg.StartWarning(StartupWarning{Summary: summarizeStartupResults(failures)})
}

func (o *Orchestrator) closeControlSocket(ctx context.Context, control *controlSocketServer) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCloseTimeout)
	defer cancel()
	if closeErr := control.Close(closeCtx); closeErr != nil {
		o.logger.Warn("control socket close error",
			slog.String("error", closeErr.Error()),
		)
	}
}

func (o *Orchestrator) startWatchRunner(
	ctx context.Context,
	mount *mountSpec,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
) (*watchRunner, error) {
	session, err := o.cfg.Runtime.SyncSession(ctx, mount.syncSessionConfig())
	if err != nil {
		return nil, fmt.Errorf("session error for mount %s: %w", mount.label(), err)
	}

	mountCollector := o.registerMountPerfCollector(mount.mountID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         mount,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: mountCollector,
	})
	if engineErr != nil {
		o.removeMountPerfCollector(mount.mountID.String())
		return nil, fmt.Errorf("engine creation failed for mount %s: %w", mount.label(), engineErr)
	}

	mountCtx, mountCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	wr := &watchRunner{
		mount:     mount,
		engine:    engine,
		parentAck: shortcutParentAckHandleForMount(mount, engine),
		cancel:    mountCancel,
		done:      done,
	}

	go func() {
		defer close(done)
		defer mountCancel()
		defer o.removeMountPerfCollector(mount.mountID.String())

		if mount.finalDrain {
			report, drainErr := engine.RunOnce(mountCtx, syncengine.SyncBidirectional, syncengine.RunOptions{FullReconcile: true})
			if drainErr != nil && mountCtx.Err() == nil {
				o.logger.Error("final-drain watch runner exited with error",
					slog.String("mount_id", mount.mountID.String()),
					slog.String("error", drainErr.Error()),
				)
			}
			if runnerEvents != nil {
				select {
				case runnerEvents <- watchRunnerEvent{mountID: mount.mountID, report: report, err: drainErr}:
				case <-ctx.Done():
				}
			}
			return
		}

		if watchErr := engine.RunWatch(mountCtx, mode, opts); watchErr != nil {
			if mountCtx.Err() == nil {
				o.logger.Error("watch runner exited with error",
					slog.String("mount_id", mount.mountID.String()),
					slog.String("error", watchErr.Error()),
				)
				if runnerEvents != nil {
					select {
					case runnerEvents <- watchRunnerEvent{mountID: mount.mountID, err: watchErr}:
					case <-ctx.Done():
					}
				}
			}
		}
	}()

	o.logger.Info("watch runner started",
		slog.String("mount_id", mount.mountID.String()),
	)

	return wr, nil
}

func (o *Orchestrator) registerMountPerfCollector(mountID string) *perf.Collector {
	if o == nil || o.perfRuntime == nil {
		return nil
	}

	return o.perfRuntime.RegisterMount(mountID)
}

func (o *Orchestrator) removeMountPerfCollector(mountID string) {
	if o == nil || o.perfRuntime == nil {
		return
	}

	o.perfRuntime.RemoveMount(mountID)
}

func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
) {
	newCfg, newSelection, err := o.loadReloadSelection(ctx)
	if err != nil {
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", err.Error()),
		)

		return
	}

	oldCfg := o.cfg.Holder.Config()
	oldMounts := o.cfg.StandaloneMounts
	oldStartup := o.cfg.InitialStartupResults
	o.cfg.Holder.Update(newCfg)
	o.cfg.StandaloneMounts = newSelection.Mounts
	o.cfg.InitialStartupResults = newSelection.StartupResults
	o.cfg.Runtime.FlushTokenCache()

	newMounts, err := o.buildRunnerDecisionSet(ctx, newSelection.Mounts, newSelection.StartupResults)
	if err != nil {
		o.cfg.Holder.Update(oldCfg)
		o.cfg.StandaloneMounts = oldMounts
		o.cfg.InitialStartupResults = oldStartup
		o.cfg.Runtime.FlushTokenCache()
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", fmt.Errorf("building mount specs after reload: %w", err).Error()),
		)
		return
	}
	stopped, started, startResults := o.applyWatchMountSet(
		ctx,
		runners,
		newMounts,
		mode,
		opts,
		runnerEvents,
	)
	if len(startResults) > 0 {
		o.emitStartWarning(summarizeStartupResults(startResults))
	}

	o.logger.Info("config reload complete",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	)
}

func (o *Orchestrator) reconcileWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
) {
	decisions, err := o.buildRunnerDecisionSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		o.logger.Warn("shortcut runner reconciliation refresh failed, keeping current runners",
			slog.String("error", err.Error()),
		)
		return
	}

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, decisions, mode, opts, runnerEvents)
	if len(startResults) > 0 {
		o.emitStartWarning(summarizeStartupResults(startResults))
	}
	o.logger.Info("shortcut runner reconciliation refresh complete",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	)
}

func (o *Orchestrator) handleWatchRunnerEvent(
	ctx context.Context,
	event watchRunnerEvent,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
) {
	if event.parentPublicationChanged {
		o.reconcileWatchRunnersForParent(ctx, event.mountID, mode, opts, runners, runnerEvents)
		return
	}

	wr := runners[event.mountID]
	if wr != nil {
		<-wr.done
		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error after watch runner exit",
				slog.String("mount_id", event.mountID.String()),
				slog.String("error", closeErr.Error()),
			)
		}
		delete(runners, event.mountID)
		if wr.mount != nil && wr.mount.projectionKind == MountProjectionStandalone {
			o.forgetParentRunnerPublication(event.mountID)
			o.stopChildWatchRunnersForParent(ctx, runners, event.mountID)
		}
	}

	if wr != nil && wr.mount.finalDrain {
		o.handleFinalDrainWatchRunnerEvent(ctx, runners, wr, event)
	}

	if event.err != nil && errors.Is(event.err, syncengine.ErrMountRootUnavailable) {
		o.logger.Info("watch runner stopped because mount root is unavailable",
			slog.String("mount_id", event.mountID.String()),
			slog.String("error", event.err.Error()),
		)
	} else if event.err != nil {
		o.logger.Warn("watch runner stopped; reconciling mount set",
			slog.String("mount_id", event.mountID.String()),
			slog.String("error", event.err.Error()),
		)
	}

	o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents)
}

func (o *Orchestrator) handleFinalDrainWatchRunnerEvent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	wr *watchRunner,
	event watchRunnerEvent,
) {
	decisions, err := o.buildRunnerDecisionsFromParentPublications(o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		o.logger.Warn("compiling mount set after final-drain child completion failed",
			slog.String("mount_id", event.mountID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	report := &MountReport{
		SelectionIndex: wr.mount.selectionIndex,
		Identity:       wr.mount.identity(),
		DisplayName:    wr.mount.displayName,
		Report:         event.report,
		Err:            event.err,
	}
	finalized, err := o.finalizeSuccessfulFinalDrainMounts(
		ctx,
		decisions,
		[]*MountReport{report},
		watchParentDrainAckers(runners),
	)
	if err != nil {
		o.logger.Warn("finalizing drained shortcut child mount after watch completion failed",
			slog.String("mount_id", event.mountID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	if finalized {
		o.logger.Info("finalized drained shortcut child mount",
			slog.String("mount_id", event.mountID.String()),
		)
	}
}

func watchParentDrainAckers(runners map[mountID]*watchRunner) map[mountID]syncengine.ShortcutChildAckHandle {
	ackers := make(map[mountID]syncengine.ShortcutChildAckHandle)
	for id, runner := range runners {
		if runner == nil || runner.mount == nil || runner.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		if runner.parentAck.IsZero() {
			continue
		}
		ackers[id] = runner.parentAck
	}
	return ackers
}

func watchParentArtifactCleanupAckers(
	runners map[mountID]*watchRunner,
) map[mountID]syncengine.ShortcutChildAckHandle {
	ackers := make(map[mountID]syncengine.ShortcutChildAckHandle)
	for id, runner := range runners {
		if runner == nil || runner.mount == nil || runner.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		if runner.parentAck.IsZero() {
			continue
		}
		ackers[id] = runner.parentAck
	}
	return ackers
}

func (o *Orchestrator) loadReloadSelection(
	ctx context.Context,
) (*config.Config, StandaloneMountSelection, error) {
	if ctx == nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("reload context is required")
	}

	newCfg, err := config.LoadOrDefault(o.cfg.Holder.Path(), o.logger)
	if err != nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("loading config for reload: %w", err)
	}

	config.ClearExpiredPauses(o.cfg.Holder.Path(), newCfg, time.Now(), o.logger)

	if o.cfg.ReloadStandaloneMounts == nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("standalone mount reload compiler is required")
	}
	newSelection, err := o.cfg.ReloadStandaloneMounts(newCfg)
	if err != nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("compiling standalone mounts after reload: %w", err)
	}

	return newCfg, newSelection, nil
}

func (o *Orchestrator) applyWatchMountSet(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	decisions *runnerDecisionSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
) (int, int, []MountStartupResult) {
	runnable := runnableMountMap(decisions.Mounts)
	stopped := o.stopInactiveWatchRunners(ctx, runners, runnable)
	parentArtifactAckers := watchParentArtifactCleanupAckers(runners)
	if purgeErr := o.purgeShortcutChildArtifactsForDecisions(ctx, decisions, parentArtifactAckers); purgeErr != nil {
		o.logger.Warn("purging shortcut child state artifacts",
			slog.String("error", purgeErr.Error()),
		)
	}
	started, startResults := o.startReloadWatchRunners(
		ctx,
		runners,
		runnable,
		decisions.Skipped,
		mode,
		opts,
		runnerEvents,
	)
	o.setControlMountIDs(sortedRunnerMountIDs(runners))

	return stopped, started, startResults
}

func (o *Orchestrator) reconcileWatchRunnersForParent(
	ctx context.Context,
	parentID mountID,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
) {
	if parentID == "" {
		o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents)
		return
	}
	parents, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
	if err != nil {
		o.logger.Warn("parent-scoped shortcut runner reconciliation failed, keeping current runners",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	var parent *mountSpec
	for i := range parents {
		if parents[i] != nil && parents[i].mountID == parentID {
			parent = parents[i]
			break
		}
	}
	if parent == nil {
		stopped := o.stopChildWatchRunnersForParent(ctx, runners, parentID)
		o.forgetParentRunnerPublication(parentID)
		o.setControlMountIDs(sortedRunnerMountIDs(runners))
		o.logger.Info("parent-scoped shortcut runner reconciliation removed missing parent children",
			slog.String("parent_mount_id", parentID.String()),
			slog.Int("stopped", stopped),
		)
		return
	}
	publications := map[mountID]syncengine.ShortcutChildRunnerPublication{
		parentID: o.latestParentRunnerPublicationFor(parentID),
	}
	decisions, err := buildRunnerDecisionsForParents([]*mountSpec{parent}, publications, o.logger)
	if err != nil {
		o.logger.Warn("parent-scoped shortcut runner reconciliation failed, keeping current runners",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", err.Error()),
		)
		return
	}
	stopped, started, startResults := o.applyWatchMountSetForParent(
		ctx,
		runners,
		parentID,
		decisions,
		mode,
		opts,
		runnerEvents,
	)
	if len(startResults) > 0 {
		o.emitStartWarning(summarizeStartupResults(startResults))
	}
	o.logger.Info("parent-scoped shortcut runner reconciliation complete",
		slog.String("parent_mount_id", parentID.String()),
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	)
}

func (o *Orchestrator) applyWatchMountSetForParent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	parentID mountID,
	decisions *runnerDecisionSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
) (int, int, []MountStartupResult) {
	childMounts := filterMountSpecsByProjection(decisions.Mounts, MountProjectionChild)
	runnable := runnableMountMap(childMounts)
	stopped := o.stopInactiveChildWatchRunnersForParent(ctx, runners, parentID, runnable)
	parentArtifactAckers := watchParentArtifactCleanupAckers(runners)
	if purgeErr := o.purgeShortcutChildArtifactsForDecisions(ctx, decisions, parentArtifactAckers); purgeErr != nil {
		o.logger.Warn("purging shortcut child state artifacts",
			slog.String("error", purgeErr.Error()),
		)
	}
	started, startResults := o.startReloadWatchRunners(
		ctx,
		runners,
		runnable,
		decisions.Skipped,
		mode,
		opts,
		runnerEvents,
	)
	o.setControlMountIDs(sortedRunnerMountIDs(runners))
	return stopped, started, startResults
}

func runnableMountMap(mounts []*mountSpec) map[mountID]*mountSpec {
	active := make(map[mountID]*mountSpec)
	for i := range mounts {
		if mounts[i].paused {
			continue
		}
		active[mounts[i].mountID] = mounts[i]
	}
	return active
}

func (o *Orchestrator) stopInactiveWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
) int {
	stopped := 0
	for _, id := range stopOrderForWatchRunners(runners) {
		wr := runners[id]
		if next, ok := runnable[id]; ok {
			if mountSpecsEquivalentForWatchRestart(wr.mount, next) {
				wr.mount = next
				continue
			}
		}

		o.logger.Info("stopping watch runner for removed/paused mount",
			slog.String("mount_id", id.String()),
		)

		wr.cancel()
		<-wr.done

		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error during reload",
				slog.String("mount_id", id.String()),
				slog.String("error", closeErr.Error()),
			)
		}

		delete(runners, id)
		stopped++
	}

	return stopped
}

func stopOrderForWatchRunners(runners map[mountID]*watchRunner) []mountID {
	ids := make([]mountID, 0, len(runners))
	for id := range runners {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		left := runners[ids[i]]
		right := runners[ids[j]]
		leftChild := left != nil && left.mount != nil && left.mount.projectionKind == MountProjectionChild
		rightChild := right != nil && right.mount != nil && right.mount.projectionKind == MountProjectionChild
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
		if wr == nil || wr.mount == nil || wr.mount.projectionKind != MountProjectionChild {
			continue
		}
		if wr.mount.parentMountID != parentID {
			continue
		}
		o.logger.Info("stopping child watch runner because parent stopped",
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		wr.cancel()
		<-wr.done
		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error while stopping child after parent stop",
				slog.String("mount_id", id.String()),
				slog.String("error", closeErr.Error()),
			)
		}
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
		if wr == nil || wr.mount == nil || wr.mount.projectionKind != MountProjectionChild {
			continue
		}
		if wr.mount.parentMountID != parentID {
			continue
		}
		if next, ok := runnable[id]; ok {
			if mountSpecsEquivalentForWatchRestart(wr.mount, next) {
				wr.mount = next
				continue
			}
		}

		o.logger.Info("stopping child watch runner for parent publication change",
			slog.String("mount_id", id.String()),
			slog.String("parent_mount_id", parentID.String()),
		)
		wr.cancel()
		<-wr.done
		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error while stopping child after parent publication",
				slog.String("mount_id", id.String()),
				slog.String("error", closeErr.Error()),
			)
		}
		delete(runners, id)
		stopped++
	}
	return stopped
}

func (o *Orchestrator) startReloadWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
	initialStartup []MountStartupResult,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
) (int, []MountStartupResult) {
	started := 0
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for _, id := range sortedRunnableMountIDs(runnable) {
		mount := runnable[id]
		if _, ok := runners[id]; ok {
			continue
		}

		o.attachParentRunnerPublicationSink(mount, runnerEvents, nil)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, runnerEvents)
		if err != nil {
			o.logger.Error("failed to start watch runner during reload",
				slog.String("mount_id", id.String()),
				slog.String("error", err.Error()),
			)
			startResults = append(startResults, mountStartupResultForMount(mount, err))
			continue
		}

		startResults = append(startResults, MountStartupResult{
			SelectionIndex: mount.selectionIndex,
			Identity:       mount.identity(),
			DisplayName:    mount.displayName,
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
		leftChild := left != nil && left.projectionKind == MountProjectionChild
		rightChild := right != nil && right.projectionKind == MountProjectionChild
		if leftChild != rightChild {
			return !leftChild
		}
		return ids[i].String() < ids[j].String()
	})
	return ids
}

func mountSpecsEquivalentForWatchRestart(current *mountSpec, next *mountSpec) bool {
	if current == nil || next == nil {
		return current == next
	}
	return mountSpecCoreEquivalent(current, next)
}

func mountSpecCoreEquivalent(current *mountSpec, next *mountSpec) bool {
	return mountSpecIdentityEquivalent(current, next) &&
		mountSpecRemoteEquivalent(current, next) &&
		mountSpecRuntimeEquivalent(current, next) &&
		mountSpecTuningEquivalent(current, next)
}

func mountSpecIdentityEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.mountID == next.mountID &&
		current.parentMountID == next.parentMountID &&
		current.projectionKind == next.projectionKind &&
		current.driveType == next.driveType &&
		current.rejectSharePointRootForms == next.rejectSharePointRootForms
}

func mountSpecRemoteEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.remoteDriveID == next.remoteDriveID &&
		current.remoteRootItemID == next.remoteRootItemID &&
		current.tokenOwnerCanonical == next.tokenOwnerCanonical &&
		current.accountEmail == next.accountEmail
}

func mountSpecRuntimeEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.syncRoot == next.syncRoot &&
		current.statePath == next.statePath &&
		current.enableWebsocket == next.enableWebsocket &&
		current.remoteRootDeltaCapable == next.remoteRootDeltaCapable &&
		mountRootIdentitiesEqual(current.expectedSyncRootIdentity, next.expectedSyncRootIdentity)
}

func mountRootIdentitiesEqual(
	current *syncengine.ShortcutRootIdentity,
	next *syncengine.ShortcutRootIdentity,
) bool {
	if current == nil || next == nil {
		return current == nil && next == nil
	}
	return syncengine.SameShortcutRootIdentity(*current, *next)
}

func mountSpecTuningEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.transferWorkers == next.transferWorkers &&
		current.checkWorkers == next.checkWorkers &&
		current.minFreeSpace == next.minFreeSpace
}

func reconcileWatchInterval(pollInterval time.Duration) time.Duration {
	const defaultReconcileInterval = 5 * time.Minute

	if pollInterval <= 0 {
		return defaultReconcileInterval
	}
	if pollInterval < syncengine.MinPollInterval {
		return syncengine.MinPollInterval
	}

	return pollInterval
}

func mountIDsForSpecs(mounts []*mountSpec) []string {
	ids := make([]string, 0, len(mounts))
	for i := range mounts {
		if mounts[i] == nil {
			continue
		}
		ids = append(ids, mounts[i].mountID.String())
	}

	return ids
}

func sortedRunnerMountIDs(runners map[mountID]*watchRunner) []string {
	ids := make([]string, 0, len(runners))
	for id := range runners {
		ids = append(ids, id.String())
	}
	sort.Strings(ids)
	return ids
}

func (o *Orchestrator) setControlMountIDs(ids []string) {
	if o == nil {
		return
	}

	o.statusMu.Lock()
	o.controlMounts = append([]string(nil), ids...)
	o.statusMu.Unlock()
}

func (o *Orchestrator) controlMountIDs() []string {
	if o == nil {
		return nil
	}

	o.statusMu.RLock()
	defer o.statusMu.RUnlock()

	return append([]string(nil), o.controlMounts...)
}
