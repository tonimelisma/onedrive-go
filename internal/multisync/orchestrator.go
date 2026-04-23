package multisync

import (
	"context"
	"fmt"
	"log/slog"
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
// The CLI layer populates this from resolved config and HTTP clients.
// Config and config path are accessed via Holder — a single source of truth
// shared with SessionRuntime. Control-socket reload updates config in one place.
type OrchestratorConfig struct {
	Holder            *config.Holder
	Drives            []*config.ResolvedDrive
	Runtime           *driveops.SessionRuntime // token caching + Session creation
	Logger            *slog.Logger
	ControlSocketPath string
	StartWarning      func(StartupWarning)
	DebugEventHook    func(syncengine.DebugEvent)
	PerfParent        *perf.Collector
}

// Orchestrator manages per-mount sync runners. It is always used, even for a
// single mount, so one-shot and watch mode share the same top-level lifecycle.
type Orchestrator struct {
	cfg           *OrchestratorConfig
	engineFactory engineFactoryFunc // injectable for tests
	logger        *slog.Logger
	perfRuntime   *perf.Runtime
}

// NewOrchestrator creates an Orchestrator with real Engine factory.
// Token/client caching is handled by the SessionRuntime in cfg.Runtime.
// Tests inject stubs via cfg.Runtime.TokenSourceFn and engineFactory.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg: cfg,
		engineFactory: func(ctx context.Context, req engineFactoryRequest) (engineRunner, error) {
			engine, err := syncengine.NewDriveEngine(
				ctx,
				req.Session,
				req.Mount.resolved,
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
	}
}

// driveWork pairs a DriveRunner with the sync function it will execute.
type driveWork struct {
	runner *DriveRunner
	fn     func(context.Context) (*syncengine.Report, error)
}

type indexedDriveWork struct {
	index int
	work  driveWork
}

// RunOnce executes a single sync pass for all configured runtime mounts. Each mount
// runs in its own goroutine via a DriveRunner with panic recovery. RunOnce
// never returns an error — individual drive errors are captured in each
// syncengine.Report. The caller inspects reports to determine success or failure.
func (o *Orchestrator) RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) RunOnceResult {
	selected := resolvedDrivesWithSelection(o.cfg.Drives)
	if len(selected) == 0 {
		return RunOnceResult{}
	}
	mounts, err := buildConfiguredMountSpecs(selected)
	if err != nil {
		return controlFailureRunOnceResult(o.cfg.Drives, fmt.Errorf("building mount specs: %w", err))
	}

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeOneShot, nil)
	if err != nil {
		return controlFailureRunOnceResult(o.cfg.Drives, err)
	}
	if control != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCloseTimeout)
			defer cancel()
			if closeErr := control.Close(closeCtx); closeErr != nil {
				o.logger.Warn("control socket close error",
					slog.String("error", closeErr.Error()),
				)
			}
		}()
	}

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("mounts", len(mounts)),
		slog.String("mode", mode.String()),
	)

	work, startup, reports := o.prepareRunOnceWork(ctx, mode, mounts, opts)

	// Run all mounts concurrently.
	var wg gosync.WaitGroup

	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedDriveWork) {
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

func controlFailureRunOnceResult(drives []*config.ResolvedDrive, err error) RunOnceResult {
	mounts, buildErr := buildConfiguredMountSpecs(resolvedDrivesWithSelection(drives))
	if buildErr != nil {
		return RunOnceResult{
			Startup: summarizeStartupResults([]DriveStartupResult{{
				Status: DriveStartupFatal,
				Err:    fmt.Errorf("building mount specs: %w", buildErr),
			}}),
		}
	}

	results := make([]DriveStartupResult, 0, len(mounts))
	for i := range mounts {
		results = append(results, driveStartupResultForMount(mounts[i], err))
	}

	return RunOnceResult{
		Startup: summarizeStartupResults(results),
	}
}

// prepareRunOnceWork resolves sessions and builds engines for each selected
// mount. Errors are captured as closures that return the error when the
// DriveRunner executes — no early abort for individual mount failures.
func (o *Orchestrator) prepareRunOnceWork(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	opts syncengine.RunOptions,
) ([]indexedDriveWork, StartupSelectionSummary, []*DriveReport) {
	work := make([]indexedDriveWork, 0, len(mounts))
	reports := make([]*DriveReport, 0, len(mounts))
	startResults := make([]DriveStartupResult, 0, len(mounts))

	for i := range mounts {
		mount := mounts[i]
		if mount.paused {
			startResults = append(startResults, DriveStartupResult{
				SelectionIndex: mount.selectionIndex,
				CanonicalID:    mount.canonicalID,
				DisplayName:    mount.displayName,
				Status:         DriveStartupPaused,
			})
			continue
		}

		session, err := o.cfg.Runtime.SyncSession(ctx, mount.resolved)
		if err != nil {
			startResults = append(startResults, driveStartupResultForMount(
				mount,
				fmt.Errorf("session error for drive %s: %w", mount.canonicalID, err),
			))
			continue
		}

		w, engineErr := o.buildEngineWork(ctx, mount, session, mode, opts)
		if engineErr != nil {
			startResults = append(startResults, driveStartupResultForMount(mount, engineErr))
			continue
		}
		startResults = append(startResults, DriveStartupResult{
			SelectionIndex: mount.selectionIndex,
			CanonicalID:    mount.canonicalID,
			DisplayName:    mount.displayName,
			Status:         DriveStartupRunnable,
		})
		work = append(work, indexedDriveWork{index: len(reports), work: w})
		reports = append(reports, nil)
	}

	return work, summarizeStartupResults(startResults), reports
}

func driveStartupResultForMount(mount *mountSpec, err error) DriveStartupResult {
	return DriveStartupResult{
		SelectionIndex: mount.selectionIndex,
		CanonicalID:    mount.canonicalID,
		DisplayName:    mount.displayName,
		Status:         classifyDriveStartupError(err),
		Err:            err,
	}
}

// buildEngineWork creates a driveWork item for a successfully-resolved mount.
// If engine creation fails, the error is captured and reported at run time.
func (o *Orchestrator) buildEngineWork(
	ctx context.Context,
	mount *mountSpec,
	session *driveops.Session,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (driveWork, error) {
	driveCollector := o.registerDrivePerfCollector(mount.canonicalID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         mount,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: driveCollector,
	})
	if engineErr != nil {
		o.removeDrivePerfCollector(mount.canonicalID.String())
		return driveWork{}, fmt.Errorf("engine creation failed for %s: %w", mount.canonicalID, engineErr)
	}

	return driveWork{
		runner: &DriveRunner{
			selectionIndex: mount.selectionIndex,
			canonID:        mount.canonicalID,
			displayName:    mount.displayName,
		},
		fn: func(c context.Context) (*syncengine.Report, error) {
			defer func() {
				o.removeDrivePerfCollector(mount.canonicalID.String())
				if closeErr := engine.Close(c); closeErr != nil {
					o.logger.Warn("engine close error",
						slog.String("drive", mount.canonicalID.String()),
						slog.String("error", closeErr.Error()))
				}
			}()

			return engine.RunOnce(c, mode, opts)
		},
	}, nil
}

// ---------------------------------------------------------------------------
// RunWatch — multi-drive daemon mode
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
	selected := resolvedDrivesWithSelection(o.cfg.Drives)
	if len(selected) == 0 {
		return fmt.Errorf("sync: no drives configured")
	}
	mounts, err := buildConfiguredMountSpecs(selected)
	if err != nil {
		return fmt.Errorf("sync: building mount specs: %w", err)
	}

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("mounts", len(mounts)),
		slog.String("mode", mode.String()),
	)

	commands := make(chan controlCommand)
	control, err := o.startControlServer(ctx, synccontrol.OwnerModeWatch, commands)
	if err != nil {
		return err
	}
	if control != nil {
		defer func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), controlCloseTimeout)
			defer cancel()
			if closeErr := control.Close(closeCtx); closeErr != nil {
				o.logger.Warn("control socket close error",
					slog.String("error", closeErr.Error()),
				)
			}
		}()
	}

	runners, startResults := o.startInitialWatchRunners(ctx, mode, mounts, opts)
	startSummary := summarizeStartupResults(startResults)
	if err := validateInitialWatchStart(runners, startSummary); err != nil {
		return err
	}
	if startSummary.SelectedCount() > 0 {
		o.emitStartWarning(startSummary)
	}

	defer func() {
		for id, wr := range runners {
			wr.cancel()
			<-wr.done

			if closeErr := wr.engine.Close(ctx); closeErr != nil {
				o.logger.Warn("engine close error on shutdown",
					slog.String("drive", id.String()),
					slog.String("error", closeErr.Error()),
				)
			}
		}

		o.logger.Info("orchestrator RunWatch stopped")
	}()

	// Main select loop.
	for {
		select {
		case cmd := <-commands:
			if o.handleControlCommand(ctx, &cmd, mode, opts, runners) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func (o *Orchestrator) startInitialWatchRunners(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	opts syncengine.WatchOptions,
) (map[mountID]*watchRunner, []DriveStartupResult) {
	runners := make(map[mountID]*watchRunner)
	startResults := make([]DriveStartupResult, 0, len(mounts))

	for i := range mounts {
		mount := mounts[i]
		// Pause semantics are handled by config before runtime mount specs are
		// built. The control plane consumes the resolved pause state; it does not
		// recompute pause policy itself.
		if mount.paused {
			o.logger.Info("skipping paused drive",
				slog.String("drive", mount.canonicalID.String()),
			)
			startResults = append(startResults, DriveStartupResult{
				SelectionIndex: mount.selectionIndex,
				CanonicalID:    mount.canonicalID,
				DisplayName:    mount.displayName,
				Status:         DriveStartupPaused,
			})

			continue
		}

		wr, err := o.startWatchRunner(ctx, mount, mode, opts)
		if err != nil {
			o.logger.Error("failed to start watch runner",
				slog.String("drive", mount.canonicalID.String()),
				slog.String("error", err.Error()),
			)
			startResults = append(startResults, driveStartupResultForMount(mount, err))

			continue
		}

		startResults = append(startResults, DriveStartupResult{
			SelectionIndex: mount.selectionIndex,
			CanonicalID:    mount.canonicalID,
			DisplayName:    mount.displayName,
			Status:         DriveStartupRunnable,
		})
		runners[mount.mountID] = wr
	}

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

// startWatchRunner creates and starts a watch-mode engine for a single mount.
func (o *Orchestrator) startWatchRunner(
	ctx context.Context, mount *mountSpec, mode syncengine.SyncMode, opts syncengine.WatchOptions,
) (*watchRunner, error) {
	session, err := o.cfg.Runtime.SyncSession(ctx, mount.resolved)
	if err != nil {
		return nil, fmt.Errorf("session error for drive %s: %w", mount.canonicalID, err)
	}

	driveCollector := o.registerDrivePerfCollector(mount.canonicalID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Mount:         mount,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: driveCollector,
	})
	if engineErr != nil {
		o.removeDrivePerfCollector(mount.canonicalID.String())
		return nil, fmt.Errorf("engine creation failed for %s: %w", mount.canonicalID, engineErr)
	}

	driveCtx, driveCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	wr := &watchRunner{
		mount:  mount,
		engine: engine,
		cancel: driveCancel,
		done:   done,
	}

	go func() {
		defer close(done)
		defer driveCancel()
		defer o.removeDrivePerfCollector(mount.canonicalID.String())

		if watchErr := engine.RunWatch(driveCtx, mode, opts); watchErr != nil {
			// Context cancellation is normal shutdown — don't log as error.
			if driveCtx.Err() == nil {
				o.logger.Error("watch runner exited with error",
					slog.String("drive", mount.canonicalID.String()),
					slog.String("error", watchErr.Error()),
				)
			}
		}
	}()

	o.logger.Info("watch runner started",
		slog.String("drive", mount.canonicalID.String()),
	)

	return wr, nil
}

func (o *Orchestrator) registerDrivePerfCollector(canonicalID string) *perf.Collector {
	if o == nil || o.perfRuntime == nil {
		return nil
	}

	return o.perfRuntime.RegisterDrive(canonicalID)
}

func (o *Orchestrator) removeDrivePerfCollector(canonicalID string) {
	if o == nil || o.perfRuntime == nil {
		return
	}

	o.perfRuntime.RemoveDrive(canonicalID)
}

// reload re-reads the config file, rebuilds runtime mount specs, diffs the
// active mount set against running runners, stops removed/paused mounts, and
// starts newly added/resumed mounts.
func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
) {
	newCfg, newMounts, err := o.loadReloadMounts()
	if err != nil {
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", err.Error()),
		)

		return
	}

	newActive := make(map[mountID]*mountSpec, len(newMounts))
	for i := range newMounts {
		newActive[newMounts[i].mountID] = newMounts[i]
	}

	stopped := o.stopInactiveWatchRunners(ctx, runners, newActive)
	started, startResults := o.startReloadWatchRunners(ctx, runners, newActive, mode, opts)

	// Single-point config update — both Orchestrator and SessionRuntime
	// read through the shared Holder.
	o.cfg.Holder.Update(newCfg)

	// Flush cached token sources so the next Session() call re-reads
	// token files from disk. Handles logout + re-login between reloads.
	o.cfg.Runtime.FlushTokenCache()

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

func (o *Orchestrator) loadReloadMounts() (*config.Config, []*mountSpec, error) {
	newCfg, err := config.LoadOrDefault(o.cfg.Holder.Path(), o.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("loading config for reload: %w", err)
	}

	// Clear expired timed pauses before resolving, so newly unpaused drives
	// are included in the active set. Pause semantics are owned by the config
	// package — the orchestrator is a consumer, not an implementor.
	config.ClearExpiredPauses(o.cfg.Holder.Path(), newCfg, time.Now(), o.logger)

	newDrives, err := config.ResolveDrives(newCfg, nil, false, o.logger)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving drives after reload: %w", err)
	}

	newMounts, err := buildConfiguredMountSpecs(resolvedDrivesWithSelection(newDrives))
	if err != nil {
		return nil, nil, fmt.Errorf("building mount specs after reload: %w", err)
	}

	return newCfg, newMounts, nil
}

