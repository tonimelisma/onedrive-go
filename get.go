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
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// defaultDirPerm is the permission mode for directories created during recursive get.
const defaultDirPerm = 0o755

func newGetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get <remote-path> [local-path]",
		Short: "Download a file or folder",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runGet,
	}
}

// getJSONOutput is the JSON output schema for downloading a single file.
type getJSONOutput struct {
	Path         string `json:"path"`
	Size         int64  `json:"size"`
	HashVerified bool   `json:"hash_verified"`
}

// getFolderJSONOutput is the JSON output schema for downloading a folder.
type getFolderJSONOutput struct {
	Files          []getJSONOutput `json:"files"`
	FoldersCreated int             `json:"folders_created"`
	TotalSize      int64           `json:"total_size"`
	Errors         []string        `json:"errors"`
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
		localPath := filepath.Base(driveops.CleanRemotePath(remotePath))
		if item.IsRoot {
			localPath = item.Name
		}

		if len(args) > 1 {
			localPath = args[1]
		}

		return downloadFolder(cmd, cc, session, item, remotePath, localPath)
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

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(getJSONOutput{
			Path:         localPath,
			Size:         result.Size,
			HashVerified: result.HashVerified,
		})
	}

	cc.Statusf("Downloaded %s (%s)\n", localPath, formatSize(result.Size))

	return nil
}

func downloadFolder(
	cmd *cobra.Command,
	cc *CLIContext,
	session *driveops.Session,
	_ *graph.Item,
	remotePath, localPath string,
) error {
	ctx := cmd.Context()
	logger := cc.Logger

	if err := os.MkdirAll(localPath, defaultDirPerm); err != nil {
		return fmt.Errorf("creating local directory %q: %w", localPath, err)
	}

	children, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("listing %q: %w", remotePath, err)
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	var result getFolderJSONOutput
	result.FoldersCreated = 1 // count the root folder

	total := 0
	for i := range children {
		if !children[i].IsFolder {
			total++
		}
	}

	done := 0

	for i := range children {
		childLocal := filepath.Join(localPath, children[i].Name)

		if children[i].IsFolder {
			childRemote := remotePath
			if driveops.CleanRemotePath(childRemote) == "" {
				childRemote = children[i].Name
			} else {
				childRemote = driveops.CleanRemotePath(childRemote) + "/" + children[i].Name
			}

			subResult, subErr := downloadFolderRecursive(ctx, session, tm, childRemote, childLocal)
			if subErr != nil {
				result.Errors = append(result.Errors, subErr.Error())

				continue
			}

			result.Files = append(result.Files, subResult.Files...)
			result.FoldersCreated += subResult.FoldersCreated
			result.TotalSize += subResult.TotalSize
			result.Errors = append(result.Errors, subResult.Errors...)

			continue
		}

		dlResult, dlErr := tm.DownloadToFile(ctx, session.DriveID, children[i].ID, childLocal, driveops.DownloadOpts{
			RemoteHash: children[i].QuickXorHash,
		})
		if dlErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", children[i].Name, dlErr))

			continue
		}

		result.Files = append(result.Files, getJSONOutput{
			Path:         childLocal,
			Size:         dlResult.Size,
			HashVerified: dlResult.HashVerified,
		})
		result.TotalSize += dlResult.Size
		done++
		cc.Statusf("Downloaded %d/%d files\n", done, total)
	}

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(result)
	}

	cc.Statusf("Downloaded %d files, %d folders (%s)\n",
		len(result.Files), result.FoldersCreated, formatSize(result.TotalSize))

	if len(result.Errors) > 0 {
		return fmt.Errorf("%d errors during download", len(result.Errors))
	}

	return nil
}

func downloadFolderRecursive(
	ctx context.Context,
	session *driveops.Session,
	tm *driveops.TransferManager,
	remotePath, localPath string,
) (*getFolderJSONOutput, error) {
	if err := os.MkdirAll(localPath, defaultDirPerm); err != nil {
		return nil, fmt.Errorf("creating directory %q: %w", localPath, err)
	}

	children, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		return nil, fmt.Errorf("listing %q: %w", remotePath, err)
	}

	result := &getFolderJSONOutput{FoldersCreated: 1}

	for i := range children {
		childLocal := filepath.Join(localPath, children[i].Name)

		if children[i].IsFolder {
			childRemote := driveops.CleanRemotePath(remotePath) + "/" + children[i].Name
			subResult, subErr := downloadFolderRecursive(ctx, session, tm, childRemote, childLocal)
			if subErr != nil {
				result.Errors = append(result.Errors, subErr.Error())

				continue
			}

			result.Files = append(result.Files, subResult.Files...)
			result.FoldersCreated += subResult.FoldersCreated
			result.TotalSize += subResult.TotalSize
			result.Errors = append(result.Errors, subResult.Errors...)

			continue
		}

		dlResult, dlErr := tm.DownloadToFile(ctx, session.DriveID, children[i].ID, childLocal, driveops.DownloadOpts{
			RemoteHash: children[i].QuickXorHash,
		})
		if dlErr != nil {
			result.Errors = append(result.Errors, fmt.Sprintf("%s: %v", children[i].Name, dlErr))

			continue
		}

		result.Files = append(result.Files, getJSONOutput{
			Path:         childLocal,
			Size:         dlResult.Size,
			HashVerified: dlResult.HashVerified,
		})
		result.TotalSize += dlResult.Size
	}

	return result, nil
}
