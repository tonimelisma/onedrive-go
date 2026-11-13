package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const (
	watchRunnerEventBuffer = 64
)

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on clean context cancel or a stop request. It returns an error
// when a helper the watch cannot run without ends (the control server or the
// config file watcher: a watch nobody can pause, reload or query must not
// keep running), when a drive's sync did not stop within its bound
// (ErrSyncStuck), whether a reload, a runner exit or the shutdown itself
// asked it to, and when the loop itself panicked (panicguard.ErrPanic).
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) (err error) {
	// Registered first so it runs last, after the drain and the socket close
	// below: a panic on the loop leaves its maps and its report memory of
	// unknown consistency, so the watch does not carry on. The drives stop
	// first, the panic is logged once with its stack and returned, the CLI
	// exits 1, and a service manager starts a fresh process (R-6.3.10).
	defer func() {
		if r := recover(); r != nil {
			err = errors.Join(panicguard.Recovered(o.logger, "in the watch loop", r), err)
		}
	}()

	o.watchReportMemory = newWatchReportState()
	commands := make(chan controlCommand)
	// fatal carries the end of a helper the watch cannot run without (the
	// control server, the config file watcher). Buffered so the reporter
	// never waits on the loop.
	fatal := make(chan error, 1)
	runnerEvents := make(chan watchRunnerEvent, watchRunnerEventBuffer)
	childWork := newParentChildWorkSnapshots()

	// The drain and the socket close are registered before anything starts,
	// so whatever ends the watch, a failed or panicking startup included,
	// stops the drives that started and then closes the socket. Closed after
	// the drain, so the owner lock is held until every drive has stopped: an
	// offline state change or another sync never sees a drive still writing
	// its store as unowned. startWatchRuntime fills runners and control in
	// place as it starts them.
	runners := make(map[mountID]*watchRunner)
	var control *controlSocketServer
	watching := false
	defer func() { o.closeControlSocket(ctx, control) }()
	defer func() {
		if drainErr := o.drainWatch(ctx, runners, watching); drainErr != nil {
			err = errors.Join(err, drainErr)
		}
	}()

	startResults, err := o.startWatchRuntime(ctx, mode, opts, commands, fatal, runnerEvents, childWork, runners, &control)
	if err != nil {
		return err
	}
	watching = true
	// Registered after the drain above so it runs first: the service manager
	// hears "stopping" before the runners take time to settle.
	defer o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchStopping}, nil, nil)
	o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchReady}, runners, startResults)

	reconcileTickCh, stopReconcileTicks := o.reconcileTicks(reconcileWatchInterval(opts.PollInterval))
	defer stopReconcileTicks()

	restarts := watchRestartWake{orchestrator: o}
	defer restarts.stop()
	pauseExpiry := watchPauseExpiryWake{orchestrator: o}
	defer pauseExpiry.stop()

	reloadTriggers, stopReloadTriggers := watchConfigReloadTriggers(ctx, configReloadWatch{
		path:     o.configPath(),
		interval: configFilePollInterval,
		logger:   o.logger,
		fatal:    fatal,
	})
	defer stopReloadTriggers()

	for {
		restarts.arm()
		pauseExpiry.arm()
		// stuckErr is set when a runner this pass had to stop missed its
		// bound; the watch ends with it, and the deferred drain leaves that
		// runner alone.
		var stuckErr error
		select {
		case cmd := <-commands:
			stopRequested, cmdErr := o.handleControlCommand(ctx, &cmd, mode, opts, runners, runnerEvents, childWork)
			if stopRequested {
				return nil
			}
			stuckErr = cmdErr
		case reason := <-reloadTriggers:
			stuckErr = o.reloadForReason(ctx, reason, mode, opts, runners, runnerEvents, childWork)
		case <-pauseExpiry.ch:
			// The reload clears the expired pause from the config and
			// starts the drive, exactly as a `resume` would.
			stuckErr = o.reloadForReason(ctx, "timed pause expired", mode, opts, runners, runnerEvents, childWork)
		case event := <-runnerEvents:
			stuckErr = o.handleWatchRunnerEvent(ctx, event, mode, opts, runners, runnerEvents, childWork)
		case <-reconcileTickCh:
			stuckErr = o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case <-restarts.ch:
			stuckErr = o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
		case fatalErr := <-fatal:
			// The deferred drain still stops the runners and closes the
			// socket; the caller exits non-zero, so a service manager
			// restarts a watch it can reach again.
			return fatalErr
		case <-ctx.Done():
			return nil
		}
		if stuckErr != nil {
			return stuckErr
		}
	}
}

