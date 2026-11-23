package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

type syncCommandOptions struct {
	Mode           syncdomain.SyncMode
	Watch          bool
	DryRun         *bool
	FullReconcile  bool
	ConfirmDeletes bool
}

type syncWatchRunner func(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode syncdomain.SyncMode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error

type syncRunOnceRunner func(
	ctx context.Context,
	holder *config.Holder,
	drives []*config.ResolvedDrive,
	mode syncdomain.SyncMode,
	opts syncengine.RunOptions,
	logger *slog.Logger,
	controlSocketPath string,
) multisync.RunOnceResult

func runSyncCommand(ctx context.Context, cc *CLIContext, opts syncCommandOptions) error {
	logger := cc.Logger
	rawCfg, err := loadSyncConfig(cc, logger)
	if err != nil {
		return err
	}

	selectors := cc.Flags.Drive
	effectiveDryRun, err := resolveSyncDryRun(rawCfg.DryRun, opts.DryRun, opts.Watch)
	if err != nil {
		return err
	}

	// Usually a no-op: the root already attached these logging settings.
	if logErr := cc.useLoggingConfig(rawCfg.LoggingConfig); logErr != nil {
		return logErr
	}
	logger = cc.Logger

	// One session runtime serves the sync's identity probe and its engines
	// (R-6.8.27), so the process keeps one token source per sign-in file.
	// It is built before the probe, with the logger the engines log
	// through, and its holder is the one the engines and the control plane
	// read.
	holder := config.NewHolder(rawCfg, cc.CfgPath)
	cc.syncSessionRuntime(holder, syncRuntimeLogger(cc, logger, opts.Watch))

	rawCfg, err = reconcileLoadedSyncConfig(ctx, cc, rawCfg, logger)
	if err != nil {
		return err
	}
	holder.Update(rawCfg)

	controlSocketPath, err := config.ControlSocketPath()
	if err != nil {
		return fmt.Errorf("resolve control socket path: %w", err)
	}

	if opts.Watch {
		return runSyncWatch(ctx, cc, holder, selectors, opts.Mode, syncengine.WatchOptions{
			PollInterval: parsePollInterval(rawCfg.PollInterval),
		}, logger, cc.Status(), controlSocketPath)
	}

	drives, err := config.ResolveDrives(rawCfg, selectors, true, logger)
	if err != nil {
		return fmt.Errorf("resolve drives: %w", err)
	}

	if len(drives) == 0 {
		return errNoDrivesConfigured()
	}

	// Refuse before announcing: "Checking (dry run) ~/OneDrive..." followed
	// by "another onedrive-go sync is already running" read as a run that
	// started and then broke.
	if probe, probeErr := probeControlOwner(ctx); probeErr == nil &&
		(probe.state == controlOwnerStateWatchOwner || probe.state == controlOwnerStateOneShotOwner) {
		return &multisync.ControlSocketInUseError{Path: controlSocketPath}
	}

	console := newSyncConsole(cc, false)
	console.Starting(standaloneMountSelectionFromResolvedDrives(drives).Mounts, opts.Mode, effectiveDryRun)

	result := runSyncOnce(ctx, cc, holder, drives, opts.Mode, syncengine.RunOptions{
		DryRun:         effectiveDryRun,
		FullReconcile:  opts.FullReconcile,
		ConfirmDeletes: opts.ConfirmDeletes,
	}, logger, controlSocketPath, console.ActivityHook())

	// The summary returns what each drive left unsynced, the numbers its
	// lines just printed, so the exit status cannot say done while a summary
	// line (or `status`) says otherwise (R-6.6.28).
	left := console.Summary(result)
	if err := runOnceResultError(result, left); err != nil {
		// The run's own failure is the one returned; a stdout that also
		// refused a line already said so on stderr.
		return err
	}

	return console.OutputError()
}

func runSyncWatch(
	ctx context.Context,
	cc *CLIContext,
	holder *config.Holder,
	selectors []string,
	mode syncdomain.SyncMode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error {
	if cc != nil && cc.seams.syncWatch != nil {
		return cc.seams.syncWatch(ctx, holder, selectors, mode, opts, logger, statusWriter, controlSocketPath)
	}
	console := newSyncConsole(cc, true)
	if console != nil && console.stamp {
		// Warnings in a long-running watch say when they happened, like the
		// watch's own lines.
		logger = withConsoleTimestamps(logger, console.now)
	}

	getenv := os.Getenv
	if cc != nil && cc.seams.notifyGetenv != nil {
		getenv = cc.seams.notifyGetenv
	}
	notifier := openSyncNotifier(getenv, logger)
	defer func() {
		if closeErr := notifier.Close(); closeErr != nil {
			logger.Debug("close systemd notify socket", slog.Any("error", closeErr))
		}
	}()

	if cc != nil && cc.seams.syncDaemonOrchestrator != nil {
		return runSyncDaemonWithFactory(
			ctx,
			holder,
			selectors,
			mode,
			opts,
			logger,
			statusWriter,
			controlSocketPath,
			console,
			notifier,
			cc.seams.syncDaemonOrchestrator,
		)
	}

	return runSyncDaemon(ctx, holder, selectors, mode, opts, logger, statusWriter, controlSocketPath, console, notifier)
}

func runSyncOnce(
	ctx context.Context,
	cc *CLIContext,
	holder *config.Holder,
	drives []*config.ResolvedDrive,
	mode syncdomain.SyncMode,
	opts syncengine.RunOptions,
	logger *slog.Logger,
	controlSocketPath string,
	activity func(multisync.MountActivity),
) multisync.RunOnceResult {
	if cc != nil && cc.seams.syncRunOnce != nil {
		return cc.seams.syncRunOnce(ctx, holder, drives, mode, opts, logger, controlSocketPath)
	}

	runtime := cc.syncSessionRuntime(holder, logger)
	standaloneSelection := standaloneMountSelectionFromResolvedDrives(drives)

	orch := multisync.NewOrchestrator(&multisync.OrchestratorConfig{
		Holder:                 holder,
		StandaloneMounts:       standaloneSelection.Mounts,
		InitialStartupResults:  standaloneSelection.StartupResults,
		ReloadStandaloneMounts: reloadStandaloneMountsFunc(nil, logger),
		Runtime:                runtime,
		DataDir:                config.DefaultDataDir(),
		Logger:                 logger,
		ControlSocketPath:      controlSocketPath,
		ActivityHook:           activity,
		PerfParent:             perf.FromContext(ctx),
	})

	return orch.RunOnce(ctx, mode, opts)
}

// syncSessionRuntime is the one session runtime of a sync command: the
// command's own (CLIContext.Runtime) once there is one, else a new one over
// holder logging through logger, which becomes the command's. A nil cc (a
// test of the daemon without a command context) gets a runtime nobody keeps.
// Call it from the command goroutine.
func (cc *CLIContext) syncSessionRuntime(holder *config.Holder, logger *slog.Logger) *driveops.SessionRuntime {
	if cc == nil {
		return cc.newSessionRuntime(holder, logger)
	}
	if cc.Runtime == nil {
		cc.Runtime = cc.newSessionRuntime(holder, logger)
	}

	return cc.runtime()
}

// syncRuntimeLogger is the logger a sync's session runtime logs through:
// for a watch on a terminal, the one that stamps its console lines with the
// time of day (newSyncConsole), so a retry warning in a long watch says
// when it happened like the watch's own lines.
func syncRuntimeLogger(cc *CLIContext, logger *slog.Logger, watch bool) *slog.Logger {
	if watch && !isWriterJournal(cc.Status()) {
		return withConsoleTimestamps(logger, time.Now)
	}

	return logger
}

func loadSyncConfig(
	cc *CLIContext,
	logger *slog.Logger,
) (*config.Config, error) {
	rawCfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	return rawCfg, nil
}

func reconcileLoadedSyncConfig(
	ctx context.Context,
	cc *CLIContext,
	rawCfg *config.Config,
	logger *slog.Logger,
) (*config.Config, error) {
	selectedDrives, resolveErr := config.ResolveDrives(rawCfg, cc.Flags.Drive, true, logger)
	if resolveErr == nil {
		accountCIDs, accountErr := accountIDsFromResolvedDrives(selectedDrives)
		if accountErr != nil {
			return nil, accountErr
		}

		if !reconcileSyncAccounts(ctx, cc, accountCIDs, logger) {
			return rawCfg, nil
		}

		var err error
		rawCfg, err = config.LoadOrDefault(cc.CfgPath, logger)
		if err != nil {
			return nil, fmt.Errorf("reload config after email reconciliation: %w", err)
		}
	}

	return rawCfg, nil
}

func reconcileSyncAccounts(
	ctx context.Context,
	cc *CLIContext,
	accountCIDs []driveid.CanonicalID,
	logger *slog.Logger,
) bool {
	reconciled := false

	results, errs := cc.probeAccountIdentities(ctx, accountCIDs, "sync-bootstrap")
	for i, accountCID := range accountCIDs {
		if errs[i] != nil {
			logger.Debug("skip email reconciliation during sync bootstrap",
				"account", accountCID.String(),
				"error", errs[i],
			)
			continue
		}
		if results[i].Changed() {
			reconciled = true
		}
	}

	return reconciled
}

// errNoDrivesConfigured is a configuration problem a restart cannot fix, so it
// carries config.ErrInvalidConfig for the exit status (R-6.6.31).
func errNoDrivesConfigured() error {
	return newPresentedError("no drives configured — run 'onedrive-go drive add' to add a drive", config.ErrInvalidConfig)
}
