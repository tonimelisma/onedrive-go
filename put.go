package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

func newPutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "put <local-path> [remote-path]",
		Short: "Upload a file",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runPut,
	}
}

// splitParentAndName splits a remote path into parent path and name.
// For "foo/bar/baz" returns ("foo/bar", "baz").
// For "baz" returns ("", "baz").
func splitParentAndName(path string) (string, string) {
	clean := driveops.CleanRemotePath(path)
	idx := strings.LastIndex(clean, "/")

	if idx < 0 {
		return "", clean
	}

	return clean[:idx], clean[idx+1:]
}

func runPut(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	ctx := cmd.Context()

	fi, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stating local file: %w", err)
	}

	if fi.IsDir() {
		return fmt.Errorf("%q is a directory, not a file", localPath)
	}

	// Default remote path is root + local filename.
	remotePath := "/" + filepath.Base(localPath)
	if len(args) > 1 {
		remotePath = args[1]
	}

	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("put", "local_path", localPath, "remote_path", remotePath, "size", fi.Size())

	parentPath, name := splitParentAndName(remotePath)

	// Resolve parent folder ID.
	parentItem, err := session.ResolveItem(ctx, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent %q: %w", parentPath, err)
	}

	progress := func(uploaded, total int64) {
		cc.Statusf("Uploading: %s / %s\n", formatSize(uploaded), formatSize(total))
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	result, err := tm.UploadFile(ctx, session.DriveID, parentItem.ID, name, localPath, driveops.UploadOpts{
		Mtime:    fi.ModTime(),
		Progress: progress,
	})
	if err != nil {
		return fmt.Errorf("uploading %q: %w", remotePath, err)
	}

	logger.Debug("upload complete", "remote_path", remotePath, "item_id", result.Item.ID, "size", fi.Size())
	cc.Statusf("Uploaded %s (%s)\n", remotePath, formatSize(fi.Size()))

	return nil
}