func (o *Orchestrator) stopInactiveWatchRunners(
	ctx context.Context,
	runners map[mountID]*watchRunner,
	active map[mountID]*mountSpec,
) int {
	stopped := 0
	for id, wr := range runners {
		if _, ok := active[id]; ok {
			continue
		}

		o.logger.Info("stopping watch runner for removed/paused mount",
			slog.String("drive", id.String()),
		)

		wr.cancel()
		<-wr.done

		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error during reload",
				slog.String("drive", id.String()),
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
	active map[mountID]*mountSpec,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
) (int, []DriveStartupResult) {
	started := 0
	startResults := make([]DriveStartupResult, 0)

	for id, mount := range active {
		if _, ok := runners[id]; ok {
			continue
		}

		wr, err := o.startWatchRunner(ctx, mount, mode, opts)
		if err != nil {
			o.logger.Error("failed to start watch runner during reload",
				slog.String("drive", id.String()),
				slog.String("error", err.Error()),
			)
			startResults = append(startResults, driveStartupResultForMount(mount, err))
			continue
		}

		startResults = append(startResults, DriveStartupResult{
			SelectionIndex: mount.selectionIndex,
			CanonicalID:    mount.canonicalID,
			DisplayName:    mount.displayName,
			Status:         DriveStartupRunnable,
		})
		runners[id] = wr
		started++
	}

	return started, startResults
}
