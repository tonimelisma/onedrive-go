package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/humanunit"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/localtrash"
)

// The local trash keeps what sync removed or replaced on this computer for
// localtrash.Retention (R-6.4.9). It is per drive and lives under the data
// directory, so these commands work without network access. A drive's trash
// commands also cover the trash of each of its shortcut children
// (localtrash.Trash.Children): a child is a mount, not a configured drive,
// and each entry names the sync folder it came from.

func newTrashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trash",
		Short: "Recover files sync removed or replaced on this computer",
		Long: `Files that sync deletes on this computer, and the previous version of every
file a download replaces, go to a per-drive trash for 30 days instead of being
destroyed. List them, put one back, or empty the trash.

Select the drive with --drive. A drive's trash also holds what sync removed
from the shared folders added to it as shortcuts, including the files it set
aside when a shared folder was removed from the account.`,
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

// driveTrash is a configured drive's trash and the trash of each of its
// shortcut children, the drive's own first.
type driveTrash struct {
	drive   string
	trashes []*localtrash.Trash
}

// openDriveTrash resolves --drive to a configured drive's trash and its
// shortcut children's.
func openDriveTrash(cc *CLIContext, verb string) (*driveTrash, error) {
	own, drive, err := openConfiguredDriveTrash(cc, verb)
	if err != nil {
		return nil, err
	}
	children, err := own.Children()
	if err != nil {
		return nil, fmt.Errorf("open the trash of %s's shortcuts: %w", drive, err)
	}

	return &driveTrash{drive: drive, trashes: append([]*localtrash.Trash{own}, children...)}, nil
}

// list returns every entry of every trash of the drive, newest first.
func (d *driveTrash) list() ([]localtrash.Entry, error) {
	var entries []localtrash.Entry
	for _, trash := range d.trashes {
		found, err := trash.List()
		if err != nil {
			return nil, fmt.Errorf("list trash %s: %w", trash.Dir(), err)
		}
		entries = append(entries, found...)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].TrashedAt.Equal(entries[j].TrashedAt) {
			return entries[i].TrashedAt.After(entries[j].TrashedAt)
		}
		return entries[i].ID > entries[j].ID
	})

	return entries, nil
}

// restore restores entry id from whichever of the drive's trashes holds it.
// It returns as localtrash.Trash.Restore does.
func (d *driveTrash) restore(id string) (string, error) {
	for _, trash := range d.trashes {
		target, err := trash.Restore(id)
		if errors.Is(err, localtrash.ErrEntryNotFound) && target == "" {
			continue
		}
		return target, err //nolint:wrapcheck // reportTrashRestore names the command
	}

	return "", fmt.Errorf("%w: %s", localtrash.ErrEntryNotFound, id)
}

// empty empties every trash of the drive, going on past a failure, and
// returns how many files it removed in all.
func (d *driveTrash) empty() (int, error) {
	removed := 0
	var errs error
	for _, trash := range d.trashes {
		count, err := trash.Empty()
		removed += count
		if err != nil {
			errs = localpath.JoinOnOneLine(errs, fmt.Errorf("empty trash %s: %w", trash.Dir(), err))
		}
	}

	return removed, errs
}

// openConfiguredDriveTrash resolves --drive to a configured drive's own trash.
func openConfiguredDriveTrash(cc *CLIContext, verb string) (*localtrash.Trash, string, error) {
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

func (out trashListJSONOutput) withEmptyLists() trashListJSONOutput {
	out.Entries = emptyIfNil(out.Entries)

	return out
}

func runTrashList(_ context.Context, cc *CLIContext) error {
	trash, err := openDriveTrash(cc, "list")
	if err != nil {
		return err
	}
	entries, err := trash.list()
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		out := trashListJSONOutput{Drive: trash.drive}
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
		return writeJSONDocument(cc.Output(), out.withEmptyLists())
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
	what, why := trashReasonText(entry.Reason)
	suffix := ""
	if entry.State == localtrash.StatePending {
		suffix = " [interrupted, recoverable]"
	}
	return writef(cc.Output(), "%s  %s  %s %s (%s)%s%s\n",
		entry.ID, entry.TrashedAt.Local().Format(time.DateTime), what, entry.Path, humanunit.Size(entry.Size), why, suffix)
}

// trashReasonText is how `trash list` words why a file is in the trash: the
// word before its path and what follows its size. The switch names every
// reason localtrash declares (a test reads them from its source); an entry
// with a reason outside that set, which a later version may write, is listed
// with the word its record holds.
func trashReasonText(reason localtrash.Reason) (string, string) {
	switch reason {
	case localtrash.ReasonDeleted:
		return "deleted", ""
	case localtrash.ReasonReplaced:
		return "replaced", ""
	case localtrash.ReasonReleased:
		return "released", ": its shared folder was removed from this account"
	}
	if reason == "" {
		return "removed", ""
	}

	return string(reason), ""
}

type trashRestoreJSONOutput struct {
	Drive      string `json:"drive"`
	ID         string `json:"id"`
	RestoredTo string `json:"restored_to"`
	// Warning is what went wrong after the file was put back. The command
	// exits 1 when it is set, although the file is at RestoredTo.
	Warning string `json:"warning,omitempty"`
}

func runTrashRestore(_ context.Context, cc *CLIContext, id string) error {
	trash, err := openDriveTrash(cc, "restore from")
	if err != nil {
		return err
	}
	target, restoreErr := trash.restore(id)

	return reportTrashRestore(cc, trash.drive, id, target, restoreErr)
}

// reportTrashRestore prints what a restore did and returns its failure. A
// restore that put the file back and then failed is printed as restored, with
// what went wrong, and still fails the command: the file is at target
// whatever the error says, so the user is never told only that it failed.
func reportTrashRestore(cc *CLIContext, drive, id, target string, restoreErr error) error {
	if target == "" {
		return fmt.Errorf("restore from trash: %w", restoreErr)
	}

	result := trashRestoreJSONOutput{Drive: drive, ID: id, RestoredTo: target}
	if restoreErr != nil {
		result.Warning = restoreErr.Error()
	}
	var printErr error
	if cc.Flags.JSON {
		printErr = writeJSONDocument(cc.Output(), result)
	} else {
		printErr = writef(cc.Output(), "Restored %s. The next sync uploads it again.\n", target)
	}
	if restoreErr != nil {
		return fmt.Errorf("restore from trash: %w", localpath.JoinOnOneLine(restoreErr, printErr))
	}

	return printErr
}

type trashEmptyJSONOutput struct {
	Drive   string `json:"drive"`
	Removed int    `json:"removed"`
}

func runTrashEmpty(_ context.Context, cc *CLIContext, confirm bool) error {
	if !confirm {
		return fmt.Errorf("trash empty permanently deletes every file in the local trash; rerun with --confirm")
	}
	trash, err := openDriveTrash(cc, "empty")
	if err != nil {
		return err
	}
	removed, err := trash.empty()
	if err != nil {
		return err
	}

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), trashEmptyJSONOutput{Drive: trash.drive, Removed: removed})
	}

	return writef(cc.Output(), "Permanently deleted %s from the local trash.\n", countOf(removed, "file"))
}
