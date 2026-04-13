package multisync

import (
	"context"
	"fmt"
	"log/slog"
	gosync "sync"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// engineRunner is the interface the Orchestrator uses to run sync passes.
// Implemented by *sync.Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode syncengine.Mode, opts syncengine.RunOptions) (*syncengine.Report, error)
	RunWatch(ctx context.Context, mode syncengine.Mode, opts syncengine.WatchOptions) error
	Close(ctx context.Context) error
}

type engineFactoryRequest struct {
	Session       *driveops.Session
	Drive         *config.ResolvedDrive
	Logger        *slog.Logger
	VerifyDrive   bool
	PerfCollector *perf.Collector
}

// engineFactoryFunc creates an engineRunner from the resolved drive/session
// pair used by production orchestration. Tests inject mocks at this boundary.
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
	DebugEventHook    func(syncengine.DebugEvent)
	PerfParent        *perf.Collector
}

// Orchestrator manages per-drive sync runners. It is always used, even for a
// single drive, so one-shot and watch mode share the same top-level lifecycle.
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
			engine, err := syncengine.NewDriveEngine(ctx, req.Session, req.Drive, syncengine.DriveEngineOptions{
				Logger:        req.Logger,
				PerfCollector: req.PerfCollector,
				VerifyDrive:   req.VerifyDrive,
			})
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

// RunOnce executes a single sync pass for all configured drives. Each drive
// runs in its own goroutine via a DriveRunner with panic recovery. RunOnce
// never returns an error — individual drive errors are captured in each
// syncengine.Report. The caller inspects reports to determine success or failure.
func (o *Orchestrator) RunOnce(ctx context.Context, mode syncengine.Mode, opts syncengine.RunOptions) []*DriveReport {
	drives := o.cfg.Drives
	if len(drives) == 0 {
		return nil
	}

	control, err := o.startControlServer(ctx, synccontrol.OwnerModeOneShot, nil)
	if err != nil {
		return controlFailureReports(drives, err)
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
		slog.Int("drives", len(drives)),
		slog.String("mode", mode.String()),
	)

	work := o.prepareDriveWork(ctx, mode, opts)

	// Run all drives concurrently.
	reports := make([]*DriveReport, len(work))
	var wg gosync.WaitGroup

	for i, w := range work {
		wg.Add(1)

		go func(idx int, dw driveWork) {
			defer wg.Done()
			reports[idx] = dw.runner.run(ctx, dw.fn)
		}(i, w)
	}

	wg.Wait()

	o.logger.Info("orchestrator RunOnce complete", slog.Int("reports", len(reports)))

	return reports
}

func controlFailureReports(drives []*config.ResolvedDrive, err error) []*DriveReport {
	reports := make([]*DriveReport, len(drives))
	for i, rd := range drives {
		reports[i] = &DriveReport{
			CanonicalID: rd.CanonicalID,
			DisplayName: rd.DisplayName,
			Err:         err,
		}
	}
	return reports
}

// prepareDriveWork resolves sessions and builds engines for each configured
// drive. Errors are captured as closures that return the error when the
// DriveRunner executes — no early abort for individual drive failures.
func (o *Orchestrator) prepareDriveWork(ctx context.Context, mode syncengine.Mode, opts syncengine.RunOptions) []driveWork {
	drives := o.cfg.Drives
	work := make([]driveWork, 0, len(drives))

	for _, rd := range drives {
		session, err := o.cfg.Runtime.SyncSession(ctx, rd)
		if err != nil {
			capturedErr := err
			capturedCID := rd.CanonicalID

			work = append(work, driveWork{
				runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
				fn: func(_ context.Context) (*syncengine.Report, error) {
					return nil, fmt.Errorf("session error for drive %s: %w", capturedCID, capturedErr)
				},
			})

			continue
		}

		w := o.buildEngineWork(ctx, rd, session, mode, opts)
		work = append(work, w)
	}

	return work
}

