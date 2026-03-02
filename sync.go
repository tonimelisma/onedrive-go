package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/signal"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	isync "github.com/tonimelisma/onedrive-go/internal/sync"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync cycle between the local directory and OneDrive.

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

	cmd.MarkFlagsMutuallyExclusive("download-only", "upload-only")
	cmd.MarkFlagsMutuallyExclusive("dry-run", "watch")

	return cmd
}

func runSync(cmd *cobra.Command, _ []string) error {
	watch, err := cmd.Flags().GetBool("watch")
	if err != nil {
		return err
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
		return err
	}

	// Load raw config and resolve drives (sync does its own multi-drive
	// resolution instead of relying on PersistentPreRunE Phase 2).
	rawCfg, err := config.LoadOrDefault(cc.CfgPath, logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	selectors := cc.Flags.Drive

	if watch {
		return runSyncDaemon(ctx, rawCfg, selectors, mode, isync.WatchOpts{
			Force: force,
		}, cc.CfgPath, logger)
	}

	// One-shot: resolve drives (excludes paused drives).
	drives, err := config.ResolveDrives(rawCfg, selectors, false, logger)
	if err != nil {
		return err
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
		return err
	}

	holder := config.NewHolder(rawCfg, cc.CfgPath)
	provider := driveops.NewSessionProvider(holder,
		defaultHTTPClient(), transferHTTPClient(), "onedrive-go/"+version, logger)

	orch := isync.NewOrchestrator(&isync.OrchestratorConfig{
		Holder:   holder,
		Drives:   drives,
		Provider: provider,
		Logger:   logger,
	})

	reports := orch.RunOnce(ctx, mode, isync.RunOpts{
		DryRun: dryRun,
		Force:  force,
	})

	printDriveReports(reports, cc)

	return driveReportsError(reports)
}

// runSyncDaemon starts multi-drive watch mode via the Orchestrator. PID file
// prevents duplicate daemons. SIGHUP triggers config reload (add/remove/pause
// drives without restart).
func runSyncDaemon(
	ctx context.Context, rawCfg *config.Config, selectors []string,
	mode isync.SyncMode, opts isync.WatchOpts, cfgPath string, logger *slog.Logger,
) error {
	// Include paused drives — Orchestrator handles pause/resume internally.
	drives, err := config.ResolveDrives(rawCfg, selectors, true, logger)
	if err != nil {
		return err
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

	holder := config.NewHolder(rawCfg, cfgPath)
	provider := driveops.NewSessionProvider(holder,
		defaultHTTPClient(), transferHTTPClient(), "onedrive-go/"+version, logger)

	orch := isync.NewOrchestrator(&isync.OrchestratorConfig{
		Holder:     holder,
		Drives:     drives,
		Provider:   provider,
		Logger:     logger,
		SIGHUPChan: sighup,
	})

	return orch.RunWatch(ctx, mode, opts)
}

// syncModeFromFlags determines the SyncMode from CLI flags.
// Uses Changed() instead of GetBool() — both flags are boolean with default
// false, so GetBool() would work identically. Changed() is preferred because
// it directly expresses intent: "did the user explicitly set this flag?" This
// is the standard Cobra pattern for flags where presence equals activation.
func syncModeFromFlags(cmd *cobra.Command) isync.SyncMode {
	if cmd.Flags().Changed("download-only") {
		return isync.SyncDownloadOnly
	}

	if cmd.Flags().Changed("upload-only") {
		return isync.SyncUploadOnly
	}

	return isync.SyncBidirectional
}

// printDriveReports prints sync reports for all drives. When there's only
// one drive, the output is identical to the pre-Orchestrator format. For
// multiple drives, each drive's output is prefixed with a header.
func printDriveReports(reports []*isync.DriveReport, cc *CLIContext) {
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
func driveReportsError(reports []*isync.DriveReport) error {
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
func printSyncReport(r *isync.SyncReport, cc *CLIContext) {
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
