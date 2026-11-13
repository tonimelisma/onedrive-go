package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	gosync "sync"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/perf"
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
// never returns an error: individual mount errors are captured in each
// MountReport, and an engine that did not close within its bound in
// RunOnceResult.Stuck. The caller inspects the result to determine success or
// failure.
func (o *Orchestrator) RunOnce(
	ctx context.Context,
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
) (result RunOnceResult) {
	if len(o.cfg.StandaloneMounts) == 0 {
		if len(o.cfg.InitialStartupResults) > 0 {
			return RunOnceResult{
				Startup: summarizeStartupResults(o.cfg.InitialStartupResults),
			}
		}
		return RunOnceResult{}
	}
	// No fatal channel: a one-shot pass that loses its control channel keeps
	// its owner lock (the flock, not the socket) and finishes; the end is
	// logged.
	control, err := o.startControlServer(ctx, synccontrol.OwnerModeOneShot, nil, nil)
	if err != nil {
		return processFailureRunOnceResult(err)
	}
	// Closed when every mount's work is done, which is when the owner lock
	// may go.
	defer o.closeControlSocket(ctx, control)

	parentMounts, err := buildStandaloneMountSpecs(o.cfg.StandaloneMounts)
	if err != nil {
		return processFailureRunOnceResult(err)
	}
	o.setControlMountIDs(mountIDsForSpecs(parentMounts))

	o.logger.Info("Syncing drives",
		slog.Int("drives", len(parentMounts)),
		slog.String("mode", mode.String()),
	)

	childCoordinator := newOneShotChildRuns(o, mode, opts, parentMounts)
	parentWork, parentStartup, parentReports := o.prepareRunOnceWork(
		ctx,
		mode,
		parentMounts,
		o.cfg.InitialStartupResults,
		opts,
		childCoordinator.cache,
		childCoordinator.notifyParentSnapshot,
	)
	childCoordinator.registerParents(parentWork)

	runIndexedMountWork(ctx, parentWork, parentReports, childCoordinator)
	defer func() {
		result.Stuck = errors.Join(result.Stuck, o.closeRunOnceEngines(ctx, parentWork, MountProjectionStandalone))
	}()

	childCoordinator.wait()

	allReports := append([]*MountReport{}, parentReports...)
	allReports = append(allReports, childCoordinator.reports()...)
	o.logger.Info("Sync pass finished", slog.Int("drives", len(allReports)))

	startupResults := append([]MountStartupResult{}, parentStartup.Results...)
	startupResults = append(startupResults, childCoordinator.startupResults()...)
	return RunOnceResult{
		Startup: summarizeStartupResults(startupResults),
		Reports: allReports,
		Stuck:   childCoordinator.stuck(),
	}
}

// runIndexedMountWork runs every work item on a goroutine of its own and
// waits for all of them; each report lands at its item's index. With a
// coordinator (the drives of a pass, not their shortcuts), each drive that
// finishes tells it, which may start that drive's shortcuts.
func runIndexedMountWork(
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
			// MountRunner.run recovers a panic of the drive's own run. This
			// covers the work around it, so the pass still finishes and fails.
			defer func() {
				if r := recover(); r != nil {
					reports[indexed.index] = withRecoveredPanic(reports[indexed.index], indexed.work.runner, r)
				}
			}()
			reports[indexed.index] = indexed.work.runner.run(ctx, indexed.work.fn)
			if coordinator != nil && indexed.work.mount != nil {
				coordinator.markParentDone(ctx, indexed.work.mount.id())
			}
		}(w)
	}

	wg.Wait()
}

// withRecoveredPanic is a drive's report once a panic recovered around its
// run is added: joined into the error of the report the run already made, or
// a report of its own when the run made none. Either way the drive has one
// report, and the panic is not dropped.
func withRecoveredPanic(report *MountReport, runner *MountRunner, r any) *MountReport {
	var logger *slog.Logger
	name := "a drive"
	if runner != nil {
		logger = runner.logger
		name = runner.name()
	}
	err := panicguard.Recovered(logger, "while running the sync of "+name, r)

	switch {
	case report != nil:
		report.Err = errors.Join(report.Err, err)
		return report
	case runner != nil:
		return runner.report(nil, err)
	default:
		return &MountReport{Err: err}
	}
}

