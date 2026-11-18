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

// ChildLister hands the children of a remote folder, listed by item ID, on a
// page at a time (graph.Client.ListChildrenPages). *graph.Client satisfies it.
type ChildLister interface {
	ListChildrenPages(ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc) error
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
// depth first, a folder at a time: it reads the folder a page at a time,
// creates it locally as a folder in the user's tree (R-5.10.2) once the first
// page is in, queues each page's files for TreeWorkers workers, walks each
// subfolder before the next entry, and asks for the next page only once it is
// done with the page before. Files download while the walk goes on, and the
// walk waits while the queue is full, so a tree of any size holds one page of
// a listing per level of depth and 2×TreeWorkers files in flight. Nothing
// counts or caches the tree first.
//
// A failure of one file or folder is an event, and the walk goes on with its
// siblings; a page that fails after a folder's first one is that folder's
// failure, the root's included. The returned error is a *TreeRootError when
// the root folder's first page cannot be read, before anything is reported;
// the end of ctx, once every file the run queued and did not finish was
// reported failed with it and every folder it cut short was reported; or a
// panic of the walk or of an event's report. The summary counts what was
// reported either way.
func DownloadTree(ctx context.Context, d TreeDownload) (TreeSummary, error) {
	pool := startTreePool(ctx, d.Logger, "downloading", "folder download", d.OnEvent)
	walk := downloadWalk{tree: &d, pool: pool}
	pool.walk(func() { walk.folder(ctx, d.FolderID, "", d.LocalRoot) })

	summary, err := pool.finish(ctx)
	if walk.unlistedRoot != nil {
		return TreeSummary{}, &TreeRootError{Err: walk.unlistedRoot}
	}

	return summary, err
}

// downloadWalk is the walk of one DownloadTree, on the caller's goroutine.
type downloadWalk struct {
	tree *TreeDownload
	pool *treePool
	// unlistedRoot is the failure of the root folder's first page, after
	// which nothing was reported.
	unlistedRoot error
}

// folder reads the remote folder folderID a page at a time and walks each
// page before it asks for the next. The first page creates the local folder:
// one that cannot be created is the folder's failure, and its listing stops.
// A page that fails is the folder's failure, after the pages before it were
// walked; the root's first page failing is unlistedRoot instead. It returns
// false once ctx has ended the walk.
func (w *downloadWalk) folder(ctx context.Context, folderID, relPath, localPath string) bool {
	pages, goesOn := 0, true
	err := w.tree.Lister.ListChildrenPages(ctx, w.tree.DriveID, folderID, func(children []graph.Item) bool {
		pages++
		if pages == 1 && !w.createFolder(relPath, localPath) {
			return false
		}
		goesOn = w.walkPage(ctx, children, relPath, localPath)

		return goesOn
	})

	switch {
	case !goesOn:
		return false
	case err == nil:
		return true
	case pages == 0 && relPath == "":
		w.unlistedRoot = fmt.Errorf("list children: %w", err)
		return false
	default:
		return w.listingFailed(ctx, err, relPath, localPath)
	}
}

// createFolder creates the local folder of a listed remote folder and reports
// it when this call made it. A folder it cannot create is one failure, and
// nothing is walked into it.
func (w *downloadWalk) createFolder(relPath, localPath string) bool {
	created, err := createDownloadFolder(localPath)
	if err != nil {
		w.pool.reportWalked(TreeFolderFailed, relPath, localPath, err)
		return false
	}
	if created {
		w.pool.reportWalked(TreeFolderCreated, relPath, localPath, nil)
	}

	return true
}

// walkPage queues the files of one page of a folder's children and walks its
// subfolders, in listing order. It returns false once ctx has ended the
// walk, the folder reported as cut short.
func (w *downloadWalk) walkPage(ctx context.Context, children []graph.Item, relPath, localPath string) bool {
	for i := range children {
		if !w.child(ctx, &children[i], relPath, localPath) {
			w.pool.reportUnfinished(ctx, relPath, localPath)
			return false
		}
	}

	return true
}

// listingFailed reports a folder whose listing failed with err. The end of
// ctx cuts the walk short; any other failure is one failure of the folder:
// the event names the local folder it would have filled, its error the
// remote folder. Its siblings go on.
func (w *downloadWalk) listingFailed(ctx context.Context, err error, relPath, localPath string) bool {
	if ctx.Err() != nil {
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	}

	remoteFolder := path.Join(w.tree.RemoteRoot, relPath)
	w.pool.reportWalked(TreeFolderFailed, relPath, localPath,
		fmt.Errorf("listing the OneDrive folder %q: %w", remoteFolder, err))

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
		w.pool.reportWalked(kind, relPath, parentLocal, refusal)
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
				return err //nolint:wrapcheck // the event names the file the downloader's error is about
			}
			file.Size, file.HashVerified = result.Size, result.HashVerified

			return nil
		},
	})
}

// subfolder lists a remote folder and walks it, unless ctx has already ended
// the walk. A folder that cannot be listed is one failure (listingFailed);
// its siblings go on.
func (w *downloadWalk) subfolder(ctx context.Context, folderID, relPath, localPath string) bool {
	if ctx.Err() != nil {
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	}

	return w.folder(ctx, folderID, relPath, localPath)
}

// createDownloadFolder creates a folder of a tree download, asking for
// localpath.UserTreeDirPerm (0777 minus the umask), and reports whether this
// call created it: an existing folder is used as it is, keeps its mode, and
// is not counted as created. A failure of localpath names the path; the one
// it makes itself names none, since the event names the folder.
func createDownloadFolder(localPath string) (bool, error) {
	info, err := localpath.Stat(localPath)
	switch {
	case err == nil && info.IsDir():
		return false, nil
	case err == nil:
		return false, errors.New("creating directory: it is not a folder")
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
