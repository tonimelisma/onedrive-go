package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sdnotify"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func standaloneMountSelectionFromResolvedDrives(drives []*config.ResolvedDrive) multisync.StandaloneMountSelection {
	selection := multisync.StandaloneMountSelection{
		Mounts:         make([]multisync.StandaloneMountConfig, 0, len(drives)),
		StartupResults: make([]multisync.MountStartupResult, 0),
	}
	for i := range drives {
		mount, err := standaloneMountConfigFromResolvedDrive(i, drives[i])
		if err != nil {
			selection.StartupResults = append(selection.StartupResults, standaloneMountStartupFailure(i, drives[i], err))
			continue
		}
		selection.Mounts = append(selection.Mounts, mount)
	}

	return selection
}

func standaloneMountConfigFromResolvedDrive(
	selectionIndex int,
	rd *config.ResolvedDrive,
) (multisync.StandaloneMountConfig, error) {
	if rd == nil {
		return multisync.StandaloneMountConfig{}, fmt.Errorf("resolved drive is required")
	}

	if !rd.Paused {
		if err := config.ValidateResolvedForSync(rd); err != nil {
			return multisync.StandaloneMountConfig{}, fmt.Errorf("validate sync config for %s: %w", rd.CanonicalID, err)
		}
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

	accountEmail := tokenOwnerCanonical.Email()
	if accountEmail == "" {
		accountEmail = rd.CanonicalID.Email()
	}

	return multisync.StandaloneMountConfig{
		SelectionIndex:         selectionIndex,
		CanonicalID:            rd.CanonicalID,
		DisplayName:            rd.DisplayName,
		SyncRoot:               rd.SyncDir,
		StatePath:              statePath,
		RemoteDriveID:          rd.DriveID,
		RemoteRootItemID:       rd.RemoteRootItemID,
		TokenOwnerCanonical:    tokenOwnerCanonical,
		AccountEmail:           accountEmail,
		Paused:                 rd.Paused,
		EnableWebsocket:        rd.Websocket,
		RemoteRootDeltaCapable: rd.RemoteRootDeltaCapable,
		TransferWorkers:        rd.TransferWorkers,
		CheckWorkers:           rd.CheckWorkers,
		MinFreeSpaceBytes:      minFreeSpace,
		ContentFilter:          contentFilterConfigFor(&rd.DriveFilterConfig),
	}, nil
}

func standaloneMountStartupFailure(
	selectionIndex int,
	rd *config.ResolvedDrive,
	err error,
) multisync.MountStartupResult {
	result := multisync.MountStartupResult{
		SelectionIndex: selectionIndex,
		Status:         multisync.MountStartupFatal,
		Err:            err,
	}
	if rd != nil {
		result.Identity = multisync.MountIdentity{
			MountID:        rd.CanonicalID.String(),
			ProjectionKind: multisync.MountProjectionStandalone,
			CanonicalID:    rd.CanonicalID,
		}
		result.DisplayName = rd.DisplayName
	}

	return result
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
	console *syncConsole,
	notifier *sdnotify.Notifier,
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
		console,
		notifier,
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
	console *syncConsole,
	notifier *sdnotify.Notifier,
	orchestratorFactory syncDaemonOrchestratorFactory,
) (err error) {
	if orchestratorFactory == nil {
		orchestratorFactory = defaultSyncDaemonOrchestratorFactory
	}

	// Under a Type=notify unit (R-6.6.32): readiness, reload and stop
	// transitions come from the watch loop; a failed run leaves its reason
	// as the final STATUS=.
	serviceNotifier := newSyncNotifier(notifier, logger, console)
	defer func() { serviceNotifier.failed(err) }()
	// A helper goroutine of the watch that panics (the keepalive here, or one
	// of the command's: the signal handler, the performance logger) cancels
	// the watch with the panic as the cause; the watch drains as on a stop,
	// and the cause is its error.
	ctx, failWatch := context.WithCancelCause(ctx)
	defer failWatch(nil)
	stopWatchdog := startSyncWatchdog(ctx, serviceNotifier, failWatch)
	defer stopWatchdog()
	// Include paused drives — Orchestrator handles pause/resume internally.
	drives, err := config.ResolveDrives(holder.Config(), selectors, true, logger)
	if err != nil {
		return fmt.Errorf("resolve drives: %w", err)
	}

	if len(drives) == 0 {
		return errNoDrivesConfigured()
	}
	standaloneSelection := standaloneMountSelectionFromResolvedDrives(drives)

	runtime := driveops.NewSessionRuntime(holder, graph.UserAgent(version), logger)

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
					slog.Any("error", closeErr),
				)
			}
		}()
	}

	orch := orchestratorFactory(&multisync.OrchestratorConfig{
		Holder:                 holder,
		StandaloneMounts:       standaloneSelection.Mounts,
		InitialStartupResults:  standaloneSelection.StartupResults,
		ReloadStandaloneMounts: reloadStandaloneMountsFunc(selectors, logger),
		Runtime:                runtime,
		DataDir:                config.DefaultDataDir(),
		Logger:                 logger,
		ControlSocketPath:      controlSocketPath,
		StartWarning: func(warning multisync.StartupWarning) {
			writeWatchStartWarnings(statusWriter, warning)
		},
		DebugEventHook: debugEventHook,
		ActivityHook:   console.ActivityHook(),
		WatchLifecycle: serviceNotifier.lifecycle,
		PerfParent:     perf.FromContext(ctx),
		ApplyLogLevel:  applyLogLevelFunc(ctx),
	})

	console.Watching(standaloneSelection.Mounts, mode)

	watchErr := orch.RunWatch(ctx, mode, opts)
	if watchErr == nil {
		watchErr = commandPanicCause(ctx)
	}
	if watchErr != nil {
		// The terminal gets the message through main; the log file gets it
		// here, so a watch that refuses to start (its sync folder missing,
		// another sync owning the socket) leaves a trace where the rest of
		// its history is (R-6.6.35).
		logger.Error("sync --watch stopped with an error", slog.Any("error", watchErr))
		// The orchestrator's error already says what failed; a "run watch
		// sync:" prefix only lengthened the line main prints.
		return formatWatchStartupError(watchErr)
	}

	console.Stopped()

	return nil
}

func reloadStandaloneMountsFunc(
	selectors []string,
	logger *slog.Logger,
) func(*config.Config) (multisync.StandaloneMountSelection, error) {
	selectors = append([]string(nil), selectors...)

	return func(cfg *config.Config) (multisync.StandaloneMountSelection, error) {
		drives, err := config.ResolveDrives(cfg, selectors, false, logger)
		if err != nil {
			return multisync.StandaloneMountSelection{}, fmt.Errorf("resolve drives: %w", err)
		}

		return standaloneMountSelectionFromResolvedDrives(drives), nil
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
