package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	gosync "sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// engineRunner is the interface the Orchestrator uses to run sync passes.
// Implemented by *Engine; mock implementations are used in tests.
type engineRunner interface {
	RunOnce(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) (*synctypes.SyncReport, error)
	RunWatch(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error
	Close(ctx context.Context) error
}

// engineFactoryFunc creates an engineRunner from an synctypes.EngineConfig.
// The real implementation calls NewEngine; tests inject mocks.
type engineFactoryFunc func(ctx context.Context, cfg *synctypes.EngineConfig) (engineRunner, error)

// OrchestratorConfig holds the inputs for creating an Orchestrator.
// The CLI layer populates this from resolved config and HTTP clients.
// Config and config path are accessed via Holder — a single source of truth
// shared with SessionProvider. SIGHUP reload updates config in one place.
type OrchestratorConfig struct {
	Holder     *config.Holder
	Drives     []*config.ResolvedDrive
	Provider   *driveops.SessionProvider // token caching + Session creation
	Logger     *slog.Logger
	SIGHUPChan <-chan os.Signal // injectable for tests; nil uses no-op channel
}

// Orchestrator manages per-drive sync runners. It is ALWAYS used, even
// for a single drive — no separate single-drive code path (MULTIDRIVE.md §11.7).
type Orchestrator struct {
	cfg           *OrchestratorConfig
	engineFactory engineFactoryFunc // injectable for tests
	logger        *slog.Logger
}

// NewOrchestrator creates an Orchestrator with real Engine factory.
// Token/client caching is handled by the SessionProvider in cfg.Provider.
// Tests inject stubs via cfg.Provider.TokenSourceFn and engineFactory.
func NewOrchestrator(cfg *OrchestratorConfig) *Orchestrator {
	return &Orchestrator{
		cfg: cfg,
		engineFactory: func(ctx context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
			return NewEngine(ctx, ecfg)
		},
		logger: cfg.Logger,
	}
}

// driveWork pairs a DriveRunner with the sync function it will execute.
type driveWork struct {
	runner *DriveRunner
	fn     func(context.Context) (*synctypes.SyncReport, error)
}

// RunOnce executes a single sync pass for all configured drives. Each drive
// runs in its own goroutine via a DriveRunner with panic recovery. RunOnce
// never returns an error — individual drive errors are captured in each
// synctypes.DriveReport. The caller inspects reports to determine success or failure.
func (o *Orchestrator) RunOnce(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) []*synctypes.DriveReport {
	drives := o.cfg.Drives
	if len(drives) == 0 {
		return nil
	}

	o.logger.Info("orchestrator starting RunOnce",
		slog.Int("drives", len(drives)),
		slog.String("mode", mode.String()),
	)

	work := o.prepareDriveWork(ctx, mode, opts)

	// Run all drives concurrently.
	reports := make([]*synctypes.DriveReport, len(work))
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

// prepareDriveWork resolves sessions and builds engines for each configured
// drive. Errors are captured as closures that return the error when the
// DriveRunner executes — no early abort for individual drive failures.
func (o *Orchestrator) prepareDriveWork(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) []driveWork {
	drives := o.cfg.Drives
	work := make([]driveWork, 0, len(drives))

	for _, rd := range drives {
		session, err := o.cfg.Provider.Session(ctx, rd)
		if err != nil {
			capturedErr := err
			capturedCID := rd.CanonicalID

			work = append(work, driveWork{
				runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
				fn: func(_ context.Context) (*synctypes.SyncReport, error) {
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
	ctx context.Context, rd *config.ResolvedDrive, session *driveops.Session, mode synctypes.SyncMode, opts synctypes.RunOpts,
) driveWork {
	minFree, parseErr := config.ParseSize(rd.MinFreeSpace)
	if parseErr != nil {
		capturedErr := fmt.Errorf("invalid min_free_space %q: %w", rd.MinFreeSpace, parseErr)

		return driveWork{
			runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
			fn: func(_ context.Context) (*synctypes.SyncReport, error) {
				return nil, capturedErr
			},
		}
	}

	engine, engineErr := o.engineFactory(ctx, &synctypes.EngineConfig{
		DBPath:             rd.StatePath(),
		SyncRoot:           rd.SyncDir,
		DataDir:            config.DefaultDataDir(),
		DriveID:            session.DriveID,
		Fetcher:            session.Meta,
		Items:              session.Meta,
		Downloads:          session.Transfer,
		Uploads:            session.Transfer,
		DriveVerifier:      session.Meta,
		Logger:             o.logger,
		UseLocalTrash:      rd.UseLocalTrash,
		TransferWorkers:    rd.TransferWorkers,
		CheckWorkers:       rd.CheckWorkers,
		BigDeleteThreshold: rd.BigDeleteThreshold,
		MinFreeSpace:       minFree,
	})
	if engineErr != nil {
		capturedErr := engineErr

		return driveWork{
			runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
			fn: func(_ context.Context) (*synctypes.SyncReport, error) {
				return nil, capturedErr
			},
		}
	}

	return driveWork{
		runner: &DriveRunner{canonID: rd.CanonicalID, displayName: rd.DisplayName},
		fn: func(c context.Context) (*synctypes.SyncReport, error) {
			defer func() {
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
	canonID driveid.CanonicalID
	engine  engineRunner
	cancel  context.CancelFunc
	done    chan struct{}
}

// RunWatch runs all configured (non-paused) drives in watch mode. On SIGHUP,
// it re-reads the config file and diffs the active drive set: stopped drives
// are removed, new drives are started. Returns nil on clean context cancel.
func (o *Orchestrator) RunWatch(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error {
	drives := o.cfg.Drives
	if len(drives) == 0 {
		return fmt.Errorf("sync: no drives configured")
	}

	o.logger.Info("orchestrator starting RunWatch",
		slog.Int("drives", len(drives)),
		slog.String("mode", mode.String()),
	)

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

	// Resolve SIGHUP channel (nil → blocking channel that never fires).
	sighup := o.cfg.SIGHUPChan
	if sighup == nil {
		sighup = make(<-chan os.Signal)
	}

	// Main select loop.
	for {
		select {
		case <-sighup:
			o.logger.Info("SIGHUP received, reloading config")
			o.reload(ctx, mode, opts, runners)

		case <-ctx.Done():
			return nil
		}
	}
}

// startWatchRunner creates and starts a watch-mode engine for a single drive.
func (o *Orchestrator) startWatchRunner(
	ctx context.Context, rd *config.ResolvedDrive, mode synctypes.SyncMode, opts synctypes.WatchOpts,
) (*watchRunner, error) {
	session, err := o.cfg.Provider.Session(ctx, rd)
	if err != nil {
		return nil, fmt.Errorf("session error for drive %s: %w", rd.CanonicalID, err)
	}

	minFreeW, parseErr := config.ParseSize(rd.MinFreeSpace)
	if parseErr != nil {
		return nil, fmt.Errorf("invalid min_free_space %q for %s: %w", rd.MinFreeSpace, rd.CanonicalID, parseErr)
	}

	engine, engineErr := o.engineFactory(ctx, &synctypes.EngineConfig{
		DBPath:             rd.StatePath(),
		SyncRoot:           rd.SyncDir,
		DataDir:            config.DefaultDataDir(),
		DriveID:            session.DriveID,
		Fetcher:            session.Meta,
		Items:              session.Meta,
		Downloads:          session.Transfer,
		Uploads:            session.Transfer,
		DriveVerifier:      session.Meta,
		Logger:             o.logger,
		UseLocalTrash:      rd.UseLocalTrash,
		TransferWorkers:    rd.TransferWorkers,
		CheckWorkers:       rd.CheckWorkers,
		BigDeleteThreshold: rd.BigDeleteThreshold,
		MinFreeSpace:       minFreeW,
	})
	if engineErr != nil {
		return nil, fmt.Errorf("engine creation failed for %s: %w", rd.CanonicalID, engineErr)
	}

	driveCtx, driveCancel := context.WithCancel(ctx)
	done := make(chan struct{})

	wr := &watchRunner{
		canonID: rd.CanonicalID,
		engine:  engine,
		cancel:  driveCancel,
		done:    done,
	}

	go func() {
		defer close(done)

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

// reload re-reads the config file, diffs the active drive set against running
// runners, stops removed/paused drives, and starts newly added/resumed drives.
func (o *Orchestrator) reload(
	ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts,
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

	// Single-point config update — both Orchestrator and SessionProvider
	// read through the shared Holder.
	o.cfg.Holder.Update(newCfg)

	// Flush cached token sources so the next Session() call re-reads
	// token files from disk. Handles logout + re-login between reloads.
	o.cfg.Provider.FlushTokenCache()

	o.logger.Info("config reload complete",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
	)
}
