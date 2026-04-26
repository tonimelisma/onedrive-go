package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	gosync "sync"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// mountWork pairs a MountRunner with the sync function it will execute.
type mountWork struct {
	runner *MountRunner
	mount  *mountSpec
	engine engineRunner
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
	control, err := o.startControlServer(ctx, synccontrol.OwnerModeOneShot, nil)
	if err != nil {
		return controlFailureRunOnceResult(o.cfg.StandaloneMounts, o.cfg.InitialStartupResults, err)
	}
	if control != nil {
		defer o.closeControlSocket(ctx, control)
	}

	compiled, err := o.buildRuntimeMountSet(ctx, o.cfg.StandaloneMounts, o.cfg.InitialStartupResults)
	if err != nil {
		return controlFailureRunOnceResult(
			o.cfg.StandaloneMounts,
			o.cfg.InitialStartupResults,
			fmt.Errorf("building mount specs: %w", err),
		)
	}
	var startupParents startupParentEngines
	compiled, startupParents, err = o.publishParentStartupChildTopology(
		ctx,
		compiled,
		o.cfg.StandaloneMounts,
		o.cfg.InitialStartupResults,
		nil,
		mode,
		opts,
	)
	if err != nil {
		return controlFailureRunOnceResult(
			o.cfg.StandaloneMounts,
			o.cfg.InitialStartupResults,
			fmt.Errorf("publishing parent startup child topology: %w", err),
		)
	}
	defer o.closeStartupParentEngines(ctx, startupParents)
	parentArtifactAckers := parentStartupArtifactCleanupAckers(startupParents)
	if purgeErr := o.purgeShortcutChildArtifactsForCompiled(ctx, compiled, parentArtifactAckers); purgeErr != nil {
		o.logger.Warn("purging shortcut child state artifacts",
			slog.String("error", purgeErr.Error()),
		)
	}
	o.setControlMountIDs(mountIDsForSpecs(compiled.Mounts))

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("mounts", len(compiled.Mounts)),
		slog.String("mode", mode.String()),
	)

	work, startup, reports := o.prepareRunOnceWork(ctx, mode, compiled.Mounts, compiled.Skipped, opts, startupParents)

	var wg gosync.WaitGroup
	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedMountWork) {
			defer wg.Done()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
		}(w)
	}

	wg.Wait()

	o.closeRunOnceChildEngines(ctx, work)
	defer o.closeRunOnceParentEngines(ctx, work)

	if finalized, finalizeErr := o.finalizeSuccessfulFinalDrainMounts(
		ctx,
		compiled,
		reports,
		runOnceParentDrainAckers(work),
	); finalizeErr != nil {
		o.logger.Warn("finalizing drained shortcut child mounts",
			slog.String("error", finalizeErr.Error()),
		)
	} else if finalized {
		o.logger.Info("finalized drained shortcut child mounts")
	}

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

// prepareRunOnceWork resolves sessions and builds engines for each selected
// mount. Errors are captured as closures that return the error when the
// MountRunner executes — no early abort for individual mount failures.
func (o *Orchestrator) prepareRunOnceWork(
	ctx context.Context,
	mode syncengine.SyncMode,
	mounts []*mountSpec,
	initialStartup []MountStartupResult,
	opts syncengine.RunOptions,
	startupParents startupParentEngines,
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

		o.attachShortcutChildTopologySink(mount, false)
		if startup := startupParents[mount.mountID]; startup != nil {
			delete(startupParents, mount.mountID)
			setShortcutChildTopologySink(startup, mount.shortcutChildTopologySink)
			w := o.buildEngineWorkFromExisting(mount, startup, mode, opts)
			startResults = append(startResults, MountStartupResult{
				SelectionIndex: mount.selectionIndex,
				Identity:       mount.identity(),
				DisplayName:    mount.displayName,
				Status:         MountStartupRunnable,
			})
			work = append(work, indexedMountWork{index: len(reports), work: w})
			reports = append(reports, nil)
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
		mount:  mount,
		engine: engine,
		fn: func(c context.Context) (*syncengine.Report, error) {
			runMode := mode
			runOpts := opts
			if mount.finalDrain {
				runMode = syncengine.SyncBidirectional
				runOpts = syncengine.RunOptions{FullReconcile: true}
			}
			return engine.RunOnce(c, runMode, runOpts)
		},
	}, nil
}

func (o *Orchestrator) buildEngineWorkFromExisting(
	mount *mountSpec,
	engine engineRunner,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) mountWork {
	return mountWork{
		runner: &MountRunner{
			selectionIndex: mount.selectionIndex,
			identity:       mount.identity(),
			displayName:    mount.displayName,
		},
		mount:  mount,
		engine: engine,
		fn: func(c context.Context) (*syncengine.Report, error) {
			runMode := mode
			runOpts := opts
			if mount.finalDrain {
				runMode = syncengine.SyncBidirectional
				runOpts = syncengine.RunOptions{FullReconcile: true}
			}
			return engine.RunOnce(c, runMode, runOpts)
		},
	}
}

func (o *Orchestrator) closeRunOnceChildEngines(ctx context.Context, work []indexedMountWork) {
	for _, item := range work {
		if item.work.mount == nil || item.work.mount.projectionKind != MountProjectionChild {
			continue
		}
		o.closeRunOnceEngine(ctx, item.work.mount, item.work.engine)
	}
}

func (o *Orchestrator) closeRunOnceParentEngines(ctx context.Context, work []indexedMountWork) {
	for _, item := range work {
		if item.work.mount == nil || item.work.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		o.closeRunOnceEngine(ctx, item.work.mount, item.work.engine)
	}
}

