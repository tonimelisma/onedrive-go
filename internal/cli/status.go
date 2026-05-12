package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show account, drive, and shared folder status",
		Long: `Display the status of configured accounts, drives, and shared folders.

Status groups configured drives under their accounts. Shared folder shortcuts
appear below the drive that owns the shortcut. Use --drive to filter configured
drives, and --verbose to expand sampled path and row lists.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runStatus,
	}

	cmd.Flags().Bool("perf", false, "include live performance snapshots from the active sync owner")

	return cmd
}

func runStatus(cmd *cobra.Command, _ []string) error {
	showPerf, err := cmd.Flags().GetBool("perf")
	if err != nil {
		return fmt.Errorf("read --perf flag: %w", err)
	}

	return runStatusCommand(mustCLIContext(cmd.Context()), false, showPerf)
}
