package multisync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// reload applies the config file to the running sync. It returns an error in
// two cases. When nothing could be applied (the config would not load or
// compile), the running drives keep their previous settings and the caller
// tells the client that asked for the reload. When a drive the new config
// stops did not stop within its bound, the error wraps ErrSyncStuck,
// the reload ends there, and the caller ends the watch; no WatchReloaded is
// reported, WatchStopping follows. Drives that fail to start under the new
// config are not a reload failure: they are reported as startup results.
func (o *Orchestrator) reload(
	ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions,
	runners map[mountID]*watchRunner,
	runnerEvents chan<- watchRunnerEvent,
	childWork *parentChildWorkSnapshots,
) error {
	o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchReloading}, nil, nil)

	newCfg, newSelection, err := o.loadReloadSelection()
	if err != nil {
		o.logger.Warn("Couldn't reload the config; keeping the previous settings",
			slog.Any("error", err),
		)
		o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchReloaded, Reload: ReloadKeptPreviousSettings, Err: err}, nil, nil)

		return err
	}

	oldCfg := o.cfg.Holder.Config()
	oldMounts := o.cfg.StandaloneMounts
	oldStartup := o.cfg.InitialStartupResults
	o.cfg.Holder.Update(newCfg)
	o.cfg.StandaloneMounts = newSelection.Mounts
	o.cfg.InitialStartupResults = newSelection.StartupResults
	o.cfg.Runtime.FlushTokenCache()
	if o.cfg.ApplyLogLevel != nil {
		o.cfg.ApplyLogLevel(newCfg.LogLevel)
	}

	newMounts, err := o.buildRuntimeWorkSet(ctx, newSelection.Mounts, newSelection.StartupResults, childWork)
	if err != nil {
		o.cfg.Holder.Update(oldCfg)
		o.cfg.StandaloneMounts = oldMounts
		o.cfg.InitialStartupResults = oldStartup
		o.cfg.Runtime.FlushTokenCache()
		o.logger.Warn("Couldn't reload the config; keeping the previous settings",
			slog.Any("error", err),
		)
		o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchReloaded, Reload: ReloadKeptPreviousSettings, Err: err}, nil, nil)
		return err
	}
	// The user asked for the config to be applied now, so drives waiting out
	// a restart backoff get their next attempt immediately.
	o.releaseRunnerRestartWaits()
	stopped, started, startResults, err := o.applyWatchMountSet(
		ctx,
		runners,
		newMounts,
		mode,
		opts,
		runnerEvents,
		childWork,
	)
	if err != nil {
		return err
	}
	o.emitStartWarning(startResults, startAnnounceFull)
	o.emitWatchLifecycle(WatchLifecycleEvent{Phase: WatchReloaded, Reload: ReloadApplied}, runners, startResults)

	o.logger.Info("Reloaded the config",
		slog.Int("started", started),
		slog.Int("stopped", stopped),
		slog.Int("active", len(runners)),
		slog.Int("skipped", len(summarizeStartupResults(startResults).SkippedResults())),
	)
	return nil
}

// loadReloadSelection reads the config file, clears expired timed pauses in
// it, and compiles the drive selection it describes.
func (o *Orchestrator) loadReloadSelection() (*config.Config, StandaloneMountSelection, error) {
	newCfg, err := config.LoadOrDefault(o.cfg.Holder.Path(), o.logger)
	if err != nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("loading config for reload: %w", err)
	}

	config.ClearExpiredPauses(o.cfg.Holder.Path(), newCfg, o.clockNow(), o.logger)

	if o.cfg.ReloadStandaloneMounts == nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("standalone mount reload compiler is required")
	}
	newSelection, err := o.cfg.ReloadStandaloneMounts(newCfg)
	if err != nil {
		return nil, StandaloneMountSelection{}, fmt.Errorf("compiling standalone mounts after reload: %w", err)
	}

	return newCfg, newSelection, nil
}
