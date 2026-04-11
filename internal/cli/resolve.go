package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newResolveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Record sync decisions that need engine execution",
		Long: `Resolve held delete approvals and unresolved conflicts for the selected drive.

Use 'resolve deletes' to approve held delete-safety rows. Use 'resolve local',
'resolve remote', or 'resolve both' to queue a conflict strategy for one
conflict or for all unresolved conflicts.`,
	}

	cmd.AddCommand(
		newResolveDeletesCmd(),
		newResolveActionCmd("local", resolutionKeepLocal),
		newResolveActionCmd("remote", resolutionKeepRemote),
		newResolveActionCmd("both", resolutionKeepBoth),
	)

	return cmd
}

func newResolveDeletesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "deletes",
		Short: "Approve all currently held deletes",
		Long: `Approve all currently held delete-safety rows for the selected drive.

The sync engine executes matching approved deletes on a later pass.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return newIssuesService(mustCLIContext(cmd.Context())).runApproveDeletes(cmd.Context())
		},
	}
}

func newResolveActionCmd(name string, resolution string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   name,
		Short: fmt.Sprintf("Queue %s for unresolved conflicts", name),
		Long: fmt.Sprintf(
			`Queue the %s conflict strategy for one unresolved conflict or for all unresolved conflicts on the selected drive.`,
			name,
		),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			resolveAll, err := cmd.Flags().GetBool("all")
			if err != nil {
				return fmt.Errorf("read --all flag: %w", err)
			}
			dryRun, err := cmd.Flags().GetBool("dry-run")
			if err != nil {
				return fmt.Errorf("read --dry-run flag: %w", err)
			}

			if !resolveAll && len(args) == 0 {
				return fmt.Errorf("specify a conflict path or ID, or use --all to resolve all conflicts")
			}
			if resolveAll && len(args) > 0 {
				return fmt.Errorf("--all and a specific conflict argument are mutually exclusive")
			}

			return newConflictsService(mustCLIContext(cmd.Context())).runResolve(
				cmd.Context(),
				args,
				resolution,
				resolveAll,
				dryRun,
			)
		},
	}

	cmd.Flags().Bool("all", false, "queue this strategy for all unresolved conflicts")
	cmd.Flags().Bool("dry-run", false, "preview resolution without executing")

	return cmd
}
