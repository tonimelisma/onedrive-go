package multisync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// watchRunner holds per-mount state for a running watch-mode engine. All
// fields but done are owned by the RunWatch goroutine.
type watchRunner struct {
	mount     *mountSpec
	engine    engineRunner
	parentAck shortcutChildAckHandle
	cancel    context.CancelFunc
	done      chan struct{} // closed exactly once by the goroutine started in startWatchRunner
	// startedAt tells a runner that failed straight away from one that ran
	// for a while first; see runnerHealthyRunDuration.
	startedAt time.Time
	// generation tells this runner apart from every other runner started
	// under its mount ID, and its events carry it. mount may be swapped for
	// an equivalent spec; the generation never changes.
	generation uint64
	// stopRequestedAt is when the runner was first told to stop; its stop
	// bound runs from then (retireWatchRunner).
	stopRequestedAt time.Time
	// stuckErr is set once the runner missed a stop or close bound, or its
	// engine's close panicked. A stuck runner stays in the runner set and is
	// never closed or replaced.
	stuckErr error
}

type watchRunnerEvent struct {
	mountID mountID
	// generation is the sending runner's. An event whose generation is not
	// that of the runner now under mountID came from a runner the
	// orchestrator already stopped, and changes nothing.
	generation            uint64
	report                *syncengine.Report
	err                   error
	parentSnapshot        syncengine.ShortcutChildWorkSnapshot
	parentSnapshotChanged bool
}

// startNextWatchRunner starts the runner for mount under the next generation,
// and points the mount's parent child-work sink at that generation first, so
// the snapshots the new runner publishes carry it.
func (o *Orchestrator) startNextWatchRunner(
	ctx context.Context,
	mount *mountSpec,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) (*watchRunner, error) {
	generation := o.nextWatchRunnerGeneration()
	o.attachParentChildWorkSink(mount, generation, childWork, runnerEvents, nil)

	return o.startWatchRunner(ctx, mount, generation, mode, opts, runnerEvents)
}

// startWatchRunner starts mount's engine on a goroutine of its own. The
// goroutine reports a failure, or a final drain's result, on runnerEvents,
// stamped with generation; it gives up on the send once the runner is
// canceled, because the loop that canceled it may be waiting for done.
func (o *Orchestrator) startWatchRunner(
	ctx context.Context,
	mount *mountSpec,
	generation uint64,
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
	// HTTP requests and retries, store transactions and transfers are recorded
	// on the collector the context carries. The mount's collector rolls up to
	// the owner aggregate and the command collector, so nothing is lost there.
	mountCtx = perf.WithCollector(mountCtx, mountCollector)
	done := make(chan struct{})

	wr := &watchRunner{
		mount:      mount,
		engine:     engine,
		parentAck:  shortcutParentAckHandleForMount(mount, engine),
		cancel:     mountCancel,
		done:       done,
		startedAt:  o.clockNow(),
		generation: generation,
	}

	go func() {
		defer close(done)
		defer mountCancel()
		defer o.removeMountPerfCollector(mount.id().String())
		// runWatchRunnerEngine turns a panic of the engine into runErr. This
		// covers the hand-off after it: a panic there is the runner's failure
		// all the same, sent as its event so the loop restarts the drive
		// (R-6.3.8).
		defer func() {
			if r := recover(); r != nil {
				sendWatchRunnerEvent(mountCtx, runnerEvents, watchRunnerEvent{
					mountID: mount.id(), generation: generation,
					err: recoveredSyncPanic(o.logger, mount.label(), mount.id().String(), r),
				})
			}
		}()

		report, runErr := o.runWatchRunnerEngine(mountCtx, mount, engine, mode, opts)

		if mount.isFinalDrainChild() {
			// The main loop reports a failed drain once; this goroutine only
			// hands it over. When the orchestrator canceled the drain itself
			// (reload, parent snapshot change, shutdown), the error is just
			// that cancellation and nobody needs to hear about it.
			if runErr != nil && mountCtx.Err() != nil {
				o.logger.Debug("final-drain runner stopped",
					slog.String("mount_id", mount.id().String()),
					slog.Any("error", runErr),
				)
				return
			}
			sendWatchRunnerEvent(mountCtx, runnerEvents, watchRunnerEvent{
				mountID: mount.id(), generation: generation, report: report, err: runErr,
			})
			return
		}

		if runErr != nil && mountCtx.Err() == nil {
			// Reported once, in user words, by handleWatchRunnerEvent.
			o.logger.Debug("watch runner exited",
				slog.String("mount_id", mount.id().String()),
				slog.Any("error", runErr),
			)
			sendWatchRunnerEvent(mountCtx, runnerEvents, watchRunnerEvent{
				mountID: mount.id(), generation: generation, err: runErr,
			})
		}
	}()

	o.logger.Info("Started syncing drive",
		slog.String("drive", mount.label()),
		slog.String("mount_id", mount.id().String()),
	)

	return wr, nil
}

