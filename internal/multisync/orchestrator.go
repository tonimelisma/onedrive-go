package multisync

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	gosync "sync"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineRunner is the interface the Orchestrator uses to run sync passes.
// Implemented by *sync.Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
	Close(ctx context.Context) error
}

type engineFactoryRequest struct {
	Session       *driveops.Session
	Mount         *mountSpec
	Logger        *slog.Logger
	VerifyDrive   bool
	PerfCollector *perf.Collector
}

// engineFactoryFunc creates an engineRunner from the runtime mount/session pair
// used by production orchestration. Tests inject mocks at this boundary.
type engineFactoryFunc func(ctx context.Context, req engineFactoryRequest) (engineRunner, error)

// OrchestratorConfig holds the inputs for creating an Orchestrator.
// The CLI layer populates this from standalone mount config and HTTP clients.
// Config and config path are accessed via Holder — a single source of truth
// shared with SessionRuntime. Control-socket reload updates config in one place.
type OrchestratorConfig struct {
	Holder                 *config.Holder
	StandaloneMounts       []StandaloneMountConfig
	InitialStartupResults  []MountStartupResult
	ReloadStandaloneMounts func(*config.Config) (StandaloneMountSelection, error)
	Runtime                *driveops.SessionRuntime // token caching + Session creation
	Logger                 *slog.Logger
	ControlSocketPath      string
	StartWarning           func(StartupWarning)
	DebugEventHook         func(syncengine.DebugEvent)
	PerfParent             *perf.Collector
}

// Orchestrator manages per-mount sync runners. It is always used, even for a
// single mount, so one-shot and watch mode share the same top-level lifecycle.
type Orchestrator struct {
	cfg            *OrchestratorConfig
	engineFactory  engineFactoryFunc // injectable for tests
	logger         *slog.Logger
	perfRuntime    *perf.Runtime
	statusMu       gosync.RWMutex
	controlMounts  []string
	reconcileTicks func(time.Duration) (<-chan time.Time, func())
}

// NewOrchestrator creates an Orchestrator with real Engine factory.
// Token/client caching is handled by the SessionRuntime in cfg.Runtime.
// Tests inject stubs via cfg.Runtime.TokenSourceFn and engineFactory.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg: cfg,
		engineFactory: func(ctx context.Context, req engineFactoryRequest) (engineRunner, error) {
			mountCfg, err := engineMountConfigForMount(req.Mount)
			if err != nil {
				return nil, fmt.Errorf("engine mount config: %w", err)
			}
			engine, err := syncengine.NewMountEngine(
				ctx,
				req.Session,
				mountCfg,
				req.Logger,
				req.PerfCollector,
				req.VerifyDrive,
			)
			if err != nil {
				return nil, fmt.Errorf("new engine: %w", err)
			}
			if cfg.DebugEventHook != nil {
				engine.SetDebugEventHook(cfg.DebugEventHook)
			}
			return engine, nil
		},
		logger:      cfg.Logger,
		perfRuntime: perf.NewRuntime(cfg.PerfParent),
		reconcileTicks: func(interval time.Duration) (<-chan time.Time, func()) {
			if interval <= 0 {
				return nil, func() {}
			}

			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		},
	}
}

// mountWork pairs a MountRunner with the sync function it will execute.
type mountWork struct {
	runner *MountRunner
	fn     func(context.Context) (*syncengine.Report, error)
}

type indexedMountWork struct {
	index int
	work  mountWork
}

// RunOnce executes a single sync pass for all configured runtime mounts. Each mount
// runs in its own goroutine via a MountRunner with panic recovery. RunOnce
// never returns an error — individual mount errors are captured in each
// syncengine.Report. The caller inspects reports to determine success or failure.
func (o *Orchestrator) RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) RunOnceResult {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return RunOnceResult{
				Startup: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return RunOnceResult{}
	}
	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return controlFailureRunOnceResult(
			o.cfg.StandaloneMounts,
			o.cfg.InitialStartupResults,
			fmt.Errorf("building mount specs: %w", err),
		)
	}
	o.setControlMountIDs(mountIDsForSpecs(compiled.Mounts))
	if purgeErr := purgeManagedMountStateDBs(o.logger, compiled.RemovedMountIDs); purgeErr != nil {
		o.logger.Warn("purging removed child mount state during startup",
			slog.String("error", purgeErr.Error()),
		)
	} else if finalizeErr := finalizePendingMountRemovals(compiled.RemovedMountIDs); finalizeErr != nil {
		o.logger.Warn("finalizing removed child mounts during startup",
			slog.String("error", finalizeErr.Error()),
		)
	}

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeOneShot, nil)
	if err != nil {
		return controlFailureRunOnceResult(o.cfg.StandaloneMounts, o.cfg.InitialStartupResults, err)
	}
	if control != nil {
		defer o.closeControlSocket(ctx, control)
	}

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("mounts", len(compiled.Mounts)),
		slog.String("mode", mode.String()),
	)

	work, startup, reports := o.prepareRunOnceWork(ctx, mode, compiled.Mounts, compiled.Skipped, opts)

	// Run all mounts concurrently.
	var wg gosync.WaitGroup

	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedMountWork) {
			defer wg.Done()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
		}(w)
	}

	wg.Wait()

	o.logger.Info("orchestrator RunOnce complete", slog.Int("reports", len(reports)))

	return RunOnceResult{
		Startup: startup,
		Reports: reports,
	}
}

