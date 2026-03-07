package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newCpCmd() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "cp <source> <dest>",
		Short: "Copy a file or folder (server-side)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCp(cmd, args, force)
		},
	}

	cmd.Flags().BoolVarP(&force, "force", "f", false, "overwrite existing file at destination")

	return cmd
}

// cpJSONOutput is the JSON output schema for the cp command.
type cpJSONOutput struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
	ID          string `json:"id"`
}

// copyPollInterval is the polling interval for async copy status.
const copyPollInterval = 1 * time.Second

// copyTimeout is the maximum time to wait for an async copy to complete.
const copyTimeout = 5 * time.Minute

// checkSelfCopy returns an error if the copy would overwrite the source item.
func checkSelfCopy(sourceID string, dest destInfo) error {
	if isSelfReference(sourceID, dest) {
		return fmt.Errorf("cannot copy a file over itself")
	}

	return nil
}

func runCp(cmd *cobra.Command, args []string, force bool) error {
	sourcePath := args[0]
	destPath := args[1]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	cc.Logger.Debug("cp", "source", sourcePath, "dest", destPath)

	sourceItem, err := session.ResolveItem(ctx, sourcePath)
	if err != nil {
		return fmt.Errorf("resolving source %q: %w", sourcePath, err)
	}

	dest, err := resolveDest(ctx, session, destPath, sourceItem.Name, force)
	if err != nil {
		return err
	}

	// Bail early if copying a file over itself — prevents both data loss
	// (delete then fail) and silent duplicates (API auto-rename).
	if selfErr := checkSelfCopy(sourceItem.ID, dest); selfErr != nil {
		return selfErr
	}

	// If --force resolved to an existing file, delete it before copying.
	// NOTE: This is a TOCTOU race — another client could recreate the file
	// between delete and copy. Server-side copy has no atomic overwrite.
	if dest.existingID != "" {
		if delErr := session.DeleteItem(ctx, dest.existingID); delErr != nil {
			return fmt.Errorf("deleting existing %q: %w", destPath, delErr)
		}
	}

	copyResult, err := session.CopyItem(ctx, sourceItem.ID, dest.parentID, dest.newName)
	if err != nil {
		return fmt.Errorf("copying %q: %w", sourcePath, err)
	}

	resourceID, err := awaitCopy(ctx, cc, session.Meta, copyResult.MonitorURL)
	if err != nil {
		return err
	}

	displayDest := destPath
	if dest.destIsDir {
		clean := driveops.CleanRemotePath(destPath)
		if clean != "" {
			displayDest = clean + "/" + dest.newName
		}
	}

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(cpJSONOutput{
			Source:      sourcePath,
			Destination: displayDest,
			ID:          resourceID,
		})
	}

	cc.Statusf("Copied %s → %s\n", sourcePath, displayDest)

	return nil
}

// awaitCopy polls the monitor URL until the copy completes, fails, or times out.
func awaitCopy(ctx context.Context, cc *CLIContext, meta *graph.Client, monitorURL string) (string, error) {
	deadline := time.Now().Add(copyTimeout)

	for time.Now().Before(deadline) {
		status, pollErr := meta.PollCopyStatus(ctx, monitorURL)
		if pollErr != nil {
			return "", fmt.Errorf("polling copy status: %w", pollErr)
		}

		if status.Status == "completed" {
			return status.ResourceID, nil
		}

		if status.Status == "failed" || status.Status == "canceled" {
			return "", fmt.Errorf("copy %s", status.Status)
		}

		cc.Statusf("Copying: %.0f%%\n", status.PercentageComplete)

		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(copyPollInterval):
		}
	}

	return "", fmt.Errorf("copy timed out after %v", copyTimeout)
}
