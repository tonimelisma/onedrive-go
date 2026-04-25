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
	managedRootEventBuffer = 64
)

// watchRunner holds per-mount state for a running watch-mode engine.
type watchRunner struct {
	mount  *mountSpec
	engine engineRunner
	cancel context.CancelFunc
	done   chan struct{} // closed exactly once by the goroutine started in startWatchRunner
}

type watchRunnerEvent struct {
	mountID mountID
	report  *syncengine.Report
	err     error
}

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	commands := make(chan controlCommand)
	managedRootEvents := make(chan syncengine.ManagedRootEvent, managedRootEventBuffer)
	runnerEvents := make(chan watchRunnerEvent, watchRunnerEventBuffer)
	runners, control, err := o.startWatchRuntime(ctx, mode, opts, commands, managedRootEvents, runnerEvents)
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
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners, managedRootEvents, runnerEvents) {
				return nil
			}
		case event := <-managedRootEvents:
			o.logger.Info("managed shortcut root event received",
				slog.String("type", string(event.Type)),
				slog.String("path", event.Path),
				slog.String("reserved_path", event.ReservedPath),
				slog.String("mount_id", event.MountID),
			)
			o.reconcileWatchRunners(ctx, mode, opts, runners, managedRootEvents, runnerEvents)
		case event := <-runnerEvents:
			o.handleWatchRunnerEvent(ctx, event, mode, opts, runners, managedRootEvents, runnerEvents)
		case <-reconcileTickCh:
			o.reconcileWatchRunners(ctx, mode, opts, runners, managedRootEvents, runnerEvents)
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
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

	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return nil, control, fmt.Errorf("sync: building mount specs: %w", err)
	}
	compiled, err = o.finalizeRuntimeMountSetLifecycle(
		ctx,
		compiled,
		o.cfg.StandaloneMounts,
		o.cfg.InitialStartupResults,
		"watch startup",
		true,
	)
	if err != nil {
		return nil, control, fmt.Errorf("sync: finalizing mount lifecycle: %w", err)
	}
	compiled, err = o.preflightShortcutTopology(
		ctx,
		compiled,
		o.cfg.StandaloneMounts,
		o.cfg.InitialStartupResults,
		"watch startup shortcut topology preflight",
	)
	if err != nil {
		return nil, control, fmt.Errorf("sync: preflighting shortcut topology: %w", err)
	}
	o.setControlMountIDs(mountIDsForSpecs(compiled.Mounts))

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("mounts", len(compiled.Mounts)),
		slog.String("mode", mode.String()),
	)

	runners, startResults := o.startInitialWatchRunners(ctx, mode, compiled.Mounts, compiled.Skipped, opts, managedRootEvents, runnerEvents)
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
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

		o.attachShortcutTopologyHandler(mount, true)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, managedRootEvents, runnerEvents)
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
	runnerEvents chan<- watchRunnerEvent,
) (*watchRunner, error) {
	session, err := o.cfg.Runtime.SyncSession(ctx, mount.syncSessionConfig())
	if err != nil {
		return nil, fmt.Errorf("session error for mount %s: %w", mount.label(), err)
	}

	mountCollector := o.registerMountPerfCollector(mount.mountID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:           session,
		Mount:             mount,
		Logger:            o.logger,
		VerifyDrive:       true,
		PerfCollector:     mountCollector,
		ManagedRootEvents: o.managedRootEventSink(ctx, managedRootEvents),
	})
	if engineErr != nil {
		o.removeMountPerfCollector(mount.mountID.String())
		return nil, fmt.Errorf("engine creation failed for mount %s: %w", mount.label(), engineErr)
	}

	mountCtx, mountCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	wr := &watchRunner{
		mount:  mount,
		engine: engine,
		cancel: mountCancel,
		done:   done,
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

func (o *Orchestrator) managedRootEventSink(
	ctx context.Context,
	events chan<- syncengine.ManagedRootEvent,
) syncengine.ManagedRootEventSink {
	if events == nil {
		return nil
	}

	return func(event syncengine.ManagedRootEvent) {
		select {
		case events <- event:
		case <-ctx.Done():
		default:
			o.logger.Warn("managed shortcut root event channel full; periodic reconciliation will catch up",
				slog.String("path", event.Path),
				slog.String("mount_id", event.MountID),
			)
		}
	}
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
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

	newMounts, err := o.buildRuntimeMountSet(ctx, newSelection.Mounts, newSelection.StartupResults)
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
	newMounts, err = o.preflightShortcutTopology(
		ctx,
		newMounts,
		newSelection.Mounts,
		newSelection.StartupResults,
		"watch reload shortcut topology preflight",
	)
	if err != nil {
		o.cfg.Holder.Update(oldCfg)
		o.cfg.StandaloneMounts = oldMounts
		o.cfg.InitialStartupResults = oldStartup
		o.cfg.Runtime.FlushTokenCache()
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", fmt.Errorf("preflighting shortcut topology after reload: %w", err).Error()),
		)
		return
	}

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, newMounts, mode, opts, managedRootEvents, runnerEvents)
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
	runnerEvents chan<- watchRunnerEvent,
) {
	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		o.logger.Warn("shortcut reconciliation refresh failed, keeping current runners",
			slog.String("error", err.Error()),
		)
		return
	}

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, compiled, mode, opts, managedRootEvents, runnerEvents)
	if len(startResults) > 0 {
		o.emitStartWarning(summarizeStartupResults(startResults))
	}
	o.logger.Info("shortcut reconciliation refresh complete",
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
	managedRootEvents chan<- syncengine.ManagedRootEvent,
	runnerEvents chan<- watchRunnerEvent,
) {
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
	}

	if wr != nil && wr.mount.finalDrain {
		o.handleFinalDrainWatchRunnerEvent(wr, event)
	}

	if event.err != nil && errors.Is(event.err, syncengine.ErrMountTopologyChanged) {
		o.logger.Info("watch runner requested topology reconciliation",
			slog.String("mount_id", event.mountID.String()),
		)
	} else if event.err != nil && errors.Is(event.err, syncengine.ErrMountRootUnavailable) {
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

	o.reconcileWatchRunners(ctx, mode, opts, runners, managedRootEvents, runnerEvents)
}

