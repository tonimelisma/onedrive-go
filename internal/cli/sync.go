package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync run between the local directory and OneDrive.

By default, sync is bidirectional. Use --download-only or --upload-only for
one-way sync. Use --dry-run to preview what would happen without making changes.`,
		// sync handles its own config resolution via ResolveDrives (multi-drive).
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runSync,
	}

	cmd.Flags().Bool("download-only", false, "only download remote changes")
	cmd.Flags().Bool("upload-only", false, "only upload local changes")
	cmd.Flags().Bool("dry-run", false, "preview sync actions without executing")
	cmd.Flags().Bool("watch", false, "continuously sync changes (watch mode)")
	cmd.Flags().Bool("full", false, "run full remote refresh (enumerate all remote items, detect orphans)")
	cmd.Flags().Bool(allowRootFlag, false, "allow sync to run as root (files it creates will be owned by root)")

	cmd.MarkFlagsMutuallyExclusive("download-only", "upload-only")
	cmd.MarkFlagsMutuallyExclusive("full", "watch")

	return cmd
}

func runSync(cmd *cobra.Command, _ []string) error {
	cc := mustCLIContext(cmd.Context())
	if err := refuseSyncAsRoot(cmd, cc); err != nil {
		return err
	}

	watch, err := cmd.Flags().GetBool("watch")
	if err != nil {
		return fmt.Errorf("read --watch flag: %w", err)
	}

	mode := syncModeFromFlags(cmd)
	// Signal handling is installed once for every command at the process
	// entry point, so the command context already cancels on SIGINT/SIGTERM.
	ctx := cmd.Context()

	dryRunValue, dryRunSet, err := syncDryRunOverride(cmd)
	if err != nil {
		return err
	}
	var dryRun *bool
	if dryRunSet {
		dryRun = &dryRunValue
	}

	fullReconcile, err := cmd.Flags().GetBool("full")
	if err != nil {
		return fmt.Errorf("read --full flag: %w", err)
	}

	return runSyncCommand(ctx, cc, syncCommandOptions{
		Mode:          mode,
		Watch:         watch,
		DryRun:        dryRun,
		FullReconcile: fullReconcile,
	})
}

func syncDryRunOverride(cmd *cobra.Command) (bool, bool, error) {
	if !cmd.Flags().Changed("dry-run") {
		return false, false, nil
	}

	dryRun, err := cmd.Flags().GetBool("dry-run")
	if err != nil {
		return false, false, fmt.Errorf("read --dry-run flag: %w", err)
	}

	return dryRun, true, nil
}

func resolveSyncDryRun(cfgDryRun bool, override *bool, watch bool) (bool, error) {
	effectiveDryRun := cfgDryRun
	if override != nil {
		effectiveDryRun = *override
	}

	if watch && effectiveDryRun {
		return false, fmt.Errorf("watch mode does not support dry-run; disable config dry_run or pass --dry-run=false")
	}

	return effectiveDryRun, nil
}

// syncModeFromFlags determines the SyncMode from CLI flags.
// Uses Changed() instead of GetBool() — both flags are boolean with default
// false, so GetBool() would work identically. Changed() is preferred because
// it directly expresses intent: "did the user explicitly set this flag?" This
// is the standard Cobra pattern for flags where presence equals activation.
func syncModeFromFlags(cmd *cobra.Command) syncengine.SyncMode {
	if cmd.Flags().Changed("download-only") {
		return syncengine.SyncDownloadOnly
	}

	if cmd.Flags().Changed("upload-only") {
		return syncengine.SyncUploadOnly
	}

	return syncengine.SyncBidirectional
}

const allowRootFlag = "allow-root"

// errSyncAsRoot is the refusal to sync as root without --allow-root (P-9).
// A root sync writes root-owned files and folders into the user's sync
// folder, which the user's own later syncs, editors and backups then cannot
// change or delete; it is almost always an accident (sudo, a system unit
// without User=), so it is refused unless explicitly asked for.
var errSyncAsRoot = errors.New("refusing to sync as root: files it creates would be owned by root. " +
	"Run sync as the user who owns the sync folder, or pass --allow-root if this is intended")

// refuseSyncAsRoot returns a usage error when sync runs with effective UID 0
// and --allow-root was not given.
func refuseSyncAsRoot(cmd *cobra.Command, cc *CLIContext) error {
	euid := os.Geteuid
	if cc.seams.effectiveUID != nil {
		euid = cc.seams.effectiveUID
	}
	if euid() != 0 {
		return nil
	}

	allowRoot, err := cmd.Flags().GetBool(allowRootFlag)
	if err != nil {
		return fmt.Errorf("read --%s flag: %w", allowRootFlag, err)
	}
	if allowRoot {
		cc.Logger.Warn("running sync as root because --allow-root was given; files it creates will be owned by root")
		return nil
	}

	return &usageError{commandPath: cmd.CommandPath(), err: errSyncAsRoot}
}