// buildEngineWork creates a driveWork item for a successfully-resolved drive.
// If engine creation fails, the error is captured and reported at run time.
func (o *Orchestrator) buildEngineWork(
	ctx context.Context, rd *config.ResolvedDrive, session *driveops.Session, mode syncengine.Mode, opts syncengine.RunOptions,
) driveWork {
	driveCollector := o.registerDrivePerfCollector(rd.CanonicalID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Drive:         rd,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: driveCollector,
	})
	if engineErr != nil {
		o.removeDrivePerfCollector(rd.CanonicalID.String())
		capturedErr := engineErr

		return driveWork{
			runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
			fn: func(_ context.Context) (*syncengine.Report, error) {
				return nil, capturedErr
			},
		}
	}

	return driveWork{
		runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
		fn: func(c context.Context) (*syncengine.Report, error) {
			defer func() {
				o.removeDrivePerfCollector(rd.CanonicalID.String())
				if closeErr := engine.Close(c); closeErr != nil {
					o.logger.Warn("engine close error",
						slog.String("drive", rd.CanonicalID.String()),
						slog.String("error", closeErr.Error()))
				}
			}()

			return engine.RunOnce(c, mode, opts)
		},
	}
}

// ---------------------------------------------------------------------------
// RunWatch — multi-drive daemon mode
// ---------------------------------------------------------------------------

// watchRunner holds per-drive state for a running watch-mode engine.
type watchRunner struct {
	canonID        driveid.CanonicalID
	engine         engineRunner
	cancel         context.CancelFunc
	userIntentWake chan struct{}
	done           chan struct{} // closed exactly once by the goroutine started in startWatchRunner
}

