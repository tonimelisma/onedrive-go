package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	parentAck syncengine.ShortcutChildAckCapability
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

	childCoordinator := newRunOnceChildCoordinator(o, mode, opts, parentMounts)
	parentWork, parentStartup, parentReports := o.prepareRunOnceWork(
		ctx,
		mode,
		parentMounts,
		o.cfg.InitialStartupResults,
		opts,
		childCoordinator.notifyParentPublication,
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
	coordinator *runOnceChildCoordinator,
) {
	var wg gosync.WaitGroup
	for _, w := range work {
		wg.Add(1)

		go func(indexed indexedMountWork) {
			defer wg.Done()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
			if coordinator != nil && indexed.work.mount != nil {
				coordinator.markParentDone(indexed.work.mount.mountID)
			}
		}(w)
	}

	wg.Wait()
}

type runOnceChildCoordinator struct {
	orchestrator *Orchestrator
	mode         syncengine.SyncMode
	opts         syncengine.RunOptions

	mu            gosync.Mutex
	parents       map[mountID]*mountSpec
	parentDone    map[mountID]chan struct{}
	parentAckers  map[mountID]syncengine.ShortcutChildAckCapability
	started       map[mountID]bool
	startup       []MountStartupResult
	childReports  []*MountReport
	childRunGroup gosync.WaitGroup
}

func newRunOnceChildCoordinator(
	orchestrator *Orchestrator,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
	parents []*mountSpec,
) *runOnceChildCoordinator {
	parentByID := make(map[mountID]*mountSpec, len(parents))
	for _, parent := range parents {
		if parent == nil {
			continue
		}
		parentByID[parent.mountID] = cloneMountSpec(parent)
	}
	return &runOnceChildCoordinator{
		orchestrator: orchestrator,
		mode:         mode,
		opts:         opts,
		parents:      parentByID,
		parentDone:   make(map[mountID]chan struct{}),
		parentAckers: make(map[mountID]syncengine.ShortcutChildAckCapability),
		started:      make(map[mountID]bool),
	}
}

func (c *runOnceChildCoordinator) registerParents(work []indexedMountWork) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, item := range work {
		if item.work.mount == nil || item.work.mount.projectionKind != MountProjectionStandalone {
			continue
		}
		parentID := item.work.mount.mountID
		if _, found := c.parentDone[parentID]; !found {
			c.parentDone[parentID] = make(chan struct{})
		}
		if item.work.parentAck != nil {
			c.parentAckers[parentID] = item.work.parentAck
		}
	}
}

func (c *runOnceChildCoordinator) notifyParentPublication(ctx context.Context, parentID mountID) error {
	if c == nil || parentID == "" {
		return nil
	}

	c.mu.Lock()
	if c.started[parentID] {
		c.mu.Unlock()
		return nil
	}
	parent := cloneMountSpec(c.parents[parentID])
	if parent == nil {
		c.mu.Unlock()
		return nil
	}
	c.started[parentID] = true
	c.childRunGroup.Add(1)
	c.mu.Unlock()

	go c.runChildrenForParent(ctx, parentID, parent)
	return nil
}

