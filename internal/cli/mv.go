package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// destInfo holds the result of resolving a destination path.
type destInfo struct {
	parentID   string
	newName    string
	existingID string // non-empty when force=true and dest is an existing file
	destIsDir  bool   // true when dest resolved to an existing folder
}

// resolveDest resolves a destination path to a destInfo.
// If dest exists and is a folder, the item moves into it keeping sourceName.
// If dest doesn't exist, the parent must exist — the item moves there with the new name.
// If dest exists and is a file, returns an error unless force is true.
// When force is true and dest is an existing file, existingID is set to the
// file's ID so the caller can delete it before the operation.
func resolveDest(
	ctx context.Context,
	session *driveops.Session,
	destPath, sourceName string,
	force bool,
) (info destInfo, err error) {
	// Attempt 1: does the dest path already exist?
	item, resolveErr := session.ResolveItem(ctx, destPath)
	if resolveErr == nil {
		if item.IsFolder {
			return destInfo{parentID: item.ID, newName: sourceName, destIsDir: true}, nil
		}

		if force {
			if item.ParentID == "" {
				return destInfo{}, fmt.Errorf("destination %q has no parent reference; cannot overwrite", destPath)
			}

			return destInfo{parentID: item.ParentID, newName: item.Name, existingID: item.ID}, nil
		}

		return destInfo{}, fmt.Errorf("destination %q already exists (file); use --force to overwrite", destPath)
	}

	// Attempt 2: if not found, split into parent + name and resolve parent.
	if !errors.Is(resolveErr, graph.ErrNotFound) {
		return destInfo{}, fmt.Errorf("resolving destination %q: %w", destPath, resolveErr)
	}

	parentPath, destName := driveops.SplitParentAndName(destPath)

	parentItem, parentErr := session.ResolveItem(ctx, parentPath)
	if parentErr != nil {
		return destInfo{}, fmt.Errorf("resolving destination parent %q: %w", parentPath, parentErr)
	}

	if !parentItem.IsFolder {
		return destInfo{}, fmt.Errorf("destination parent %q is not a folder", parentPath)
	}

	return destInfo{parentID: parentItem.ID, newName: destName}, nil
}

// isSelfReference returns true when --force resolved to the source item itself.
func isSelfReference(sourceID string, dest destInfo) bool {
	return dest.existingID != "" && dest.existingID == sourceID
}

// isNoOpMove returns true when the resolved destination is the same as the source.
func isNoOpMove(dest destInfo, sourceParentID, sourceName string) bool {
	return dest.parentID == sourceParentID && dest.newName == sourceName
}

// emitMoveResult writes the move result as JSON or status text.
func emitMoveResult(cc *CLIContext, sourcePath, displayDest, itemID string) error {
	if cc.Flags.JSON {
		return printMvJSON(os.Stdout, mvJSONOutput{
			Source:      sourcePath,
			Destination: displayDest,
			ID:          itemID,
		})
	}

	cc.Statusf("Moved %s → %s\n", sourcePath, displayDest)

	return nil
}

// printMvJSON writes the mv command's JSON output to w.
func printMvJSON(w io.Writer, out mvJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode move output: %w", err)
	}

	return nil
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

	dest, err := resolveDest(ctx, session, destPath, sourceItem.Name, force)
	if err != nil {
		return err
	}

	// Check for no-op BEFORE any destructive action (delete or move).
	if isNoOpMove(dest, sourceItem.ParentID, sourceItem.Name) {
		logger.Debug("mv: no-op, source and dest are the same")

		return emitMoveResult(cc, sourcePath, destPath, sourceItem.ID)
	}

	// If --force resolved to an existing file, delete it before moving.
	// Skip when the existing file IS the source (self-reference via different paths).
	// NOTE: This is a TOCTOU race — another client could recreate the file
	// between delete and move. The Graph API has no atomic overwrite for moves.
	if dest.existingID != "" && !isSelfReference(sourceItem.ID, dest) {
		if delErr := session.DeleteItem(ctx, dest.existingID); delErr != nil {
			return fmt.Errorf("deleting existing %q: %w", destPath, delErr)
		}
	}

	// Determine what changed — MoveItem requires at least one of parentID or newName.
	moveParentID := dest.parentID
	moveName := dest.newName

	// If the parent didn't change, don't send it (avoids unnecessary API field).
	if moveParentID == sourceItem.ParentID {
		moveParentID = ""
	}

	// If the name didn't change, don't send it.
	if moveName == sourceItem.Name {
		moveName = ""
	}

	moved, err := session.MoveItem(ctx, sourceItem.ID, moveParentID, moveName)
	if err != nil {
		return fmt.Errorf("moving %q: %w", sourcePath, err)
	}

	// Build display destination from info we already have — no extra API call.
	displayDest := destPath
	if dest.destIsDir {
		clean := driveops.CleanRemotePath(destPath)
		if clean != "" {
			displayDest = clean + "/" + moved.Name
		}
	}

	return emitMoveResult(cc, sourcePath, displayDest, moved.ID)
}
