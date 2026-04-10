package cli

import (
	"time"

	"github.com/spf13/cobra"
)

func newIssuesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "issues",
		Short: "List current sync issues",
		Long: `Display current sync issues for the selected drive.

Shows issue families that still need attention, including held deletes,
shared-folder write blocks, authentication problems, and actionable file
failures.`,
		Args: cobra.NoArgs,
		RunE: runIssuesList,
	}

	cmd.AddCommand(newIssuesApproveDeletesCmd())

	return cmd
}

// --- issues list ---

func runIssuesList(cmd *cobra.Command, _ []string) error {
	return newIssuesService(mustCLIContext(cmd.Context())).runList(cmd.Context())
}

// --- issues approve-deletes ---

func newIssuesApproveDeletesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve-deletes",
		Short: "Approve all currently held deletes",
		Long: `Approve all currently held deletes for the selected drive.

This records durable approval for held delete-safety rows on the current drive
only. The sync engine executes matching approved deletes on a later pass.`,
		Args: cobra.NoArgs,
		RunE: runIssuesApproveDeletes,
	}

	return cmd
}

func runIssuesApproveDeletes(cmd *cobra.Command, _ []string) error {
	return newIssuesService(mustCLIContext(cmd.Context())).runApproveDeletes(cmd.Context())
}

// --- helpers ---

func truncateID(id string) string {
	if len(id) <= conflictIDPrefixLen {
		return id
	}

	return id[:conflictIDPrefixLen]
}

func formatNanoTimestamp(nanos int64) string {
	if nanos == 0 {
		return ""
	}

	return time.Unix(0, nanos).UTC().Format(time.RFC3339)
}
