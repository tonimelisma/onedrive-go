package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

const syncRootDirPerms = 0o700

func ensureResolvedSyncDir(rd *config.ResolvedDrive) error {
	if rd == nil || rd.SyncDir == "" {
		return nil
	}

	if err := localpath.MkdirAll(rd.SyncDir, syncRootDirPerms); err != nil {
		return fmt.Errorf("create sync_dir for %s: %w", rd.CanonicalID, err)
	}

	return nil
}

type syncDaemonOrchestrator interface {
	RunWatch(context.Context, syncengine.Mode, syncengine.WatchOptions) error
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
	mode syncengine.Mode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
) error {
	return runSyncDaemonWithFactory(
		ctx,
		holder,
		selectors,
		mode,
		opts,
		logger,
		statusWriter,
		controlSocketPath,
		defaultSyncDaemonOrchestratorFactory,
	)
}

func runSyncDaemonWithFactory(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode syncengine.Mode,
	opts syncengine.WatchOptions,
	logger *slog.Logger,
	statusWriter io.Writer,
	controlSocketPath string,
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
		if syncErr := ensureResolvedSyncDir(rd); syncErr != nil {
			return syncErr
		}
	}

	if len(drives) == 0 {
		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	runtime := driveops.NewSessionRuntime(holder, "onedrive-go/"+version, logger)

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
		Runtime:           runtime,
		Logger:            logger,
		ControlSocketPath: controlSocketPath,
		StartWarning: func(warning multisync.StartupWarning) {
			writeWatchStartWarnings(statusWriter, warning)
		},
		DebugEventHook: debugEventHook,
		PerfParent:     perf.FromContext(ctx),
	})

	if err := orch.RunWatch(ctx, mode, opts); err != nil {
		return fmt.Errorf("run watch sync: %w", formatWatchStartupError(err))
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
