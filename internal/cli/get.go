package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

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
		// Without a local path, the folder's name in the path the user gave,
		// unless that path is the drive's root, whose name only OneDrive
		// knows (a mounted folder's root has the name its owner gave it).
		localPath := filepath.Base(driveops.CleanRemotePath(remotePath))
		if item.IsRoot || len(args) > 1 {
			var targetErr error
			if localPath, targetErr = localFolderTarget(args, item.Name); targetErr != nil {
				return targetErr
			}
		}

		return downloadFolder(ctx, cc, session.Meta, session.Transfer, session.DriveID, item.ID, remotePath, localPath)
	}

	localPath, err := localFileTarget(args, item.Name)
	if err != nil {
		return err
	}

	tm, err := newDownloadManager(cc, session.Transfer)
	if err != nil {
		return err
	}

	return downloadOneFile(ctx, cc, tm, session.DriveID, item.ID, item, remotePath, localPath)
}

// downloadOneFile downloads one file to localPath and reports it, naming a
// partial file the failure kept so the user can resume. itemID addresses the
// file on driveID; item carries the metadata the download verifies against.
// A failure names the file by subject, the remote path or selector the user
// typed, as a put names its remote path: the download's own errors name no
// local path but the one a failed filesystem call was given. A selector has
// no parent folder a list could show.
func downloadOneFile(
	ctx context.Context,
	cc *CLIContext,
	tm *driveops.TransferManager,
	driveID driveid.ID,
	itemID string,
	item *graph.Item,
	subject, localPath string,
) error {
	opts := driveops.DownloadOptsForItem(item)

	result, err := tm.DownloadToFile(ctx, driveID, itemID, localPath, opts)
	if err != nil {
		partialPath := driveops.DownloadPartialPath(localPath, opts)
		if _, statErr := localpath.Stat(partialPath); statErr == nil {
			cc.Statusf("Partial download saved: %s\n", partialPath)
			cc.Statusf("Re-run the same command to resume.\n")
		}

		return newRemoteOpError("downloading", subject, err)
	}

	cc.Logger.Debug("download complete", "local_path", localPath, "bytes", result.Size)

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), getJSONOutput{
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

// localFolderTarget is the local folder a folder get downloads into: the one
// args names, or else remoteName, the folder's name as OneDrive reports it,
// which must then be one plain local name (R-1.2.7). A shared folder's name
// is its owner's choice, so taken as it is it could name a folder outside
// the working directory.
func localFolderTarget(args []string, remoteName string) (string, error) {
	if len(args) > 1 {
		return args[1], nil
	}

	if err := driveops.RefuseRemoteName(remoteName); err != nil {
		return "", fmt.Errorf("%w: name the local folder to download into", err)
	}

	return remoteName, nil
}

// localFileTarget is the local file a single-file get downloads to. A local
// path in args that ends in a path separator or names an existing directory
// means "into that directory" under remoteName, like cp; otherwise a download
// onto "./" would fail at the final rename and leave a nameless
// ".onedrive-go...partial" file behind (R-1.2.6). Any other local path is the
// file the user chose. With no local path the file is remoteName in the
// working directory. Wherever remoteName becomes the file's name it must be
// one plain local name (R-1.2.7): a shared file's name is its owner's choice,
// and joined as it is, an empty name, . or .. would name the folder itself or
// its parent.
func localFileTarget(args []string, remoteName string) (string, error) {
	if len(args) > 1 && !namesLocalFolder(args[1]) {
		return args[1], nil
	}

	if err := driveops.RefuseRemoteName(remoteName); err != nil {
		return "", fmt.Errorf("%w: name the local file to download to", err)
	}
	if len(args) > 1 {
		return filepath.Join(args[1], remoteName), nil
	}

	return remoteName, nil
}

// namesLocalFolder reports whether a get's local path means a folder to
// download into: it ends in a path separator or names an existing directory.
func namesLocalFolder(target string) bool {
	if strings.HasSuffix(target, string(filepath.Separator)) {
		return true
	}
	info, err := localpath.Stat(target)

	return err == nil && info.IsDir()
}

// downloadFolder downloads the remote folder folderID on driveID, and
// everything below it, into localPath through driveops.DownloadTree, listing
// each folder through lister and downloading through transfer, and reports
// the result. target names the folder for the user when its listing, or a
// subfolder's below it, fails.
func downloadFolder(
	ctx context.Context,
	cc *CLIContext,
	lister driveops.ChildLister,
	transfer *graph.Client,
	driveID driveid.ID,
	folderID, target, localPath string,
) error {
	tm, err := newDownloadManager(cc, transfer)
	if err != nil {
		return err
	}

	var doc folderTransferJSONOutput[getJSONOutput]
	report := &folderTransferReport{cc: cc, verb: "Downloaded", noun: "download", keepFile: func(file *driveops.TreeEvent) {
		doc.Files = append(doc.Files, getJSONOutput{Path: file.LocalPath, Size: file.Size, HashVerified: file.HashVerified})
	}}
	summary, err := driveops.DownloadTree(ctx, driveops.TreeDownload{
		Lister:     lister,
		Files:      tm,
		DriveID:    driveID,
		FolderID:   folderID,
		RemoteRoot: target,
		LocalRoot:  localPath,
		Logger:     cc.Logger,
		OnEvent:    report.add,
	})

	var rootErr *driveops.TreeRootError
	if errors.As(err, &rootErr) {
		return newRemoteOpError("listing", target, rootErr.Err)
	}

	return report.finish(summary, err, func(foldersCreated int, failures []string) error {
		doc.FoldersCreated, doc.TotalSize, doc.Errors = foldersCreated, summary.Bytes, failures
		return writeJSONDocument(cc.Output(), doc.withEmptyLists())
	})
}

// newDownloadManager is the transfer manager of a get: downloads through
// client, checking first that the disk keeps the drive's min_free_space
// (R-6.2.6).
func newDownloadManager(cc *CLIContext, client *graph.Client) (*driveops.TransferManager, error) {
	minFree, err := configuredMinFreeSpace(cc)
	if err != nil {
		return nil, err
	}

	return driveops.NewTransferManager(client, client, nil, cc.Logger,
		driveops.WithDiskCheck(minFree, driveops.DiskAvailable),
	), nil
}
