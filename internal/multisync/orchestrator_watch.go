package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	watchRunnerEventBuffer = 64
)

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on clean context cancel or a stop request, and an error when
// the control server stops on its own: a watch nobody can pause, reload or
// query must not keep running.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	o.watchReportMemory = newWatchReportState()
	commands := make(chan controlCommand)
	// fatal carries the end of a helper the watch cannot run without (the
	// control server). Buffered so the reporter never waits on the loop.
	fatal := make(chan error, 1)
	runnerEvents := make(chan watchRunnerEvent, watchRunnerEventBuffer)
	childWork := newParentChildWorkSnapshots()
	runners, control, err := o.startWatchRuntime(ctx, mode, opts, commands, fatal, runnerEvents, childWork)
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
	pauseExpiry := watchPauseExpiryWake{orchestrator: o}
	defer pauseExpiry.stop()

	reloadTriggers, stopReloadTriggers := watchConfigReloadTriggers(ctx, o.configPath(), configFilePollInterval)
	defer stopReloadTriggers()

	for {
		restarts.arm()
		pauseExpiry.arm()
		select {
		case cmd := <-commands:
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners, runnerEvents, childWork) {
				return nil
			}
		case reason := <-reloadTriggers:
			o.reloadForReason(ctx, reason, mode, opts, runners, runnerEvents, childWork)
		case <-pauseExpiry.ch:
			// The reload clears the expired pause from the config and
			// starts the drive, exactly as a `resume` would.
			o.reloadForReason(ctx, "timed pause expired", mode, opts, runners, runnerEvents, childWork)
		case event := <-runnerEvents:
			o.handleWatchRunnerEvent(ctx, event, mode, opts, runners, runnerEvents, childWork)
		case <-reconcileTickCh:
			o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case <-restarts.ch:
			o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case fatalErr := <-fatal:
			// The deferred drain still stops the runners and closes the
			// socket; the caller exits non-zero, so a service manager
			// restarts a watch it can reach again.
			return fatalErr
		case <-ctx.Done():
			return nil
		}
	}
}

// reloadForReason runs the config reload the loop owes to a trigger it did
// not ask a client about: a changed file or an expired timed pause. The
// reload already warned and kept the previous settings when it fails; with
// no client waiting there is nobody else to tell.
func (o *Orchestrator) reloadForReason(
	ctx context.Context, reason string, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner, runnerEvents chan<- watchRunnerEvent, childWork *parentChildWorkSnapshots,
) {
	o.logger.Info("Reloading the config (" + reason + ")")
	if err := o.reload(ctx, mode, opts, runners, runnerEvents, childWork); err != nil {
		o.logger.Debug("config reload failed ("+reason+")", slog.Any("error", err))
	}
}

func (o *Orchestrator) startWatchRuntime(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	commands chan controlCommand,
	fatal chan<- error,
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

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands, fatal)
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
	if len(runners) == 0 {
		o.logger.Info("All selected drives are paused; waiting until one is resumed",
			slog.Int("paused", startSummary.PausedCount()),
		)
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
		runners[mount.id()] = wr
	}
	o.setControlMountIDs(sortedRunnerMountIDs(runners))

	return runners, startResults
}

// validateInitialWatchStart decides whether a watch with no running drive
// starts at all. Every selected drive paused is not a failure: the watch
// starts, says so, and idles until the config reload (which notices a
// resume or an expired timed pause) or a stop; a service manager that
// restarted it on exit would restart it every interval until the resume
// (R-3.4.2). Drives that failed to start are a startup refusal, so the
// restart can try them again.
func validateInitialWatchStart(
	runners map[mountID]*watchRunner,
	startSummary StartupSelectionSummary,
) error {
	if len(runners) > 0 || startSummary.AllPaused() {
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
			slog.Any("error", closeErr),
		)
	}
}

// configPath is the config file a running watch reloads from.
func (o *Orchestrator) configPath() string {
	if o.cfg == nil || o.cfg.Holder == nil {
		return ""
	}

	return o.cfg.Holder.Path()
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