func (o *Orchestrator) closeRunOnceEngine(ctx context.Context, mount *mountSpec, engine engineRunner) {
	if mount == nil || engine == nil {
		return
	}
	defer o.removeMountPerfCollector(mount.mountID.String())
	if closeErr := engine.Close(ctx); closeErr != nil {
		o.logger.Warn("engine close error",
			slog.String("mount_id", mount.mountID.String()),
			slog.String("error", closeErr.Error()))
	}
}

func runOnceParentDrainAckers(work []indexedMountWork) map[mountID]shortcutChildLifecycleAcker {
	ackers := make(map[mountID]shortcutChildLifecycleAcker)
	for _, item := range work {
		if item.work.mount == nil || item.work.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		acker, ok := item.work.engine.(shortcutChildLifecycleAcker)
		if !ok {
			continue
		}
		ackers[item.work.mount.mountID] = acker
	}
	return ackers
}

func (o *Orchestrator) finalizeSuccessfulFinalDrainMounts(
	ctx context.Context,
	compiled *compiledMountSet,
	reports []*MountReport,
	parentAckers map[mountID]shortcutChildLifecycleAcker,
) (bool, error) {
	if compiled == nil || len(compiled.FinalDrainMountIDs) == 0 {
		return false, nil
	}
	results := classifyShortcutChildDrainResults(compiled.FinalDrainMountIDs, compiled.Mounts, reports)
	successful := cleanShortcutChildDrainResults(results)
	if len(successful) == 0 {
		logUnfinishedFinalDrains(results, o.logger)
		return false, nil
	}

	cleanups, err := acknowledgeSuccessfulFinalDrains(ctx, successful, compiled.Mounts, parentAckers)
	if err != nil {
		return false, err
	}
	if err := purgeShortcutChildArtifactsForCleanups(ctx, cleanups, o.logger); err != nil {
		return false, err
	}
	if err := o.acknowledgeShortcutChildArtifactCleanups(ctx, cleanups, artifactCleanupAckersFromLifecycle(parentAckers)); err != nil {
		return false, err
	}
	if o.logger != nil {
		for _, result := range successful {
			o.logger.Info("parent acknowledged drained shortcut child",
				"mount_id", result.MountID,
			)
		}
	}
	return true, nil
}

func logUnfinishedFinalDrains(results []shortcutChildDrainResult, logger *slog.Logger) {
	if logger == nil {
		return
	}
	for _, result := range results {
		if result.Status == shortcutChildDrainClean {
			continue
		}
		logger.Info("shortcut child final drain not ready for parent release",
			"mount_id", result.MountID,
			"status", string(result.Status),
			"detail", result.Detail,
		)
	}
}

func acknowledgeSuccessfulFinalDrains(
	ctx context.Context,
	successful []shortcutChildDrainResult,
	mounts []*mountSpec,
	parentAckers map[mountID]shortcutChildLifecycleAcker,
) ([]shortcutChildArtifactCleanup, error) {
	mountByID := make(map[string]*mountSpec, len(mounts))
	for _, mount := range mounts {
		if mount != nil {
			mountByID[mount.mountID.String()] = mount
		}
	}
	cleanups := make([]shortcutChildArtifactCleanup, 0, len(successful))
	for _, result := range successful {
		mount := mountByID[result.MountID]
		bindingItemID := result.BindingItemID
		if bindingItemID == "" && mount != nil {
			bindingItemID = mount.bindingItemID
		}
		if mount == nil || bindingItemID == "" {
			return nil, fmt.Errorf("final-drain child mount %s is missing parent binding identity", result.MountID)
		}
		acker := parentAckers[mount.parentMountID]
		if acker == nil {
			return nil, fmt.Errorf("parent mount %s is unavailable for final-drain acknowledgement", mount.parentMountID)
		}
		snapshot, err := acker.AcknowledgeChildFinalDrain(ctx, syncengine.ShortcutChildDrainAck{
			BindingItemID: bindingItemID,
		})
		if err != nil {
			return nil, fmt.Errorf("acknowledging final drain for child mount %s: %w", result.MountID, err)
		}
		cleanups = append(cleanups, shortcutChildArtifactCleanups(map[mountID]syncengine.ShortcutChildTopologyPublication{
			mount.parentMountID: snapshot,
		})...)
	}
	return cleanups, nil
}

func artifactCleanupAckersFromLifecycle(
	lifecycle map[mountID]shortcutChildLifecycleAcker,
) map[mountID]shortcutChildArtifactCleanupAcker {
	ackers := make(map[mountID]shortcutChildArtifactCleanupAcker, len(lifecycle))
	for id, acker := range lifecycle {
		ackers[id] = acker
	}
	return ackers
}

func parentStartupArtifactCleanupAckers(
	started startupParentEngines,
) map[mountID]shortcutChildArtifactCleanupAcker {
	ackers := make(map[mountID]shortcutChildArtifactCleanupAcker)
	for parentID, engine := range started {
		acker, ok := engine.(shortcutChildArtifactCleanupAcker)
		if !ok {
			continue
		}
		ackers[parentID] = acker
	}
	return ackers
}

func purgeShortcutChildArtifactsForCleanups(
	ctx context.Context,
	cleanups []shortcutChildArtifactCleanup,
	logger *slog.Logger,
) error {
	var errs []error
	for _, cleanup := range cleanups {
		scope := shortcutChildArtifactScope{mountID: cleanup.mountID}
		if err := purgeShortcutChildArtifacts(ctx, scope, logger); err != nil {
			errs = append(errs, fmt.Errorf("purging final-drain child mount %s: %w", cleanup.mountID, err))
		}
	}
	return errors.Join(errs...)
}
