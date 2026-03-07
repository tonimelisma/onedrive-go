package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// defaultDirPerm is the permission mode for directories created during recursive get.
const defaultDirPerm = 0o755

// defaultDownloadConcurrency is the number of concurrent file downloads per directory.
const defaultDownloadConcurrency = 4

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

// downloadState holds mutable state shared across the recursive download.
type downloadState struct {
	mu     sync.Mutex
	result getFolderJSONOutput
	done   int
	total  int
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

	state := &downloadState{}

	// Pass 1: count all files recursively.
	if err := countRemoteFiles(ctx, session, remotePath, state); err != nil {
		return err
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	// Pass 2: download recursively.
	downloadRecursive(ctx, cc, session, tm, state, remotePath, localPath)

	if cc.Flags.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		return enc.Encode(state.result)
	}

	cc.Statusf("Downloaded %d files, %d folders (%s)\n",
		len(state.result.Files), state.result.FoldersCreated, formatSize(state.result.TotalSize))

	if len(state.result.Errors) > 0 {
		return fmt.Errorf("%d errors during download", len(state.result.Errors))
	}

	return nil
}

func countRemoteFiles(ctx context.Context, session *driveops.Session, remotePath string, state *downloadState) error {
	children, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("listing %q: %w", remotePath, err)
	}

	for i := range children {
		if children[i].IsFolder {
			if err := countRemoteFiles(ctx, session, joinRemotePath(remotePath, children[i].Name), state); err != nil {
				return err
			}
		} else {
			state.total++
		}
	}

	return nil
}

func downloadRecursive(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.Session,
	tm *driveops.TransferManager,
	state *downloadState,
	remotePath, localPath string,
) {
	if err := os.MkdirAll(localPath, defaultDirPerm); err != nil {
		state.mu.Lock()
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("creating %q: %v", localPath, err))
		state.mu.Unlock()

		return
	}

	children, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		state.mu.Lock()
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("listing %q: %v", remotePath, err))
		state.mu.Unlock()

		return
	}

	state.mu.Lock()
	state.result.FoldersCreated++
	state.mu.Unlock()

	// Recurse into subdirectories sequentially (must exist before children).
	for i := range children {
		if children[i].IsFolder {
			childLocal := filepath.Join(localPath, children[i].Name)
			downloadRecursive(ctx, cc, session, tm, state, joinRemotePath(remotePath, children[i].Name), childLocal)
		}
	}

	// Download files in this directory concurrently with bounded parallelism.
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(defaultDownloadConcurrency)

	for i := range children {
		if children[i].IsFolder {
			continue
		}

		child := children[i]
		childLocal := filepath.Join(localPath, child.Name)

		g.Go(func() error {
			dlResult, dlErr := tm.DownloadToFile(gCtx, session.DriveID, child.ID, childLocal, driveops.DownloadOpts{
				RemoteHash: child.QuickXorHash,
			})

			state.mu.Lock()
			defer state.mu.Unlock()

			if dlErr != nil {
				state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", child.Name, dlErr))

				return nil
			}

			state.result.Files = append(state.result.Files, getJSONOutput{
				Path:         childLocal,
				Size:         dlResult.Size,
				HashVerified: dlResult.HashVerified,
			})
			state.result.TotalSize += dlResult.Size
			state.done++
			cc.Statusf("Downloaded %d/%d files\n", state.done, state.total)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		state.mu.Lock()
		state.result.Errors = append(state.result.Errors, err.Error())
		state.mu.Unlock()
	}
}

// joinRemotePath joins a parent and child remote path segment.
func joinRemotePath(parent, child string) string {
	clean := driveops.CleanRemotePath(parent)
	if clean == "" {
		return child
	}

	return clean + "/" + child
}
