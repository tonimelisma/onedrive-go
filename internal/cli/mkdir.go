package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
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

	for _, seg := range segments {
		if seg == "" {
			continue
		}

		item, createErr := session.EnsureFolder(ctx, parentID, seg)
		if createErr != nil {
			return fmt.Errorf("creating folder %q: %w", seg, createErr)
		}

		parentID = item.ID
	}

	item, err := session.WaitPathVisible(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("confirming folder %q visibility: %w", remotePath, err)
	}

	logger.Debug("mkdir complete", "path", remotePath, "folder_id", parentID)

	if cc.Flags.JSON {
		return printMkdirJSON(cc.Output(), mkdirJSONOutput{Created: remotePath, ID: item.ID})
	}

	cc.Statusf("Created %s\n", remotePath)

	return nil
}

// printMkdirJSON writes the mkdir command's JSON output to w.
func printMkdirJSON(w io.Writer, out mkdirJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode mkdir output: %w", err)
	}

	return nil
}
