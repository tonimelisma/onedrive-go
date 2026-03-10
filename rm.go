package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
)

func newRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file or folder (moves to OneDrive recycle bin)",
		Long: `Delete a file or folder on OneDrive. Items are moved to the OneDrive
recycle bin by default and can be restored from the OneDrive web interface.

Folder deletion is recursive — all contents will be deleted.
Use --recursive (-r) to confirm intent when deleting folders.

Use --permanent for permanent deletion (Business/SharePoint accounts only;
Personal accounts always use the recycle bin).`,
		Args: cobra.ExactArgs(1),
		RunE: runRm,
	}

	cmd.Flags().BoolP("recursive", "r", false, "confirm recursive folder deletion")
	cmd.Flags().Bool("permanent", false, "permanently delete instead of moving to recycle bin (Business/SharePoint only)")

	return cmd
}

// rmJSONOutput is the JSON output schema for the rm command.
type rmJSONOutput struct {
	Deleted string `json:"deleted"`
}

func runRm(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("rm", "path", remotePath)

	item, err := session.ResolveItem(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	// Require --recursive for folder deletion (B-156).
	recursive, err := cmd.Flags().GetBool("recursive")
	if err != nil {
		return err
	}

	if item.IsFolder && !recursive {
		return fmt.Errorf("cannot delete folder %q without --recursive (-r) flag", remotePath)
	}

	permanent, err := cmd.Flags().GetBool("permanent")
	if err != nil {
		return err
	}

	if permanent {
		if err := session.PermanentDeleteItem(ctx, item.ID); err != nil {
			return fmt.Errorf("permanently deleting %q: %w", remotePath, err)
		}

		logger.Debug("permanent delete complete", "path", remotePath, "item_id", item.ID)
	} else {
		if err := session.DeleteItem(ctx, item.ID); err != nil {
			return fmt.Errorf("deleting %q: %w", remotePath, err)
		}

		logger.Debug("delete complete", "path", remotePath, "item_id", item.ID)
	}

	if cc.Flags.JSON {
		return printRmJSON(os.Stdout, rmJSONOutput{Deleted: remotePath})
	}

	if permanent {
		cc.Statusf("Permanently deleted %s\n", remotePath)
	} else {
		cc.Statusf("Deleted %s (moved to recycle bin)\n", remotePath)
	}

	return nil
}

// printRmJSON writes the rm command's JSON output to w.
func printRmJSON(w io.Writer, out rmJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}
