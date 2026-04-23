package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show sync status and mount health",
		Long: `Display the status of all configured accounts and runtime mounts.

Status always shows the same per-mount sync-health contract for every displayed
mount. Use --drive to filter which configured parent drive set is shown, and
--verbose to expand sampled path and row lists.`,
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

	return runStatusCommand(mustCLIContext(cmd.Context()), showPerf)
}
