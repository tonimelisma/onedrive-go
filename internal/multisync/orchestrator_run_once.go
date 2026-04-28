package multisync

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// mountWork pairs a MountRunner with the sync function it will execute.
type mountWork struct {
	runner    *MountRunner
	mount     *mountSpec
	engine    engineRunner
	parentAck shortcutChildAckHandle
	fn        func(context.Context) (*syncengine.Report, error)
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

	parentMounts, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
	if err != nil {
		return controlFailureRunOnceResult(
			o.cfg.StandaloneMounts,
			o.cfg.InitialStartupResults,
			fmt.Errorf("building mount specs: %w", err),
		)
	}
	o.setControlMountIDs(mountIDsForSpecs(parentMounts))

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("mounts", len(parentMounts)),
		slog.String("mode", mode.String()),
	)

	childCoordinator := newOneShotChildRuns(o, mode, opts, parentMounts)
	parentWork, parentStartup, parentReports := o.prepareRunOnceWork(
		ctx,
		mode,
		parentMounts,
		o.cfg.InitialStartupResults,
		opts,
		childCoordinator.notifyParentSnapshot,
	)
	childCoordinator.registerParents(parentWork)

	runIndexedParentMountWork(ctx, parentWork, parentReports, childCoordinator)
	defer o.closeRunOnceParentEngines(ctx, parentWork)

	childCoordinator.wait()

	allReports := append([]*MountReport{}, parentReports...)
	allReports = append(allReports, childCoordinator.reports()...)
	o.logger.Info("orchestrator RunOnce complete", slog.Int("reports", len(allReports)))

	startupResults := append([]MountStartupResult{}, parentStartup.Results...)
	startupResults = append(startupResults, childCoordinator.startupResults()...)
	return RunOnceResult{
		Startup: summarizeStartupResults(startupResults),
		Reports: allReports,
	}
}

func runIndexedMountWork(ctx context.Context, work []indexedMountWork, reports []*MountReport) {
	var wg gosync.WaitGroup
	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedMountWork) {
			defer wg.Done()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
		}(w)
	}

	wg.Wait()
}

func runIndexedParentMountWork(
	ctx context.Context,
	work []indexedMountWork,
	reports []*MountReport,
	coordinator *oneShotChildRuns,
) {
	var wg gosync.WaitGroup
	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedMountWork) {
			defer wg.Done()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
			if coordinator != nil && indexed.work.mount != nil {
				coordinator.markParentDone(ctx, indexed.work.mount.mountID)
			}
		}(w)
	}

	wg.Wait()
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
	notify parentChildWorkNotify,
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

		o.attachParentChildWorkSink(mount, nil, notify)

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
		mount:     mount,
		engine:    engine,
		parentAck: shortcutParentAckHandleForMount(mount, engine),
		fn: func(c context.Context) (*syncengine.Report, error) {
			runMode := mode
			runOpts := opts
			if mount.isFinalDrainChild() {
				runMode = syncengine.SyncBidirectional
				runOpts = syncengine.RunOptions{FullReconcile: true}
			}
			return engine.RunOnce(c, runMode, runOpts)
		},
	}, nil
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

func (o *Orchestrator) finalizeSuccessfulFinalDrainMounts(
	ctx context.Context,
	decisions *runnerDecisionSet,
	reports []*MountReport,
	parentAckers map[mountID]shortcutChildAckHandle,
) (bool, error) {
	if decisions == nil {
		return false, nil
	}
	results := classifyShortcutChildDrainResults(decisions.Mounts, reports)
	if len(results) == 0 {
		return false, nil
	}
	successful := cleanShortcutChildDrainResults(results)
	if len(successful) == 0 {
		logUnfinishedFinalDrains(results, o.logger)
		return false, nil
	}

	cleanups, err := acknowledgeSuccessfulFinalDrains(ctx, successful, decisions.Mounts, parentAckers)
	if err != nil {
		return false, err
	}
	if err := o.purgeAndAcknowledgeShortcutChildArtifacts(
		ctx,
		shortcutChildArtifactCleanupSourceFinalDrain,
		cleanups,
		cloneParentAckHandles(parentAckers),
	); err != nil {
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
	parentAckers map[mountID]shortcutChildAckHandle,
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
		if mount == nil || result.AckRef.IsZero() {
			return nil, fmt.Errorf("final-drain child mount %s is missing parent acknowledgement reference", result.MountID)
		}
		parentID := mount.childParentMountID()
		acker := parentAckers[parentID]
		if shortcutChildAckHandleIsZero(acker) {
			return nil, fmt.Errorf("parent mount %s is unavailable for final-drain acknowledgement", parentID)
		}
		snapshot, err := acker.AcknowledgeChildFinalDrain(ctx, syncengine.ShortcutChildDrainAck{
			Ref: result.AckRef,
		})
		if err != nil {
			return nil, fmt.Errorf("acknowledging final drain for child mount %s: %w", result.MountID, err)
		}
		publishedCleanups, cleanupErr := shortcutChildArtifactCleanups(map[mountID]syncengine.ShortcutChildWorkSnapshot{
			parentID: snapshot,
		})
		if cleanupErr != nil {
			return nil, cleanupErr
		}
		cleanups = append(cleanups, publishedCleanups...)
	}
	return cleanups, nil
}