// processFailureRunOnceResult reports a failure that stops the whole run
// before any drive starts, such as another sync already owning the control
// socket. It is one condition, so it is one result with no drive identity:
// copying it onto every selected drive printed the same line once per drive.
// Per-drive config failures from InitialStartupResults are left out on
// purpose; the next run that gets past this point reports them.
func processFailureRunOnceResult(err error) RunOnceResult {
	return RunOnceResult{
		Startup: summarizeStartupResults([]MountStartupResult{{
			Status: classifyMountStartupError(err),
			Err:    err,
		}}),
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
	childWork *parentChildWorkSnapshots,
	notify parentChildWorkNotify,
) ([]indexedMountWork, StartupSelectionSummary, []*MountReport) {
	work := make([]indexedMountWork, 0, len(mounts))
	reports := make([]*MountReport, 0, len(mounts))
	startResults := append([]MountStartupResult(nil), initialStartup...)

	for i := range mounts {
		mount := mounts[i]
		if mount.paused() {
			startResults = append(startResults, MountStartupResult{
				SelectionIndex: mount.selectionIndex(),
				Identity:       mount.identity(),
				DisplayName:    mount.displayName(),
				Status:         MountStartupPaused,
			})
			continue
		}

		o.attachParentChildWorkSink(mount, 0, childWork, nil, notify)

		session, err := o.cfg.Runtime.SyncSession(ctx, mount.syncSessionConfig())
		if err != nil {
			startResults = append(startResults, mountStartupResultForMount(
				mount,
				fmt.Errorf("connect to OneDrive: %w", err),
			))
			continue
		}

		w, engineErr := o.buildEngineWork(ctx, mount, session, mode, opts)
		if engineErr != nil {
			startResults = append(startResults, mountStartupResultForMount(mount, engineErr))
			continue
		}
		startResults = append(startResults, MountStartupResult{
			SelectionIndex: mount.selectionIndex(),
			Identity:       mount.identity(),
			DisplayName:    mount.displayName(),
			Status:         MountStartupRunnable,
		})
		work = append(work, indexedMountWork{index: len(reports), work: w})
		reports = append(reports, nil)
	}

	return work, summarizeStartupResults(startResults), reports
}

func mountStartupResultForMount(mount *mountSpec, err error) MountStartupResult {
	return MountStartupResult{
		SelectionIndex: mount.selectionIndex(),
		Identity:       mount.identity(),
		DisplayName:    mount.displayName(),
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
		return mountWork{}, engineErr
	}

	return mountWork{
		runner: &MountRunner{
			selectionIndex: mount.selectionIndex(),
			identity:       mount.identity(),
			displayName:    mount.displayName(),
			logger:         o.logger,
		},
		mount:     mount,
		engine:    engine,
		parentAck: shortcutParentAckHandleForMount(mount, engine),
		fn: func(c context.Context) (*syncengine.Report, error) {
			// HTTP requests and retries, store transactions and transfers are
			// recorded on the collector the context carries. The mount's
			// collector rolls up to the owner aggregate and the command
			// collector, so nothing is lost there.
			c = perf.WithCollector(c, mountCollector)
			runMode := mode
			runOpts := opts
			if mount.isFinalDrainChild() {
				runMode = syncengine.SyncBidirectional
				runOpts.FullReconcile = true
				runOpts.ReleaseLocalTree = true
			}
			return engine.RunOnce(c, runMode, runOpts)
		},
	}, nil
}

// closeRunOnceEngines closes the engines of the work items of one projection
// (the drives, or one drive's shortcuts), each with the same bound as a watch
// runner's close (closeEngineWithin). An engine whose close runs over is
// reported once with every goroutine's stack, and one whose close panicked
// once with its own; the returned error joins them and wraps ErrSyncStuck or
// panicguard.ErrPanic: the pass's reports stand, but the command fails so the
// process exits and the engine's locks go.
func (o *Orchestrator) closeRunOnceEngines(
	ctx context.Context,
	work []indexedMountWork,
	projection MountProjectionKind,
) error {
	var stuck []error
	for _, item := range work {
		mount, engine := item.work.mount, item.work.engine
		if mount == nil || engine == nil || mount.projectionKind() != projection {
			continue
		}
		if err := o.closeEngineWithin(ctx, mount, engine); err != nil {
			stuck = append(stuck, err)
		}
		o.removeMountPerfCollector(mount.id().String())
	}

	return errors.Join(stuck...)
}

func (o *Orchestrator) finalizeSuccessfulFinalDrainMounts(
	ctx context.Context,
	decisions *runtimeWorkSet,
	reports []*MountReport,
	parentAckers map[mountID]shortcutChildAckHandle,
	childWork *parentChildWorkSnapshots,
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
		o.reportKeptFinalDrains(results, decisions.Mounts)
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
		childWork,
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

// reportKeptFinalDrains tells the user, once, that a removed shared folder's
// local copy stays on disk because its final sync has not finished cleanly.
// The notice repeats only when the reason changes: watch mode re-runs the
// final drain on every child pass, and the condition is already visible in
// `status`, so each repeat goes to Debug.
func (o *Orchestrator) reportKeptFinalDrains(results []shortcutChildDrainResult, mounts []*mountSpec) {
	if o.logger == nil {
		return
	}
	labels := make(map[string]string, len(mounts))
	for _, mount := range mounts {
		if mount != nil {
			labels[mount.id().String()] = mount.label()
		}
	}
	for _, result := range results {
		if result.Status == shortcutChildDrainClean {
			continue
		}
		label := labels[result.MountID]
		if label == "" {
			label = result.MountID
		}
		if !o.markKeptFinalDrainNotice(result) {
			o.logger.Debug("removed shared folder final drain still not finished",
				slog.String("mount_id", result.MountID),
				slog.String("status", string(result.Status)),
				slog.String("detail", result.Detail),
			)
			continue
		}
		o.logger.Info(keptFinalDrainNotice(label, result),
			slog.String("detail", result.Detail),
		)
	}
}

// keptFinalDrainNotice is the plain sentence for a removed shared folder
// whose local copy is kept, by why its final sync did not finish. A blocked
// drain says what holds the copy, its release's leftovers named.
func keptFinalDrainNotice(label string, result shortcutChildDrainResult) string {
	switch result.Status {
	case shortcutChildDrainRootUnavailable:
		return fmt.Sprintf("Keeping removed shared folder %s: its local folder is unavailable, "+
			"so its last changes can't be synced yet", label)
	case shortcutChildDrainBlocked:
		return fmt.Sprintf("Keeping the local copy of removed shared folder %s: %s; "+
			"run 'onedrive-go status' for details", label, strings.TrimPrefix(result.Detail, childSyncDetailPrefix))
	case shortcutChildDrainClean, shortcutChildDrainFailed:
	}
	return fmt.Sprintf("Keeping the local copy of removed shared folder %s until its last sync "+
		"finishes; it is retried on the next sync", label)
}

// markKeptFinalDrainNotice records that result is about to be reported and
// says whether it is new: first seen for the mount, or with a different
// status or detail. One-shot parents finalize concurrently, so the memory is
// guarded by keptDrainNoticesMu.
func (o *Orchestrator) markKeptFinalDrainNotice(result shortcutChildDrainResult) bool {
	fingerprint := string(result.Status) + "\x00" + result.Detail

	o.keptDrainNoticesMu.Lock()
	defer o.keptDrainNoticesMu.Unlock()

	if o.keptDrainNotices == nil {
		o.keptDrainNotices = make(map[string]string)
	}
	if o.keptDrainNotices[result.MountID] == fingerprint {
		return false
	}
	o.keptDrainNotices[result.MountID] = fingerprint
	return true
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
			mountByID[mount.id().String()] = mount
		}
	}
	cleanups := make([]shortcutChildArtifactCleanup, 0, len(successful))
	for _, result := range successful {
		mount := mountByID[result.MountID]
		if mount == nil || result.AckRef.IsZero() {
			return nil, fmt.Errorf("removed shortcut %s has no reference to its drive's removal record", result.MountID)
		}
		parentID := mount.childParentMountID()
		acker := parentAckers[parentID]
		if shortcutChildAckHandleIsZero(acker) {
			return nil, fmt.Errorf("drive %s is not running, so removed shortcut %s can't be released yet", parentID, mount.label())
		}
		snapshot, err := acker.AcknowledgeChildFinalDrain(ctx, syncengine.ShortcutChildDrainAck{
			Ref: result.AckRef,
		})
		if err != nil {
			return nil, fmt.Errorf("release removed shortcut %s: %w", mount.label(), err)
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