func (o *Orchestrator) handleFinalDrainWatchRunnerEvent(
	wr *watchRunner,
	event watchRunnerEvent,
) {
	if event.err != nil || event.report == nil || event.report.Failed > 0 || len(event.report.Errors) > 0 {
		o.logger.Info("final-drain child mount is still waiting",
			slog.String("mount_id", event.mountID.String()),
		)
		return
	}
	compiled, err := o.compileRuntimeMountSetFromInventory(o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
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
	}
	finalized, err := finalizeSuccessfulFinalDrainMounts(compiled, []*MountReport{report}, o.logger)
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
	compiled *compiledMountSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	managedRootEvents chan<- syncengine.ManagedRootEvent,
	runnerEvents chan<- watchRunnerEvent,
) (int, int, []MountStartupResult) {
	runnable := runnableMountMap(compiled.Mounts)
	stopped := o.stopInactiveWatchRunners(ctx, runners, runnable)
	stopped += o.stopProjectionMoveWatchRunners(ctx, runners, compiled.ProjectionMoves)
	stopped += o.stopLocalRootActionWatchRunners(ctx, runners, compiled.LocalRootActions)
	var finalizeErr error
	compiled, finalizeErr = o.finalizeRuntimeMountSetLifecycle(
		ctx,
		compiled,
		o.cfg.StandaloneMounts,
		o.cfg.InitialStartupResults,
		"watch reload",
		false,
	)
	if finalizeErr != nil {
		o.logger.Warn("finalizing mount lifecycle after mount diff failed; using current mount set",
			slog.String("error", finalizeErr.Error()),
		)
	}
	runnable = runnableMountMap(compiled.Mounts)
	stopped += o.stopInactiveWatchRunners(ctx, runners, runnable)
	started, startResults := o.startReloadWatchRunners(ctx, runners, runnable, compiled.Skipped, mode, opts, managedRootEvents, runnerEvents)
	o.setControlMountIDs(sortedRunnerMountIDs(runners))

	return stopped, started, startResults
}

