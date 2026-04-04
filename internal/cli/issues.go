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
		RunE: runIssuesList,
	}

	cmd.AddCommand(newIssuesForceDeletesCmd())

	return cmd
}

// --- issues list ---

func runIssuesList(cmd *cobra.Command, _ []string) error {
	return newIssuesService(mustCLIContext(cmd.Context())).runList(cmd.Context())
}

// --- issues force-deletes ---

func newIssuesForceDeletesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "force-deletes",
		Short: "Approve all currently held deletes",
		Long: `Approve all currently held deletes for the selected drive.

This releases big-delete protection for the current drive only. It does not
affect any other issue type.`,
		Args: cobra.NoArgs,
		RunE: runIssuesForceDeletes,
	}

	return cmd
}

func runIssuesForceDeletes(cmd *cobra.Command, _ []string) error {
	return newIssuesService(mustCLIContext(cmd.Context())).runForceDeletes(cmd.Context())
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
