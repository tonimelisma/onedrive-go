package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
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
		RunE: func(cmd *cobra.Command, _ []string) error {
			yes, err := cmd.Flags().GetBool("yes")
			if err != nil {
				return fmt.Errorf("read --yes flag: %w", err)
			}

			return runDriveResetSyncStateWithInput(
				cmd.Context(),
				mustCLIContext(cmd.Context()),
				cmd.InOrStdin(),
				yes,
			)
		},
	}

	cmd.Flags().Bool("yes", false, "skip the interactive RESET confirmation prompt")

	return cmd
}

func runDriveResetSyncStateWithInput(
	ctx context.Context,
	cc *CLIContext,
	stdin io.Reader,
	yes bool,
) error {
	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}
	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive to reset)")
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cid, inConfig, err := resolveExactDriveSelector(cfg, driveSelector)
	if err != nil {
		return err
	}
	if !inConfig {
		return fmt.Errorf("drive %q not found in config", driveSelector)
	}

	if err := ensureNoLiveSyncOwner(ctx, cid, "reset sync state"); err != nil {
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

	if err := syncengine.ResetStateDB(ctx, dbPath, cc.Logger); err != nil {
		return fmt.Errorf("reset sync state DB: %w", err)
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

// ensureNoLiveSyncOwner refuses an offline state mutation (named by action)
// while a sync owner holds the drive: the owner's next commit would race it.
func ensureNoLiveSyncOwner(ctx context.Context, canonicalID driveid.CanonicalID, action string) error {
	probe, err := probeControlOwner(ctx)
	if err != nil && probe.state == controlOwnerStateProbeFailed {
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
