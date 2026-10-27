package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

func newDriveRebindRootCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rebind-root",
		Short: "Accept the folder now at a drive's sync path as its sync folder",
		Long: `Accept the folder now at the selected drive's sync path as its sync folder.

Sync records which folder its state belongs to and refuses to run when a
different folder is there: an unmounted disk, a wiped folder, or a symbolic
link pointing somewhere new would otherwise look like every file was deleted.

Run this after moving, restoring, or re-pointing the sync folder on purpose.
Sync state is kept. If the folder is missing files the state remembers, the
next sync holds those deletes for confirmation rather than deleting them on
OneDrive.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDriveRebindRoot(cmd.Context(), mustCLIContext(cmd.Context()))
		},
	}
}

func runDriveRebindRoot(ctx context.Context, cc *CLIContext) error {
	driveSelector, driveErr := cc.Flags.SingleDrive()
	if driveErr != nil {
		return driveErr
	}
	if driveSelector == "" {
		return fmt.Errorf("--drive is required (specify which drive's sync folder to rebind)")
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

	if err := ensureNoLiveSyncOwner(ctx, cid, "rebind the sync folder"); err != nil {
		return err
	}

	dbPath := config.DriveStatePath(cid)
	if dbPath == "" {
		return fmt.Errorf("cannot determine state DB path for drive %q", cid)
	}

	if err := syncengine.RebindSyncRoot(ctx, dbPath, cc.Logger); err != nil {
		return fmt.Errorf("rebind sync folder: %w", err)
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), driveRebindRootJSONOutput{Drive: cid.String(), Rebound: true})
	}

	return writef(cc.Output(), "The next sync for %s will use the folder now at its sync path.\n", cid.String())
}

// driveRebindRootJSONOutput is `drive rebind-root --json`.
type driveRebindRootJSONOutput struct {
	Drive   string `json:"drive"`
	Rebound bool   `json:"rebound"`
}
