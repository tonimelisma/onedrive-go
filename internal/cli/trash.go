package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
)

// The local trash keeps what sync removed or replaced on this computer for
// localtrash.Retention (P-4). It is per drive and lives under the data
// directory, so these commands work without network access.

func newTrashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Recover files sync removed or replaced on this computer",
		Long: `Files that sync deletes on this computer, and the previous version of every
file a download replaces, go to a per-drive trash for 30 days instead of being
destroyed. List them, put one back, or empty the trash.

Select the drive with --drive.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
	}

	cmd.AddCommand(newTrashListCmd(), newTrashRestoreCmd(), newTrashEmptyCmd())

	return cmd
}

func newTrashListCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "list",
		Short:       "List files in the drive's local trash",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runTrashList(cmd.Context(), mustCLIContext(cmd.Context()))
		},
	}
}

func newTrashRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:         "restore <entry-id>",
		Short:       "Put a file from the local trash back where sync removed it",
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runTrashRestore(cmd.Context(), mustCLIContext(cmd.Context()), args[0])
		},
	}
}

func newTrashEmptyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete everything in the drive's local trash",
		Long: `Permanently delete everything in the drive's local trash. This cannot be undone.
Requires --confirm.`,
		Annotations: map[string]string{skipConfigAnnotation: skipConfigValue},
		Args:        cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			confirm, err := cmd.Flags().GetBool("confirm")
			if err != nil {
				return fmt.Errorf("read --confirm flag: %w", err)
			}
			return runTrashEmpty(cmd.Context(), mustCLIContext(cmd.Context()), confirm)
		},
	}
	cmd.Flags().Bool("confirm", false, "confirm permanent deletion of everything in the local trash")

	return cmd
}

// openDriveTrash resolves --drive to a configured drive's trash.
func openDriveTrash(cc *CLIContext, verb string) (*localtrash.Trash, string, error) {
	driveSelector, err := cc.Flags.SingleDrive()
	if err != nil {
		return nil, "", err
	}
	if driveSelector == "" {
		return nil, "", fmt.Errorf("--drive is required (specify which drive's trash to %s)", verb)
	}

	cfg, err := config.LoadOrDefault(cc.CfgPath, cc.Logger)
	if err != nil {
		return nil, "", fmt.Errorf("loading config: %w", err)
	}
	cid, inConfig, err := resolveExactDriveSelector(cfg, driveSelector)
	if err != nil {
		return nil, "", err
	}
	if !inConfig {
		return nil, "", fmt.Errorf("drive %q not found in config", driveSelector)
	}

	dir := localtrash.DirForStatePath(config.DriveStatePath(cid))
	if dir == "" {
		return nil, "", fmt.Errorf("cannot determine the trash folder for drive %q", cid)
	}

	return localtrash.Open(dir, nil), cid.String(), nil
}

// trashEntryJSON is one entry of `trash list --json`.
type trashEntryJSON struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	SyncRoot  string `json:"sync_root"`
	Reason    string `json:"reason"`
	Size      int64  `json:"size"`
	TrashedAt string `json:"trashed_at"`
	State     string `json:"state"`
}

type trashListJSONOutput struct {
	Drive   string           `json:"drive"`
	Entries []trashEntryJSON `json:"entries"`
}

func runTrashList(_ context.Context, cc *CLIContext) error {
	trash, drive, err := openDriveTrash(cc, "list")
	if err != nil {
		return err
	}
	entries, err := trash.List()
	if err != nil {
		return fmt.Errorf("list trash: %w", err)
	}

	if cc.Flags.JSON {
		out := trashListJSONOutput{Drive: drive, Entries: make([]trashEntryJSON, 0, len(entries))}
		for i := range entries {
			out.Entries = append(out.Entries, trashEntryJSON{
				ID:        entries[i].ID,
				Path:      entries[i].Path,
				SyncRoot:  entries[i].SyncRoot,
				Reason:    string(entries[i].Reason),
				Size:      entries[i].Size,
				TrashedAt: entries[i].TrashedAt.UTC().Format(time.RFC3339),
				State:     string(entries[i].State),
			})
		}
		return writeJSONDocument(cc.Output(), out)
	}

	if len(entries) == 0 {
		return writeln(cc.Output(), "The local trash is empty.")
	}
	for i := range entries {
		entry := entries[i]
		if err := writeTrashEntryLine(cc, &entry); err != nil {
			return err
		}
	}

	return nil
}

func writeTrashEntryLine(cc *CLIContext, entry *localtrash.Entry) error {
	switch entry.State {
	case localtrash.StateOrphan:
		return writef(cc.Output(), "%s  %s  unrecorded file (interrupted run): %s\n",
			entry.ID, entry.TrashedAt.Local().Format(time.DateTime), entry.ItemPath)
	case localtrash.StatePending, localtrash.StateCommitted:
	}
	what := "deleted"
	if entry.Reason == localtrash.ReasonReplaced {
		what = "replaced"
	}
	suffix := ""
	if entry.State == localtrash.StatePending {
		suffix = " [interrupted, recoverable]"
	}
	return writef(cc.Output(), "%s  %s  %s %s (%s)%s\n",
		entry.ID, entry.TrashedAt.Local().Format(time.DateTime), what, entry.Path, formatSize(entry.Size), suffix)
}

type trashRestoreJSONOutput struct {
	Drive      string `json:"drive"`
	ID         string `json:"id"`
	RestoredTo string `json:"restored_to"`
}

func runTrashRestore(_ context.Context, cc *CLIContext, id string) error {
	trash, drive, err := openDriveTrash(cc, "restore from")
	if err != nil {
		return err
	}
	target, err := trash.Restore(id)
	if err != nil {
		return fmt.Errorf("restore from trash: %w", err)
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), trashRestoreJSONOutput{Drive: drive, ID: id, RestoredTo: target})
	}

	return writef(cc.Output(), "Restored %s. The next sync uploads it again.\n", target)
}

type trashEmptyJSONOutput struct {
	Drive   string `json:"drive"`
	Removed int    `json:"removed"`
}

func runTrashEmpty(_ context.Context, cc *CLIContext, confirm bool) error {
	if !confirm {
		return fmt.Errorf("trash empty permanently deletes every file in the local trash; rerun with --confirm")
	}
	trash, drive, err := openDriveTrash(cc, "empty")
	if err != nil {
		return err
	}
	removed, err := trash.Empty()
	if err != nil {
		return fmt.Errorf("empty trash: %w", err)
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), trashEmptyJSONOutput{Drive: drive, Removed: removed})
	}

	return writef(cc.Output(), "Permanently deleted %d files from the local trash.\n", removed)
}
