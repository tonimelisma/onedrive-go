package cli

import (
	"context"
	"fmt"
	"time"

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

A running sync --watch picks up the change immediately.

Examples:
  onedrive-go resume --drive personal:user@example.com
  onedrive-go resume`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runResume,
	}
}

func runResume(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	return runResumeCommand(ctx, mustCLIContext(ctx), time.Now)
}

// clearPausedKeys removes both paused and paused_until keys from a drive
// section, in one locked write.
func clearPausedKeys(ctx context.Context, cfgPath string, cid driveid.CanonicalID) error {
	if err := config.DeleteDriveKeys(ctx, cfgPath, cid, "paused", "paused_until"); err != nil {
		return fmt.Errorf("clear pause keys: %w", err)
	}

	return nil
}