func controlFailureRunOnceResult(
	configs []StandaloneMountConfig,
	initialStartup []MountStartupResult,
	err error,
) RunOnceResult {
	mounts, buildErr := buildStandaloneMountSpecs(configs)
	if buildErr != nil {
		results := append([]MountStartupResult(nil), initialStartup...)
		results = append(results, MountStartupResult{
			Status: MountStartupFatal,
			Err:    fmt.Errorf("building mount specs: %w", buildErr),
		})
		return RunOnceResult{
			Startup: summarizeStartupResults(results),
		}
	}

	results := append([]MountStartupResult(nil), initialStartup...)
	for i := range mounts {
		results = append(results, mountStartupResultForMount(mounts[i], err))
	}

	return RunOnceResult{
		Startup: summarizeStartupResults(results),
	}
}

func (o *Orchestrator) buildRuntimeMountSet(
	ctx context.Context,
	standaloneMounts []StandaloneMountConfig,
	initialStartup []MountStartupResult,
) (*compiledMountSet, error) {
	parents, err := buildStandaloneMountSpecs(standaloneMounts)
	if err != nil {
		return nil, err
	}

	namespaceResult, reconcileErr := o.reconcileManagedShortcutMounts(ctx, parents)
	if reconcileErr != nil && o.logger != nil {
		o.logger.Warn("shortcut reconciliation failed; keeping existing mount inventory",
			slog.String("error", reconcileErr.Error()),
		)
	}

	inventory := namespaceResult.inventory
	if inventory == nil {
		loadedInventory, loadErr := config.LoadMountInventory()
		if loadErr != nil {
			return nil, fmt.Errorf("loading mount inventory: %w", loadErr)
		}
		inventory = loadedInventory
	}

	compiled, err := compileRuntimeMountsForParents(parents, inventory, o.logger)
	if err != nil {
		return nil, err
	}
	if reconcileErr == nil {
		compiled.RemovedMountIDs = append(compiled.RemovedMountIDs, namespaceResult.removedMountIDs...)
	}
	offsetCompiledSelectionIndexes(compiled, nextStartupSelectionIndex(initialStartup))
	compiled.Skipped = append(append([]MountStartupResult(nil), initialStartup...), compiled.Skipped...)

	return compiled, nil
}

func nextStartupSelectionIndex(results []MountStartupResult) int {
	next := 0
	for i := range results {
		if results[i].SelectionIndex >= next {
			next = results[i].SelectionIndex + 1
		}
	}

	return next
}

func offsetCompiledSelectionIndexes(compiled *compiledMountSet, offset int) {
	if compiled == nil || offset == 0 {
		return
	}
	for i := range compiled.Mounts {
		compiled.Mounts[i].selectionIndex += offset
	}
	for i := range compiled.Skipped {
		compiled.Skipped[i].SelectionIndex += offset
	}
}

// prepareRunOnceWork resolves sessions and builds engines for each selected
// mount. Errors are captured as closures that return the error when the
// MountRunner executes — no early abort for individual mount failures.
func (o *Orchestrator) prepareRunOnceWork(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	initialStartup []MountStartupResult,
	opts syncengine.RunOptions,
) ([]indexedMountWork, StartupSelectionSummary, []*MountReport) {
	work := make([]indexedMountWork, 0, len(mounts))
	reports := make([]*MountReport, 0, len(mounts))
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for i := range mounts {
		mount := mounts[i]
		if mount.paused {
			startResults = append(startResults, MountStartupResult{
				SelectionIndex: mount.selectionIndex,
				Identity:       mount.identity(),
				DisplayName:    mount.displayName,
				Status:         MountStartupPaused,
			})
			continue
		}

		session, err := o.cfg.Runtime.SyncSession(ctx, mount.syncSessionConfig())
		if err != nil {
			startResults = append(startResults, mountStartupResultForMount(
				mount,
				fmt.Errorf("session error for mount %s: %w", mount.label(), err),
			))
			continue
		}

		w, engineErr := o.buildEngineWork(ctx, mount, session, mode, opts)
		if engineErr != nil {
			startResults = append(startResults, mountStartupResultForMount(mount, engineErr))
			continue
		}
		startResults = append(startResults, MountStartupResult{
			SelectionIndex: mount.selectionIndex,
			Identity:       mount.identity(),
			DisplayName:    mount.displayName,
			Status:         MountStartupRunnable,
		})
		work = append(work, indexedMountWork{index: len(reports), work: w})
		reports = append(reports, nil)
	}

	return work, summarizeStartupResults(startResults), reports
}

