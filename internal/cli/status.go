package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show sync status, drive health, and pending user decisions",
		Long: `Display the status of all configured accounts and drives.

Status always shows the same per-drive sync-health contract for every displayed
drive. Use --drive to filter which drives are shown, --history to include
resolved conflict history, and --verbose to expand sampled path and row lists.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runStatus,
	}

	cmd.Flags().Bool("history", false, "include resolved conflict history for the displayed drives")
	cmd.Flags().Bool("perf", false, "include live performance snapshots from the active sync owner")

	return cmd
}

func runStatus(cmd *cobra.Command, _ []string) error {
	history, err := cmd.Flags().GetBool("history")
	if err != nil {
		return fmt.Errorf("read --history flag: %w", err)
	}
	showPerf, err := cmd.Flags().GetBool("perf")
	if err != nil {
		return fmt.Errorf("read --perf flag: %w", err)
	}

	return runStatusCommand(mustCLIContext(cmd.Context()), history, showPerf)
}
