package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
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
// file's ID so the caller can set it aside for the operation
// (replaceDestination).
func resolveDest(
	ctx context.Context,
	session *driveops.MountSession,
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
		return destInfo{}, newRemoteOpError("resolving destination", destPath, resolveErr)
	}

	parentPath, destName := driveops.SplitParentAndName(destPath)

	parentItem, parentErr := session.ResolveItem(ctx, parentPath)
	if parentErr != nil {
		return destInfo{}, newRemoteOpError("resolving destination parent", parentPath, parentErr)
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
		return printMvJSON(cc.Output(), mvJSONOutput{
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
		return newRemoteOpError("resolving source", sourcePath, err)
	}

	dest, err := resolveDest(ctx, session, destPath, sourceItem.Name, force)
	if err != nil {
		return err
	}

	// A folder cannot move into itself or its own subtree. Graph rejects it
	// with a bare 400, and with --force the replacement below would first set aside an
	// existing destination inside the source, so refuse before anything runs.
	if sourceItem.IsFolder && isWithinRemotePath(moveTargetPath(destPath, dest), sourcePath) {
		return fmt.Errorf("cannot move %s into itself", sourcePath)
	}

	// Check for no-op BEFORE any destructive action (delete or move).
	if isNoOpMove(dest, sourceItem.ParentID, sourceItem.Name) {
		logger.Debug("mv: no-op, source and dest are the same")

		return emitMoveResult(cc, sourcePath, destPath, sourceItem.ID)
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

	// With --force, an existing destination is set aside, not deleted, until
	// the move has landed (D-082). When the existing file is the source itself
	// (two paths to one item) there is nothing to set aside.
	replaced := dest
	if isSelfReference(sourceItem.ID, dest) {
		replaced.existingID = ""
	}

	var moved *graph.Item
	if force {
		hintAboutLeftoverAside(ctx, cc, session, destPath, dest)
	}

	err = replaceDestination(ctx, cc, session, destPath, replaced, time.Now(), func() error {
		var moveErr error
		moved, moveErr = session.MoveItem(ctx, sourceItem.ID, moveParentID, moveName)
		if moveErr != nil {
			return newRemoteOpError("moving", sourcePath, moveErr)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Build display destination from info we already have — no extra API call.
	displayDest := destPath
	if dest.destIsDir {
		displayDest = joinDisplayPath(destPath, moved.Name)
	}

	if _, err := session.WaitPathVisible(ctx, displayDest); err != nil {
		return fmt.Errorf("confirming move to %q visibility: %w", displayDest, err)
	}

	return emitMoveResult(cc, sourcePath, displayDest, moved.ID)
}

// moveTargetPath is the path the moved item ends up at.
func moveTargetPath(destPath string, dest destInfo) string {
	if dest.destIsDir {
		return joinDisplayPath(destPath, dest.newName)
	}

	return "/" + driveops.CleanRemotePath(destPath)
}

// isWithinRemotePath reports whether target is root or inside it, comparing
// segments under OneDrive's name equivalence (namekey).
func isWithinRemotePath(target, root string) bool {
	r := driveops.CleanRemotePath(root)
	if r == "" {
		return true
	}

	targetSegments := strings.Split(driveops.CleanRemotePath(target), "/")
	rootSegments := strings.Split(r, "/")
	if len(targetSegments) < len(rootSegments) {
		return false
	}

	return namekey.EqualPath(strings.Join(targetSegments[:len(rootSegments)], "/"), r)
}

// joinDisplayPath names child inside the remote folder dir with the leading
// slash every other command prints ("/docs/a.txt", never "docs/a.txt").
func joinDisplayPath(dir, child string) string {
	clean := driveops.CleanRemotePath(dir)
	if clean == "" {
		return "/" + child
	}

	return "/" + clean + "/" + child
}
