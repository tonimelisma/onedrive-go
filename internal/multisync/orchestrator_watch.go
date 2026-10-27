package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	parentAck shortcutChildAckHandle
	cancel    context.CancelFunc
	done      chan struct{} // closed exactly once by the goroutine started in startWatchRunner
	// startedAt tells a runner that failed straight away from one that ran
	// for a while first; see runnerHealthyRunDuration.
	startedAt time.Time
}

type watchRunnerEvent struct {
	mountID               mountID
	report                *syncengine.Report
	err                   error
	parentSnapshot        syncengine.ShortcutChildWorkSnapshot
	parentSnapshotChanged bool
}

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	o.watchReportMemory = newWatchReportState()
	commands := make(chan controlCommand)
	runnerEvents := make(chan watchRunnerEvent, watchRunnerEventBuffer)
	childWork := newParentChildWorkSnapshots()
	runners, control, err := o.startWatchRuntime(ctx, mode, opts, commands, runnerEvents, childWork)
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
		for _, wr := range runners {
			wr.cancel()
			<-wr.done
			o.closeEngine(ctx, wr.mount, wr.engine)
		}

		o.logger.Info("Stopped watching for changes")
	}()
	// Registered after the drain above so it runs first: the service manager
	// hears "stopping" before the runners take time to settle.
	defer o.emitWatchLifecycle(WatchStopping, nil, nil, nil)

	reconcileTickCh, stopReconcileTicks := o.reconcileTicks(reconcileWatchInterval(opts.PollInterval))
	defer stopReconcileTicks()

	restarts := watchRestartWake{orchestrator: o}
	defer restarts.stop()

	reloadTriggers, stopReloadTriggers := watchConfigReloadTriggers(ctx, o.configPath(), configFilePollInterval)
	defer stopReloadTriggers()

	for {
		restarts.arm()
		select {
		case cmd := <-commands:
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners, runnerEvents, childWork) {
				return nil
			}
		case reason := <-reloadTriggers:
			o.logger.Info("Reloading the config (" + reason + ")")
			if err := o.reload(ctx, mode, opts, runners, runnerEvents, childWork); err != nil {
				// reload already warned and kept the previous settings;
				// with no client waiting there is nobody else to tell.
				o.logger.Debug("config reload after file change failed", slog.String("error", err.Error()))
			}
		case event := <-runnerEvents:
			o.handleWatchRunnerEvent(ctx, event, mode, opts, runners, runnerEvents, childWork)
		case <-reconcileTickCh:
			o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case <-restarts.ch:
			o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case <-ctx.Done():
			return nil
		}
	}
}

// watchRestartWake is the RunWatch loop's single timer for the earliest
// backed-off runner restart. The loop re-arms it before every select, from
// the absolute restart times in watchReportState, so it never drifts and
// never runs when nothing is waiting.
type watchRestartWake struct {
	orchestrator *Orchestrator
	ch           <-chan time.Time
	cancel       func()
}

func (w *watchRestartWake) arm() {
	w.stop()
	delay, ok := w.orchestrator.nextRunnerRestartDelay()
	if !ok {
		return
	}
	timer := w.orchestrator.restartTimer
	if timer == nil {
		timer = newRestartTimer
	}
	w.ch, w.cancel = timer(delay)
}

func (w *watchRestartWake) stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.ch, w.cancel = nil, nil
}

