package sync

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func (o *LocalObserver) scopeAllowsUnknown(path string) bool {
	if o.scopeSnapshot.IsMarkerFile(path) {
		return false
	}

	return o.scopeSnapshot.AllowsPath(path) || o.scopeSnapshot.ShouldTraverseDir(path)
}

func (o *LocalObserver) scopeAllowsFile(path string) bool {
	if o.scopeSnapshot.IsMarkerFile(path) {
		return false
	}

	return o.scopeSnapshot.AllowsPath(path)
}

func (o *LocalObserver) scopeAllowsDir(path string) bool {
	return o.scopeSnapshot.ShouldTraverseDir(path)
}

func (o *LocalObserver) handleMarkerEvent(
	ctx context.Context,
	watcher FsWatcher,
	tree *synctree.Root,
) {
	oldSnapshot := o.scopeSnapshot
	newSnapshot, err := o.BuildScopeSnapshot(ctx, tree, oldSnapshot.Config())
	if err != nil {
		o.Logger.Warn("failed to rebuild observation scope after marker change",
			slog.String("error", err.Error()))
		return
	}

	diff := syncscope.DiffSnapshots(oldSnapshot, newSnapshot)
	if !diff.HasChanges() {
		return
	}

	o.installScopeSnapshot(newSnapshot)
	o.cancelPendingTimersUnderRoots(diff.ExitedPaths)
	o.syncWatchedDirsForScopeChange(ctx, watcher, tree, diff)
	o.publishScopeChange(&syncscope.Change{
		Old:  oldSnapshot,
		New:  newSnapshot,
		Diff: diff,
	})
}

func (o *LocalObserver) publishScopeChange(change *syncscope.Change) {
	if o.scopeChanges == nil {
		return
	}

	select {
	case o.scopeChanges <- *change:
	default:
		o.Logger.Debug("scope change channel full; dropping marker scope transition")
	}
}

func (o *LocalObserver) shouldRebuildScopeForEvent(
	fsEvent fsnotify.Event,
	fsPath string,
	dbRelPath string,
) bool {
	if o.scopeSnapshot.IgnoreMarker() == "" {
		return false
	}

	if o.scopeSnapshot.IsMarkerFile(dbRelPath) {
		return true
	}

	if !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Remove) && !fsEvent.Has(fsnotify.Rename) {
		return false
	}

	if o.pathTouchesMarkerDir(dbRelPath) {
		return true
	}

	if fsEvent.Has(fsnotify.Create) {
		info, _, err := statObservedPath(fsPath)
		return err == nil && info.IsDir()
	}

	if o.wasWatchedDir(fsPath) {
		return true
	}

	existing, ok := o.Baseline.GetByPath(dbRelPath)
	return ok && existing.ItemType == ItemTypeFolder
}

func (o *LocalObserver) pathTouchesMarkerDir(path string) bool {
	for _, markerDir := range o.scopeSnapshot.MarkerDirs() {
		if markerDir == path || syncscope.CoversPath(path, markerDir) {
			return true
		}
	}

	return false
}

func (o *LocalObserver) wasWatchedDir(fsPath string) bool {
	if len(o.watchedDirs) == 0 {
		return false
	}

	_, ok := o.watchedDirs[filepath.Clean(fsPath)]
	return ok
}

func (o *LocalObserver) cancelPendingTimersUnderRoots(roots []string) {
	if len(roots) == 0 {
		return
	}

	for path, timer := range o.PendingTimers {
		if !pathInRoots(path, roots) {
			continue
		}

		timer.Stop()
		delete(o.PendingTimers, path)
		if o.pendingTimerGenerations != nil {
			delete(o.pendingTimerGenerations, path)
		}
	}
}

func (o *LocalObserver) syncWatchedDirsForScopeChange(
	ctx context.Context,
	watcher FsWatcher,
	tree *synctree.Root,
	diff syncscope.Diff,
) {
	for _, root := range diff.ExitedPaths {
		rootFsPath := tree.Path()
		if root != "" {
			absPath, err := tree.Abs(root)
			if err != nil {
				o.Logger.Debug("scope watch sync: failed to resolve exited root",
					slog.String("root", root),
					slog.String("error", err.Error()))
				continue
			}

			rootFsPath = absPath
		}

		o.removeWatchedDescendants(watcher, rootFsPath)
	}

	for _, root := range diff.EnteredPaths {
		o.addWatchedDescendants(ctx, watcher, tree, root)
	}
}

func (o *LocalObserver) removeWatchedDescendants(watcher FsWatcher, rootFsPath string) {
	if len(o.watchedDirs) == 0 {
		return
	}

	cleanRoot := filepath.Clean(rootFsPath)
	watched := make([]string, 0, len(o.watchedDirs))
	for path := range o.watchedDirs {
		watched = append(watched, path)
	}

	for _, path := range watched {
		if path == cleanRoot || !underObservedRoot(cleanRoot, path) {
			continue
		}

		if err := watcher.Remove(path); err != nil {
			o.Logger.Debug("scope watch sync: failed to remove descendant watch",
				slog.String("path", path),
				slog.String("error", err.Error()))
		}

		delete(o.watchedDirs, path)
	}
}

func (o *LocalObserver) addWatchedDescendants(
	ctx context.Context,
	watcher FsWatcher,
	tree *synctree.Root,
	root string,
) {
	rootFsPath := tree.Path()
	rootRelPath := "."
	if root != "" {
		absPath, err := tree.Abs(root)
		if err != nil {
			o.Logger.Debug("scope watch sync: failed to resolve entered root",
				slog.String("root", root),
				slog.String("error", err.Error()))
			return
		}

		rootFsPath = absPath
		rootRelPath = root
	}

	counts := &watchSetupCounts{}
	session := newWatchAddSession()
	if err := o.addObservedDirChildrenWatches(
		ctx,
		watcher,
		rootFsPath,
		rootRelPath,
		counts,
		rootObservedDirStack(tree.Path(), o.Logger),
		session,
	); err != nil {
		if isFatalWatchSetupError(err) {
			o.rollbackAddedWatches(watcher, session)
		}

		o.Logger.Warn("scope watch sync: failed to add descendant watches",
			slog.String("root", rootRelPath),
			slog.String("error", err.Error()))
	}
}

func pathInRoots(path string, roots []string) bool {
	for _, root := range roots {
		if root == "" || root == path || strings.HasPrefix(path, root+"/") {
			return true
		}
	}

	return false
}