func mountStartupResultForMount(mount *mountSpec, err error) MountStartupResult {
	return MountStartupResult{
		SelectionIndex: mount.selectionIndex,
		Identity:       mount.identity(),
		DisplayName:    mount.displayName,
		Status:         classifyMountStartupError(err),
		Err:            err,
	}
}

// buildEngineWork creates a mountWork item for a successfully-resolved mount.
// If engine creation fails, the error is captured and reported at run time.
func (o *Orchestrator) buildEngineWork(
	ctx context.Context,
	mount *mountSpec,
	session *driveops.Session,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (mountWork, error) {
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
		return mountWork{}, fmt.Errorf("engine creation failed for mount %s: %w", mount.label(), engineErr)
	}

	return mountWork{
		runner: &MountRunner{
			selectionIndex: mount.selectionIndex,
			identity:       mount.identity(),
			displayName:    mount.displayName,
		},
		fn: func(c context.Context) (*syncengine.Report, error) {
			defer func() {
				o.removeMountPerfCollector(mount.mountID.String())
				if closeErr := engine.Close(c); closeErr != nil {
					o.logger.Warn("engine close error",
						slog.String("mount_id", mount.mountID.String()),
						slog.String("error", closeErr.Error()))
				}
			}()

			return engine.RunOnce(c, mode, opts)
		},
	}, nil
}

// ---------------------------------------------------------------------------
// RunWatch — multi-mount daemon mode
// ---------------------------------------------------------------------------

// watchRunner holds per-mount state for a running watch-mode engine.
type watchRunner struct {
	mount  *mountSpec
	engine engineRunner
	cancel context.CancelFunc
	done   chan struct{} // closed exactly once by the goroutine started in startWatchRunner
}