func (o *Orchestrator) startWatchRuntime(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	commands chan controlCommand,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (map[mountID]*watchRunner, *controlSocketServer, error) {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return nil, nil, &WatchStartupError{
				Summary: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return nil, nil, fmt.Errorf("no drives selected for sync")
	}

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands)
	if err != nil {
		return nil, nil, err
	}

	decisions, err := o.buildStandaloneRuntimeWorkSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return nil, control, err
	}
	o.setControlMountIDs(mountIDsForSpecs(decisions.Mounts))

	o.logger.Info("Watching drives for changes",
		slog.Int("drives", len(decisions.Mounts)),
		slog.String("mode", mode.String()),
	)

	runners, startResults := o.startInitialWatchRunners(
		ctx,
		mode,
		decisions.Mounts,
		decisions.Skipped,
		opts,
		runnerEvents,
		childWork,
	)
	startSummary := summarizeStartupResults(startResults)
	if err := validateInitialWatchStart(runners, startSummary); err != nil {
		return nil, control, err
	}
	o.emitStartWarning(startResults, startAnnounceFull)
	o.emitWatchLifecycle(WatchReady, runners, startResults, nil)

	return runners, control, nil
}

func (o *Orchestrator) startInitialWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	initialStartup []MountStartupResult,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (map[mountID]*watchRunner, []MountStartupResult) {
	runners := make(map[mountID]*watchRunner)
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for i := range mounts {
		mount := mounts[i]
		if mount.paused() {
			o.logger.Info("Skipping paused drive",
				slog.String("drive", mount.label()),
			)
			startResults = append(startResults, MountStartupResult{
				SelectionIndex: mount.selectionIndex(),
				Identity:       mount.identity(),
				DisplayName:    mount.displayName(),
				Status:         MountStartupPaused,
			})

			continue
		}

		o.attachParentChildWorkSink(mount, childWork, runnerEvents, nil)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, runnerEvents)
		if err != nil {
			// The startup result is the user-facing report of this failure
			// (a StartWarning, or the returned WatchStartupError).
			o.logger.Debug("watch runner did not start",
				slog.String("mount_id", mount.id().String()),
				slog.String("error", err.Error()),
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
		runners[mount.id()] = wr
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

// closeControlSocket runs on every exit path, including shutdown. A failure
// here leaves at most a stale socket file, which the next sync detects as
// stale and removes, so it is not worth a warning.
func (o *Orchestrator) closeControlSocket(ctx context.Context, control *controlSocketServer) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCloseTimeout)
	defer cancel()
	if closeErr := control.Close(closeCtx); closeErr != nil {
		o.logger.Debug("close control socket",
			slog.String("error", closeErr.Error()),
		)
	}
}

// closeEngine closes one drive's engine. Cancellation is the normal shutdown
// path and stays at DEBUG; any other close failure means the drive's sync
// state may not have been written cleanly, which is worth one warning.
func (o *Orchestrator) closeEngine(ctx context.Context, mount *mountSpec, engine engineRunner) {
	if engine == nil {
		return
	}
	closeErr := engine.Close(ctx)
	if closeErr == nil {
		return
	}
	if errors.Is(closeErr, context.Canceled) || errors.Is(closeErr, context.DeadlineExceeded) {
		o.logger.Debug("close sync engine during shutdown",
			slog.String("mount_id", mount.id().String()),
			slog.String("error", closeErr.Error()),
		)
		return
	}
	o.logger.Warn(fmt.Sprintf("Couldn't close the sync state for %s cleanly", mount.label()),
		slog.String("error", closeErr.Error()),
	)
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
		// The caller reports this against the drive, so the text names only
		// what failed, not which drive.
		return nil, fmt.Errorf("connect to OneDrive: %w", err)
	}

	mountCollector := o.registerMountPerfCollector(mount.id().String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         mount,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: mountCollector,
	})
	if engineErr != nil {
		o.removeMountPerfCollector(mount.id().String())
		return nil, engineErr
	}

	mountCtx, mountCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	wr := &watchRunner{
		mount:     mount,
		engine:    engine,
		parentAck: shortcutParentAckHandleForMount(mount, engine),
		cancel:    mountCancel,
		done:      done,
		startedAt: o.clockNow(),
	}

	go func() {
		defer close(done)
		defer mountCancel()
		defer o.removeMountPerfCollector(mount.id().String())

		if mount.isFinalDrainChild() {
			report, drainErr := engine.RunOnce(mountCtx, syncengine.SyncBidirectional, syncengine.RunOptions{FullReconcile: true})
			// The main loop reports a failed drain once; this goroutine only
			// hands it over. When the orchestrator canceled the drain itself
			// (reload, parent snapshot change, shutdown), the error is just
			// that cancellation and nobody needs to hear about it.
			if drainErr != nil && mountCtx.Err() != nil {
				o.logger.Debug("final-drain runner stopped",
					slog.String("mount_id", mount.id().String()),
					slog.String("error", drainErr.Error()),
				)
				return
			}
			if runnerEvents != nil {
				select {
				case runnerEvents <- watchRunnerEvent{mountID: mount.id(), report: report, err: drainErr}:
				case <-ctx.Done():
				}
			}
			return
		}

		if watchErr := engine.RunWatch(mountCtx, mode, opts); watchErr != nil {
			if mountCtx.Err() == nil {
				// Reported once, in user words, by handleWatchRunnerEvent.
				o.logger.Debug("watch runner exited",
					slog.String("mount_id", mount.id().String()),
					slog.String("error", watchErr.Error()),
				)
				if runnerEvents != nil {
					select {
					case runnerEvents <- watchRunnerEvent{mountID: mount.id(), err: watchErr}:
					case <-ctx.Done():
					}
				}
			}
		}
	}()

	o.logger.Info("Started syncing drive",
		slog.String("drive", mount.label()),
		slog.String("mount_id", mount.id().String()),
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

// reload applies the config file to the running sync. It returns an error
// only when nothing could be applied (the config would not load or compile);
// the running drives then keep their previous settings, and the caller tells
// the client that asked for the reload. Drives that fail to start under the
// new config are not a reload failure: they are reported as startup results.
func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) error {
	o.emitWatchLifecycle(WatchReloading, nil, nil, nil)

	newCfg, newSelection, err := o.loadReloadSelection(ctx)
	if err != nil {
		o.logger.Warn("Couldn't reload the config; keeping the previous settings",
			slog.String("error", err.Error()),
		)
		o.emitWatchLifecycle(WatchReloaded, nil, nil, err)

		return err
	}

	oldCfg := o.cfg.Holder.Config()
	oldMounts := o.cfg.StandaloneMounts
	oldStartup := o.cfg.InitialStartupResults
	o.cfg.Holder.Update(newCfg)
	o.cfg.StandaloneMounts = newSelection.Mounts
	o.cfg.InitialStartupResults = newSelection.StartupResults
	o.cfg.Runtime.FlushTokenCache()

	newMounts, err := o.buildRuntimeWorkSet(ctx, newSelection.Mounts, newSelection.StartupResults, childWork)
	if err != nil {
		o.cfg.Holder.Update(oldCfg)
		o.cfg.StandaloneMounts = oldMounts
		o.cfg.InitialStartupResults = oldStartup
		o.cfg.Runtime.FlushTokenCache()
		o.logger.Warn("Couldn't reload the config; keeping the previous settings",
			slog.String("error", err.Error()),
		)
		o.emitWatchLifecycle(WatchReloaded, nil, nil, err)
		return err
	}
	// The user asked for the config to be applied now, so drives waiting out
	// a restart backoff get their next attempt immediately.
	o.releaseRunnerRestartWaits()
	stopped, started, startResults := o.applyWatchMountSet(
		ctx,
		runners,
		newMounts,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	o.emitStartWarning(startResults, startAnnounceFull)
	o.emitWatchLifecycle(WatchReloaded, runners, startResults, nil)

	o.logger.Info("Reloaded the config",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	)
	return nil
}

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

func (o *Orchestrator) handleWatchRunnerEvent(
	ctx context.Context,
	event watchRunnerEvent,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) {
	if event.parentSnapshotChanged {
		if !parentSnapshotEventIsCurrent(event, runners, childWork) {
			o.logger.Debug("ignoring stale parent child work snapshot event",
				slog.String("parent_mount_id", event.mountID.String()),
			)
			return
		}
		o.reconcileWatchRunnersForParent(ctx, event.mountID, event.parentSnapshot, mode, opts, runners, runnerEvents, childWork)
		return
	}

	wr := runners[event.mountID]
	if wr == nil {
		// The orchestrator already stopped this runner (reload, parent
		// change, or shutdown) and reconciled; the event was queued before
		// that and describes nothing current.
		o.logger.Debug("ignoring event from a runner that was already stopped",
			slog.String("mount_id", event.mountID.String()),
		)
		return
	}

	<-wr.done
	o.closeEngine(ctx, wr.mount, wr.engine)
	delete(runners, event.mountID)
	if wr.mount != nil && wr.mount.projectionKind() == MountProjectionStandalone {
		childWork.forget(event.mountID)
		o.stopChildWatchRunnersForParent(ctx, runners, event.mountID)
	}

	if wr.mount.isFinalDrainChild() {
		o.handleFinalDrainWatchRunnerEvent(ctx, runners, wr, event, childWork)
	}

	if event.err != nil && ctx.Err() == nil {
		o.reportWatchRunnerExit(wr, event.err)
	}

	o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
}

// reportWatchRunnerExit records a runner failure for restart backoff and tells
// the user once: the first time, and again only when the error changes. The
// restart itself happens from the RunWatch loop once the backoff has passed.
func (o *Orchestrator) reportWatchRunnerExit(wr *watchRunner, err error) {
	delay, isNew := o.recordRunnerFailure(wr.mount.id(), wr.startedAt, err)
	name := wr.mount.label()

	switch {
	case errors.Is(err, syncengine.ErrMountRootUnavailable):
		// The restart attempt fails at startup with the same cause, and that
		// startup failure is what gets announced.
		o.logger.Info(fmt.Sprintf("Stopped syncing %s: its sync folder is unavailable", name),
			slog.Any("error", err),
		)
	case isNew:
		// The error value, not its text: the CLI console shows a plain
		// reason at default verbosity, and the log file and --verbose keep
		// the full chain.
		o.logger.Warn(fmt.Sprintf("Stopped syncing %s; retrying in %s", name, humanDelay(delay)),
			slog.Any("error", err),
		)
	default:
		o.logger.Debug("drive stopped again with the same error",
			slog.String("mount_id", wr.mount.id().String()),
			slog.String("retry_in", delay.String()),
			slog.String("error", err.Error()),
		)
	}
}

func parentSnapshotEventIsCurrent(
	event watchRunnerEvent,
	runners map[mountID]*watchRunner,
	childWork *parentChildWorkSnapshots,
) bool {
	if event.mountID == "" || childWork == nil {
		return false
	}
	wr := runners[event.mountID]
	if wr == nil || wr.mount == nil || wr.mount.projectionKind() != MountProjectionStandalone {
		return false
	}
	eventSnapshot := syncengine.NormalizeShortcutChildWorkSnapshot(event.mountID.String(), event.parentSnapshot)
	return syncengine.ShortcutChildWorkSnapshotsEqual(childWork.latestFor(event.mountID), eventSnapshot)
}

func (o *Orchestrator) handleFinalDrainWatchRunnerEvent(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	wr *watchRunner,
	event watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) {
	decisions, err := o.buildRuntimeWorkSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults, childWork)
	if err != nil {
		o.logger.Warn(fmt.Sprintf("Couldn't finish removing shortcut %s; will retry", wr.mount.label()),
			slog.String("error", err.Error()),
		)
		return
	}
	report := &MountReport{
		SelectionIndex: wr.mount.selectionIndex(),
		Identity:       wr.mount.identity(),
		DisplayName:    wr.mount.displayName(),
		Report:         event.report,
		Err:            event.err,
	}
	finalized, err := o.finalizeSuccessfulFinalDrainMounts(
		ctx,
		decisions,
		[]*MountReport{report},
		watchParentDrainAckers(runners),
		childWork,
	)
	if err != nil {
		o.logger.Warn(fmt.Sprintf("Couldn't finish removing shortcut %s; will retry", wr.mount.label()),
			slog.String("error", err.Error()),
		)
		return
	}
	if finalized {
		o.logger.Info("Finished removing shortcut",
			slog.String("shortcut", wr.mount.label()),
			slog.String("mount_id", event.mountID.String()),
		)
	}
}

