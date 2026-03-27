package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newStatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stat <path>",
		Short: "Display file or folder metadata",
		Args:  cobra.ExactArgs(1),
		RunE:  runStat,
	}
}

func runStat(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	cc.Logger.Debug("stat", "path", remotePath)

	item, err := session.ResolveItem(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	if cc.Flags.JSON {
		return printStatJSON(os.Stdout, item)
	}

	return printStatText(os.Stdout, item)
}

// statJSONOutput is the JSON output schema for the stat command.
type statJSONOutput struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	IsFolder   bool   `json:"is_folder"`
	ModifiedAt string `json:"modified_at"`
	CreatedAt  string `json:"created_at"`
	MimeType   string `json:"mime_type,omitempty"`
	ETag       string `json:"etag"`
}

func printStatJSON(w io.Writer, item *graph.Item) error {
	out := statJSONOutput{
		ID:         item.ID,
		Name:       item.Name,
		Size:       item.Size,
		IsFolder:   item.IsFolder,
		ModifiedAt: item.ModifiedAt.Format("2006-01-02T15:04:05Z"),
		CreatedAt:  item.CreatedAt.Format("2006-01-02T15:04:05Z"),
		MimeType:   item.MimeType,
		ETag:       item.ETag,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode stat output: %w", err)
	}

	return nil
}

func printStatText(w io.Writer, item *graph.Item) error {
	itemType := "file"
	if item.IsFolder {
		itemType = "folder"
	}

	if err := writef(w, "Name:     %s\n", item.Name); err != nil {
		return err
	}
	if err := writef(w, "Type:     %s\n", itemType); err != nil {
		return err
	}
	if err := writef(w, "Size:     %s (%d bytes)\n", formatSize(item.Size), item.Size); err != nil {
		return err
	}
	if err := writef(w, "Modified: %s\n", item.ModifiedAt.Format("2006-01-02 15:04:05 UTC")); err != nil {
		return err
	}
	if err := writef(w, "Created:  %s\n", item.CreatedAt.Format("2006-01-02 15:04:05 UTC")); err != nil {
		return err
	}
	if err := writef(w, "ID:       %s\n", item.ID); err != nil {
		return err
	}

	if item.MimeType != "" {
		if err := writef(w, "MIME:     %s\n", item.MimeType); err != nil {
			return err
		}
	}

	return nil
}
