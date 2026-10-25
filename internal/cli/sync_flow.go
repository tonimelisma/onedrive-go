package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type syncCommandOptions struct {
	Mode          syncengine.SyncMode
	Watch         bool
	DryRun        *bool
	FullReconcile bool
}

type syncWatchRunner func(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error

type syncRunOnceRunner func(
	ctx context.Context,
	holder *config.Holder,
	drives []*config.ResolvedDrive,
	mode syncengine.SyncMode,
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

	rawCfg, err = reconcileLoadedSyncConfig(ctx, cc, rawCfg, logger)
	if err != nil {
		return err
	}

	// Usually a no-op: the root already attached these logging settings.
	if logErr := cc.useLoggingConfig(rawCfg.LoggingConfig); logErr != nil {
		return logErr
	}
	logger = cc.Logger

	controlSocketPath, err := config.ControlSocketPath()
	if err != nil {
		return fmt.Errorf("resolve control socket path: %w", err)
	}

	holder := config.NewHolder(rawCfg, cc.CfgPath)
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
		DryRun:        effectiveDryRun,
		FullReconcile: opts.FullReconcile,
	}, logger, controlSocketPath, console.Activity)

	console.Summary(result)

	// Item-level failures are not a failed run: each one is printed as it
	// happens and the engine keeps it as a durable condition that status
	// reports (a read-only shared folder, a locked file, throttling).
	// Failing every run on such a state would make the exit status useless
	// to cron. A drive that could not sync at all still fails the command.
	return runOnceResultError(result)
}

func runSyncWatch(
	ctx context.Context,
	cc *CLIContext,
	holder *config.Holder,
	selectors []string,
	mode syncengine.SyncMode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error {
	if cc != nil && cc.seams.syncWatch != nil {
		return cc.seams.syncWatch(ctx, holder, selectors, mode, opts, logger, statusWriter, controlSocketPath)
	}
	console := newSyncConsole(cc, true)

	getenv := os.Getenv
	if cc != nil && cc.seams.notifyGetenv != nil {
		getenv = cc.seams.notifyGetenv
	}
	notifier := openSyncNotifier(getenv, logger)
	defer func() {
		if closeErr := notifier.Close(); closeErr != nil {
			logger.Debug("close systemd notify socket", slog.String("error", closeErr.Error()))
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
	mode syncengine.SyncMode,
	opts syncengine.RunOptions,
	logger *slog.Logger,
	controlSocketPath string,
	activity func(multisync.MountActivity),
) multisync.RunOnceResult {
	if cc != nil && cc.seams.syncRunOnce != nil {
		return cc.seams.syncRunOnce(ctx, holder, drives, mode, opts, logger, controlSocketPath)
	}

	runtime := driveops.NewSessionRuntime(holder, "onedrive-go/"+version, logger)
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

	for _, accountCID := range accountCIDs {
		reconcileResult, probeErr := cc.probeAccountIdentity(ctx, accountCID, "sync-bootstrap")
		if probeErr != nil {
			logger.Debug("skip email reconciliation during sync bootstrap",
				"account", accountCID.String(),
				"error", probeErr,
			)
			continue
		}
		if reconcileResult.Changed() {
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
