package cli

import (
	"context"
	"fmt"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type syncCommandOptions struct {
	Mode          synctypes.SyncMode
	Watch         bool
	Force         bool
	DryRun        bool
	FullReconcile bool
}

type syncService struct {
	cc *CLIContext
}

func newSyncService(cc *CLIContext) *syncService {
	return &syncService{cc: cc}
}

func (s *syncService) run(ctx context.Context, opts syncCommandOptions) error {
	logger := s.cc.Logger

	rawCfg, err := config.LoadOrDefault(s.cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cfgForLog := &config.ResolvedDrive{LoggingConfig: rawCfg.LoggingConfig}
	dualLogger, logCloser := buildLoggerDualWithStatusWriter(cfgForLog, s.cc.Flags, s.cc.Status())
	logger = dualLogger
	s.cc.Logger = logger
	s.cc.logCloser = logCloser

	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	selectors := s.cc.Flags.Drive
	if opts.Watch {
		holder := config.NewHolder(rawCfg, s.cc.CfgPath)

		return runSyncDaemon(ctx, holder, selectors, opts.Mode, synctypes.WatchOpts{
			Force:              opts.Force,
			PollInterval:       parsePollInterval(rawCfg.PollInterval),
			SafetyScanInterval: parseDurationOrZero(rawCfg.SafetyScanInterval),
		}, logger, s.cc.Status())
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

	holder := config.NewHolder(rawCfg, s.cc.CfgPath)
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
		Holder:   holder,
		Drives:   drives,
		Provider: provider,
		Logger:   logger,
	})

	reports := orch.RunOnce(ctx, opts.Mode, synctypes.RunOpts{
		DryRun:        opts.DryRun,
		Force:         opts.Force,
		FullReconcile: opts.FullReconcile,
	})

	printDriveReports(reports, s.cc)

	return driveReportsError(reports)
}
