package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func newSyncCmd() *cobra.Command {
	var flagDownloadOnly, flagUploadOnly, flagDryRun, flagForce, flagWatch bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync cycle between the local directory and OneDrive.

By default, sync is bidirectional. Use --download-only or --upload-only for
one-way sync. Use --dry-run to preview what would happen without making changes.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runSync(context.Background(), flagDownloadOnly, flagUploadOnly,
				flagDryRun, flagForce, flagWatch)
		},
	}

	cmd.Flags().BoolVar(&flagDownloadOnly, "download-only", false, "only download remote changes")
	cmd.Flags().BoolVar(&flagUploadOnly, "upload-only", false, "only upload local changes")
	cmd.Flags().BoolVar(&flagDryRun, "dry-run", false, "preview sync actions without executing")
	cmd.Flags().BoolVar(&flagForce, "force", false, "override big-delete safety threshold")
	cmd.Flags().BoolVar(&flagWatch, "watch", false, "continuous sync (not yet implemented)")

	cmd.MarkFlagsMutuallyExclusive("download-only", "upload-only")

	return cmd
}

func runSync(ctx context.Context, downloadOnly, uploadOnly, dryRun, force, watch bool) error {
	// --watch is a placeholder for Phase 5.
	if watch {
		return fmt.Errorf("--watch is not yet implemented (planned for Phase 5)")
	}

	mode := sync.SyncBidirectional
	if downloadOnly {
		mode = sync.SyncDownloadOnly
	}

	if uploadOnly {
		mode = sync.SyncUploadOnly
	}

	client, driveID, logger, err := clientAndDrive(ctx)
	if err != nil {
		return err
	}

	logger.Info("sync: starting", "mode", mode, "dry_run", dryRun, "force", force)

	statePath := config.DriveStatePath(resolvedCfg.CanonicalID)
	if statePath == "" {
		return fmt.Errorf("cannot determine state path for drive %q", resolvedCfg.CanonicalID)
	}

	store, err := sync.NewStore(statePath, logger)
	if err != nil {
		return fmt.Errorf("cannot open sync database: %w", err)
	}
	defer store.Close()

	// Inject the discovered DriveID into a copy of the resolved config so the
	// engine uses the API-discovered value when config doesn't specify one.
	cfg := *resolvedCfg
	cfg.DriveID = driveID

	engine, err := sync.NewEngine(store, client, &cfg, logger)
	if err != nil {
		return fmt.Errorf("cannot initialize sync engine: %w", err)
	}
	defer engine.Close()

	report, err := engine.RunOnce(ctx, mode, sync.SyncOptions{Force: force, DryRun: dryRun})
	if err != nil {
		return fmt.Errorf("sync failed: %w", err)
	}

	if flagJSON {
		if err := printSyncJSON(report); err != nil {
			return err
		}
	} else {
		printSyncText(report)
	}

	if len(report.Errors) > 0 {
		return fmt.Errorf("sync completed with %d errors", len(report.Errors))
	}

	return nil
}

func printSyncText(report *sync.SyncReport) {
	durationMs := report.DurationMs()

	if report.DryRun {
		printDryRunText(report, durationMs)
		return
	}

	if report.TotalChanges() == 0 && report.Conflicts == 0 && len(report.Errors) == 0 {
		statusf("Already in sync.\n")
		return
	}

	statusf("Sync complete (%s, %dms)\n", report.Mode, durationMs)
	printSyncCountsText(report)
}

func printDryRunText(report *sync.SyncReport, durationMs int64) {
	if report.TotalChanges() == 0 && report.Conflicts == 0 {
		statusf("Dry run complete (%dms) — already in sync.\n", durationMs)
		return
	}

	statusf("Dry run — no changes made (%dms)\n", durationMs)
	printSyncCountsText(report)
}

func printSyncCountsText(report *sync.SyncReport) {
	if report.FoldersCreated > 0 {
		statusf("  Folders created: %d\n", report.FoldersCreated)
	}

	if report.Downloaded > 0 {
		statusf("  Downloaded:  %d files (%s)\n", report.Downloaded, formatSize(report.BytesDownloaded))
	}

	if report.Uploaded > 0 {
		statusf("  Uploaded:    %d files (%s)\n", report.Uploaded, formatSize(report.BytesUploaded))
	}

	if report.Moved > 0 {
		statusf("  Moved:       %d\n", report.Moved)
	}

	if report.LocalDeleted > 0 || report.RemoteDeleted > 0 {
		statusf("  Deleted:     %d local, %d remote\n", report.LocalDeleted, report.RemoteDeleted)
	}

	if report.Conflicts > 0 {
		statusf("  Conflicts:   %d\n", report.Conflicts)
	}

	if len(report.Errors) > 0 {
		statusf("  Errors:      %d\n", len(report.Errors))
	}
}

// syncJSONOutput is the JSON output schema for the sync command.
type syncJSONOutput struct {
	Mode           string          `json:"mode"`
	DryRun         bool            `json:"dry_run"`
	DurationMs     int64           `json:"duration_ms"`
	FoldersCreated int             `json:"folders_created"`
	Downloaded     int             `json:"downloaded"`
	BytesDown      int64           `json:"bytes_downloaded"`
	Uploaded       int             `json:"uploaded"`
	BytesUp        int64           `json:"bytes_uploaded"`
	LocalDeleted   int             `json:"local_deleted"`
	RemoteDeleted  int             `json:"remote_deleted"`
	Moved          int             `json:"moved"`
	Conflicts      int             `json:"conflicts"`
	Errors         []syncJSONError `json:"errors"`
}

// syncJSONError represents a single action error in JSON output.
type syncJSONError struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

func printSyncJSON(report *sync.SyncReport) error {
	errs := make([]syncJSONError, 0, len(report.Errors))
	for i := range report.Errors {
		errs = append(errs, syncJSONError{
			Path:  report.Errors[i].Action.Path,
			Error: report.Errors[i].Err.Error(),
		})
	}

	out := syncJSONOutput{
		Mode:           report.Mode.String(),
		DryRun:         report.DryRun,
		DurationMs:     report.DurationMs(),
		FoldersCreated: report.FoldersCreated,
		Downloaded:     report.Downloaded,
		BytesDown:      report.BytesDownloaded,
		Uploaded:       report.Uploaded,
		BytesUp:        report.BytesUploaded,
		LocalDeleted:   report.LocalDeleted,
		RemoteDeleted:  report.RemoteDeleted,
		Moved:          report.Moved,
		Conflicts:      report.Conflicts,
		Errors:         errs,
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}
