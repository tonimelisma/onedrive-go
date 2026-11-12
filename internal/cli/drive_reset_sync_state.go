package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func newDriveResetSyncStateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reset-sync-state",
		Short: "Delete and recreate the sync state DB for one configured drive",
		Long: `Delete and recreate the per-drive sync state database.

This only resets durable sync state for the selected drive. It does not delete
the drive config, token files, sync directory, local files, or remote files.

Type RESET to confirm, or use --yes for non-interactive automation.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.NoArgs,
		RunE:        confirmedDriveStateRunE(runDriveResetSyncStateWithInput),
	}

	cmd.Flags().Bool("yes", false, "skip the interactive RESET confirmation prompt")

	return cmd
}

// confirmedDriveStateRunE adapts a drive-state command that asks for
// confirmation unless --yes is given.
func confirmedDriveStateRunE(
	run func(ctx context.Context, cc *CLIContext, stdin io.Reader, yes bool) error,
) func(cmd *cobra.Command, _ []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		yes, err := cmd.Flags().GetBool("yes")
		if err != nil {
			return fmt.Errorf("read --yes flag: %w", err)
		}
		return run(cmd.Context(), mustCLIContext(cmd.Context()), cmd.InOrStdin(), yes)
	}
}

// resolveDriveForStateChange resolves --drive to one configured drive and
// refuses while a sync owner holds it. verb completes "specify which drive
// to ..."; action completes "cannot ... while a sync owner is active".
func resolveDriveForStateChange(
	ctx context.Context,
	cc *CLIContext,
	verb string,
	action string,
) (*config.Config, driveid.CanonicalID, error) {
	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return nil, driveid.CanonicalID{}, driveErr
	}
	if driveSelector == "" {
		return nil, driveid.CanonicalID{}, fmt.Errorf("--drive is required (specify which drive to %s)", verb)
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return nil, driveid.CanonicalID{}, fmt.Errorf("loading config: %w", err)
	}

	cid, inConfig, err := resolveExactDriveSelector(cfg, driveSelector)
	if err != nil {
		return nil, driveid.CanonicalID{}, err
	}
	if !inConfig {
		return nil, driveid.CanonicalID{}, fmt.Errorf("drive %q not found in config", driveSelector)
	}

	if err := ensureNoLiveSyncOwner(ctx, cid, action); err != nil {
		return nil, driveid.CanonicalID{}, err
	}

	return cfg, cid, nil
}

func runDriveResetSyncStateWithInput(
	ctx context.Context,
	cc *CLIContext,
	stdin io.Reader,
	yes bool,
) error {
	cfg, cid, err := resolveDriveForStateChange(ctx, cc, "reset", resetSyncStateAction)
	if err != nil {
		return err
	}

	// The prompt is a conversation, not the command's result: stderr.
	if err := confirmDriveStateResetIntent(stdin, cc.Status(), cid, yes); err != nil {
		return err
	}

	dbPath := config.DriveStatePath(cid)
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cid)
	}

	syncDir := driveSyncDir(cfg, cid, cc.Logger)
	if err := changeDriveStateUnderFolderLock(ctx, syncDir, dbPath, resetSyncStateAction, cc.Logger, func() error {
		if resetErr := syncengine.ResetStateDB(ctx, dbPath, cc.Logger); resetErr != nil {
			return fmt.Errorf("reset sync state DB: %w", resetErr)
		}
		return nil
	}); err != nil {
		return err
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), driveResetSyncStateJSONOutput{Drive: cid.String(), Reset: true})
	}

	if err := writef(cc.Output(), "Reset sync state DB for %s.\n", cid.String()); err != nil {
		return err
	}

	return writeln(cc.Output(), "The next sync for this drive will rebuild state from current local and remote data.")
}

// driveResetSyncStateJSONOutput is `drive reset-sync-state --json`.
type driveResetSyncStateJSONOutput struct {
	Drive string `json:"drive"`
	Reset bool   `json:"reset"`
}

// resetSyncStateAction completes "cannot ..." in the reset's refusals.
const resetSyncStateAction = "reset sync state"

// driveSyncDir is the folder a configured drive syncs into, the way its
// engine resolves it. A "~/..." path is expanded: unexpanded it named no
// folder, and the rebind's file count read 0 for a full one (live,
// 2026-11-04).
func driveSyncDir(cfg *config.Config, cid driveid.CanonicalID, logger *slog.Logger) string {
	drive := cfg.Drives[cid]
	return config.ExpandTilde(config.EffectiveSyncDir(cfg, cid, drive.SyncDir, logger))
}

// changeDriveStateUnderFolderLock runs change, an offline rewrite of the
// drive's state store at dbPath (named by action), while holding every
// sync-folder lock an engine with that store open could hold
// (syncengine.LockDriveForOfflineChange): the folder now at the drive's sync
// path, and the folder the store records. The owner probe only sees an owner
// that holds the control socket; an engine still draining or closing its
// store after its owner stopped answering, or one run by another
// configuration sharing the data directory, holds only these locks. Callers
// take them after their prompt, so a person reading it does not hold the
// folders against a sync that starts meanwhile.
func changeDriveStateUnderFolderLock(
	ctx context.Context,
	syncDir string,
	dbPath string,
	action string,
	logger *slog.Logger,
	change func() error,
) (err error) {
	release, err := syncengine.LockDriveForOfflineChange(ctx, config.DefaultDataDir(), syncDir, dbPath, logger)
	switch {
	case errors.Is(err, syncengine.ErrSyncRootInUse):
		return fmt.Errorf("cannot %s: %w; stop that sync first", action, err)
	case err != nil:
		return fmt.Errorf("lock sync folder: %w", err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			err = errors.Join(err, fmt.Errorf("release sync folder lock: %w", releaseErr))
		}
	}()

	return change()
}

// ensureNoLiveSyncOwner refuses an offline state mutation (named by action)
// while a sync owner holds the drive: the owner's next commit would race it.
func ensureNoLiveSyncOwner(ctx context.Context, canonicalID driveid.CanonicalID, action string) error {
	probe, err := probeControlOwner(ctx)
	// An owner that did not answer is still an owner: refuse, as for a probe
	// that could not tell.
	if err != nil && (probe.state == controlOwnerStateProbeFailed || probe.state == controlOwnerStateUnresponsive) {
		return fmt.Errorf("probe control owner: %w", err)
	}
	if probe.state != controlOwnerStateWatchOwner && probe.state != controlOwnerStateOneShotOwner {
		return nil
	}
	if probe.client == nil {
		return nil
	}

	for _, drive := range probe.client.status.Mounts {
		if strings.EqualFold(drive, canonicalID.String()) {
			return fmt.Errorf(
				"cannot %s while a sync owner is active for %s (owner mode: %s); stop sync first",
				action,
				canonicalID,
				probe.client.ownerMode(),
			)
		}
	}

	return nil
}

func confirmDriveStateResetIntent(
	stdin io.Reader,
	output io.Writer,
	canonicalID driveid.CanonicalID,
	yes bool,
) error {
	if yes {
		return nil
	}

	if !isWriterTTY(stdinAsWriter(stdin)) {
		return fmt.Errorf("reset-sync-state requires confirmation; rerun with --yes or use an interactive terminal")
	}

	if err := writeln(output, "This deletes and recreates the sync state database for "+canonicalID.String()+"."); err != nil {
		return err
	}
	if err := writeln(output, "Local files, remote files, config, and tokens will not be changed."); err != nil {
		return err
	}
	if err := writef(output, "Type RESET to continue: "); err != nil {
		return err
	}

	reader := bufio.NewReader(stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read reset confirmation: %w", err)
	}
	if strings.TrimSpace(line) != "RESET" {
		return fmt.Errorf("reset-sync-state canceled: confirmation token did not match RESET")
	}

	return nil
}

func stdinAsWriter(r io.Reader) io.Writer {
	writer, ok := r.(io.Writer)
	if ok {
		return writer
	}

	return nil
}