// RunWatch runs all configured runnable mounts in watch mode. On control-socket
// reload, it re-reads the config file, rebuilds runtime mount specs, and diffs
// the active mount set: stopped mounts are removed, new mounts are started.
// Returns nil on
// clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	commands := make(chan controlCommand)
	runners, control, err := o.startWatchRuntime(ctx, mode, opts, commands)
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

	// Main select loop.
	for {
		select {
		case cmd := <-commands:
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners) {
				return nil
			}
		case <-reconcileTickCh:
			o.reconcileWatchRunners(ctx, mode, opts, runners)
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
) (map[mountID]*watchRunner, *controlSocketServer, error) {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return nil, nil, &WatchStartupError{
				Summary: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return nil, nil, fmt.Errorf("sync: no standalone mounts configured")
	}

	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return nil, nil, fmt.Errorf("sync: building mount specs: %w", err)
	}
	if purgeErr := purgeManagedMountStateDBs(o.logger, compiled.RemovedMountIDs); purgeErr != nil {
		o.logger.Warn("purging removed child mount state during watch startup",
			slog.String("error", purgeErr.Error()),
		)
	} else if finalizeErr := finalizePendingMountRemovals(compiled.RemovedMountIDs); finalizeErr != nil {
		o.logger.Warn("finalizing removed child mounts during watch startup",
			slog.String("error", finalizeErr.Error()),
		)
	}

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("mounts", len(compiled.Mounts)),
		slog.String("mode", mode.String()),
	)

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands)
	if err != nil {
		return nil, nil, err
	}

	runners, startResults := o.startInitialWatchRunners(ctx, mode, compiled.Mounts, compiled.Skipped, opts)
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
) (map[mountID]*watchRunner, []MountStartupResult) {
	runners := make(map[mountID]*watchRunner)
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for i := range mounts {
		mount := mounts[i]
		// Pause semantics are handled by config before runtime mount specs are
		// built. The control plane consumes the resolved pause state; it does not
		// recompute pause policy itself.
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

		wr, err := o.startWatchRunner(ctx, mount, mode, opts)
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

// startWatchRunner creates and starts a watch-mode engine for a single mount.
func (o *Orchestrator) startWatchRunner(
	ctx context.Context, mount *mountSpec, mode syncengine.SyncMode, opts syncengine.WatchOptions,
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
		mount:  mount,
		engine: engine,
		cancel: mountCancel,
		done:   done,
	}

	go func() {
		defer close(done)
		defer mountCancel()
		defer o.removeMountPerfCollector(mount.mountID.String())

		if watchErr := engine.RunWatch(mountCtx, mode, opts); watchErr != nil {
			// Context cancellation is normal shutdown — don't log as error.
			if mountCtx.Err() == nil {
				o.logger.Error("watch runner exited with error",
					slog.String("mount_id", mount.mountID.String()),
					slog.String("error", watchErr.Error()),
				)
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

// reload re-reads the config file, rebuilds runtime mount specs, diffs the
// active mount set against running runners, stops removed/paused mounts, and
// starts newly added/resumed mounts.
func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
) {
	newCfg, newSelection, newMounts, err := o.loadReloadMounts(ctx)
	if err != nil {
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", err.Error()),
		)

		return
	}

	// Single-point config update — both Orchestrator and SessionRuntime
	// read through the shared Holder.
	o.cfg.Holder.Update(newCfg)
	o.cfg.StandaloneMounts = newSelection.Mounts
	o.cfg.InitialStartupResults = newSelection.StartupResults

	// Flush cached token sources so the next Session() call re-reads
	// token files from disk. Handles logout + re-login between reloads.
	o.cfg.Runtime.FlushTokenCache()

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, newMounts, mode, opts)
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
) {
	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		o.logger.Warn("shortcut reconciliation refresh failed, keeping current runners",
			slog.String("error", err.Error()),
		)
		return
	}

	stopped, started, startResults := o.applyWatchMountSet(ctx, runners, compiled, mode, opts)
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

func (o *Orchestrator) loadReloadMounts(
	ctx context.Context,
) (*config.Config, StandaloneMountSelection, *compiledMountSet, error) {
	newCfg, err := config.LoadOrDefault(o.cfg.Holder.Path(), o.logger)
	if err != nil {
		return nil, StandaloneMountSelection{}, nil, fmt.Errorf("loading config for reload: %w", err)
	}

	// Clear expired timed pauses before standalone mount compilation, so newly
	// unpaused drives are included in the active set. Pause semantics are owned
	// by the config package — the orchestrator is a consumer, not an implementor.
	config.ClearExpiredPauses(o.cfg.Holder.Path(), newCfg, time.Now(), o.logger)

	if o.cfg.ReloadStandaloneMounts == nil {
		return nil, StandaloneMountSelection{}, nil, fmt.Errorf("standalone mount reload compiler is required")
	}
	newSelection, err := o.cfg.ReloadStandaloneMounts(newCfg)
	if err != nil {
		return nil, StandaloneMountSelection{}, nil, fmt.Errorf("compiling standalone mounts after reload: %w", err)
	}

	newMounts, err := o.buildRuntimeMountSet(ctx, newSelection.Mounts, newSelection.StartupResults)
	if err != nil {
		return nil, StandaloneMountSelection{}, nil, fmt.Errorf("building mount specs after reload: %w", err)
	}

	return newCfg, newSelection, newMounts, nil
}

func (o *Orchestrator) applyWatchMountSet(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	compiled *compiledMountSet,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
) (int, int, []MountStartupResult) {
	runnable := runnableMountMap(compiled.Mounts)
	stopped := o.stopInactiveWatchRunners(ctx, runners, runnable)
	if purgeErr := purgeManagedMountStateDBs(o.logger, compiled.RemovedMountIDs); purgeErr != nil {
		o.logger.Warn("purging removed child mount state after mount diff",
			slog.String("error", purgeErr.Error()),
		)
	} else if finalizeErr := finalizePendingMountRemovals(compiled.RemovedMountIDs); finalizeErr != nil {
		o.logger.Warn("finalizing removed child mounts after mount diff",
			slog.String("error", finalizeErr.Error()),
		)
	}
	started, startResults := o.startReloadWatchRunners(ctx, runners, runnable, compiled.Skipped, mode, opts)
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
) (int, []MountStartupResult) {
	started := 0
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for id, mount := range runnable {
		if _, ok := runners[id]; ok {
			continue
		}

		wr, err := o.startWatchRunner(ctx, mount, mode, opts)
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
	return mountSpecCoreEquivalent(current, next) && mountSkipDirsEqual(current.localSkipDirs, next.localSkipDirs)
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
