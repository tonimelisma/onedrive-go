package syncobserve

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// statObservedPath returns the followed target info for fsPath while also
// reporting whether the observed path itself is a symbolic link.
func statObservedPath(fsPath string) (os.FileInfo, bool, error) {
	linkInfo, err := localpath.Lstat(fsPath)
	if err != nil {
		return nil, false, fmt.Errorf("lstat observed path %s: %w", fsPath, err)
	}

	isSymlink := linkInfo.Mode()&os.ModeSymlink != 0
	if !isSymlink {
		return linkInfo, false, nil
	}

	info, err := localpath.Stat(fsPath)
	if err != nil {
		return nil, true, fmt.Errorf("stat observed symlink target %s: %w", fsPath, err)
	}

	return info, true, nil
}

func observedKindFromInfo(info os.FileInfo) observedKind {
	if info.IsDir() {
		return observedKindDir
	}

	return observedKindFile
}

func joinObservedPath(parent, name string) string {
	if parent == "." || parent == "" {
		return name
	}

	return parent + "/" + name
}

func cloneObservedDirStack(stack map[string]struct{}) map[string]struct{} {
	cloned := make(map[string]struct{}, len(stack)+1)
	for path := range stack {
		cloned[path] = struct{}{}
	}

	return cloned
}

func rootObservedDirStack(syncRoot string, logger *slog.Logger) map[string]struct{} {
	stack := make(map[string]struct{}, 1)

	realRoot, err := localpath.EvalSymlinks(syncRoot)
	if err != nil {
		if logger != nil {
			logger.Debug("could not resolve sync root while building symlink guard",
				slog.String("sync_root", syncRoot),
				slog.String("error", err.Error()))
		}

		stack[filepath.Clean(syncRoot)] = struct{}{}
		return stack
	}

	stack[filepath.Clean(realRoot)] = struct{}{}
	return stack
}

func resolvedObservedDirPath(fsPath string) (string, error) {
	resolved, err := localpath.EvalSymlinks(fsPath)
	if err != nil {
		return "", fmt.Errorf("resolve observed directory %s: %w", fsPath, err)
	}

	return filepath.Clean(resolved), nil
}

func shouldSkipObservedSymlink(isSymlink bool, filter synctypes.LocalFilterConfig) bool {
	return isSymlink && filter.SkipSymlinks
}

func (o *LocalObserver) rememberExcludedSymlink(path string) {
	if path == "" || path == "." {
		return
	}

	if o.excludedSymlinkPaths == nil {
		o.excludedSymlinkPaths = make(map[string]struct{})
	}

	o.excludedSymlinkPaths[path] = struct{}{}
}

func (o *LocalObserver) forgetExcludedSymlink(path string) {
	if o.excludedSymlinkPaths == nil {
		return
	}

	delete(o.excludedSymlinkPaths, path)
}

func (o *LocalObserver) hasExcludedSymlinkAncestor(path string) bool {
	if o.excludedSymlinkPaths == nil {
		return false
	}

	for current := path; current != ""; {
		if _, ok := o.excludedSymlinkPaths[current]; ok {
			return true
		}

		idx := strings.LastIndex(current, "/")
		if idx < 0 {
			break
		}

		current = current[:idx]
	}

	return false
}

func (o *LocalObserver) nextObservedDirStack(
	dirStack map[string]struct{},
	fsPath string,
	dbRelPath string,
	cycleLog string,
) (map[string]struct{}, bool, error) {
	resolvedDir, err := resolvedObservedDirPath(fsPath)
	if err != nil {
		return nil, false, err
	}

	if _, cycle := dirStack[resolvedDir]; cycle {
		o.Logger.Debug(cycleLog,
			slog.String("path", dbRelPath),
			slog.String("target", resolvedDir))
		return nil, true, nil
	}

	nextStack := cloneObservedDirStack(dirStack)
	nextStack[resolvedDir] = struct{}{}

	return nextStack, false, nil
}

