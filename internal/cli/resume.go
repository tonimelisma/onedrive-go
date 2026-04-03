package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume syncing for a paused drive",
		Long: `Resume syncing for the specified drive. With --drive, resumes that drive.
Without --drive, resumes ALL paused drives.

If a sync --watch daemon is running, it receives a SIGHUP to pick up the change.

Examples:
  onedrive-go resume --drive personal:user@example.com
  onedrive-go resume`,
		Annotations: map[string]string{skipConfigAnnotation: "true"},
		RunE:        runResume,
	}
}

func runResume(cmd *cobra.Command, _ []string) error {
	return newSyncControlService(mustCLIContext(cmd.Context())).runResume()
}

func resumeSingleDrive(cc *CLIContext, cfg *config.Config, selector string) error {
	return newSyncControlService(cc).resumeSingleDrive(cfg, selector)
}

// clearPausedKeys removes both paused and paused_until keys from a drive section.
func clearPausedKeys(cfgPath string, cid driveid.CanonicalID) error {
	if err := config.DeleteDriveKey(cfgPath, cid, "paused"); err != nil {
		return fmt.Errorf("clearing paused flag: %w", err)
	}

	if err := config.DeleteDriveKey(cfgPath, cid, "paused_until"); err != nil {
		return fmt.Errorf("clearing paused_until: %w", err)
	}

	return nil
}