// sendWatchRunnerEvent hands event to the watch loop, or drops it once
// runnerCtx is canceled. A canceled runner's event describes a runner the
// loop is stopping or already stopped, which the generation check would
// ignore anyway; blocking on a full channel instead held the runner's done
// open while the loop waited for it.
func sendWatchRunnerEvent(runnerCtx context.Context, runnerEvents chan<- watchRunnerEvent, event watchRunnerEvent) {
	if runnerEvents == nil {
		return
	}
	select {
	case runnerEvents <- event:
	case <-runnerCtx.Done():
	}
}

// runWatchRunnerEngine runs the mount's engine -- the final drain for a
// removed shortcut child, watch mode for everything else -- and is the panic
// boundary of the runner goroutine. A panic becomes this mount's error, so
// handleWatchRunnerEvent restarts the one mount with the usual backoff and
// its siblings keep running; without the recover it unwound the whole process
// past every other mount and the drain the design promises.
func (o *Orchestrator) runWatchRunnerEngine(
	ctx context.Context,
	mount *mountSpec,
	engine engineRunner,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
) (report *syncengine.Report, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		report = nil
		err = recoveredSyncPanic(o.logger, mount.label(), mount.id().String(), r)
	}()

	// The engine's errors already name what it was doing; see the
	// engineRunnerAdapter comment for why this layer adds no prefix.
	if mount.isFinalDrainChild() {
		//nolint:wrapcheck // The engine error carries its own context; this layer only reports it.
		return engine.RunOnce(ctx, syncengine.SyncBidirectional, syncengine.RunOptions{
			FullReconcile:    true,
			ReleaseLocalTree: true,
		})
	}

	//nolint:wrapcheck // The engine error carries its own context; this layer only reports it.
	return nil, engine.RunWatch(ctx, mode, opts)
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

// handleWatchRunnerEvent applies one runner event. It returns an error only
// when a runner it had to stop missed its bound (ErrSyncStuck), which
// ends the watch.
func (o *Orchestrator) handleWatchRunnerEvent(
	ctx context.Context,
	event watchRunnerEvent,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) error {
	if event.parentSnapshotChanged {
		if !parentSnapshotEventIsCurrent(event, runners, childWork) {
			o.logger.Debug("ignoring stale parent child work snapshot event",
				slog.String("parent_mount_id", event.mountID.String()),
			)
			return nil
		}
		return o.reconcileWatchRunnersForParent(ctx, event.mountID, event.parentSnapshot, mode, opts, runners, runnerEvents, childWork)
	}

	wr := runners[event.mountID]
	if wr == nil || wr.generation != event.generation {
		// The orchestrator already stopped the sender (reload, parent
		// change, or shutdown), and may have started a new runner under the
		// same mount ID; the event was queued before that and describes
		// nothing current.
		o.logger.Debug("ignoring event from a runner that was already stopped",
			slog.String("mount_id", event.mountID.String()),
			slog.Uint64("generation", event.generation),
		)
		return nil
	}

	// The sender has returned or is about to: only its deferred cleanup is
	// left, so the bounded stop returns at once unless that cleanup hangs.
	if err := o.retireWatchRunner(ctx, runners, event.mountID); err != nil {
		return err
	}
	if wr.mount != nil && wr.mount.projectionKind() == MountProjectionStandalone {
		childWork.forget(event.mountID)
		if _, err := o.stopChildWatchRunnersForParent(ctx, runners, event.mountID); err != nil {
			return err
		}
	}

	if wr.mount.isFinalDrainChild() {
		o.handleFinalDrainWatchRunnerEvent(ctx, runners, wr, event, childWork)
	}

	if event.err != nil && ctx.Err() == nil {
		o.reportWatchRunnerExit(wr, event.err)
	}

	return o.reconcileWatchRunners(ctx, mode, opts, runners, runnerEvents, childWork)
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
			slog.Any("error", err),
		)
	}
}

// parentSnapshotEventIsCurrent accepts a parent snapshot event only from the
// parent runner now running under its mount ID, and only while the snapshot
// is still the one cached for that parent. Equal content is not enough: a
// replaced parent's snapshot can equal its successor's cached one.
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
	if wr.generation != event.generation {
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
			slog.Any("error", err),
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
		watchParentAckers(runners),
		childWork,
	)
	if err != nil {
		o.logger.Warn(fmt.Sprintf("Couldn't finish removing shortcut %s; will retry", wr.mount.label()),
			slog.Any("error", err),
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

// watchParentAckers maps each running drive to the handle its shortcut
// children acknowledge a final drain or an artifact cleanup through.
func watchParentAckers(runners map[mountID]*watchRunner) map[mountID]shortcutChildAckHandle {
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