func (o *LocalObserver) walkFollowedDirectory(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	events *[]synctypes.ChangeEvent,
	jobs *[]hashJob,
	skipped *[]synctypes.SkippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
) error {
	nextStack, skipChildren, err := o.nextObservedDirStack(
		dirStack,
		fsPath,
		dbRelPath,
		"skipping symlinked directory cycle",
	)
	if err != nil {
		o.Logger.Debug("skipping symlinked directory with unresolved target",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		return nil
	}

	if skipChildren {
		return nil
	}

	return o.walkObservedEntries(
		ctx,
		fsPath,
		dbRelPath,
		observed,
		events,
		jobs,
		skipped,
		scanStartNano,
		nextStack,
		"while following symlinked directory",
	)
}

func (o *LocalObserver) walkObservedDirectory(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	events *[]synctypes.ChangeEvent,
	jobs *[]hashJob,
	skipped *[]synctypes.SkippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
) error {
	nextStack, _, err := o.nextObservedDirStack(
		dirStack,
		fsPath,
		dbRelPath,
		"skipping observed directory cycle",
	)
	if err != nil {
		o.Logger.Warn("resolve observed directory failed",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		return nil
	}

	return o.walkObservedEntries(
		ctx,
		fsPath,
		dbRelPath,
		observed,
		events,
		jobs,
		skipped,
		scanStartNano,
		nextStack,
		"during observed walk",
	)
}

func (o *LocalObserver) walkObservedEntries(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	events *[]synctypes.ChangeEvent,
	jobs *[]hashJob,
	skipped *[]synctypes.SkippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
	stage string,
) error {
	entries, err := localpath.ReadDir(fsPath)
	if err != nil {
		o.Logger.Warn("read dir failed "+stage,
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		return nil
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return fmt.Errorf("walk observed entries %s: %w", dbRelPath, ctx.Err())
		}

		if err := o.walkObservedEntry(
			ctx,
			fsPath,
			dbRelPath,
			entry,
			observed,
			events,
			jobs,
			skipped,
			scanStartNano,
			dirStack,
			stage,
		); err != nil {
			return err
		}
	}

	return nil
}

func (o *LocalObserver) walkObservedEntry(
	ctx context.Context,
	parentFsPath string,
	parentRelPath string,
	entry os.DirEntry,
	observed map[string]bool,
	events *[]synctypes.ChangeEvent,
	jobs *[]hashJob,
	skipped *[]synctypes.SkippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
	stage string,
) error {
	entryName := nfcNormalize(entry.Name())
	entryRelPath := joinObservedPath(parentRelPath, entryName)
	entryFsPath := filepath.Join(parentFsPath, entry.Name())

	if entry.Type()&fs.ModeSymlink != 0 {
		if o.filterConfig.SkipSymlinks {
			o.Logger.Debug("skipping symlink",
				slog.String("path", entryRelPath))
			return nil
		}

		return o.processSymlinkPath(
			ctx,
			entryFsPath,
			entryRelPath,
			entryName,
			observed,
			events,
			jobs,
			skipped,
			scanStartNano,
			dirStack,
		)
	}

	kind := dirEntryKind(entry)
	if skipItem := shouldObserveWithFilter(entryName, entryRelPath, kind, o.filterConfig, o.observationRules); skipItem != nil {
		if skipItem.Reason != "" {
			*skipped = append(*skipped, *skipItem)
		}

		return nil
	}

	info, err := entry.Info()
	if err != nil {
		o.Logger.Warn("stat failed "+stage,
			slog.String("path", entryRelPath),
			slog.String("error", err.Error()))
		return nil
	}

	if err := o.processObservedInfo(
		entryFsPath,
		entryRelPath,
		entryName,
		info,
		kind,
		observed,
		events,
		jobs,
		skipped,
		scanStartNano,
	); err != nil {
		return err
	}

	if kind == observedKindDir {
		return o.walkObservedDirectory(
			ctx,
			entryFsPath,
			entryRelPath,
			observed,
			events,
			jobs,
			skipped,
			scanStartNano,
			dirStack,
		)
	}

	return nil
}

type watchSetupCounts struct {
	watched int
	failed  int
}

func (o *LocalObserver) addObservedDirWatches(
	ctx context.Context,
	watcher FsWatcher,
	fsPath string,
	dbRelPath string,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
) error {
	if ctx.Err() != nil {
		return fmt.Errorf("watch setup context: %w", ctx.Err())
	}

	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.Logger.Warn("stat failed during watch setup",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(dbRelPath)
		o.Logger.Debug("skipping symlink in watch setup",
			slog.String("path", fsPath))
		return nil
	}

	o.forgetExcludedSymlink(dbRelPath)

	if !info.IsDir() {
		return nil
	}

	if dbRelPath != "." {
		name := nfcNormalize(filepath.Base(fsPath))
		if shouldObserveWithFilter(name, dbRelPath, observedKindDir, o.filterConfig, o.observationRules) != nil {
			return nil
		}
	}

	if addErr := o.addObservedWatch(watcher, fsPath, counts); addErr != nil {
		return addErr
	}

	nextStack, skipChildren, err := o.nextObservedDirStack(
		dirStack,
		fsPath,
		dbRelPath,
		"skipping symlinked directory cycle in watch setup",
	)
	if err != nil {
		o.Logger.Warn("failed to resolve watched directory",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	if skipChildren {
		return nil
	}

	return o.addObservedDirChildrenWatches(ctx, watcher, fsPath, dbRelPath, counts, nextStack)
}

func (o *LocalObserver) addObservedWatch(
	watcher FsWatcher,
	fsPath string,
	counts *watchSetupCounts,
) error {
	addErr := watcher.Add(fsPath)
	if addErr == nil {
		counts.watched++
		return nil
	}

	if IsWatchLimitError(addErr) {
		o.Logger.Error("inotify watch limit exhausted",
			slog.String("path", fsPath),
			slog.Int("watches_added", counts.watched),
		)

		return synctypes.ErrWatchLimitExhausted
	}

	counts.failed++
	o.Logger.Warn("failed to add watch",
		slog.String("path", fsPath),
		slog.String("error", addErr.Error()))
	return nil
}

func (o *LocalObserver) addObservedDirChildrenWatches(
	ctx context.Context,
	watcher FsWatcher,
	fsPath string,
	dbRelPath string,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
) error {
	entries, err := localpath.ReadDir(fsPath)
	if err != nil {
		o.Logger.Warn("read dir failed during watch setup",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return fmt.Errorf("watch setup context: %w", ctx.Err())
		}

		if err := o.addObservedChildWatch(ctx, watcher, fsPath, dbRelPath, entry, counts, dirStack); err != nil {
			return err
		}
	}

	return nil
}

func (o *LocalObserver) addObservedChildWatch(
	ctx context.Context,
	watcher FsWatcher,
	parentFsPath string,
	parentRelPath string,
	entry os.DirEntry,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
) error {
	childFsPath := filepath.Join(parentFsPath, entry.Name())
	childName := nfcNormalize(entry.Name())
	childRelPath := joinObservedPath(parentRelPath, childName)

	if entry.Type()&fs.ModeSymlink != 0 {
		if o.filterConfig.SkipSymlinks {
			o.rememberExcludedSymlink(childRelPath)
			return nil
		}

		return o.addObservedDirWatches(ctx, watcher, childFsPath, childRelPath, counts, dirStack)
	}

	o.forgetExcludedSymlink(childRelPath)

	if !entry.IsDir() {
		return nil
	}

	if shouldObserveWithFilter(childName, childRelPath, observedKindDir, o.filterConfig, o.observationRules) != nil {
		return nil
	}

	return o.addObservedDirWatches(ctx, watcher, childFsPath, childRelPath, counts, dirStack)
}

func (o *LocalObserver) processSymlinkPath(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	name string,
	observed map[string]bool,
	events *[]synctypes.ChangeEvent,
	jobs *[]hashJob,
	skipped *[]synctypes.SkippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
) error {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.Logger.Debug("skipping symlink with unreadable target",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		return nil
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.Logger.Debug("skipping symlink",
			slog.String("path", dbRelPath))
		return nil
	}

	o.forgetExcludedSymlink(dbRelPath)

	kind := observedKindFromInfo(info)
	if skipItem := shouldObserveWithFilter(name, dbRelPath, kind, o.filterConfig, o.observationRules); skipItem != nil {
		if skipItem.Reason != "" {
			*skipped = append(*skipped, *skipItem)
		}

		return nil
	}

	if err := o.processObservedInfo(
		fsPath,
		dbRelPath,
		name,
		info,
		kind,
		observed,
		events,
		jobs,
		skipped,
		scanStartNano,
	); err != nil {
		return err
	}

	if kind != observedKindDir {
		return nil
	}

	if err := o.walkFollowedDirectory(
		ctx,
		fsPath,
		dbRelPath,
		observed,
		events,
		jobs,
		skipped,
		scanStartNano,
		dirStack,
	); err != nil {
		return fmt.Errorf("walk followed symlinked directory %s: %w", dbRelPath, err)
	}

	return nil
}
