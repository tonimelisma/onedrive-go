package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newMvCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "mv <source> <dest>",
		Short: "Move or rename a file or folder",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runMv(cmd, args, force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing file at destination")

	return cmd
}

// mvJSONOutput is the JSON output schema for the mv command.
type mvJSONOutput struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ID          string `json:"id"`
}

// resolveDest resolves a destination path to (parentID, newName).
// If dest exists and is a folder, the item moves into it keeping sourceName.
// If dest doesn't exist, the parent must exist — the item moves there with the new name.
// If dest exists and is a file, returns an error unless force is true.
func resolveDest(
	ctx context.Context,
	session *driveops.Session,
	destPath, sourceName string,
	force bool,
) (parentID, newName string, err error) {
	// Attempt 1: does the dest path already exist?
	item, resolveErr := session.ResolveItem(ctx, destPath)
	if resolveErr == nil {
		if item.IsFolder {
			// Move into the folder, keep the source name.
			return item.ID, sourceName, nil
		}

		if force {
			return item.ParentID, item.Name, nil
		}

		return "", "", fmt.Errorf("destination %q already exists (file); use --force to overwrite", destPath)
	}

	// Attempt 2: if not found, split into parent + name and resolve parent.
	if !errors.Is(resolveErr, graph.ErrNotFound) {
		return "", "", fmt.Errorf("resolving destination %q: %w", destPath, resolveErr)
	}

	parentPath, destName := driveops.SplitParentAndName(destPath)

	parentItem, parentErr := session.ResolveItem(ctx, parentPath)
	if parentErr != nil {
		return "", "", fmt.Errorf("resolving destination parent %q: %w", parentPath, parentErr)
	}

	if !parentItem.IsFolder {
		return "", "", fmt.Errorf("destination parent %q is not a folder", parentPath)
	}

	return parentItem.ID, destName, nil
}

func runMv(cmd *cobra.Command, args []string, force bool) error {
	sourcePath := args[0]
	destPath := args[1]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("mv", "source", sourcePath, "dest", destPath)

	sourceItem, err := session.ResolveItem(ctx, sourcePath)
	if err != nil {
		return fmt.Errorf("resolving source %q: %w", sourcePath, err)
	}

	parentID, newName, err := resolveDest(ctx, session, destPath, sourceItem.Name, force)
	if err != nil {
		return err
	}

	// Determine what changed — MoveItem requires at least one of parentID or newName.
	moveParentID := parentID
	moveName := newName

	// If the parent didn't change, don't send it (avoids unnecessary API field).
	if moveParentID == sourceItem.ParentID {
		moveParentID = ""
	}

	// If the name didn't change, don't send it.
	if moveName == sourceItem.Name {
		moveName = ""
	}

	// If nothing changed, it's a no-op move to the same location.
	if moveParentID == "" && moveName == "" {
		moveParentID = parentID // force the move to go through
	}

	moved, err := session.MoveItem(ctx, sourceItem.ID, moveParentID, moveName)
	if err != nil {
		return fmt.Errorf("moving %q: %w", sourcePath, err)
	}

	// Build display destination.
	displayDest := destPath
	if driveops.CleanRemotePath(destPath) != "" {
		// If dest was a folder we moved into, show the full new path.
		destItem, resolveErr := session.ResolveItem(ctx, destPath)
		if resolveErr == nil && destItem.IsFolder {
			clean := driveops.CleanRemotePath(destPath)
			displayDest = clean + "/" + moved.Name
		}
	}

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(mvJSONOutput{
			Source:      sourcePath,
			Destination: displayDest,
			ID:          moved.ID,
		})
	}

	cc.Statusf("Moved %s → %s\n", sourcePath, displayDest)

	return nil
}
