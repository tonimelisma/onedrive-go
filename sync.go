package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
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
	cc := mustCLIContext(cmd.Context())
	logger := cc.Logger

	// Wrap the command context with signal handling: first SIGINT/SIGTERM
	// triggers graceful shutdown (context cancel → drain in-flight actions),
	// second signal force-exits. Applies to both one-shot and watch modes.
	ctx := shutdownContext(cmd.Context(), logger)

	session, err := NewDriveSession(ctx, cc.Cfg, cc.RawConfig, cc.Logger)
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

	engine, err := sync.NewEngine(&sync.EngineConfig{
		DBPath:        dbPath,
		SyncRoot:      syncDir,
		DataDir:       config.DefaultDataDir(),
		DriveID:       session.DriveID,
		Fetcher:       session.Client,
		Items:         session.Client,
		Downloads:     session.Transfer,
		Uploads:       session.Transfer,
		DriveVerifier: session.Client,
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
		cfgPath := cc.CfgPath

		return runSyncWatch(ctx, engine, mode, sync.WatchOpts{
			Force: force,
		}, cfgPath, cc.Cfg.CanonicalID, logger)
	}

	// One-shot mode: reject if paused.
	if cc.Cfg.Paused {
		return fmt.Errorf("drive %s is paused — run 'onedrive-go resume' to unpause", cc.Cfg.CanonicalID)
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
		printSyncReport(report, cc.Flags.Quiet)
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
func printSyncReport(r *sync.SyncReport, quiet bool) {
	if r.DryRun {
		statusf(quiet, "Dry run — no changes applied\n")
	}

	statusf(quiet, "Mode: %s\n", r.Mode)
	statusf(quiet, "Duration: %s\n", r.Duration)

	total := r.FolderCreates + r.Moves + r.Downloads + r.Uploads +
		r.LocalDeletes + r.RemoteDeletes + r.Conflicts +
		r.SyncedUpdates + r.Cleanups

	if total == 0 {
		statusf(quiet, "No changes detected\n")
		return
	}

	statusf(quiet, "\nPlan:\n")

	if r.FolderCreates > 0 {
		statusf(quiet, "  Folder creates: %d\n", r.FolderCreates)
	}

	if r.Moves > 0 {
		statusf(quiet, "  Moves:          %d\n", r.Moves)
	}

	if r.Downloads > 0 {
		statusf(quiet, "  Downloads:      %d\n", r.Downloads)
	}

	if r.Uploads > 0 {
		statusf(quiet, "  Uploads:        %d\n", r.Uploads)
	}

	if r.LocalDeletes > 0 {
		statusf(quiet, "  Local deletes:  %d\n", r.LocalDeletes)
	}

	if r.RemoteDeletes > 0 {
		statusf(quiet, "  Remote deletes: %d\n", r.RemoteDeletes)
	}

	if r.Conflicts > 0 {
		statusf(quiet, "  Conflicts:      %d\n", r.Conflicts)
	}

	if r.SyncedUpdates > 0 {
		statusf(quiet, "  Synced updates: %d\n", r.SyncedUpdates)
	}

	if r.Cleanups > 0 {
		statusf(quiet, "  Cleanups:       %d\n", r.Cleanups)
	}

	if !r.DryRun {
		statusf(quiet, "\nResults:\n")
		statusf(quiet, "  Succeeded: %d\n", r.Succeeded)
		statusf(quiet, "  Failed:    %d\n", r.Failed)

		for _, e := range r.Errors {
			statusf(quiet, "  Error:     %v\n", e)
		}
	}
}

// watchRunner abstracts sync.Engine.RunWatch for testability.
type watchRunner interface {
	RunWatch(ctx context.Context, mode sync.SyncMode, opts sync.WatchOpts) error
}

// runSyncWatch wraps engine.RunWatch with PID file management, SIGHUP-based
// config reload, and pause/resume support. On SIGHUP, the current RunWatch
// session is canceled and the loop re-reads config to check paused state.
// The engine is reused across RunWatch invocations (safe: BaselineManager
// holds the DB connection, watch-session-scoped fields are overwritten).
func runSyncWatch(
	ctx context.Context, engine *sync.Engine, mode sync.SyncMode,
	opts sync.WatchOpts, cfgPath string, cid driveid.CanonicalID, logger *slog.Logger,
) error {
	// PID file prevents multiple daemons and enables SIGHUP delivery.
	pidPath := config.PIDFilePath()

	cleanup, err := writePIDFile(pidPath)
	if err != nil {
		return err
	}

	defer cleanup()

	sighup := sighupChannel()
	defer signal.Stop(sighup)

	return watchLoop(ctx, engine, mode, opts, cfgPath, cid, sighup, logger)
}

// watchLoop is the core watch loop extracted for testability. It re-reads
// config on each iteration: if paused, blocks in waitForResume; otherwise
// starts a RunWatch session that can be interrupted by SIGHUP.
func watchLoop(
	ctx context.Context, runner watchRunner, mode sync.SyncMode,
	opts sync.WatchOpts, cfgPath string, cid driveid.CanonicalID,
	sighup <-chan os.Signal, logger *slog.Logger,
) error {
	for {
		paused, pausedUntil := checkPausedState(cfgPath, cid, logger)

		if paused {
			logger.Info("drive paused, waiting for SIGHUP or timed expiry",
				"canonical_id", cid.String())

			if err := waitForResume(ctx, sighup, cfgPath, cid, pausedUntil, logger); err != nil {
				return err
			}

			continue
		}

		// Create cancellable context for this watch session.
		watchCtx, cancelWatch := context.WithCancel(ctx)

		// SIGHUP listener: cancels current watch session so the loop
		// re-reads config (might have changed paused state, etc.).
		go func() {
			select {
			case <-sighup:
				logger.Info("SIGHUP received, reloading config")
				cancelWatch()
			case <-watchCtx.Done():
			}
		}()

		watchErr := runner.RunWatch(watchCtx, mode, opts)
		cancelWatch()

		// Parent shutdown (SIGINT/SIGTERM) — exit cleanly.
		if ctx.Err() != nil {
			return nil
		}

		// RunWatch returned because SIGHUP canceled watchCtx.
		// Log and loop back to re-check config.
		if watchErr != nil {
			logger.Debug("RunWatch exited", "error", watchErr)
		}

		logger.Info("config reloaded, re-entering watch loop")
	}
}

// checkPausedState reads the config file and returns the paused state for the
// given drive. Returns (false, "") if the drive is not paused or if the config
// cannot be read.
func checkPausedState(cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) (paused bool, pausedUntil string) {
	cfg, err := config.LoadOrDefault(cfgPath, logger)
	if err != nil {
		logger.Warn("could not reload config, assuming not paused", "error", err)

		return false, ""
	}

	d, ok := cfg.Drives[cid]
	if !ok {
		logger.Warn("drive not found in config after reload, assuming not paused",
			"canonical_id", cid.String())

		return false, ""
	}

	if d.Paused == nil || !*d.Paused {
		return false, ""
	}

	var until string
	if d.PausedUntil != nil {
		until = *d.PausedUntil
	}

	return true, until
}

// waitForResume blocks until one of: SIGHUP received, timed pause expires, or
// parent context is canceled. When timed pause expires, the daemon clears the
// paused/paused_until keys from config so restarts don't re-pause.
func waitForResume(
	ctx context.Context, sighup <-chan os.Signal, cfgPath string,
	cid driveid.CanonicalID, pausedUntil string, logger *slog.Logger,
) error {
	var timer <-chan time.Time

	if pausedUntil != "" {
		until, err := time.Parse(time.RFC3339, pausedUntil)
		if err != nil {
			logger.Warn("invalid paused_until value, ignoring timer",
				"paused_until", pausedUntil, "error", err)
		} else if until.After(time.Now()) {
			remaining := time.Until(until)
			logger.Info("timed pause active", "expires_in", remaining.Round(time.Second))
			timer = time.After(remaining)
		} else {
			// Already expired — clear config and return immediately.
			daemonClearPausedKeys(cfgPath, cid, logger)

			return nil
		}
	}

	select {
	case <-sighup:
		logger.Info("SIGHUP received while paused, checking config")

		return nil
	case <-timer:
		logger.Info("timed pause expired, resuming")
		daemonClearPausedKeys(cfgPath, cid, logger)

		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// daemonClearPausedKeys removes paused/paused_until from config when the
// daemon's timed pause expires. Errors are logged but not fatal — the daemon
// will re-read config on next loop iteration.
func daemonClearPausedKeys(cfgPath string, cid driveid.CanonicalID, logger *slog.Logger) {
	if err := config.DeleteDriveKey(cfgPath, cid, "paused"); err != nil {
		logger.Warn("could not clear paused key from config", "error", err)
	}

	if err := config.DeleteDriveKey(cfgPath, cid, "paused_until"); err != nil {
		logger.Warn("could not clear paused_until key from config", "error", err)
	}
}
