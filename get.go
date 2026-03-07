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

// cachedChild holds the subset of graph.Item fields needed for download.
type cachedChild struct {
	name         string
	id           string
	isFolder     bool
	quickXorHash string
}

// downloadState holds mutable state shared across the recursive download.
type downloadState struct {
	mu         sync.Mutex
	result     getFolderJSONOutput
	done       int
	total      int
	childCache map[string][]cachedChild // keyed by remote path
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

	state := &downloadState{
		childCache: make(map[string][]cachedChild),
	}

	// Pass 1: count files and cache directory listings.
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

	cached := make([]cachedChild, len(children))
	for i := range children {
		cached[i] = cachedChild{
			name:         children[i].Name,
			id:           children[i].ID,
			isFolder:     children[i].IsFolder,
			quickXorHash: children[i].QuickXorHash,
		}

		if children[i].IsFolder {
			if err := countRemoteFiles(ctx, session, joinRemotePath(remotePath, children[i].Name), state); err != nil {
				return err
			}
		} else {
			state.total++
		}
	}

	state.childCache[remotePath] = cached

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

	// Read from cache populated by countRemoteFiles — no extra API call.
	children := state.childCache[remotePath]

	state.mu.Lock()
	state.result.FoldersCreated++
	state.mu.Unlock()

	// Process all children concurrently with bounded parallelism.
	// Subdirectory traversal is safe to parallelize because:
	// - Directory listings are cached (no redundant API calls)
	// - MkdirAll is idempotent (concurrent dir creation is safe)
	// - State mutations are mutex-protected
	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(defaultDownloadConcurrency)

	for i := range children {
		if children[i].isFolder {
			child := children[i]
			childLocal := filepath.Join(localPath, child.name)
			childRemote := joinRemotePath(remotePath, child.name)

			g.Go(func() error {
				downloadRecursive(gCtx, cc, session, tm, state, childRemote, childLocal)

				return nil
			})

			continue
		}

		child := children[i]
		childLocal := filepath.Join(localPath, child.name)

		g.Go(func() error {
			dlResult, dlErr := tm.DownloadToFile(gCtx, session.DriveID, child.id, childLocal, driveops.DownloadOpts{
				RemoteHash: child.quickXorHash,
			})

			state.mu.Lock()
			defer state.mu.Unlock()

			if dlErr != nil {
				state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", child.name, dlErr))

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
