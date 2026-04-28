package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

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
	rawCfg, err := loadSyncConfigWithEmailReconcile(ctx, cc, logger)
	if err != nil {
		return err
	}

	cfgForLog := &config.ResolvedDrive{LoggingConfig: rawCfg.LoggingConfig}
	dualLogger, logCloser := buildLoggerDualWithStatusWriter(cfgForLog, cc.Flags, cc.Status())
	if swapErr := cc.replaceCommandLogger(dualLogger, logCloser); swapErr != nil {
		return swapErr
	}
	logger = cc.Logger

	selectors := cc.Flags.Drive
	effectiveDryRun, err := resolveSyncDryRun(rawCfg.DryRun, opts.DryRun, opts.Watch)
	if err != nil {
		return err
	}
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

	for _, rd := range drives {
		if rd.Paused {
			continue
		}
		if syncErr := config.ValidateResolvedForSync(rd); syncErr != nil {
			return fmt.Errorf("validate drive %s: %w", rd.CanonicalID, syncErr)
		}
		if syncErr := ensureResolvedSyncDir(rd); syncErr != nil {
			return syncErr
		}
	}

	if len(drives) == 0 {
		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	result := runSyncOnce(ctx, cc, holder, drives, opts.Mode, syncengine.RunOptions{
		DryRun:        effectiveDryRun,
		FullReconcile: opts.FullReconcile,
	}, logger, controlSocketPath)

	printRunOnceResult(result, cc)

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
	if cc != nil && cc.syncWatchRunner != nil {
		return cc.syncWatchRunner(ctx, holder, selectors, mode, opts, logger, statusWriter, controlSocketPath)
	}
	if cc != nil && cc.syncDaemonOrchestratorFactory != nil {
		return runSyncDaemonWithFactory(
			ctx,
			holder,
			selectors,
			mode,
			opts,
			logger,
			statusWriter,
			controlSocketPath,
			cc.syncDaemonOrchestratorFactory,
		)
	}

	return runSyncDaemon(ctx, holder, selectors, mode, opts, logger, statusWriter, controlSocketPath)
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
) multisync.RunOnceResult {
	if cc != nil && cc.syncRunOnceRunner != nil {
		return cc.syncRunOnceRunner(ctx, holder, drives, mode, opts, logger, controlSocketPath)
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
		PerfParent:             perf.FromContext(ctx),
	})

	return orch.RunOnce(ctx, mode, opts)
}

func loadSyncConfigWithEmailReconcile(
	ctx context.Context,
	cc *CLIContext,
	logger *slog.Logger,
) (*config.Config, error) {
	rawCfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	selectedDrives, resolveErr := config.ResolveDrives(rawCfg, cc.Flags.Drive, true, logger)
	if resolveErr == nil {
		accountCIDs, accountErr := accountIDsFromResolvedDrives(selectedDrives)
		if accountErr != nil {
			return nil, accountErr
		}

		if !reconcileSyncAccounts(ctx, cc, accountCIDs, logger) {
			return rawCfg, nil
		}

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