// drainWatch stops every runner the watch started, and says the watch
// stopped when it got as far as watching: a start that failed ends quietly.
// It returns the errors of the runners that did not stop within their bound.
func (o *Orchestrator) drainWatch(ctx context.Context, runners map[mountID]*watchRunner, watching bool) error {
	drainErr := o.retireAllWatchRunners(ctx, runners)
	if watching {
		o.logger.Info("Stopped watching for changes")
	}

	return drainErr
}

// reloadForReason runs the config reload the loop owes to a trigger it did
// not ask a client about: a changed file or an expired timed pause. A reload
// that could not apply the config already warned and kept the previous
// settings; with no client waiting there is nobody else to tell. A drive
// that did not stop is returned, and ends the watch.
func (o *Orchestrator) reloadForReason(
	ctx context.Context, reason string, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner, runnerEvents chan<- watchRunnerEvent, childWork *parentChildWorkSnapshots,
) error {
	o.logger.Info("Reloading the config (" + reason + ")")
	err := o.reload(ctx, mode, opts, runners, runnerEvents, childWork)
	if errors.Is(err, ErrSyncStuck) {
		return err
	}
	if err != nil {
		o.logger.Debug("config reload failed ("+reason+")", slog.Any("error", err))
	}

	return nil
}

// startWatchRuntime binds the control socket and starts the initial runners,
// and returns the start results the ready event reports. It sets *control
// once the socket is bound and adds each runner to runners as it starts, so
// the caller, which closes the one and drains the other on every exit, stops
// exactly what started, also when the start fails or panics part way.
func (o *Orchestrator) startWatchRuntime(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	commands chan controlCommand,
	fatal chan<- error,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
	runners map[mountID]*watchRunner,
	control **controlSocketServer,
) ([]MountStartupResult, error) {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return nil, &WatchStartupError{
				Summary: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return nil, fmt.Errorf("no drives selected for sync")
	}

	server, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands, fatal)
	if err != nil {
		return nil, err
	}
	*control = server

	decisions, err := o.buildStandaloneRuntimeWorkSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return nil, err
	}
	o.setControlMountIDs(mountIDsForSpecs(decisions.Mounts))

	o.logger.Info("Watching drives for changes",
		slog.Int("drives", len(decisions.Mounts)),
		slog.String("mode", mode.String()),
	)

	startResults := o.startInitialWatchRunners(
		ctx,
		mode,
		decisions.Mounts,
		decisions.Skipped,
		opts,
		runnerEvents,
		childWork,
		runners,
	)
	startSummary := summarizeStartupResults(startResults)
	if err := validateInitialWatchStart(runners, startSummary); err != nil {
		return nil, err
	}
	if len(runners) == 0 {
		o.logger.Info("All selected drives are paused; waiting until one is resumed",
			slog.Int("paused", startSummary.PausedCount()),
		)
	}
	o.emitStartWarning(startResults, startAnnounceFull)

	return startResults, nil
}

// startInitialWatchRunners starts a runner for every mount that is not paused
// and adds it to runners, and returns every mount's start result.
func (o *Orchestrator) startInitialWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	initialStartup []MountStartupResult,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
	runners map[mountID]*watchRunner,
) []MountStartupResult {
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

		wr, err := o.startNextWatchRunner(ctx, mount, mode, opts, runnerEvents, childWork)
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

	return startResults
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
