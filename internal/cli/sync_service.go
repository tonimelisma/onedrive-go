package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type syncCommandOptions struct {
	Mode          synctypes.SyncMode
	Watch         bool
	DryRun        *bool
	FullReconcile bool
}

type syncWatchRunner func(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error

type syncRunOnceRunner func(
	ctx context.Context,
	holder *config.Holder,
	drives []*config.ResolvedDrive,
	mode synctypes.SyncMode,
	opts synctypes.RunOpts,
	logger *slog.Logger,
	controlSocketPath string,
) []*synctypes.DriveReport

type syncService struct {
	cc            *CLIContext
	watchRunner   syncWatchRunner
	runOnceRunner syncRunOnceRunner
}

func newSyncService(cc *CLIContext) *syncService {
	service := &syncService{
		cc: cc,
		watchRunner: func(
			ctx context.Context,
			holder *config.Holder,
			selectors []string,
			mode synctypes.SyncMode,
			opts synctypes.WatchOpts,
			logger *slog.Logger,
			statusWriter io.Writer,
			controlSocketPath string,
		) error {
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
		},
		runOnceRunner: func(
			ctx context.Context,
			holder *config.Holder,
			drives []*config.ResolvedDrive,
			mode synctypes.SyncMode,
			opts synctypes.RunOpts,
			logger *slog.Logger,
			controlSocketPath string,
		) []*synctypes.DriveReport {
			httpProvider := graphhttp.NewProvider(logger)
			provider := driveops.NewSessionProvider(
				holder,
				func(_ *config.ResolvedDrive) driveops.HTTPClients {
					clients := httpProvider.Sync()

					return driveops.HTTPClients{
						Meta:     clients.Meta,
						Transfer: clients.Transfer,
					}
				},
				"onedrive-go/"+version,
				logger,
			)

			orch := multisync.NewOrchestrator(&multisync.OrchestratorConfig{
				Holder:            holder,
				Drives:            drives,
				Provider:          provider,
				Logger:            logger,
				ControlSocketPath: controlSocketPath,
				PerfParent:        perf.FromContext(ctx),
			})

			return orch.RunOnce(ctx, mode, opts)
		},
	}
	if cc != nil && cc.syncWatchRunner != nil {
		service.watchRunner = cc.syncWatchRunner
	}

	return service
}

func (s *syncService) run(ctx context.Context, opts syncCommandOptions) error {
	logger := s.cc.Logger
	rawCfg, err := s.loadConfigWithEmailReconcile(ctx, logger)
	if err != nil {
		return err
	}

	cfgForLog := &config.ResolvedDrive{LoggingConfig: rawCfg.LoggingConfig}
	dualLogger, logCloser := buildLoggerDualWithStatusWriter(cfgForLog, s.cc.Flags, s.cc.Status())
	if swapErr := s.cc.replaceCommandLogger(dualLogger, logCloser); swapErr != nil {
		return swapErr
	}
	logger = s.cc.Logger

	selectors := s.cc.Flags.Drive
	effectiveDryRun, err := resolveSyncDryRun(rawCfg.DryRun, opts.DryRun, opts.Watch)
	if err != nil {
		return err
	}
	controlSocketPath, err := config.ControlSocketPath()
	if err != nil {
		return fmt.Errorf("resolve control socket path: %w", err)
	}

	holder := config.NewHolder(rawCfg, s.cc.CfgPath)
	if opts.Watch {
		return s.watchRunner(ctx, holder, selectors, opts.Mode, synctypes.WatchOpts{
			PollInterval:       parsePollInterval(rawCfg.PollInterval),
			SafetyScanInterval: parseDurationOrZero(rawCfg.SafetyScanInterval),
		}, logger, s.cc.Status(), controlSocketPath)
	}

	drives, err := config.ResolveDrives(rawCfg, selectors, false, logger)
	if err != nil {
		return fmt.Errorf("resolve drives: %w", err)
	}

	for _, rd := range drives {
		if syncErr := config.ValidateResolvedForSync(rd); syncErr != nil {
			return fmt.Errorf("validate drive %s: %w", rd.CanonicalID, syncErr)
		}
	}

	if len(drives) == 0 {
		allDrives, resolveErr := config.ResolveDrives(rawCfg, selectors, true, logger)
		if resolveErr == nil && len(allDrives) > 0 {
			return fmt.Errorf("all drives are paused — run 'onedrive-go resume' to unpause")
		}

		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	reports := s.runOnceRunner(ctx, holder, drives, opts.Mode, synctypes.RunOpts{
		DryRun:        effectiveDryRun,
		FullReconcile: opts.FullReconcile,
	}, logger, controlSocketPath)

	printDriveReports(reports, s.cc)

	return driveReportsError(reports)
}

func (s *syncService) loadConfigWithEmailReconcile(
	ctx context.Context,
	logger *slog.Logger,
) (*config.Config, error) {
	rawCfg, err := config.LoadOrDefault(s.cc.CfgPath, logger)
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	selectedDrives, resolveErr := config.ResolveDrives(rawCfg, s.cc.Flags.Drive, true, logger)
	if resolveErr == nil {
		accountCIDs, accountErr := accountIDsFromResolvedDrives(selectedDrives)
		if accountErr != nil {
			return nil, accountErr
		}

		if !s.reconcileSyncAccounts(ctx, accountCIDs, logger) {
			return rawCfg, nil
		}

		rawCfg, err = config.LoadOrDefault(s.cc.CfgPath, logger)
		if err != nil {
			return nil, fmt.Errorf("reload config after email reconciliation: %w", err)
		}
	}

	return rawCfg, nil
}

func (s *syncService) reconcileSyncAccounts(
	ctx context.Context,
	accountCIDs []driveid.CanonicalID,
	logger *slog.Logger,
) bool {
	reconciled := false

	for _, accountCID := range accountCIDs {
		reconcileResult, probeErr := s.cc.probeAccountIdentity(ctx, accountCID, "sync-bootstrap")
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
