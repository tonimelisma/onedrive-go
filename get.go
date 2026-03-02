package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <remote-path> [local-path]",
		Short: "Download a file",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runGet,
	}
}

func runGet(cmd *cobra.Command, args []string) error {
	remotePath := args[0]
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("get", "remote_path", remotePath)

	item, err := session.ResolveItem(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("resolving %q: %w", remotePath, err)
	}

	if item.IsFolder {
		return fmt.Errorf("%q is a folder, not a file", remotePath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = args[1]
	}

	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, nil, logger)

	result, err := tm.DownloadToFile(ctx, session.DriveID, item.ID, localPath, driveops.DownloadOpts{
		RemoteHash: item.QuickXorHash,
	})
	if err != nil {
		partialPath := localPath + ".partial"
		if _, statErr := os.Stat(partialPath); statErr == nil {
			cc.Statusf("Partial download saved: %s\n", partialPath)
			cc.Statusf("Re-run the same command to resume.\n")
		}

		return err
	}

	logger.Debug("download complete", "local_path", localPath, "bytes", result.Size)
	cc.Statusf("Downloaded %s (%s)\n", localPath, formatSize(result.Size))

	return nil
}
