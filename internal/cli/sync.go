package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/multisync"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync run between the local directory and OneDrive.

By default, sync is bidirectional. Use --download-only or --upload-only for
one-way sync. Use --dry-run to preview what would happen without making changes.`,
		// sync handles its own config resolution via ResolveDrives (multi-drive).
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runSync,
	}

	cmd.Flags().Bool("download-only", false, "only download remote changes")
	cmd.Flags().Bool("upload-only", false, "only upload local changes")
	cmd.Flags().Bool("dry-run", false, "preview sync actions without executing")
	cmd.Flags().Bool("force", false, "override big-delete safety threshold")
	cmd.Flags().Bool("watch", false, "continuously sync changes (watch mode)")
	cmd.Flags().Bool("full", false, "run full reconciliation (enumerate all remote items, detect orphans)")

	cmd.MarkFlagsMutuallyExclusive("download-only", "upload-only")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "watch")
	cmd.MarkFlagsMutuallyExclusive("full", "watch")

	return cmd
}

func runSync(cmd *cobra.Command, _ []string) error {
	watch, err := cmd.Flags().GetBool("watch")
	if err != nil {
		return fmt.Errorf("read --watch flag: %w", err)
	}

	mode := syncModeFromFlags(cmd)
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger

	// Wrap the command context with signal handling: first SIGINT/SIGTERM
	// triggers graceful shutdown (context cancel → drain in-flight actions),
	// second signal force-exits. Applies to both one-shot and watch modes.
	ctx := shutdownContext(cmd.Context(), logger)

	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return fmt.Errorf("read --force flag: %w", err)
	}

	// Load raw config and resolve drives (sync does its own multi-drive
	// resolution instead of relying on PersistentPreRunE Phase 2).
	rawCfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Rebuild logger now that we have the config-file log_level. CLI flags
	// still override (--verbose, --debug, --quiet). This mirrors Phase 2
	// logger construction in PersistentPreRunE, which is skipped for sync
	// because it uses skipConfigAnnotation to handle multi-drive resolution
	// itself.
	cfgForLog := &config.ResolvedDrive{LoggingConfig: rawCfg.LoggingConfig}
	dualLogger, logCloser := buildLoggerDual(cfgForLog, cc.Flags)
	logger = dualLogger
	cc.Logger = logger
	cc.logCloser = logCloser

	if logCloser != nil {
		defer func() { _ = logCloser.Close() }()
	}

	selectors := cc.Flags.Drive

	if watch {
		holder := config.NewHolder(rawCfg, cc.CfgPath)

		return runSyncDaemon(ctx, holder, selectors, mode, synctypes.WatchOpts{
			Force:              force,
			PollInterval:       parsePollInterval(rawCfg.PollInterval),
			SafetyScanInterval: parseDurationOrZero(rawCfg.SafetyScanInterval),
		}, logger)
	}

	// One-shot: resolve drives (excludes paused drives).
	drives, err := config.ResolveDrives(rawCfg, selectors, false, logger)
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
		// Distinguish "all paused" from "none configured" for a clearer message.
		allDrives, resolveErr := config.ResolveDrives(rawCfg, selectors, true, logger)
		if resolveErr == nil && len(allDrives) > 0 {
			return fmt.Errorf("all drives are paused — run 'onedrive-go resume' to unpause")
		}

		return fmt.Errorf("no drives configured — run 'onedrive-go drive add' to add a drive")
	}

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return fmt.Errorf("read --dry-run flag: %w", err)
	}

	fullReconcile, err := cmd.Flags().GetBool("full")
	if err != nil {
		return fmt.Errorf("read --full flag: %w", err)
	}

	holder := config.NewHolder(rawCfg, cc.CfgPath)
	provider := driveops.NewSessionProvider(holder,
		syncMetaHTTPClient(), syncTransferHTTPClient(), "onedrive-go/"+version, logger)

	orch := multisync.NewOrchestrator(&multisync.OrchestratorConfig{
		Holder:   holder,
		Drives:   drives,
		Provider: provider,
		Logger:   logger,
	})

	reports := orch.RunOnce(ctx, mode, synctypes.RunOpts{
		DryRun:        dryRun,
		Force:         force,
		FullReconcile: fullReconcile,
	})

	printDriveReports(reports, cc)

	return driveReportsError(reports)
}

// runSyncDaemon starts multi-drive watch mode via the Orchestrator. PID file
// prevents duplicate daemons. SIGHUP triggers config reload (add/remove/pause
// drives without restart).
func runSyncDaemon(
	ctx context.Context, holder *config.Holder, selectors []string,
	mode synctypes.SyncMode, opts synctypes.WatchOpts, logger *slog.Logger,
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

	cleanup, pidErr := writePIDFile(config.PIDFilePath())
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

// syncModeFromFlags determines the SyncMode from CLI flags.
// Uses Changed() instead of GetBool() — both flags are boolean with default
// false, so GetBool() would work identically. Changed() is preferred because
// it directly expresses intent: "did the user explicitly set this flag?" This
// is the standard Cobra pattern for flags where presence equals activation.
func syncModeFromFlags(cmd *cobra.Command) synctypes.SyncMode {
	if cmd.Flags().Changed("download-only") {
		return synctypes.SyncDownloadOnly
	}

	if cmd.Flags().Changed("upload-only") {
		return synctypes.SyncUploadOnly
	}

	return synctypes.SyncBidirectional
}

// printDriveReports prints sync reports for all drives. When there's only
// one drive, the output is identical to the pre-Orchestrator format. For
// multiple drives, each drive's output is prefixed with a header.
func printDriveReports(reports []*synctypes.DriveReport, cc *CLIContext) {
	multiDrive := len(reports) > 1

	for _, dr := range reports {
		if multiDrive {
			cc.Statusf("\n--- %s ---\n", dr.DisplayName)
		}

		if dr.Err != nil {
			cc.Statusf("Error: %v\n", dr.Err)

			continue
		}

		if dr.Report != nil {
			printSyncReport(dr.Report, cc)
		}
	}
}

// driveReportsError returns an error if any drive report has an error.
// Returns nil when all drives succeeded.
func driveReportsError(reports []*synctypes.DriveReport) error {
	var firstErr error

	failCount := 0

	for _, dr := range reports {
		if dr.Err != nil {
			failCount++

			if firstErr == nil {
				firstErr = dr.Err
			}
		}
	}

	if failCount == 0 {
		return nil
	}

	if len(reports) == 1 {
		return firstErr
	}

	return fmt.Errorf("%d of %d drives failed: %w", failCount, len(reports), firstErr)
}

// printNonZero prints a labeled count line only when n > 0.
func printNonZero(cc *CLIContext, label string, n int) {
	if n > 0 {
		cc.Statusf("  %-16s%d\n", label+":", n)
	}
}

// printSyncReport formats and prints the sync report to stderr.
func printSyncReport(r *synctypes.SyncReport, cc *CLIContext) {
	if r.DryRun {
		cc.Statusf("Dry run — no changes applied\n")
	}

	cc.Statusf("Mode: %s\n", r.Mode)
	cc.Statusf("Duration: %s\n", r.Duration)

	total := r.FolderCreates + r.Moves + r.Downloads + r.Uploads +
		r.LocalDeletes + r.RemoteDeletes + r.Conflicts +
		r.SyncedUpdates + r.Cleanups

	if total == 0 {
		cc.Statusf("No changes detected\n")
		return
	}

	cc.Statusf("\nPlan:\n")
	printNonZero(cc, "Folder creates", r.FolderCreates)
	printNonZero(cc, "Moves", r.Moves)
	printNonZero(cc, "Downloads", r.Downloads)
	printNonZero(cc, "Uploads", r.Uploads)
	printNonZero(cc, "Local deletes", r.LocalDeletes)
	printNonZero(cc, "Remote deletes", r.RemoteDeletes)
	printNonZero(cc, "Conflicts", r.Conflicts)
	printNonZero(cc, "Synced updates", r.SyncedUpdates)
	printNonZero(cc, "Cleanups", r.Cleanups)

	if !r.DryRun {
		cc.Statusf("\nResults:\n")
		cc.Statusf("  Succeeded: %d\n", r.Succeeded)
		cc.Statusf("  Failed:    %d\n", r.Failed)

		for _, e := range r.Errors {
			cc.Statusf("  Error:     %v\n", e)
		}
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