func watchParentDrainAckers(runners map[mountID]*watchRunner) map[mountID]shortcutChildAckHandle {
	ackers := make(map[mountID]shortcutChildAckHandle)
	for id, runner := range runners {
		if runner == nil || runner.mount == nil || runner.mount.projectionKind() != MountProjectionStandalone {
			continue
		}
		if shortcutChildAckHandleIsZero(runner.parentAck) {
			continue
		}
		ackers[id] = runner.parentAck
	}
	return ackers
}

func watchParentArtifactCleanupAckers(
	runners map[mountID]*watchRunner,
) map[mountID]shortcutChildAckHandle {
	ackers := make(map[mountID]shortcutChildAckHandle)
	for id, runner := range runners {
		if runner == nil || runner.mount == nil || runner.mount.projectionKind() != MountProjectionStandalone {
			continue
		}
		if shortcutChildAckHandleIsZero(runner.parentAck) {
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
				slog.String("error", err.Error()),
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
	return current.id() == next.id() &&
		current.childParentMountID() == next.childParentMountID() &&
		current.projectionKind() == next.projectionKind() &&
		current.parentDriveType() == next.parentDriveType() &&
		current.rejectSharePointRootForms() == next.rejectSharePointRootForms()
}

func mountSpecRemoteEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.remoteDriveID() == next.remoteDriveID() &&
		current.remoteRootItemID() == next.remoteRootItemID() &&
		current.tokenOwnerCanonical() == next.tokenOwnerCanonical() &&
		current.accountEmail() == next.accountEmail()
}

func mountSpecRuntimeEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.syncRoot() == next.syncRoot() &&
		current.statePath() == next.statePath() &&
		current.enableWebsocket() == next.enableWebsocket() &&
		current.remoteRootDeltaCapable() == next.remoteRootDeltaCapable() &&
		contentFilterConfigsEquivalent(current.contentFilter(), next.contentFilter()) &&
		childEngineSpecsEquivalent(current, next)
}

