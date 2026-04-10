package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graphhttp"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

type syncDaemonOrchestrator interface {
	RunWatch(context.Context, synctypes.SyncMode, synctypes.WatchOpts) error
}

type syncDaemonOrchestratorFactory func(*multisync.OrchestratorConfig) syncDaemonOrchestrator

func defaultSyncDaemonOrchestratorFactory(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
	return multisync.NewOrchestrator(cfg)
}

// runSyncDaemon starts multi-drive watch mode via the Orchestrator. The Unix
// control socket prevents duplicate sync owners and handles reload/status/user
// intent RPCs.
func runSyncDaemon(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	logger *slog.Logger,
	statusWriter io.Writer,
) error {
	return runSyncDaemonWithFactory(ctx, holder, selectors, mode, opts, logger, statusWriter, defaultSyncDaemonOrchestratorFactory)
}

func runSyncDaemonWithFactory(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	logger *slog.Logger,
	_ io.Writer,
	orchestratorFactory syncDaemonOrchestratorFactory,
) (err error) {
	if orchestratorFactory == nil {
		orchestratorFactory = defaultSyncDaemonOrchestratorFactory
	}
	// Include paused drives — Orchestrator handles pause/resume internally.
	drives, err := config.ResolveDrives(holder.Config(), selectors, true, logger)
	if err != nil {
		return fmt.Errorf("resolve drives: %w", err)
	}

	// Sync requires sync_dir on every drive (file ops like ls/get don't).
	for _, rd := range drives {
		if syncErr := config.ValidateResolvedForSync(rd); syncErr != nil {
			return fmt.Errorf("validate drive %s: %w", rd.CanonicalID, syncErr)
		}
	}

	if len(drives) == 0 {
		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	httpProvider := graphhttp.NewProvider(logger)
	provider := driveops.NewSessionProvider(holder, func(_ *config.ResolvedDrive) driveops.HTTPClients {
		clients := httpProvider.Sync()

		return driveops.HTTPClients{
			Meta:     clients.Meta,
			Transfer: clients.Transfer,
		}
	}, "onedrive-go/"+version, logger)

	debugEventHook, closeDebugEvents, err := openSyncDebugEventHookFromEnv(logger)
	if err != nil {
		return fmt.Errorf("open sync debug event sink: %w", err)
	}
	if closeDebugEvents != nil {
		defer func() {
			if closeErr := closeDebugEvents(); closeErr != nil {
				if err == nil {
					err = closeErr
					return
				}

				logger.Warn("debug event sink close failed",
					slog.String("error", closeErr.Error()),
				)
			}
		}()
	}

	orch := orchestratorFactory(&multisync.OrchestratorConfig{
		Holder:            holder,
		Drives:            drives,
		Provider:          provider,
		Logger:            logger,
		ControlSocketPath: config.ControlSocketPath(),
		DebugEventHook:    debugEventHook,
	})

	if err := orch.RunWatch(ctx, mode, opts); err != nil {
		return fmt.Errorf("run watch sync: %w", err)
	}

	return nil
}

// parsePollInterval converts the config poll_interval string to a
// time.Duration. Returns 0 (use default) if the string is empty or invalid.
// The value has already been validated by config loading, so parse failure
// is not expected in practice.
func parsePollInterval(s string) time.Duration {
	return parseDurationOrZero(s)
}

// parseDurationOrZero converts a duration string to time.Duration, returning
// 0 (use default) if the string is empty or invalid. Config values have
// already been validated by config loading.
func parseDurationOrZero(s string) time.Duration {
	if s == "" {
		return 0
	}

	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}

	return d
}
