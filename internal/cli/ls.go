package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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
		// Like ls(1), ls on a file lists that one file. OneDrive refuses to
		// list a file's children (HTTP 422), so look the path up once and,
		// if it is a file, print it; anything else keeps the listing error.
		file, ok := lsFileFallback(ctx, session, remotePath, err)
		if !ok {
			return newRemoteOpError("listing", remotePath, err)
		}

		items = []graph.Item{*file}
	}

	if cc.Flags.JSON {
		return printItemsJSON(cc.Output(), items)
	}

	return printItemsTable(cc.Output(), items)
}

// lsFileFallback returns the item at remotePath when listing it failed
// because it is a file. Only OneDrive's "not a folder" refusal (HTTP 400 or
// 422) triggers the lookup, so a network, sign-in or not-found failure is
// reported as it was, without a second request.
func lsFileFallback(
	ctx context.Context,
	session *driveops.MountSession,
	remotePath string,
	listErr error,
) (*graph.Item, bool) {
	var graphErr *graph.GraphError
	if !errors.As(listErr, &graphErr) ||
		(graphErr.StatusCode != http.StatusUnprocessableEntity && graphErr.StatusCode != http.StatusBadRequest) {
		return nil, false
	}

	item, err := session.ResolveItem(ctx, remotePath)
	if err != nil || item.IsFolder {
		return nil, false
	}

	return item, true
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
			ModifiedAt: formatAPITime(items[i].ModifiedAt),
			ID:         items[i].ID,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode ls output: %w", err)
	}

	return nil
}

func printItemsTable(w io.Writer, items []graph.Item) error {
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

	return printTable(w, headers, rows)
}
