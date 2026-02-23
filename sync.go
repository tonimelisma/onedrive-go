package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newSyncCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Synchronize files with OneDrive",
		Long: `Run a one-shot sync cycle between the local directory and OneDrive.

By default, sync is bidirectional. Use --download-only or --upload-only for
one-way sync. Use --dry-run to preview what would happen without making changes.`,
		RunE: func(_ *cobra.Command, _ []string) error {
			return fmt.Errorf("sync engine not yet implemented (Phase 4v2)")
		},
	}

	// Flags preserved for forward compatibility with the event-driven engine.
	cmd.Flags().Bool("download-only", false, "only download remote changes")
	cmd.Flags().Bool("upload-only", false, "only upload local changes")
	cmd.Flags().Bool("dry-run", false, "preview sync actions without executing")
	cmd.Flags().Bool("force", false, "override big-delete safety threshold")
	cmd.Flags().Bool("watch", false, "continuous sync (not yet implemented)")

	cmd.MarkFlagsMutuallyExclusive("download-only", "upload-only")

	return cmd
}
