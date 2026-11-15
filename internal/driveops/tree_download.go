package driveops

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// ChildLister lists the children of a remote folder by item ID, every page of
// them. *graph.Client satisfies it.
type ChildLister interface {
	ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
}

// FileDownloader downloads one remote file to a local path. *TransferManager
// satisfies it.
type FileDownloader interface {
	DownloadToFile(ctx context.Context, driveID driveid.ID, itemID, targetPath string, opts DownloadOpts) (*DownloadResult, error)
}

// TreeDownload is one recursive download: the remote folder FolderID on
// DriveID and everything below it, into the local folder LocalRoot.
type TreeDownload struct {
	Lister   ChildLister
	Files    FileDownloader
	DriveID  driveid.ID
	FolderID string
	// RemoteRoot is FolderID as the user named it (a drive path, or a shared
	// item's selector); a failure names a remote subfolder by its path below
	// it.
	RemoteRoot string
	LocalRoot  string
	Logger     *slog.Logger
	// OnEvent, which may be nil, receives every event from one goroutine, in
	// the order the events happen.
	OnEvent func(TreeEvent)
}

// DownloadTree downloads the tree d names. The caller's goroutine walks it
// depth first, a folder at a time: it lists the folder, creates it locally
// as a folder in the user's tree (R-5.10.2), queues the folder's files for
// TreeWorkers workers and walks each subfolder before the next entry. Files
// download while the walk goes on, and the walk waits while the queue is
// full, so a tree of any size holds one listing per level of depth and
// 2×TreeWorkers files in flight. Nothing counts or caches the tree first.
//
// A failure of one file or folder is an event, and the walk goes on with its
// siblings. The returned error is a *TreeRootError when the root folder
// cannot be listed, before anything is reported; the end of ctx, once every
// file the run queued and did not finish was reported failed with it and
// every folder it cut short was reported; or a panic of the walk or of an
// event's report. The summary counts what was reported either way.
func DownloadTree(ctx context.Context, d TreeDownload) (TreeSummary, error) {
	children, err := d.Lister.ListChildren(ctx, d.DriveID, d.FolderID)
	if err != nil {
		return TreeSummary{}, &TreeRootError{Err: fmt.Errorf("list children: %w", err)}
	}

	pool := startTreePool(ctx, d.Logger, "downloading", "folder download", d.OnEvent)
	walk := downloadWalk{tree: &d, pool: pool}
	pool.walk(func() { walk.folder(ctx, children, "", d.LocalRoot) })

	return pool.finish(ctx)
}

// downloadWalk is the walk of one DownloadTree, on the caller's goroutine.
type downloadWalk struct {
	tree *TreeDownload
	pool *treePool
}

// folder creates the local folder of a listed remote folder, queues its files
// and walks its subfolders, in listing order. It returns false once ctx has
// ended the walk.
func (w *downloadWalk) folder(ctx context.Context, children []graph.Item, relPath, localPath string) bool {
	created, err := createDownloadFolder(localPath)
	if err != nil {
		w.pool.reportWalked(TreeFolderFailed, relPath, localPath, err)
		return true
	}
	if created {
		w.pool.reportWalked(TreeFolderCreated, relPath, localPath, nil)
	}

	for i := range children {
		if !w.child(ctx, &children[i], relPath, localPath) {
			w.pool.reportUnfinished(ctx, relPath, localPath)
			return false
		}
	}

	return true
}

// child refuses a remote name that is not one plain local name, walks a
// folder and queues a file.
func (w *downloadWalk) child(ctx context.Context, item *graph.Item, parentRel, parentLocal string) bool {
	relPath := joinRelPath(parentRel, item.Name)
	if refusal := RefuseRemoteName(item.Name); refusal != nil {
		kind := TreeFileFailed
		if item.IsFolder {
			kind = TreeFolderFailed
		}
		w.pool.reportWalked(kind, relPath, parentLocal, fmt.Errorf("%s: %w", parentLocal, refusal))
		return true
	}

	localPath := filepath.Join(parentLocal, item.Name)
	if item.IsFolder {
		return w.subfolder(ctx, item.ID, relPath, localPath)
	}

	itemID, opts := item.ID, DownloadOptsForItem(item)
	return w.pool.send(ctx, treeJob{
		file: TreeEvent{RelPath: relPath, LocalPath: localPath},
		transfer: func(ctx context.Context, file *TreeEvent) error {
			result, err := w.tree.Files.DownloadToFile(ctx, w.tree.DriveID, itemID, file.LocalPath, opts)
			if err != nil {
				return err //nolint:wrapcheck // the event puts the file's path in front of the downloader's error
			}
			file.Size, file.HashVerified = result.Size, result.HashVerified

			return nil
		},
	})
}

// subfolder lists a remote folder and walks it. A folder that cannot be
// listed is one failure, which names the remote folder and the local folder
// it would have filled; its siblings go on.
func (w *downloadWalk) subfolder(ctx context.Context, folderID, relPath, localPath string) bool {
	if ctx.Err() != nil {
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	}

	children, err := w.tree.Lister.ListChildren(ctx, w.tree.DriveID, folderID)
	switch {
	case err != nil && ctx.Err() != nil:
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	case err != nil:
		remoteFolder := path.Join(w.tree.RemoteRoot, relPath)
		w.pool.reportWalked(TreeFolderFailed, relPath, localPath,
			fmt.Errorf("listing the OneDrive folder %q to fill %s: %w", remoteFolder, localPath, err))
		return true
	default:
		return w.folder(ctx, children, relPath, localPath)
	}
}

// createDownloadFolder creates a folder of a tree download, asking for
// localpath.UserTreeDirPerm (0777 minus the umask), and reports whether this
// call created it: an existing folder is used as it is, keeps its mode, and
// is not counted as created. Every error names the path once.
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

// The names Linux keeps in every folder for the folder itself and its parent.
const (
	selfDirName   = "."
	parentDirName = ".."
)

// The reasons a tree download refuses a remote name.
const (
	nameIsEmpty        = "is empty"
	nameIsDotOrDotDot  = "is . or .."
	nameHoldsSeparator = "holds a path separator"
	nameIsNotLocal     = "is not a local name"
)

// RefusedNameError is a remote item a download does not write under its own
// name, as a child of a tree or as the one file or folder a command was asked
// for: the name is not one plain local name, so joining it to a local folder
// would land somewhere else, or nowhere. Reason says which rule it broke.
type RefusedNameError struct {
	Name   string
	Reason string
}

func (e *RefusedNameError) Error() string {
	return fmt.Sprintf("refused the remote name %q: it %s", e.Name, e.Reason)
}

// RefuseRemoteName is the refusal, a *RefusedNameError, of a remote item's
// name as a local name, or nil for a name a download may join to a local
// folder: one that is not empty, not . or .., holds neither / nor \ and is
// local by filepath.IsLocal (which, on Windows, also refuses reserved names
// such as NUL). OneDrive refuses / and \ in names itself, and Linux cannot
// hold . or .. as a name, so nothing OneDrive can serve and Linux can hold is
// refused. DownloadTree applies it to every child; a command applies it to a
// folder or a single file whose remote name it takes as the local one.
func RefuseRemoteName(name string) error {
	var reason string
	switch {
	case name == "":
		reason = nameIsEmpty
	case name == selfDirName || name == parentDirName:
		reason = nameIsDotOrDotDot
	case strings.ContainsAny(name, `/\`):
		reason = nameHoldsSeparator
	case !filepath.IsLocal(name):
		reason = nameIsNotLocal
	default:
		return nil
	}

	return &RefusedNameError{Name: name, Reason: reason}
}
