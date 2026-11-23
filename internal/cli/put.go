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
	"github.com/tonimelisma/onedrive-go/internal/humanunit"
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

// putJSONOutput is the JSON output of one uploaded file: the document of a
// single put, and each entry of a folder put's files.
type putJSONOutput struct {
	Path string `json:"path"`
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// putPrintedPath is the one form in which put prints a remote path, in its
// text line and under --json, for a single file and for each file of a
// folder: the cleaned path with one leading slash, whatever form it was typed
// in.
func putPrintedPath(remotePath string) string {
	return "/" + driveops.CleanRemotePath(remotePath)
}

type uploadParentResolver interface {
	WaitPathVisible(ctx context.Context, remotePath string) (*graph.Item, error)
}

func resolveUploadParent(ctx context.Context, session uploadParentResolver, parentPath string) (*graph.Item, error) {
	parentItem, err := session.WaitPathVisible(ctx, parentPath)
	if err != nil {
		return nil, newRemoteOpError("resolving parent", parentPath, err)
	}

	return parentItem, nil
}

func runPut(cmd *cobra.Command, args []string) error {
	localPath := args[0]
	ctx := cmd.Context()

	fi, err := localpath.Stat(localPath)
	if err != nil {
		return fmt.Errorf("stating local path: %w", err)
	}

	cc := mustCLIContext(ctx)

	if cc.SharedTarget != nil {
		return runSharedPut(cmd, args, cc, fi)
	}

	session, err := cc.Session(ctx)
	if err != nil {
		return err
	}

	logger := cc.Logger

	// Default remote path is root + local name.
	remotePath := "/" + filepath.Base(localPath)
	if fi.IsDir() {
		if len(args) > 1 {
			remotePath = args[1]
		}

		return uploadFolder(ctx, cc, session, localPath, remotePath)
	}

	if len(args) > 1 {
		remotePath, err = resolvePutFileTarget(ctx, session, args[1], filepath.Base(localPath))
		if err != nil {
			return err
		}
	}

	logger.Debug("put", "local_path", localPath, "remote_path", remotePath, "size", fi.Size())

	parentPath, name := driveops.SplitParentAndName(remotePath)

	parentItem, err := resolveUploadParent(ctx, session, parentPath)
	if err != nil {
		return err
	}

	progress := func(uploaded, total int64) {
		cc.Progressf("Uploading: %s / %s", humanunit.Size(uploaded), humanunit.Size(total))
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	result, err := putFile(ctx, session.Meta, tm, session.DriveID, parentItem.ID, name, localPath, driveops.UploadOpts{
		Mtime:    fi.ModTime(),
		Progress: progress,
	})
	if err != nil {
		return newRemoteOpError("uploading", remotePath, err)
	}

	visibleItem, err := session.WaitPathVisible(ctx, remotePath)
	if err != nil {
		return fmt.Errorf("confirming upload %q visibility: %w", remotePath, err)
	}

	logger.Debug("upload complete", "remote_path", remotePath, "item_id", result.Item.ID, "size", fi.Size())

	if cc.Flags.JSON {
		return writeJSONDocument(cc.Output(), putJSONOutput{
			Path: putPrintedPath(remotePath),
			ID:   visibleItem.ID,
			Size: fi.Size(),
		})
	}

	cc.Statusf("Uploaded %s (%s)\n", putPrintedPath(remotePath), humanunit.Size(fi.Size()))

	return nil
}

type putTargetResolver interface {
	ResolveItem(ctx context.Context, remotePath string) (*graph.Item, error)
}

// resolvePutFileTarget treats a destination that ends in "/" or names an
// existing folder as "into that folder", like cp and scp. Otherwise an upload
// to the folder's own name would send the file's bytes onto the folder, which
// OneDrive rejects with a 501 that reads as a temporary server problem.
func resolvePutFileTarget(ctx context.Context, session putTargetResolver, target, localName string) (string, error) {
	if strings.HasSuffix(target, "/") {
		return strings.TrimRight(target, "/") + "/" + localName, nil
	}

	item, err := session.ResolveItem(ctx, target)
	switch {
	case err == nil && item.IsFolder:
		return strings.TrimRight(target, "/") + "/" + localName, nil
	case err == nil, errors.Is(err, graph.ErrNotFound):
		return target, nil
	default:
		return "", newRemoteOpError("resolving", target, err)
	}
}

// uploadFolder uploads the local folder localPath as the remote folder
// remotePath through driveops.UploadTree, which opens localPath before it
// makes sure of remotePath, and reports the result. Every file goes through
// putFile, so a file already there is overwritten the way a single put
// overwrites it (R-1.3.8).
func uploadFolder(
	ctx context.Context,
	cc *CLIContext,
	session *driveops.MountSession,
	localPath, remotePath string,
) error {
	logger := cc.Logger

	logger.Debug("put folder", "local_path", localPath, "remote_path", remotePath)

	parentPath, name := driveops.SplitParentAndName(remotePath)

	parentItem, err := resolveUploadParent(ctx, session, parentPath)
	if err != nil {
		return err
	}

	store := driveops.NewSessionStore(config.DefaultDataDir(), logger)
	tm := driveops.NewTransferManager(session.Transfer, session.Transfer, store, logger)

	remoteRoot := driveops.CleanRemotePath(remotePath)
	var doc folderTransferJSONOutput[putJSONOutput]
	report := &folderTransferReport{cc: cc, verb: "Uploaded", noun: "upload", keepFile: func(file *driveops.TreeEvent) {
		doc.Files = append(doc.Files, putJSONOutput{Path: putPrintedPath(remoteRoot + "/" + file.RelPath), ID: file.ItemID, Size: file.Size})
	}}
	summary, err := driveops.UploadTree(ctx, driveops.TreeUpload{
		Folders: session,
		// Zero UploadOpts: the transfer reads each file's own modification
		// time, and four files at once leave no line for byte progress.
		PutFile: func(ctx context.Context, parentID, fileName, filePath string) (*driveops.UploadResult, error) {
			return putFile(ctx, session.Meta, tm, session.DriveID, parentID, fileName, filePath, driveops.UploadOpts{})
		},
		LocalRoot: localPath,
		ParentID:  parentItem.ID,
		Name:      name,
		Logger:    logger,
		OnEvent:   report.add,
	})

	var rootErr *driveops.TreeRootError
	if errors.As(err, &rootErr) {
		return newRemoteOpError("uploading", remotePath, rootErr.Err)
	}

	return report.finish(summary, err, func(foldersCreated int, failures []string) error {
		doc.FoldersCreated, doc.TotalSize, doc.Errors = foldersCreated, summary.Bytes, failures
		return writeJSONDocument(cc.Output(), doc.withEmptyLists())
	})
}

// putFile uploads localPath as name under parentID with put's semantics: a
// new name is created (never replacing, R-5.6.14), and an existing file of
// that name, or else the one file of a name OneDrive treats as the same, is
// overwritten by ID (two such files and none of that spelling answer the
// 409: neither is replaced),
// conditional on the eTag just read, so a concurrent edit answers 412 instead
// of being replaced blindly. The existing item is found by
// driveops.FindExistingChild, one read of the name. An existing folder is an
// error. An interrupted put left an empty placeholder under the name; the
// retry overwrites it the same way (or resumes its recorded session).
func putFile(
	ctx context.Context, lookup driveops.ChildLookup, tm *driveops.TransferManager,
	driveID driveid.ID, parentID, name, localPath string, opts driveops.UploadOpts,
) (*driveops.UploadResult, error) {
	result, err := tm.UploadFile(ctx, driveID, parentID, name, localPath, opts)
	if !errors.Is(err, graph.ErrConflict) {
		return result, err //nolint:wrapcheck // the caller names the file
	}

	child, findErr := driveops.FindExistingChild(ctx, lookup, driveID, parentID, name)
	existing := child.Item
	switch {
	case findErr != nil:
		return nil, errors.Join(err, findErr)
	case existing == nil:
		// The 409 itself: nothing visible holds the name yet, or more than
		// one item does and none is spelled as asked, so none is replaced.
		return nil, err //nolint:wrapcheck // see above
	case existing.IsFolder:
		return nil, fmt.Errorf("%q is a folder on OneDrive", existing.Name)
	}

	opts.IfMatch = existing.ETag
	return tm.UploadFileToItem(ctx, driveID, existing.ID, localPath, opts) //nolint:wrapcheck // the caller names the file
}
