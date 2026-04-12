package cli

import (
	"encoding/json"
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
	return runRecycleBinListWithFactory(ctx, cc, defaultRecycleBinSessionFactory(cc))
}

func runRecycleBinRestore(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)
	return runRecycleBinRestoreWithFactory(ctx, cc, args[0], defaultRecycleBinSessionFactory(cc))
}

func runRecycleBinEmpty(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()

	confirm, err := cmd.Flags().GetBool("confirm")
	if err != nil {
		return fmt.Errorf("read --confirm flag: %w", err)
	}

	cc := mustCLIContext(ctx)
	return runRecycleBinEmptyWithFactory(ctx, cc, confirm, defaultRecycleBinSessionFactory(cc))
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
			Deleted: formatAPITime(items[i].ModifiedAt),
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode recycle bin output: %w", err)
	}

	return nil
}
