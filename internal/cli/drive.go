package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDriveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:         "drive",
		Short:       "Manage drives (list, add, remove, search)",
		Long:        "List, add, remove, or search drives in the configuration.",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
	}

	cmd.AddCommand(newDriveListCmd())
	cmd.AddCommand(newDriveAddCmd())
	cmd.AddCommand(newDriveRemoveCmd())
	cmd.AddCommand(newDriveResetSyncStateCmd())
	cmd.AddCommand(newDriveSearchCmd())

	return cmd
}

// --- drive list ---

func newDriveListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show configured and available drives",
		Long: `Display all configured drives with their sync status, plus all available
drives discovered from your accounts (personal, business, SharePoint).

SharePoint discovery is limited to the first 10 sites by default.
Use --all to show all discoverable drives, or 'drive search' for
targeted SharePoint queries.`,
		// skipConfig: drive list loads config leniently itself (R-4.8.4) —
		// Phase 2 strict loading must not run.
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveList,
	}

	cmd.Flags().Bool("all", false, "show all discoverable drives (remove SharePoint site cap)")

	return cmd
}

func runDriveList(cmd *cobra.Command, _ []string) error {
	showAll, err := cmd.Flags().GetBool("all")
	if err != nil {
		return fmt.Errorf("reading --all flag: %w", err)
	}

	return runDriveListWithContext(cmd.Context(), mustCLIContext(cmd.Context()), showAll)
}

// --- drive add ---

func newDriveAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [canonical-id]",
		Short: "Add a new drive to the configuration",
		Long: `Add a drive to the configuration by canonical ID, raw shared-folder URL, or shared folder name.

If the drive already exists in config, reports it as already configured.
If the drive is new, it is added with a default sync directory.

For shared drives, you can use:
- a shared selector (shared:<recipient>:<drive>:<item>)
- the original raw share URL for a shared folder
- a search term matched against shared folder names (case-insensitive substring)

Without arguments, lists available drives that can be added.

Examples:
  onedrive-go drive add personal:user@example.com
  onedrive-go drive add sharepoint:user@contoso.com:marketing:Documents
  onedrive-go drive add https://1drv.ms/f/c/example
  onedrive-go drive add "Shared Folder"`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveAdd,
		Args:        cobra.MaximumNArgs(1),
	}
}

func runDriveAdd(cmd *cobra.Command, args []string) error {
	return runDriveAddWithContext(cmd.Context(), mustCLIContext(cmd.Context()), args)
}

// --- drive remove ---

func newDriveRemoveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a drive from the configuration",
		Long: `Remove a drive's config section. The token, state database, and sync directory
are preserved so the drive can be re-added later without data loss.

With --purge, the state database is also deleted.
The sync directory is never deleted automatically.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveRemove,
	}

	cmd.Flags().Bool("purge", false, "also delete the state database")

	return cmd
}

func runDriveRemove(cmd *cobra.Command, _ []string) error {
	purge, err := cmd.Flags().GetBool("purge")
	if err != nil {
		return fmt.Errorf("reading --purge flag: %w", err)
	}

	return runDriveRemoveWithContext(cmd.Context(), mustCLIContext(cmd.Context()), purge)
}

// --- drive search ---

func newDriveSearchCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "search <term>",
		Short: "Search SharePoint sites by name",
		Long: `Search for SharePoint sites matching the given term.

Lists matching sites and their document libraries with canonical IDs
that can be used with 'drive add'.

Use --account to restrict the search to a specific business account.

Examples:
  onedrive-go drive search marketing
  onedrive-go drive search "project docs" --account user@contoso.com`,
		// skipConfig: drive search loads config leniently itself —
		// Phase 2 strict loading must not run.
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		RunE:        runDriveSearch,
		Args:        cobra.ExactArgs(1),
	}
}

func runDriveSearch(cmd *cobra.Command, args []string) error {
	return runDriveSearchWithContext(cmd.Context(), mustCLIContext(cmd.Context()), args[0])
}