func (c *runOnceChildCoordinator) markParentDone(parentID mountID) {
	if c == nil || parentID == "" {
		return
	}
	c.mu.Lock()
	done := c.parentDone[parentID]
	if done != nil {
		delete(c.parentDone, parentID)
	}
	c.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (c *runOnceChildCoordinator) wait() {
	if c == nil {
		return
	}
	c.childRunGroup.Wait()
}

func (c *runOnceChildCoordinator) startupResults() []MountStartupResult {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	results := append([]MountStartupResult(nil), c.startup...)
	sort.SliceStable(results, func(i, j int) bool {
		return results[i].SelectionIndex < results[j].SelectionIndex
	})
	return results
}

func (c *runOnceChildCoordinator) reports() []*MountReport {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	reports := append([]*MountReport(nil), c.childReports...)
	sort.SliceStable(reports, func(i, j int) bool {
		if reports[i] == nil || reports[j] == nil {
			return reports[j] != nil
		}
		return reports[i].SelectionIndex < reports[j].SelectionIndex
	})
	return reports
}

func (c *runOnceChildCoordinator) runChildrenForParent(
	ctx context.Context,
	parentID mountID,
	parent *mountSpec,
) {
	defer c.childRunGroup.Done()
	compiled, err := c.orchestrator.compileRuntimeMountSetForParent(parent)
	if err != nil {
		c.appendStartup(MountStartupResult{
			SelectionIndex: parent.selectionIndex,
			Identity:       parent.identity(),
			DisplayName:    parent.displayName,
			Status:         MountStartupFatal,
			Err:            fmt.Errorf("building child mount specs after parent publication: %w", err),
		})
		return
	}

	childMounts := filterMountSpecsByProjection(compiled.Mounts, MountProjectionChild)
	childWork, childStartup, childReports := c.orchestrator.prepareRunOnceWork(
		ctx,
		c.mode,
		childMounts,
		compiled.Skipped,
		c.opts,
		nil,
	)
	c.appendStartup(childStartup.Results...)
	runIndexedMountWork(ctx, childWork, childReports)
	c.orchestrator.closeRunOnceChildEngines(ctx, childWork)
	c.appendReports(childReports...)

	c.waitParentDone(parentID)
	parentAckers := c.parentAckersFor(parentID)
	if purgeErr := c.orchestrator.purgeShortcutChildArtifactsForCompiled(
		ctx,
		compiled,
		cloneParentAckCapabilities(parentAckers),
	); purgeErr != nil {
		c.orchestrator.logger.Warn("purging shortcut child state artifacts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", purgeErr.Error()),
		)
	}
	if finalized, finalizeErr := c.orchestrator.finalizeSuccessfulFinalDrainMounts(
		ctx,
		compiled,
		childReports,
		parentAckers,
	); finalizeErr != nil {
		c.orchestrator.logger.Warn("finalizing drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
			slog.String("error", finalizeErr.Error()),
		)
	} else if finalized {
		c.orchestrator.logger.Info("finalized drained shortcut child mounts",
			slog.String("parent_mount_id", parentID.String()),
		)
	}
}

func (c *runOnceChildCoordinator) waitParentDone(parentID mountID) {
	c.mu.Lock()
	done := c.parentDone[parentID]
	c.mu.Unlock()
	if done != nil {
		<-done
	}
}

func (c *runOnceChildCoordinator) parentAckersFor(parentID mountID) map[mountID]syncengine.ShortcutChildAckCapability {
	c.mu.Lock()
	defer c.mu.Unlock()
	acker := c.parentAckers[parentID]
	if acker == nil {
		return nil
	}
	return map[mountID]syncengine.ShortcutChildAckCapability{parentID: acker}
}

func (c *runOnceChildCoordinator) appendStartup(results ...MountStartupResult) {
	if c == nil || len(results) == 0 {
		return
	}
	c.mu.Lock()
	c.startup = append(c.startup, results...)
	c.mu.Unlock()
}

func (c *runOnceChildCoordinator) appendReports(reports ...*MountReport) {
	if c == nil || len(reports) == 0 {
		return
	}
	c.mu.Lock()
	c.childReports = append(c.childReports, reports...)
	c.mu.Unlock()
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
	notify parentRunnerPublicationNotify,
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

		o.attachParentRunnerPublicationSink(mount, nil, notify)

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
		parentAck: shortcutParentAckCapabilityForMount(mount, engine),
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
	compiled *compiledMountSet,
	reports []*MountReport,
	parentAckers map[mountID]syncengine.ShortcutChildAckCapability,
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
	if err := o.acknowledgeShortcutChildArtifactCleanups(ctx, cleanups, cloneParentAckCapabilities(parentAckers)); err != nil {
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
	parentAckers map[mountID]syncengine.ShortcutChildAckCapability,
) ([]shortcutChildArtifactCleanup, error) {
	mountByID := make(map[string]*mountSpec, len(mounts))
	for _, mount := range mounts {
		if mount != nil {
			mountByID[mount.mountID.String()] = mount
		}
	}
	parentByID := indexStandaloneMounts(filterMountSpecsByProjection(mounts, MountProjectionStandalone))
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
		cleanups = append(cleanups, shortcutChildArtifactCleanups(parentByID, map[mountID]syncengine.ShortcutChildRunnerPublication{
			mount.parentMountID: snapshot,
		})...)
	}
	return cleanups, nil
}

func cloneParentAckCapabilities(
	ackersByParent map[mountID]syncengine.ShortcutChildAckCapability,
) map[mountID]syncengine.ShortcutChildAckCapability {
	ackers := make(map[mountID]syncengine.ShortcutChildAckCapability, len(ackersByParent))
	for id, acker := range ackersByParent {
		ackers[id] = acker
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
		scope := shortcutChildArtifactScope{mountID: cleanup.mountID, localRoot: cleanup.localRoot}
		if err := purgeShortcutChildArtifacts(ctx, scope, logger); err != nil {
			errs = append(errs, fmt.Errorf("purging final-drain child mount %s: %w", cleanup.mountID, err))
		}
	}
	return errors.Join(errs...)
}
