package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

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
	ctx := cmd.Context()
	cc := mustCLIContext(ctx)

	if cc.SharedTarget != nil {
		return runSharedGet(cmd, args, cc)
	}

	remotePath := args[0]

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger
	logger.Debug("get", "remote_path", remotePath)

	item, err := session.ResolveItem(ctx, remotePath)
	if err != nil {
		return newRemoteOpError("resolving", remotePath, err)
	}

	if item.IsFolder {
		localPath := filepath.Base(driveops.CleanRemotePath(remotePath))
		if item.IsRoot {
			localPath = item.Name
		}

		if len(args) > 1 {
			localPath = args[1]
		}

		return downloadFolder(ctx, cc, session, item.ID, remotePath, localPath)
	}

	localPath := item.Name
	if len(args) > 1 {
		localPath = localFileTarget(args[1], item.Name)
	}

	minFree, err := configuredMinFreeSpace(cc)
	if err != nil {
		return err
	}

	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, nil, logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	return downloadOneFile(ctx, cc, tm, session.DriveID, item.ID, item, localPath)
}

// downloadOneFile downloads one file to localPath and reports it, naming a
// partial file the failure kept so the user can resume. itemID addresses the
// file on driveID; item carries the metadata the download verifies against.
func downloadOneFile(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	driveID driveid.ID,
	itemID string,
	item *graph.Item,
	localPath string,
) error {
	opts := driveops.DownloadOptsForItem(item)

	result, err := tm.DownloadToFile(ctx, driveID, itemID, localPath, opts)
	if err != nil {
		partialPath := driveops.DownloadPartialPath(localPath, opts)
		if _, statErr := localpath.Stat(partialPath); statErr == nil {
			cc.Statusf("Partial download saved: %s\n", partialPath)
			cc.Statusf("Re-run the same command to resume.\n")
		}

		return fmt.Errorf("download file: %w", err)
	}

	cc.Logger.Debug("download complete", "local_path", localPath, "bytes", result.Size)

	if cc.Flags.JSON {
		return printGetJSON(cc.Output(), getJSONOutput{
			Path:         localPath,
			Size:         result.Size,
			HashVerified: result.HashVerified,
		})
	}

	cc.Statusf("Downloaded %s (%s)\n", localPath, formatSize(result.Size))

	return nil
}

// configuredMinFreeSpace is the drive's min_free_space in bytes for the disk
// space check before a download (R-6.2.6): 0 without a configured drive or
// with the check turned off. Config validates the value when it loads, so an
// error here is a value that never went through that validation.
func configuredMinFreeSpace(cc *CLIContext) (int64, error) {
	if cc.Cfg == nil {
		return 0, nil
	}

	minFree, err := config.ParseSize(cc.Cfg.MinFreeSpace)
	if err != nil {
		return 0, fmt.Errorf("reading min_free_space: %w", err)
	}

	return minFree, nil
}

// localFileTarget treats a destination that ends in a path separator or
// names an existing directory as "into that directory", like cp. Downloading
// onto "./" used to fail at the final rename and leave a nameless
// ".onedrive-go...partial" file behind.
func localFileTarget(target, remoteName string) string {
	if strings.HasSuffix(target, string(filepath.Separator)) {
		return filepath.Join(target, remoteName)
	}
	if info, err := localpath.Stat(target); err == nil && info.IsDir() {
		return filepath.Join(target, remoteName)
	}

	return target
}

// printGetJSON writes the get command's single-file JSON output to w.
func printGetJSON(w io.Writer, out getJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode download output: %w", err)
	}

	return nil
}

// printGetFolderJSON writes the get command's folder JSON output to w.
func printGetFolderJSON(w io.Writer, out getFolderJSONOutput) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if err := enc.Encode(out); err != nil {
		return fmt.Errorf("encode folder download output: %w", err)
	}

	return nil
}

// downloadState holds mutable state shared across the recursive download.
type downloadState struct {
	// mu guards result, done, total, and countErrors against the concurrent
	// file-download goroutines. childCache needs no lock: the counting pass
	// fills it before any download starts, and the directory walk that reads
	// it runs inline on a single goroutine.
	mu          sync.Mutex
	wg          sync.WaitGroup
	sem         chan struct{} // shared semaphore bounding total concurrency
	result      getFolderJSONOutput
	done        int
	total       int
	childCache  map[string][]graph.Item // keyed by folder item ID
	countErrors []string                // non-fatal errors from counting pass
}

func newDownloadState() *downloadState {
	return &downloadState{
		childCache: make(map[string][]graph.Item),
		sem:        make(chan struct{}, defaultDownloadConcurrency),
	}
}

// recordError adds one failure to the result.
func (state *downloadState) recordError(failure string) {
	state.mu.Lock()
	defer state.mu.Unlock()

	state.result.Errors = append(state.result.Errors, failure)
}

// downloadFolder downloads the folder folderID at remotePath into localPath.
func downloadFolder(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.MountSession,
	folderID, remotePath, localPath string,
) error {
	state := newDownloadState()

	// Pass 1: count files and cache directory listings.
	if err := countRemoteFiles(ctx, session, remotePath, folderID, state); err != nil {
		return err
	}

	minFree, err := configuredMinFreeSpace(cc)
	if err != nil {
		return err
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), cc.Logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, cc.Logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	)

	// Pass 2: download. File downloads are bounded by the shared semaphore.
	return finishFolderDownload(ctx, cc, tm, state, session.DriveID, folderID, localPath)
}

