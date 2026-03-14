package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// defaultDirPerm is the permission mode for directories created during recursive get.
const defaultDirPerm = 0o755

// defaultDownloadConcurrency is the maximum number of concurrent file downloads
// across the entire directory tree (shared semaphore).
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

		return downloadFolder(cmd, cc, session, remotePath, localPath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = args[1]
	}

	// Parse min_free_space from config for disk space pre-check (R-6.2.6).
	// Config is validated at load time, so ParseSize won't fail here;
	// on error we leave minFree at 0 which disables the check (safe default).
	var minFree int64
	if cc.Cfg != nil && cc.Cfg.MinFreeSpace != "" {
		if parsed, parseErr := config.ParseSize(cc.Cfg.MinFreeSpace); parseErr == nil {
			minFree = parsed
		}
	}

	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, nil, logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	result, err := tm.DownloadToFile(ctx, session.DriveID, item.ID, localPath, driveops.DownloadOpts{
		RemoteHash: item.QuickXorHash,
		RemoteSize: item.Size,
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
		return printGetJSON(os.Stdout, getJSONOutput{
			Path:         localPath,
			Size:         result.Size,
			HashVerified: result.HashVerified,
		})
	}

	cc.Statusf("Downloaded %s (%s)\n", localPath, formatSize(result.Size))

	return nil
}

// printGetJSON writes the get command's single-file JSON output to w.
func printGetJSON(w io.Writer, out getJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// printGetFolderJSON writes the get command's folder JSON output to w.
func printGetFolderJSON(w io.Writer, out getFolderJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(out)
}

// downloadState holds mutable state shared across the recursive download.
type downloadState struct {
	mu          sync.Mutex
	wg          sync.WaitGroup
	sem         chan struct{} // shared semaphore bounding total concurrency
	result      getFolderJSONOutput
	done        int
	total       int
	childCache  map[string][]graph.Item // keyed by remote path
	countErrors []string                // non-fatal errors from counting pass
}

func downloadFolder(
	cmd *cobra.Command,
	cc *CLIContext,
	session *driveops.Session,
	remotePath, localPath string,
) error {
	ctx := cmd.Context()
	logger := cc.Logger

	state := &downloadState{
		childCache: make(map[string][]graph.Item),
		sem:        make(chan struct{}, defaultDownloadConcurrency),
	}

	// Pass 1: count files and cache directory listings.
	if err := countRemoteFiles(ctx, session, remotePath, state); err != nil {
		return err
	}

	// Parse min_free_space from config for disk space pre-check (R-6.2.6).
	// Config is validated at load time, so ParseSize won't fail here;
	// on error we leave minFree at 0 which disables the check (safe default).
	var minFree int64
	if cc.Cfg != nil && cc.Cfg.MinFreeSpace != "" {
		if parsed, parseErr := config.ParseSize(cc.Cfg.MinFreeSpace); parseErr == nil {
			minFree = parsed
		}
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	// Propagate non-fatal counting errors to the result.
	state.result.Errors = append(state.result.Errors, state.countErrors...)

	// Pass 2: download recursively. Goroutines are bounded by the shared semaphore.
	downloadRecursive(ctx, cc, session, tm, state, remotePath, localPath)
	state.wg.Wait()

	if cc.Flags.JSON {
		return printGetFolderJSON(os.Stdout, state.result)
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
			childRemote := joinRemotePath(remotePath, children[i].Name)
			if subErr := countRemoteFiles(ctx, session, childRemote, state); subErr != nil {
				// Record subdirectory errors but continue counting accessible parts.
				state.countErrors = append(state.countErrors, subErr.Error())
			}
		} else {
			state.total++
		}
	}

	state.childCache[remotePath] = children

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

	// Process all children concurrently with a shared semaphore that bounds
	// total goroutines across the entire tree (not per directory).
	// This is safe because:
	// - Directory listings are cached (no redundant API calls)
	// - MkdirAll is idempotent (concurrent dir creation is safe)
	// - State mutations are mutex-protected
	for i := range children {
		if children[i].IsFolder {
			child := children[i]
			childLocal := filepath.Join(localPath, child.Name)
			childRemote := joinRemotePath(remotePath, child.Name)

			state.wg.Add(1)

			go func() {
				defer state.wg.Done()
				downloadRecursive(ctx, cc, session, tm, state, childRemote, childLocal)
			}()

			continue
		}

		child := children[i]
		childLocal := filepath.Join(localPath, child.Name)

		state.wg.Add(1)

		go func() {
			defer state.wg.Done()

			// Acquire semaphore slot before starting the download.
			// Respect context cancellation to avoid blocking forever.
			select {
			case state.sem <- struct{}{}:
			case <-ctx.Done():
				state.mu.Lock()
				state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", child.Name, ctx.Err()))
				state.mu.Unlock()

				return
			}

			defer func() { <-state.sem }()

			dlResult, dlErr := tm.DownloadToFile(ctx, session.DriveID, child.ID, childLocal, driveops.DownloadOpts{
				RemoteHash: child.QuickXorHash,
				RemoteSize: child.Size,
			})

			state.mu.Lock()
			defer state.mu.Unlock()

			if dlErr != nil {
				state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", child.Name, dlErr))

				return
			}

			state.result.Files = append(state.result.Files, getJSONOutput{
				Path:         childLocal,
				Size:         dlResult.Size,
				HashVerified: dlResult.HashVerified,
			})
			state.result.TotalSize += dlResult.Size
			state.done++
			cc.Statusf("Downloaded %d/%d files\n", state.done, state.total)
		}()
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