// RunWatch runs all configured (non-paused) drives in watch mode. On
// control-socket reload, it re-reads the config file and diffs the active drive
// set: stopped drives are removed, new drives are started. Returns nil on
// clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode syncengine.Mode, opts syncengine.WatchOptions) error {
	drives := o.cfg.Drives
	if len(drives) == 0 {
		return fmt.Errorf("sync: no drives configured")
	}

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("drives", len(drives)),
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

	// Build runners for all active (non-paused) drives.
	runners := make(map[driveid.CanonicalID]*watchRunner)

	for _, rd := range drives {
		// Pause semantics are handled by config.Drive.IsPaused() — the
		// ResolvedDrive.Paused field is already expiry-aware after
		// buildResolvedDrive uses IsPaused(time.Now()).
		if rd.Paused {
			o.logger.Info("skipping paused drive",
				slog.String("drive", rd.CanonicalID.String()),
			)

			continue
		}

		wr, err := o.startWatchRunner(ctx, rd, mode, opts)
		if err != nil {
			o.logger.Error("failed to start watch runner",
				slog.String("drive", rd.CanonicalID.String()),
				slog.String("error", err.Error()),
			)

			continue
		}

		runners[rd.CanonicalID] = wr
	}

	defer func() {
		for cid, wr := range runners {
			wr.cancel()
			<-wr.done

			if closeErr := wr.engine.Close(ctx); closeErr != nil {
				o.logger.Warn("engine close error on shutdown",
					slog.String("drive", cid.String()),
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

// startWatchRunner creates and starts a watch-mode engine for a single drive.
func (o *Orchestrator) startWatchRunner(
	ctx context.Context, rd *config.ResolvedDrive, mode syncengine.Mode, opts syncengine.WatchOptions,
) (*watchRunner, error) {
	session, err := o.cfg.Runtime.SyncSession(ctx, rd)
	if err != nil {
		return nil, fmt.Errorf("session error for drive %s: %w", rd.CanonicalID, err)
	}

	driveCollector := o.registerDrivePerfCollector(rd.CanonicalID.String())
	engine, engineErr := o.engineFactory(ctx, engineFactoryRequest{
		Session:       session,
		Drive:         rd,
		Logger:        o.logger,
		VerifyDrive:   true,
		PerfCollector: driveCollector,
	})
	if engineErr != nil {
		o.removeDrivePerfCollector(rd.CanonicalID.String())
		return nil, fmt.Errorf("engine creation failed for %s: %w", rd.CanonicalID, engineErr)
	}

	driveCtx, driveCancel := context.WithCancel(ctx)
	userIntentWake := make(chan struct{}, 1)
	done := make(chan struct{})

	wr := &watchRunner{
		canonID:        rd.CanonicalID,
		engine:         engine,
		cancel:         driveCancel,
		userIntentWake: userIntentWake,
		done:           done,
	}
	opts.UserIntentWake = userIntentWake

	go func() {
		defer close(done)
		defer driveCancel()
		defer o.removeDrivePerfCollector(rd.CanonicalID.String())

		if watchErr := engine.RunWatch(driveCtx, mode, opts); watchErr != nil {
			// Context cancellation is normal shutdown — don't log as error.
			if driveCtx.Err() == nil {
				o.logger.Error("watch runner exited with error",
					slog.String("drive", rd.CanonicalID.String()),
					slog.String("error", watchErr.Error()),
				)
			}
		}
	}()

	o.logger.Info("watch runner started",
		slog.String("drive", rd.CanonicalID.String()),
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

// reload re-reads the config file, diffs the active drive set against running
// runners, stops removed/paused drives, and starts newly added/resumed drives.
func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.Mode, opts syncengine.WatchOptions,
	runners map[driveid.CanonicalID]*watchRunner,
) {
	newCfg, err := config.LoadOrDefault(o.cfg.Holder.Path(), o.logger)
	if err != nil {
		o.logger.Warn("config reload failed, keeping current state",
			slog.String("error", err.Error()),
		)

		return
	}

	// Clear expired timed pauses before resolving, so newly unpaused drives
	// are included in the active set. Pause semantics are owned by the config
	// package — the orchestrator is a consumer, not an implementor.
	config.ClearExpiredPauses(o.cfg.Holder.Path(), newCfg, time.Now(), o.logger)

	// Resolve non-paused drives from the new config. Use the same selectors
	// as initial startup (none — all drives).
	newDrives, resolveErr := config.ResolveDrives(newCfg, nil, false, o.logger)
	if resolveErr != nil {
		o.logger.Warn("drive resolution failed after pause clearing, keeping current state",
			slog.String("error", resolveErr.Error()),
		)

		return
	}

	// Build set of new active drive CIDs.
	newActive := make(map[driveid.CanonicalID]*config.ResolvedDrive, len(newDrives))
	for _, rd := range newDrives {
		newActive[rd.CanonicalID] = rd
	}

	// Stop runners for removed/paused drives.
	var stopped int

	for cid, wr := range runners {
		if _, ok := newActive[cid]; ok {
			continue
		}

		o.logger.Info("stopping watch runner for removed/paused drive",
			slog.String("drive", cid.String()),
		)

		wr.cancel()
		<-wr.done

		if closeErr := wr.engine.Close(ctx); closeErr != nil {
			o.logger.Warn("engine close error during reload",
				slog.String("drive", cid.String()),
				slog.String("error", closeErr.Error()),
			)
		}

		delete(runners, cid)
		stopped++
	}

	// Start runners for newly added/resumed drives.
	var started int

	for cid, rd := range newActive {
		if _, ok := runners[cid]; ok {
			continue // already running
		}

		wr, startErr := o.startWatchRunner(ctx, rd, mode, opts)
		if startErr != nil {
			o.logger.Error("failed to start watch runner during reload",
				slog.String("drive", cid.String()),
				slog.String("error", startErr.Error()),
			)

			continue
		}

		runners[cid] = wr
		started++
	}

	// Single-point config update — both Orchestrator and SessionRuntime
	// read through the shared Holder.
	o.cfg.Holder.Update(newCfg)

	// Flush cached token sources so the next Session() call re-reads
	// token files from disk. Handles logout + re-login between reloads.
	o.cfg.Runtime.FlushTokenCache()

	o.logger.Info("config reload complete",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
	)
}
