package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

func newPutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "put <local-path> [remote-path]",
		Short: "Upload a file or directory",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runPut,
	}
}

// putJSONOutput is the JSON output schema for uploading a single file.
type putJSONOutput struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// putFolderJSONOutput is the JSON output schema for uploading a directory.
type putFolderJSONOutput struct {
	Files          []putJSONOutput `json:"files"`
	FoldersCreated int             `json:"folders_created"`
	TotalSize      int64           `json:"total_size"`
	Errors         []string        `json:"errors"`
}

func runPut(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	ctx := cmd.Context()

	fi, err := os.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stating local path: %w", err)
	}

	cc := mustCLIContext(ctx)

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger

	if fi.IsDir() {
		remotePath := "/" + filepath.Base(localPath)
		if len(args) > 1 {
			remotePath = args[1]
		}

		return uploadFolder(cmd, cc, session, localPath, remotePath)
	}

	// Default remote path is root + local filename.
	remotePath := "/" + filepath.Base(localPath)
	if len(args) > 1 {
		remotePath = args[1]
	}

	logger.Debug("put", "local_path", localPath, "remote_path", remotePath, "size", fi.Size())

	parentPath, name := driveops.SplitParentAndName(remotePath)

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

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(putJSONOutput{
			Path: remotePath,
			ID:   result.Item.ID,
			Size: fi.Size(),
		})
	}

	cc.Statusf("Uploaded %s (%s)\n", remotePath, formatSize(fi.Size()))

	return nil
}

// uploadWalkState holds mutable state for the upload walk callback.
type uploadWalkState struct {
	result putFolderJSONOutput
	dirIDs map[string]string
	done   int
	total  int
}

func uploadFolder(
	cmd *cobra.Command,
	cc *CLIContext,
	session *driveops.Session,
	localPath, remotePath string,
) error {
	ctx := cmd.Context()
	logger := cc.Logger

	logger.Debug("put folder", "local_path", localPath, "remote_path", remotePath)

	// Resolve or create the root remote folder.
	parentPath, name := driveops.SplitParentAndName(remotePath)

	parentItem, err := session.ResolveItem(ctx, parentPath)
	if err != nil {
		return fmt.Errorf("resolving parent %q: %w", parentPath, err)
	}

	rootFolder, err := session.EnsureFolder(ctx, parentItem.ID, name)
	if err != nil {
		return fmt.Errorf("creating remote folder %q: %w", remotePath, err)
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	state := &uploadWalkState{
		dirIDs: map[string]string{localPath: rootFolder.ID},
	}
	state.result.FoldersCreated = 1

	// Count total files for progress.
	walkErr := filepath.WalkDir(localPath, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // skip inaccessible entries
		}

		if !d.IsDir() {
			state.total++
		}

		return nil
	})
	if walkErr != nil {
		return walkErr
	}

	walkErr = filepath.WalkDir(localPath, func(path string, d os.DirEntry, err error) error {
		return uploadWalkEntry(ctx, cc, session, tm, state, localPath, remotePath, path, d, err)
	})
	if walkErr != nil {
		return walkErr
	}

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(state.result)
	}

	cc.Statusf("Uploaded %d files, %d folders (%s)\n",
		len(state.result.Files), state.result.FoldersCreated, formatSize(state.result.TotalSize))

	if len(state.result.Errors) > 0 {
		return fmt.Errorf("%d errors during upload", len(state.result.Errors))
	}

	return nil
}

func uploadWalkEntry(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.Session,
	tm *driveops.TransferManager,
	state *uploadWalkState,
	localRoot, remotePath, path string,
	d os.DirEntry,
	walkErr error,
) error {
	if walkErr != nil {
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", path, walkErr))

		return nil
	}

	if path == localRoot {
		return nil // skip root, already created
	}

	parentDir := filepath.Dir(path)
	parentID, ok := state.dirIDs[parentDir]

	if !ok {
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: parent folder ID not found", path))

		return nil
	}

	if d.IsDir() {
		folder, folderErr := session.EnsureFolder(ctx, parentID, d.Name())
		if folderErr != nil {
			state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", path, folderErr))

			return nil
		}

		state.dirIDs[path] = folder.ID
		state.result.FoldersCreated++

		return nil
	}

	return uploadFileEntry(ctx, cc, session, tm, state, localRoot, remotePath, path, d, parentID)
}

func uploadFileEntry(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.Session,
	tm *driveops.TransferManager,
	state *uploadWalkState,
	localRoot, remotePath, path string,
	d os.DirEntry,
	parentID string,
) error {
	fi, statErr := d.Info()
	if statErr != nil {
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", path, statErr))

		return nil
	}

	progress := func(uploaded, totalBytes int64) {
		cc.Statusf("Uploading %s: %s / %s\n", d.Name(), formatSize(uploaded), formatSize(totalBytes))
	}

	uploadResult, uploadErr := tm.UploadFile(ctx, session.DriveID, parentID, d.Name(), path, driveops.UploadOpts{
		Mtime:    fi.ModTime(),
		Progress: progress,
	})
	if uploadErr != nil {
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", path, uploadErr))

		return nil
	}

	rel, relErr := filepath.Rel(localRoot, path)
	if relErr != nil {
		rel = d.Name()
	}

	remoteFilePath := driveops.CleanRemotePath(remotePath) + "/" + filepath.ToSlash(rel)

	state.result.Files = append(state.result.Files, putJSONOutput{
		Path: remoteFilePath,
		ID:   uploadResult.Item.ID,
		Size: fi.Size(),
	})
	state.result.TotalSize += fi.Size()
	state.done++
	cc.Statusf("Uploaded %d/%d files\n", state.done, state.total)

	return nil
}
