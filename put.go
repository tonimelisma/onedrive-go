package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

	fi, err := localpath.Stat(localPath)
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
		return printPutJSON(os.Stdout, putJSONOutput{
			Path: remotePath,
			ID:   result.Item.ID,
			Size: fi.Size(),
		})
	}

	cc.Statusf("Uploaded %s (%s)\n", remotePath, formatSize(fi.Size()))

	return nil
}

// printPutJSON writes the put command's single-file JSON output to w.
func printPutJSON(w io.Writer, out putJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode upload output: %w", err)
	}

	return nil
}

// printPutFolderJSON writes the put command's folder JSON output to w.
func printPutFolderJSON(w io.Writer, out putFolderJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode folder upload output: %w", err)
	}

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

	// Count total files for progress using the same partial-failure traversal
	// policy as the upload pass.
	walkErr := countUploadFiles(localPath, state)
	if walkErr != nil {
		return walkErr
	}

	walkErr = walkUploadTree(localPath, func(path string, d os.DirEntry) error {
		return uploadWalkEntry(ctx, cc, session, tm, state, localPath, remotePath, path, d)
	}, func(path string, err error) {
		appendUploadWalkError(state, path, err)
	})
	if walkErr != nil {
		return walkErr
	}

	if cc.Flags.JSON {
		return printPutFolderJSON(os.Stdout, state.result)
	}

	cc.Statusf("Uploaded %d files, %d folders (%s)\n",
		len(state.result.Files), state.result.FoldersCreated, formatSize(state.result.TotalSize))

	if len(state.result.Errors) > 0 {
		return fmt.Errorf("%d errors during upload", len(state.result.Errors))
	}

	return nil
}

func countUploadFiles(localRoot string, state *uploadWalkState) error {
	return walkUploadTree(localRoot, func(_ string, d os.DirEntry) error {
		if !d.IsDir() {
			state.total++
		}

		return nil
	}, func(path string, err error) {
		appendUploadWalkError(state, path, err)
	})
}

func walkUploadTree(root string, visit func(path string, d os.DirEntry) error, onError func(path string, err error)) error {
	entries, err := localpath.ReadDir(root)
	if err != nil {
		return fmt.Errorf("reading upload root %s: %w", root, err)
	}

	for i := range entries {
		entryPath := filepath.Join(root, entries[i].Name())
		if err := walkUploadTreeEntry(entryPath, entries[i], visit, onError); err != nil {
			return err
		}
	}

	return nil
}

func walkUploadTreeEntry(
	path string,
	d os.DirEntry,
	visit func(path string, d os.DirEntry) error,
	onError func(path string, err error),
) error {
	if err := visit(path, d); err != nil {
		return err
	}

	if !d.IsDir() {
		return nil
	}

	children, err := localpath.ReadDir(path)
	if err != nil {
		onError(path, err)
		return nil
	}

	for i := range children {
		childPath := filepath.Join(path, children[i].Name())
		if err := walkUploadTreeEntry(childPath, children[i], visit, onError); err != nil {
			return err
		}
	}

	return nil
}

func appendUploadWalkError(state *uploadWalkState, path string, err error) {
	state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", path, err))
}

func isFatalUploadWalkError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func uploadWalkEntry(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.Session,
	tm *driveops.TransferManager,
	state *uploadWalkState,
	localRoot, remotePath, path string,
	d os.DirEntry,
) error {
	parentDir := filepath.Dir(path)
	parentID, ok := state.dirIDs[parentDir]

	if !ok {
		appendUploadWalkError(state, path, errors.New("parent folder ID not found"))

		return nil
	}

	if d.IsDir() {
		folder, folderErr := session.EnsureFolder(ctx, parentID, d.Name())
		if folderErr != nil {
			if isFatalUploadWalkError(folderErr) {
				return fmt.Errorf("ensure folder %q: %w", d.Name(), folderErr)
			}

			appendUploadWalkError(state, path, folderErr)

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
		appendUploadWalkError(state, path, statErr)

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
		if isFatalUploadWalkError(uploadErr) {
			return fmt.Errorf("upload file %q: %w", path, uploadErr)
		}

		appendUploadWalkError(state, path, uploadErr)

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
