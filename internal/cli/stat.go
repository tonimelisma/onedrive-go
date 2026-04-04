package cli

import (
	"encoding/json"
	"fmt"
	"io"

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
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	if cc.SharedTarget != nil {
		item, _, err := cc.resolveSharedItem(ctx)
		if err != nil {
			return err
		}

		opts := statPrintOptions{
			SharedSelector: cc.SharedTarget.Selector(),
			AccountEmail:   cc.SharedTarget.Ref.AccountEmail,
			RemoteDriveID:  cc.SharedTarget.Ref.RemoteDriveID,
			RemoteItemID:   cc.SharedTarget.Ref.RemoteItemID,
		}

		if cc.Flags.JSON {
			return printStatJSONWithOptions(cc.Output(), item, opts)
		}

		return printStatTextWithOptions(cc.Output(), item, opts)
	}

	remotePath := args[0]

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
		return printStatJSONWithOptions(cc.Output(), item, statPrintOptions{})
	}

	return printStatTextWithOptions(cc.Output(), item, statPrintOptions{})
}

// statJSONOutput is the JSON output schema for the stat command.
type statJSONOutput struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	IsFolder       bool   `json:"is_folder"`
	ModifiedAt     string `json:"modified_at"`
	CreatedAt      string `json:"created_at"`
	MimeType       string `json:"mime_type,omitempty"`
	ETag           string `json:"etag"`
	AccountEmail   string `json:"account_email,omitempty"`
	RemoteDriveID  string `json:"remote_drive_id,omitempty"`
	RemoteItemID   string `json:"remote_item_id,omitempty"`
	SharedSelector string `json:"shared_selector,omitempty"`
}

func printStatJSON(w io.Writer, item *graph.Item) error {
	return printStatJSONWithOptions(w, item, statPrintOptions{})
}

type statPrintOptions struct {
	SharedSelector string
	AccountEmail   string
	RemoteDriveID  string
	RemoteItemID   string
}

func printStatJSONWithOptions(w io.Writer, item *graph.Item, opts statPrintOptions) error {
	out := statJSONOutput{
		ID:             item.ID,
		Name:           item.Name,
		Size:           item.Size,
		IsFolder:       item.IsFolder,
		ModifiedAt:     formatAPITime(item.ModifiedAt),
		CreatedAt:      formatAPITime(item.CreatedAt),
		MimeType:       item.MimeType,
		ETag:           item.ETag,
		AccountEmail:   opts.AccountEmail,
		RemoteDriveID:  opts.RemoteDriveID,
		RemoteItemID:   opts.RemoteItemID,
		SharedSelector: opts.SharedSelector,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode stat output: %w", err)
	}

	return nil
}

func printStatText(w io.Writer, item *graph.Item) error {
	return printStatTextWithOptions(w, item, statPrintOptions{})
}

func printStatTextWithOptions(w io.Writer, item *graph.Item, opts statPrintOptions) error {
	itemType := typeFile
	if item.IsFolder {
		itemType = typeFolder
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
	if err := writef(w, "Modified: %s\n", formatExactTime(item.ModifiedAt)); err != nil {
		return err
	}
	if err := writef(w, "Created:  %s\n", formatExactTime(item.CreatedAt)); err != nil {
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

	if opts.SharedSelector != "" {
		if err := writef(w, "Shared:   %s\n", opts.SharedSelector); err != nil {
			return err
		}
	}

	return nil
}
