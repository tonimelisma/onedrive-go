package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newLsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "ls [path]",
		Short: "List files and folders",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLs,
	}
}

func runLs(cmd *cobra.Command, args []string) error {
	remotePath := "/"
	if len(args) > 0 {
		remotePath = args[0]
	}

	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	cc.Logger.Debug("ls", "path", remotePath)

	items, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("listing %q: %w", remotePath, err)
	}

	if cc.Flags.JSON {
		return printItemsJSON(os.Stdout, items)
	}

	printItemsTable(os.Stdout, items)

	return nil
}

// lsJSONItem is the JSON output schema for a single item in ls output.
type lsJSONItem struct {
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsFolder   bool   `json:"is_folder"`
	ModifiedAt string `json:"modified_at"`
	ID         string `json:"id"`
}

func printItemsJSON(w io.Writer, items []graph.Item) error {
	out := make([]lsJSONItem, 0, len(items))
	for i := range items {
		out = append(out, lsJSONItem{
			Name:       items[i].Name,
			Size:       items[i].Size,
			IsFolder:   items[i].IsFolder,
			ModifiedAt: items[i].ModifiedAt.Format("2006-01-02T15:04:05Z"),
			ID:         items[i].ID,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

func printItemsTable(w io.Writer, items []graph.Item) {
	// Sort: folders first, then alphabetical.
	sort.Slice(items, func(i, j int) bool {
		if items[i].IsFolder != items[j].IsFolder {
			return items[i].IsFolder
		}

		return items[i].Name < items[j].Name
	})

	headers := []string{"NAME", "SIZE", "MODIFIED"}
	rows := make([][]string, 0, len(items))

	for i := range items {
		name := items[i].Name
		if items[i].IsFolder {
			name += "/"
		}

		rows = append(rows, []string{name, formatSize(items[i].Size), formatTime(items[i].ModifiedAt)})
	}

	printTable(w, headers, rows)
}
