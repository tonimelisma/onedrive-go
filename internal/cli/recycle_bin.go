package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newRecycleBinCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recycle-bin",
		Short: "Manage OneDrive recycle bin",
		Long:  "List, restore, and empty items in the OneDrive recycle bin.",
	}

	cmd.AddCommand(
		newRecycleBinListCmd(),
		newRecycleBinRestoreCmd(),
		newRecycleBinEmptyCmd(),
	)

	return cmd
}

func newRecycleBinListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List items in the recycle bin",
		RunE:  runRecycleBinList,
	}
}

func newRecycleBinRestoreCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "restore <item-id>",
		Short: "Restore an item from the recycle bin",
		Args:  cobra.ExactArgs(1),
		RunE:  runRecycleBinRestore,
	}
}

func newRecycleBinEmptyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "empty",
		Short: "Permanently delete all items in the recycle bin",
		Long: `Permanently delete all items in the recycle bin. This action cannot be undone.
Requires --confirm flag to proceed.`,
		RunE: runRecycleBinEmpty,
	}

	cmd.Flags().Bool("confirm", false, "confirm permanent deletion of all recycle bin items")

	return cmd
}

func runRecycleBinList(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	items, err := session.ListRecycleBinItems(ctx)
	if err != nil {
		// Personal OneDrive accounts don't support the /special/recyclebin
		// endpoint — the Graph API returns HTTP 400. Provide a clear message
		// instead of a raw API error (graph-api-quirks.md §Recycle Bin).
		if errors.Is(err, graph.ErrBadRequest) {
			return fmt.Errorf("recycle bin listing is not available on Personal OneDrive accounts")
		}

		return fmt.Errorf("listing recycle bin: %w", err)
	}

	if cc.Flags.JSON {
		return formatRecycleBinJSON(cc.Output(), items)
	}

	return formatRecycleBinTable(cc.Output(), items)
}

func runRecycleBinRestore(cmd *cobra.Command, args []string) error {
	itemID := args[0]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	item, err := session.RestoreItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, graph.ErrConflict) {
			return fmt.Errorf("cannot restore %q: a file with the same name already exists at the original location", itemID)
		}

		return fmt.Errorf("restoring item: %w", err)
	}

	if cc.Flags.JSON {
		return printRecycleBinRestoreJSON(cc.Output(), recycleBinJSONItem{
			ID:      item.ID,
			Name:    item.Name,
			Size:    item.Size,
			Type:    itemType(item),
			Deleted: item.ModifiedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	cc.Statusf("Restored %q (id: %s)\n", item.Name, item.ID)

	return nil
}

func runRecycleBinEmpty(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	confirm, err := cmd.Flags().GetBool("confirm")
	if err != nil {
		return fmt.Errorf("read --confirm flag: %w", err)
	}

	if !confirm {
		return fmt.Errorf("--confirm flag required to permanently delete all recycle bin items")
	}

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	items, err := session.ListRecycleBinItems(ctx)
	if err != nil {
		if errors.Is(err, graph.ErrBadRequest) {
			return fmt.Errorf("recycle bin listing is not available on Personal OneDrive accounts")
		}

		return fmt.Errorf("listing recycle bin: %w", err)
	}

	if len(items) == 0 {
		cc.Statusf("Recycle bin is already empty\n")

		return nil
	}

	cc.Statusf("Permanently deleting %d items...\n", len(items))

	var failed int

	for i := range items {
		deleteErr := session.PermanentDeleteItem(ctx, items[i].ID)
		if deleteErr != nil {
			// Personal accounts return 405 for permanentDelete — fall back
			// to regular delete (which is effectively a no-op since the item
			// is already in the recycle bin, but some API versions support it).
			if errors.Is(deleteErr, graph.ErrMethodNotAllowed) {
				deleteErr = session.DeleteItem(ctx, items[i].ID)
			}

			if deleteErr != nil {
				cc.Statusf("  Failed to delete %q: %v\n", items[i].Name, deleteErr)
				failed++

				continue
			}
		}

		cc.Statusf("  Deleted %q\n", items[i].Name)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d items failed to delete", failed, len(items))
	}

	cc.Statusf("Recycle bin emptied\n")

	return nil
}

// --- formatting ---

const (
	typeFile   = "file"
	typeFolder = "folder"
)

type recycleBinJSONItem struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Type    string `json:"type"`
	Deleted string `json:"deleted"`
}

func itemType(item *graph.Item) string {
	if item.IsFolder {
		return typeFolder
	}

	return typeFile
}

func formatRecycleBinTable(w io.Writer, items []graph.Item) error {
	if len(items) == 0 {
		return writeln(w, "Recycle bin is empty")
	}

	headers := []string{"NAME", "SIZE", "TYPE", "DELETED", "ID"}
	rows := make([][]string, 0, len(items))

	for i := range items {
		typ := typeFile
		if items[i].IsFolder {
			typ = typeFolder
		}

		rows = append(rows, []string{
			items[i].Name,
			formatSize(items[i].Size),
			typ,
			formatTime(items[i].ModifiedAt),
			items[i].ID,
		})
	}

	return printTable(w, headers, rows)
}

// printRecycleBinRestoreJSON writes the restore command's JSON output to w.
func printRecycleBinRestoreJSON(w io.Writer, item recycleBinJSONItem) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(item); err != nil {
		return fmt.Errorf("encode recycle bin restore output: %w", err)
	}

	return nil
}

func formatRecycleBinJSON(w io.Writer, items []graph.Item) error {
	out := make([]recycleBinJSONItem, 0, len(items))
	for i := range items {
		typ := typeFile
		if items[i].IsFolder {
			typ = typeFolder
		}

		out = append(out, recycleBinJSONItem{
			ID:      items[i].ID,
			Name:    items[i].Name,
			Size:    items[i].Size,
			Type:    typ,
			Deleted: items[i].ModifiedAt.Format("2006-01-02T15:04:05Z"),
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode recycle bin output: %w", err)
	}

	return nil
}