func childEngineSpecsEquivalent(current *mountSpec, next *mountSpec) bool {
	if current == nil || next == nil || current.child == nil || next.child == nil {
		return (current == nil || current.child == nil) && (next == nil || next.child == nil)
	}
	return syncengine.ShortcutChildEngineSpecsEqual(&current.child.engine, &next.child.engine)
}

func mountSpecTuningEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.transferWorkers() == next.transferWorkers() &&
		current.checkWorkers() == next.checkWorkers() &&
		current.minFreeSpace() == next.minFreeSpace()
}

func contentFilterConfigsEquivalent(current syncengine.ContentFilterConfig, next syncengine.ContentFilterConfig) bool {
	return slices.Equal(current.IgnoredDirs, next.IgnoredDirs) &&
		slices.Equal(current.IncludedDirs, next.IncludedDirs) &&
		slices.Equal(current.IgnoredPaths, next.IgnoredPaths) &&
		current.IgnoreDotfiles == next.IgnoreDotfiles &&
		current.IgnoreJunkFiles == next.IgnoreJunkFiles &&
		current.FollowSymlinks == next.FollowSymlinks &&
		current.CrossFilesystems == next.CrossFilesystems
}

// configPath is the config file a running watch reloads from.
func (o *Orchestrator) configPath() string {
	if o.cfg == nil || o.cfg.Holder == nil {
		return ""
	}

	return o.cfg.Holder.Path()
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
		ids = append(ids, mounts[i].id().String())
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
