package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// runSyncDaemon starts multi-drive watch mode via the Orchestrator. PID file
// prevents duplicate daemons. SIGHUP triggers config reload (add/remove/pause
// drives without restart). The status writer is threaded through the watch
// bootstrap so warnings stay on the CLI-owned output boundary instead of
// reaching for process-global stderr directly.
func runSyncDaemon(
	ctx context.Context,
	holder *config.Holder,
	selectors []string,
	mode synctypes.SyncMode,
	opts synctypes.WatchOpts,
	logger *slog.Logger,
	statusWriter io.Writer,
) error {
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

	cleanup, pidErr := writePIDFileWithWarningWriter(config.PIDFilePath(), statusWriter)
	if pidErr != nil {
		return pidErr
	}
	defer cleanup()

	sighup := sighupChannel()
	defer signal.Stop(sighup)

	provider := driveops.NewSessionProvider(holder,
		syncMetaHTTPClient(), syncTransferHTTPClient(), "onedrive-go/"+version, logger)

	orch := multisync.NewOrchestrator(&multisync.OrchestratorConfig{
		Holder:     holder,
		Drives:     drives,
		Provider:   provider,
		Logger:     logger,
		SIGHUPChan: sighup,
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
