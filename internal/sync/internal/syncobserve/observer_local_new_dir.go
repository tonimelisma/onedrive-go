// Watch-mode handling of a created directory: its watch registration and
// the scan of what it already holds.

package syncobserve

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// handleDirectoryCreate registers and emits a created directory, then scans
// what it already holds.
func (o *LocalObserver) handleDirectoryCreate(
	ctx context.Context, tree *synctree.Root,
	fsPath, dbRelPath string, info os.FileInfo,
	ev *syncdomain.LocalEntry, watcher FsWatcher,
) {
	ev.ItemType = syncdomain.ItemTypeFolder

	// A Create for a directory already registered with the same
	// identity is a duplicate (fsnotify's event racing the new-directory
	// scan, or a directory that left and came back as the same inode):
	// its watches are valid, so its subtree is not dropped, re-added and
	// re-hashed.
	if o.isRegisteredSameDirectory(fsPath, info) {
		o.TrySend(ctx, ev)
		return
	}

	// A directory created at a path that still has watched descendants
	// replaced one that was deleted or moved away while watched: the
	// delete event was ignored because the path exists again, or has not
	// arrived yet. Those descendant registrations belong to the old
	// inodes, so they are dropped before the new tree is registered; the
	// scan below re-adds whatever the new directory holds.
	o.forgetWatchedDescendants(watcher, fsPath, dbRelPath)

	if addErr := watcher.Add(fsPath); addErr != nil {
		o.reportWatchAddFailure("failed to add watch on new directory", dbRelPath, addErr)
	} else {
		o.watchedDirs.add(fsPath, info)
	}

	// The directory's own row goes out before its contents are scanned.
	// When it is the destination half of a folder rename, its identity
	// pairs the move; emitting it only after hashing a large tree left
	// the replan seeing just the delete half (D-080).
	o.TrySend(ctx, ev)

	// Scan directory contents for files created before the watch was
	// registered. Duplicates from fsnotify are harmless — the buffer's
	// per-path deduplication handles them.
	o.scanNewDirectory(ctx, tree, fsPath, dbRelPath, watcher)
}

// scanNewDirectory walks a newly-created directory and sends the rows of any
// entries already present. This catches files created between
// the directory's creation and the fsnotify watch registration.
func (o *LocalObserver) scanNewDirectory(
	ctx context.Context, tree *synctree.Root, dirPath, dirRelPath string,
	watcher FsWatcher,
) {
	entries, err := localpath.ReadDir(dirPath)
	if err != nil {
		o.logger.Debug("scan new directory failed",
			slog.String("path", dirRelPath), slog.Any("error", err))

		return
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		o.scanNewDirectoryEntry(ctx, tree, dirPath, dirRelPath, entry, watcher)
	}
}

func (o *LocalObserver) scanNewDirectoryEntry(
	ctx context.Context,
	tree *synctree.Root,
	dirPath string,
	dirRelPath string,
	entry os.DirEntry,
	watcher FsWatcher,
) {
	entryName := namekey.Stored(entry.Name())
	entryRelPath := dirRelPath + "/" + entryName
	entryFsPath := filepath.Join(dirPath, entry.Name())

	info, kind, ok := o.scanNewDirectoryEntryInfo(entry, entryFsPath, entryRelPath)
	if !ok {
		return
	}

	if shouldObserveWithFilter(entryName, entryRelPath, kind, o.filterConfig, o.protectedRoots, o.observationRules) != nil {
		return
	}

	if protectedRoot, ok := syncshortcut.ProtectedRootIdentityReservation(entryRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(syncshortcut.ProtectedRootEvent{
			Type:         syncshortcut.ProtectedRootEventIdentityMatch,
			Path:         entryRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		return
	}

	if kind == syncdomain.ObservedKindDir {
		o.scanNewDirectoryChildDir(ctx, tree, entryFsPath, entryRelPath, watcher)
		return
	}

	if o.IsOversizedFile(info.Size(), entryRelPath) {
		return
	}

	fileEv := syncdomain.LocalEntry{
		Path:     entryRelPath,
		ItemType: syncdomain.ItemTypeFile,
		Size:     info.Size(),
		Hash:     o.watchCreateHash(ctx, tree, entryFsPath, entryRelPath, info),
		Mtime:    info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		fileEv.LocalDevice = identity.Device
		fileEv.LocalInode = identity.Inode
		fileEv.LocalBirthTime = identity.BirthTime
		fileEv.LocalHasIdentity = true
	}
	fileEv.LocalCtime = syncdomain.FileChangeTime(info)

	o.TrySend(ctx, &fileEv)
}

func (o *LocalObserver) scanNewDirectoryEntryInfo(
	entry os.DirEntry,
	entryFsPath string,
	entryRelPath string,
) (os.FileInfo, syncdomain.ObservedKind, bool) {
	if entry.Type()&fs.ModeSymlink != 0 {
		info, isSymlink, err := statObservedPath(entryFsPath)
		if err != nil {
			o.logger.Debug("stat failed during directory scan",
				slog.String("path", entryRelPath), slog.Any("error", err))
			return nil, syncdomain.ObservedKindUnknown, false
		}

		if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
			o.rememberExcludedSymlink(entryRelPath)
			return nil, syncdomain.ObservedKindUnknown, false
		}

		o.forgetExcludedSymlink(entryRelPath)
		if isSpecialFile(info) {
			return nil, syncdomain.ObservedKindUnknown, false
		}
		return info, observedKindFromInfo(info), true
	}

	o.forgetExcludedSymlink(entryRelPath)

	info, err := entry.Info()
	if err != nil {
		o.logger.Debug("stat failed during directory scan",
			slog.String("path", entryRelPath), slog.Any("error", err))
		return nil, syncdomain.ObservedKindUnknown, false
	}
	if isSpecialFile(info) {
		return nil, syncdomain.ObservedKindUnknown, false
	}

	// The listing is by absolute path (localpath), so the birth time that
	// completes the entry's identity is read here, as statObservedPath does.
	return synctree.AttachBirthTime(info, entryFsPath), dirEntryKind(entry), true
}

func (o *LocalObserver) scanNewDirectoryChildDir(
	ctx context.Context,
	tree *synctree.Root,
	entryFsPath string,
	entryRelPath string,
	watcher FsWatcher,
) {
	info, _, statErr := statObservedPath(entryFsPath)
	if addErr := watcher.Add(entryFsPath); addErr != nil {
		o.reportWatchAddFailure("failed to add watch on nested directory", entryRelPath, addErr)
	} else {
		if statErr != nil {
			info = nil
		}
		o.watchedDirs.add(entryFsPath, info)
	}

	dirEv := syncdomain.LocalEntry{
		Path:     entryRelPath,
		ItemType: syncdomain.ItemTypeFolder,
	}
	if statErr == nil {
		dirEv.Size = info.Size()
		dirEv.Mtime = info.ModTime().UnixNano()
		if identity, ok := synctree.IdentityFromFileInfo(info); ok {
			dirEv.LocalDevice = identity.Device
			dirEv.LocalInode = identity.Inode
			dirEv.LocalBirthTime = identity.BirthTime
			dirEv.LocalHasIdentity = true
		}
	}

	o.TrySend(ctx, &dirEv)
	o.scanNewDirectory(ctx, tree, entryFsPath, entryRelPath, watcher)
}
