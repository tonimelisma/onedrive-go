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

func standaloneMountConfigsFromResolvedDrives(drives []*config.ResolvedDrive) ([]multisync.StandaloneMountConfig, error) {
	mounts := make([]multisync.StandaloneMountConfig, 0, len(drives))
	for i := range drives {
		mount, err := standaloneMountConfigFromResolvedDrive(i, drives[i])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, mount)
	}

	return mounts, nil
}

func standaloneMountConfigFromResolvedDrive(
	selectionIndex int,
	rd *config.ResolvedDrive,
) (multisync.StandaloneMountConfig, error) {
	if rd == nil {
		return multisync.StandaloneMountConfig{}, fmt.Errorf("resolved drive is required")
	}

	statePath := rd.StatePath()
	if statePath == "" {
		return multisync.StandaloneMountConfig{}, fmt.Errorf("state path is required for %s", rd.CanonicalID)
	}

	tokenOwnerCanonical, err := config.TokenAccountCanonicalID(rd.CanonicalID)
	if err != nil {
		return multisync.StandaloneMountConfig{}, fmt.Errorf("token owner for %s: %w", rd.CanonicalID, err)
	}

	minFreeSpace, err := config.ParseSize(rd.MinFreeSpace)
	if err != nil {
		return multisync.StandaloneMountConfig{}, fmt.Errorf("invalid min_free_space %q for %s: %w", rd.MinFreeSpace, rd.CanonicalID, err)
	}

	accountEmail := rd.CanonicalID.Email()
	if accountEmail == "" {
		accountEmail = tokenOwnerCanonical.Email()
	}

	return multisync.StandaloneMountConfig{
		SelectionIndex:            selectionIndex,
		CanonicalID:               rd.CanonicalID,
		DisplayName:               rd.DisplayName,
		SyncRoot:                  rd.SyncDir,
		StatePath:                 statePath,
		RemoteDriveID:             rd.DriveID,
		RemoteRootItemID:          rd.RootItemID,
		TokenOwnerCanonical:       tokenOwnerCanonical,
		AccountEmail:              accountEmail,
		Paused:                    rd.Paused,
		EnableWebsocket:           rd.Websocket,
		RootedSubtreeDeltaCapable: config.RootedSubtreeDeltaCapableForTokenOwner(tokenOwnerCanonical),
		TransferWorkers:           rd.TransferWorkers,
		CheckWorkers:              rd.CheckWorkers,
		MinFreeSpaceBytes:         minFreeSpace,
	}, nil
}

type syncDaemonOrchestrator interface {
	RunWatch(context.Context, syncengine.SyncMode, syncengine.WatchOptions) error
}

type syncDaemonOrchestratorFactory func(*multisync.OrchestratorConfig) syncDaemonOrchestrator

func defaultSyncDaemonOrchestratorFactory(cfg *multisync.OrchestratorConfig) syncDaemonOrchestrator {
	return multisync.NewOrchestrator(cfg)
}

// runSyncDaemon starts multi-mount watch mode via the Orchestrator. The Unix
// control socket prevents duplicate sync owners and handles reload/status/user
// intent RPCs.
func runSyncDaemon(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode syncengine.SyncMode,
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
	mode syncengine.SyncMode,
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
	standaloneMounts, err := standaloneMountConfigsFromResolvedDrives(drives)
	if err != nil {
		return fmt.Errorf("compile standalone mount configs: %w", err)
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
		Holder:                 holder,
		StandaloneMounts:       standaloneMounts,
		ReloadStandaloneMounts: reloadStandaloneMountsFunc(selectors, logger),
		Runtime:                runtime,
		Logger:                 logger,
		ControlSocketPath:      controlSocketPath,
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

func reloadStandaloneMountsFunc(
	selectors []string,
	logger *slog.Logger,
) func(*config.Config) ([]multisync.StandaloneMountConfig, error) {
	selectors = append([]string(nil), selectors...)

	return func(cfg *config.Config) ([]multisync.StandaloneMountConfig, error) {
		drives, err := config.ResolveDrives(cfg, selectors, false, logger)
		if err != nil {
			return nil, fmt.Errorf("resolve drives: %w", err)
		}

		return standaloneMountConfigsFromResolvedDrives(drives)
	}
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
