package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/sync"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync cycle between the local directory and OneDrive.

By default, sync is bidirectional. Use --download-only or --upload-only for
one-way sync. Use --dry-run to preview what would happen without making changes.`,
		RunE: runSync,
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
	cc := cliContextFrom(cmd.Context())
	logger := cc.Logger

	// Wrap the command context with signal handling: first SIGINT/SIGTERM
	// triggers graceful shutdown (context cancel → drain in-flight actions),
	// second signal force-exits. Applies to both one-shot and watch modes.
	ctx := shutdownContext(cmd.Context(), logger)

	client, ts, driveID, err := clientAndDrive(ctx, cc)
	if err != nil {
		return err
	}

	syncDir := cc.Cfg.SyncDir
	if syncDir == "" {
		return fmt.Errorf("sync_dir not configured — set it in the config file or add a drive with 'onedrive-go drive add'")
	}

	dbPath := cc.Cfg.StatePath()
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cc.Cfg.CanonicalID)
	}

	// Use transfer client (no timeout) for download/upload operations.
	transferClient := newTransferGraphClient(ts, logger)

	engine, err := sync.NewEngine(&sync.EngineConfig{
		DBPath:        dbPath,
		SyncRoot:      syncDir,
		DataDir:       config.DefaultDataDir(),
		DriveID:       driveID,
		Fetcher:       client,
		Items:         client,
		Downloads:     transferClient,
		Uploads:       transferClient,
		DriveVerifier: client,
		Logger:        logger,
		UseLocalTrash: cc.Cfg.UseLocalTrash,
	})
	if err != nil {
		return err
	}
	defer engine.Close()

	force, err := cmd.Flags().GetBool("force")
	if err != nil {
		return err
	}

	if watch {
		return engine.RunWatch(ctx, mode, sync.WatchOpts{
			Force: force,
			// Zero values use defaults (2s debounce, 5m poll interval).
		})
	}

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return err
	}

	report, err := engine.RunOnce(ctx, mode, sync.RunOpts{
		DryRun: dryRun,
		Force:  force,
	})

	if report != nil {
		printSyncReport(report)
	}

	return err
}

// syncModeFromFlags determines the SyncMode from CLI flags.
// Panics on programmer error (flag not registered) — these are Cobra invariants.
func syncModeFromFlags(cmd *cobra.Command) sync.SyncMode {
	downloadOnly := cmd.Flags().Changed("download-only")
	if downloadOnly {
		return sync.SyncDownloadOnly
	}

	uploadOnly := cmd.Flags().Changed("upload-only")
	if uploadOnly {
		return sync.SyncUploadOnly
	}

	return sync.SyncBidirectional
}

// printSyncReport formats and prints the sync report to stderr.
func printSyncReport(r *sync.SyncReport) {
	if r.DryRun {
		statusf("Dry run — no changes applied\n")
	}

	statusf("Mode: %s\n", r.Mode)
	statusf("Duration: %s\n", r.Duration)

	total := r.FolderCreates + r.Moves + r.Downloads + r.Uploads +
		r.LocalDeletes + r.RemoteDeletes + r.Conflicts +
		r.SyncedUpdates + r.Cleanups

	if total == 0 {
		statusf("No changes detected\n")
		return
	}

	statusf("\nPlan:\n")

	if r.FolderCreates > 0 {
		statusf("  Folder creates: %d\n", r.FolderCreates)
	}

	if r.Moves > 0 {
		statusf("  Moves:          %d\n", r.Moves)
	}

	if r.Downloads > 0 {
		statusf("  Downloads:      %d\n", r.Downloads)
	}

	if r.Uploads > 0 {
		statusf("  Uploads:        %d\n", r.Uploads)
	}

	if r.LocalDeletes > 0 {
		statusf("  Local deletes:  %d\n", r.LocalDeletes)
	}

	if r.RemoteDeletes > 0 {
		statusf("  Remote deletes: %d\n", r.RemoteDeletes)
	}

	if r.Conflicts > 0 {
		statusf("  Conflicts:      %d\n", r.Conflicts)
	}

	if r.SyncedUpdates > 0 {
		statusf("  Synced updates: %d\n", r.SyncedUpdates)
	}

	if r.Cleanups > 0 {
		statusf("  Cleanups:       %d\n", r.Cleanups)
	}

	if !r.DryRun {
		statusf("\nResults:\n")
		statusf("  Succeeded: %d\n", r.Succeeded)
		statusf("  Failed:    %d\n", r.Failed)

		for _, e := range r.Errors {
			statusf("  Error:     %v\n", e)
		}
	}
}
