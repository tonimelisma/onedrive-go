package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newMkdirCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mkdir <path>",
		Short: "Create a folder (recursive)",
		Args:  cobra.ExactArgs(1),
		RunE:  runMkdir,
	}
}

// mkdirJSONOutput is the JSON output schema for the mkdir command.
type mkdirJSONOutput struct {
	Created string `json:"created"`
	ID      string `json:"id"`
}

func runMkdir(cmd *cobra.Command, args []string) error {
	remotePath := driveops.CleanRemotePath(args[0])
	if remotePath == "" {
		return fmt.Errorf("cannot create root folder")
	}

	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("mkdir", "path", remotePath)

	// Walk path segments, creating each missing folder.
	segments := strings.Split(remotePath, "/")
	parentID := "root"
	builtPath := ""

	for _, seg := range segments {
		if seg == "" {
			continue
		}

		if builtPath == "" {
			builtPath = seg
		} else {
			builtPath = builtPath + "/" + seg
		}

		item, createErr := session.CreateFolder(ctx, parentID, seg)
		if createErr != nil {
			// If folder already exists (409 Conflict), resolve it and continue.
			if errors.Is(createErr, graph.ErrConflict) {
				existing, resolveErr := session.ResolveItem(ctx, builtPath)
				if resolveErr != nil {
					return fmt.Errorf("resolving existing folder %q: %w", seg, resolveErr)
				}

				parentID = existing.ID

				continue
			}

			return fmt.Errorf("creating folder %q: %w", seg, createErr)
		}

		parentID = item.ID
	}

	logger.Debug("mkdir complete", "path", remotePath, "folder_id", parentID)

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(mkdirJSONOutput{Created: remotePath, ID: parentID})
	}

	cc.Statusf("Created %s\n", remotePath)

	return nil
}