func (o *Orchestrator) stopLocalRootActionWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	actions []childRootLifecycleAction,
) int {
	if len(actions) == 0 || len(runners) == 0 {
		return 0
	}

	stopped := 0
	seen := make(map[mountID]struct{}, len(actions))
	for i := range actions {
		action := &actions[i]
		if _, done := seen[action.mountID]; done {
			continue
		}
		seen[action.mountID] = struct{}{}
		wr := runners[action.mountID]
		if wr == nil {
			continue
		}

		o.logger.Info("stopping watch runner before child root lifecycle action",
			slog.String("mount_id", action.mountID.String()),
			slog.String("action", string(action.kind)),
		)
		wr.cancel()
		<-wr.done
		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error before child root lifecycle action",
				slog.String("mount_id", action.mountID.String()),
				slog.String("error", closeErr.Error()),
			)
		}
		delete(runners, action.mountID)
		stopped++
	}

	return stopped
}

func (o *Orchestrator) stopProjectionMoveWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	moves []childProjectionMove,
) int {
	if len(moves) == 0 || len(runners) == 0 {
		return 0
	}

	stopped := 0
	seen := make(map[mountID]struct{}, len(moves))
	for i := range moves {
		move := &moves[i]
		if _, done := seen[move.mountID]; done {
			continue
		}
		seen[move.mountID] = struct{}{}
		wr := runners[move.mountID]
		if wr == nil {
			continue
		}

		o.logger.Info("stopping watch runner before child projection move",
			slog.String("mount_id", move.mountID.String()),
		)
		wr.cancel()
		<-wr.done
		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error before child projection move",
				slog.String("mount_id", move.mountID.String()),
				slog.String("error", closeErr.Error()),
			)
		}
		delete(runners, move.mountID)
		stopped++
	}

	return stopped
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
	for id, wr := range runners {
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

func (o *Orchestrator) startReloadWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	runnable map[mountID]*mountSpec,
	initialStartup []MountStartupResult,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	managedRootEvents chan<- syncengine.ManagedRootEvent,
	runnerEvents chan<- watchRunnerEvent,
) (int, []MountStartupResult) {
	started := 0
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for id, mount := range runnable {
		if _, ok := runners[id]; ok {
			continue
		}

		o.attachShortcutTopologyHandler(mount, true)
		wr, err := o.startWatchRunner(ctx, mount, mode, opts, managedRootEvents, runnerEvents)
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

func mountSpecsEquivalentForWatchRestart(current *mountSpec, next *mountSpec) bool {
	if current == nil || next == nil {
		return current == next
	}
	return mountSpecCoreEquivalent(current, next) &&
		mountSkipDirsEqual(current.localSkipDirs, next.localSkipDirs) &&
		mountReservationsEqual(current.localReservations, next.localReservations)
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
		current.remoteRootDeltaCapable == next.remoteRootDeltaCapable
}

func mountSpecTuningEquivalent(current *mountSpec, next *mountSpec) bool {
	return current.transferWorkers == next.transferWorkers &&
		current.checkWorkers == next.checkWorkers &&
		current.minFreeSpace == next.minFreeSpace
}

func mountSkipDirsEqual(current []string, next []string) bool {
	if len(current) != len(next) {
		return false
	}

	for i := range current {
		if current[i] != next[i] {
			return false
		}
	}

	return true
}

func mountReservationsEqual(
	current []syncengine.ManagedRootReservation,
	next []syncengine.ManagedRootReservation,
) bool {
	if len(current) != len(next) {
		return false
	}
	for i := range current {
		if current[i] != next[i] {
			return false
		}
	}
	return true
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
