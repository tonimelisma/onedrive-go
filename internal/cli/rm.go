package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newRmCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rm <path>",
		Short: "Delete a file or folder (moves to OneDrive recycle bin)",
		Long: `Delete a file or folder on OneDrive. Items are moved to the OneDrive
recycle bin by default and can be restored from the OneDrive web interface.

Folder deletion is recursive — all contents will be deleted.
Use --recursive (-r) to confirm intent when deleting folders.

Use --permanent for permanent deletion (bypasses the recycle bin).`,
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

type rmDeleteMode int

const (
	rmDeleteRecycleBin rmDeleteMode = iota + 1
	rmDeletePermanent
)

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

	item, err := session.ResolveDeleteTarget(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	// Require --recursive for folder deletion (B-156).
	recursive, err := cmd.Flags().GetBool("recursive")
	if err != nil {
		return fmt.Errorf("read --recursive flag: %w", err)
	}

	if item.IsFolder && !recursive {
		return fmt.Errorf("cannot delete folder %q without --recursive (-r) flag", remotePath)
	}

	deleteMode, err := resolveRmDeleteMode(cmd)
	if err != nil {
		return err
	}

	if err := executeRmDelete(ctx, session, item.ID, remotePath, deleteMode, logger); err != nil {
		return err
	}

	if err := confirmRmParentVisibility(ctx, session, remotePath, cc.Status()); err != nil {
		return err
	}

	if cc.Flags.JSON {
		return printRmJSON(cc.Output(), rmJSONOutput{Deleted: remotePath})
	}

	writeRmStatus(cc, remotePath, deleteMode)

	return nil
}

type rmParentVisibilitySession interface {
	WaitPathVisible(context.Context, string) (*graph.Item, error)
}

func confirmRmParentVisibility(
	ctx context.Context,
	session rmParentVisibilitySession,
	remotePath string,
	statusWriter io.Writer,
) error {
	parentPath := removableParentPath(remotePath)
	if parentPath == "" {
		return nil
	}

	if _, err := session.WaitPathVisible(ctx, parentPath); err != nil {
		var visibilityErr *driveops.PathNotVisibleError
		if errors.As(err, &visibilityErr) {
			writeWarningf(
				statusWriter,
				"warning: deleted %s, but parent /%s is still settling in Graph; follow-up reads may lag\n",
				remotePath,
				parentPath,
			)

			return nil
		}

		return fmt.Errorf("confirming parent %q visibility after delete: %w", "/"+parentPath, err)
	}

	return nil
}

func resolveRmDeleteMode(
	cmd *cobra.Command,
) (rmDeleteMode, error) {
	permanent, err := cmd.Flags().GetBool("permanent")
	if err != nil {
		return 0, fmt.Errorf("read --permanent flag: %w", err)
	}

	if !permanent {
		return rmDeleteRecycleBin, nil
	}

	return rmDeletePermanent, nil
}

func executeRmDelete(
	ctx context.Context,
	session interface {
		DeleteResolvedPath(context.Context, string, string) error
		PermanentDeleteResolvedPath(context.Context, string, string) error
	},
	itemID, remotePath string,
	deleteMode rmDeleteMode,
	logger *slog.Logger,
) error {
	switch deleteMode {
	case rmDeletePermanent:
		if err := session.PermanentDeleteResolvedPath(ctx, remotePath, itemID); err != nil {
			return fmt.Errorf("permanently deleting %q: %w", remotePath, err)
		}

		logger.Debug("permanent delete complete", "path", remotePath, "item_id", itemID)
	case rmDeleteRecycleBin:
		if err := session.DeleteResolvedPath(ctx, remotePath, itemID); err != nil {
			return fmt.Errorf("deleting %q: %w", remotePath, err)
		}

		logger.Debug("delete complete", "path", remotePath, "item_id", itemID)
	}

	return nil
}

func writeRmStatus(cc *CLIContext, remotePath string, deleteMode rmDeleteMode) {
	switch deleteMode {
	case rmDeletePermanent:
		cc.Statusf("Permanently deleted %s\n", remotePath)
	case rmDeleteRecycleBin:
		cc.Statusf("Deleted %s (moved to recycle bin)\n", remotePath)
	}
}

func removableParentPath(remotePath string) string {
	clean := driveops.CleanRemotePath(remotePath)
	if clean == "" {
		return ""
	}

	parent := path.Dir(clean)
	if parent == "." || parent == "/" || parent == "" {
		return ""
	}

	return parent
}

// printRmJSON writes the rm command's JSON output to w.
func printRmJSON(w io.Writer, out rmJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode remove output: %w", err)
	}

	return nil
}