// finishFolderDownload runs the download pass over the listings the counting
// pass cached, waits for every file, and reports the result.
func finishFolderDownload(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	state *downloadState,
	driveID driveid.ID,
	folderID, localPath string,
) error {
	// Propagate non-fatal counting errors to the result.
	state.result.Errors = append(state.result.Errors, state.countErrors...)

	downloadTree(ctx, cc, tm, state, driveID, folderID, localPath)
	state.wg.Wait()

	if cc.Flags.JSON {
		if err := printGetFolderJSON(cc.Output(), state.result); err != nil {
			return err
		}

		return bulkTransferError(nil, "download", state.result.Errors)
	}

	cc.Statusf("Downloaded %d files, %d folders (%s)\n",
		len(state.result.Files), state.result.FoldersCreated, formatSize(state.result.TotalSize))

	return bulkTransferError(cc.Status(), "download", state.result.Errors)
}

// countRemoteFiles lists the folder folderID at remotePath and every folder
// below it, caching each listing under its folder's item ID and counting the
// files.
func countRemoteFiles(ctx context.Context, session *driveops.MountSession, remotePath, folderID string, state *downloadState) error {
	children, err := session.ListChildren(ctx, remotePath)
	if err != nil {
		return newRemoteOpError("listing", remotePath, err)
	}

	for i := range children {
		if children[i].IsFolder {
			childRemote := joinRemotePath(remotePath, children[i].Name)
			if subErr := countRemoteFiles(ctx, session, childRemote, children[i].ID, state); subErr != nil {
				// Record subdirectory errors but continue counting accessible parts.
				state.countErrors = append(state.countErrors, subErr.Error())
			}
		} else {
			state.total++
		}
	}

	state.childCache[folderID] = children

	return nil
}

// downloadTree creates localPath, as a folder in the user's tree (R-5.10.2),
// downloads into it the files of the folder whose listing the counting pass
// cached under folderID, and walks its subfolders the same way. A drive path
// and a shared folder download through this one walk; only their counting
// passes list differently.
//
// File downloads run concurrently under a shared semaphore that bounds total
// in-flight transfers across the whole tree. Directory descent is
// deliberately not concurrent: a goroutine per subdirectory makes the
// goroutine count scale with the number of directories in the tree, which is
// exactly the unbounded fan-out the semaphore exists to prevent, and the
// semaphore never covered it. Nothing is lost by walking inline: the
// listings are cached, so descent performs no network I/O, only a folder
// creation and a map lookup, and transfers still start as soon as each
// folder is reached.
func downloadTree(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	state *downloadState,
	driveID driveid.ID,
	folderID, localPath string,
) {
	created, err := createDownloadFolder(localPath)
	if err != nil {
		state.recordError(err.Error())

		return
	}

	if created {
		state.mu.Lock()
		state.result.FoldersCreated++
		state.mu.Unlock()
	}

	children := state.childCache[folderID]
	for i := range children {
		child := &children[i]
		childLocal := filepath.Join(localPath, child.Name)

		if child.IsFolder {
			downloadTree(ctx, cc, tm, state, driveID, child.ID, childLocal)

			continue
		}

		state.wg.Add(1)

		go func() {
			defer state.wg.Done()

			downloadTreeFile(ctx, cc, tm, state, driveID, child, childLocal)
		}()
	}
}

// createDownloadFolder creates a folder a folder download needs and reports
// whether this call created it: an existing folder is used as it is, keeps
// its mode, and is not counted as created. Every error names the path once.
func createDownloadFolder(localPath string) (bool, error) {
	info, err := localpath.Stat(localPath)
	switch {
	case err == nil && info.IsDir():
		return false, nil
	case err == nil:
		return false, fmt.Errorf("creating directory %s: it is not a folder", localPath)
	case !errors.Is(err, os.ErrNotExist):
		return false, err //nolint:wrapcheck // localpath already names the path
	}

	if mkdirErr := localpath.MkdirAll(localPath, localpath.UserTreeDirPerm); mkdirErr != nil {
		return false, mkdirErr //nolint:wrapcheck // localpath already names the path
	}

	return true, nil
}

// downloadTreeFile downloads one file of a folder download once the shared
// semaphore admits it, and records the outcome.
func downloadTreeFile(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	state *downloadState,
	driveID driveid.ID,
	child *graph.Item,
	childLocal string,
) {
	// Respect cancellation while waiting, so a canceled download never
	// blocks on the semaphore.
	select {
	case state.sem <- struct{}{}:
	case <-ctx.Done():
		state.recordError(fmt.Sprintf("%s: %v", childLocal, ctx.Err()))

		return
	}

	defer func() { <-state.sem }()

	result, err := tm.DownloadToFile(ctx, driveID, child.ID, childLocal, driveops.DownloadOptsForItem(child))

	state.mu.Lock()
	defer state.mu.Unlock()

	if err != nil {
		state.result.Errors = append(state.result.Errors, fmt.Sprintf("%s: %v", childLocal, err))

		return
	}

	state.result.Files = append(state.result.Files, getJSONOutput{
		Path:         childLocal,
		Size:         result.Size,
		HashVerified: result.HashVerified,
	})
	state.result.TotalSize += result.Size
	state.done++
	cc.Progressf("Downloaded %d/%d files", state.done, state.total)
}

// joinRemotePath joins a parent and child remote path segment.
func joinRemotePath(parent, child string) string {
	clean := driveops.CleanRemotePath(parent)
	if clean == "" {
		return child
	}

	return clean + "/" + child
}
