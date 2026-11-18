package driveops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
)

// uploadDirBatch is how many entries of a local folder a tree upload reads at
// a time, so a folder of any size is never held as one slice.
const uploadDirBatch = 256

// FolderEnsurer creates a remote folder under a parent, or returns the folder
// of that name already there. *Session satisfies it.
type FolderEnsurer interface {
	EnsureFolder(ctx context.Context, parentID, name string) (*graph.Item, error)
}

// TreeUpload is one recursive upload: what the local folder LocalRoot holds,
// into the remote folder Name in the remote folder ParentID, which the upload
// makes sure of once LocalRoot is open.
type TreeUpload struct {
	Folders FolderEnsurer
	// PutFile uploads the local file localPath as name into the remote
	// folder parentID. How it meets a file of that name already there is the
	// caller's rule.
	PutFile   func(ctx context.Context, parentID, name, localPath string) (*UploadResult, error)
	LocalRoot string
	ParentID  string
	Name      string
	Logger    *slog.Logger
	// OnEvent, which may be nil, receives every event from one goroutine, in
	// the order the events happen.
	OnEvent func(TreeEvent)
	// readBatch reads the next batch of the open local folder at localPath;
	// nil is readUploadBatch. A test replaces it to fail a read partway
	// through a folder.
	readBatch func(dir *localpath.DirReader, localPath string) ([]os.DirEntry, error)
}

// UploadTree uploads the tree u names. It opens LocalRoot, then makes sure of
// the remote root, so a local folder that cannot be opened creates nothing on
// OneDrive. The caller's goroutine walks the tree depth first: it reads each
// local folder a batch at a time, queues each file for TreeWorkers workers,
// and opens each subfolder and makes sure of it on OneDrive before it reads
// that subfolder, so a folder exists before anything is uploaded into it.
// Files upload while the walk goes on, and the walk waits while the queue is
// full, so a tree of any size holds one open folder and one batch per level
// of depth and 2×TreeWorkers files in flight. Nothing counts the tree first
// or keeps a map of its folders.
//
// The remote root is the first event, a TreeFolderCreated with RelPath "". A
// failure of one file or folder is an event, and the walk goes on with its
// siblings. The returned error is a *TreeRootError when LocalRoot cannot be
// opened or the remote root cannot be made, before anything is reported; the
// end of ctx, once every file the run queued and did not finish was reported
// failed with it and every folder it cut short was reported; or a panic of
// the walk or of an event's report. The summary counts what was reported
// either way.
func UploadTree(ctx context.Context, u TreeUpload) (TreeSummary, error) {
	root, err := localpath.OpenDir(u.LocalRoot)
	if err != nil {
		return TreeSummary{}, &TreeRootError{Err: fmt.Errorf("reading upload root: %w", err)}
	}
	defer root.Close() //nolint:errcheck // the folder was opened read-only; there is nothing to flush

	remoteRoot, err := u.Folders.EnsureFolder(ctx, u.ParentID, u.Name)
	if err != nil {
		return TreeSummary{}, &TreeRootError{Err: fmt.Errorf("creating the remote folder: %w", err)}
	}
	if u.readBatch == nil {
		u.readBatch = readUploadBatch
	}

	pool := startTreePool(ctx, u.Logger, "uploading", "folder upload", u.OnEvent)
	pool.report(TreeEvent{Kind: TreeFolderCreated, LocalPath: u.LocalRoot, ItemID: remoteRoot.ID})
	walk := uploadWalk{tree: &u, pool: pool}
	pool.walk(func() { walk.entries(ctx, root, remoteRoot.ID, "", u.LocalRoot) })

	return pool.finish(ctx)
}

// readUploadBatch reads the next uploadDirBatch entries of dir.
func readUploadBatch(dir *localpath.DirReader, _ string) ([]os.DirEntry, error) {
	return dir.Next(uploadDirBatch) //nolint:wrapcheck // localpath names the folder in every failure
}

// uploadWalk is the walk of one UploadTree, on the caller's goroutine.
type uploadWalk struct {
	tree *TreeUpload
	pool *treePool
}

// entries uploads what the open local folder dir holds into the remote
// folder folderID, a batch at a time. It returns false once ctx has ended the
// walk.
func (w *uploadWalk) entries(ctx context.Context, dir *localpath.DirReader, folderID, relPath, localPath string) bool {
	for {
		batch, err := w.tree.readBatch(dir, localPath)
		for _, entry := range batch {
			if !w.entry(ctx, entry, folderID, relPath, localPath) {
				w.pool.reportUnfinished(ctx, relPath, localPath)
				return false
			}
		}

		switch {
		case errors.Is(err, io.EOF):
			return true
		case err != nil:
			w.pool.reportWalked(TreeFolderFailed, relPath, localPath, err)
			return true
		}
	}
}

// entry walks a subfolder and queues a file.
func (w *uploadWalk) entry(ctx context.Context, entry os.DirEntry, parentID, parentRel, parentLocal string) bool {
	name := entry.Name()
	relPath, localPath := joinRelPath(parentRel, name), filepath.Join(parentLocal, name)
	if entry.IsDir() {
		return w.subfolder(ctx, parentID, name, relPath, localPath)
	}

	return w.pool.send(ctx, treeJob{
		file: TreeEvent{RelPath: relPath, LocalPath: localPath},
		transfer: func(ctx context.Context, file *TreeEvent) error {
			result, err := w.tree.PutFile(ctx, parentID, name, file.LocalPath)
			if err != nil {
				return err
			}
			file.ItemID, file.Size = result.Item.ID, result.Size

			return nil
		},
	})
}

// subfolder opens a local folder, makes sure of it on OneDrive under
// parentID and walks it. A folder that cannot be opened is not created on
// OneDrive; either failure is one event, and its siblings go on.
func (w *uploadWalk) subfolder(ctx context.Context, parentID, name, relPath, localPath string) bool {
	if ctx.Err() != nil {
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	}

	dir, err := localpath.OpenDir(localPath)
	if err != nil {
		w.pool.reportWalked(TreeFolderFailed, relPath, localPath, err)
		return true
	}
	defer dir.Close() //nolint:errcheck // the folder was opened read-only; there is nothing to flush

	folder, err := w.tree.Folders.EnsureFolder(ctx, parentID, name)
	switch {
	case err != nil && ctx.Err() != nil:
		w.pool.reportUnfinished(ctx, relPath, localPath)
		return false
	case err != nil:
		w.pool.reportWalked(TreeFolderFailed, relPath, localPath, fmt.Errorf("creating the remote folder: %w", err))
		return true
	}

	w.pool.report(TreeEvent{Kind: TreeFolderCreated, RelPath: relPath, LocalPath: localPath, ItemID: folder.ID})

	return w.entries(ctx, dir, folder.ID, relPath, localPath)
}
