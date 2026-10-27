package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
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

const (
	unreadableDirectoryDetail     = "directory not accessible (check filesystem permissions)"
	unreadableSymlinkTargetDetail = "symlink target is missing or unreadable (is its disk mounted?)"
)

// recordUnreadableObservedBoundary marks a path observation could not read as
// a local read boundary. Its baseline descendants are absent from the observed
// set, and without the boundary they would look deleted and be removed from
// OneDrive. With it, the planner treats local truth under the path as
// unavailable and leaves remote content alone (R-2.4.12). A path that no
// longer exists is a real deletion and gets no boundary.
func recordUnreadableObservedBoundary(skipped *[]skippedItem, dbRelPath string, err error, detail string) {
	if errors.Is(err, fs.ErrNotExist) && detail != unreadableSymlinkTargetDetail {
		return
	}

	*skipped = append(*skipped, skippedItem{
		Path:               dbRelPath,
		Reason:             issueLocalReadDenied,
		Detail:             detail,
		BlocksReadBoundary: true,
	})
}

func shouldSkipObservedSymlink(isSymlink bool, filter ContentFilterConfig) bool {
	return isSymlink && !newContentFilter(filter).ShouldFollowSymlinks()
}

func (o *localObserver) rememberExcludedSymlink(path string) {
	if path == "" || path == "." {
		return
	}

	if o.excludedSymlinkPaths == nil {
		o.excludedSymlinkPaths = make(map[string]struct{})
	}

	o.excludedSymlinkPaths[path] = struct{}{}
}

func (o *localObserver) forgetExcludedSymlink(path string) {
	if o.excludedSymlinkPaths == nil {
		return
	}

	delete(o.excludedSymlinkPaths, path)
}

func (o *localObserver) hasExcludedSymlinkAncestor(path string) bool {
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

func (o *localObserver) nextObservedDirStack(
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
		o.logger.Debug(cycleLog,
			slog.String("path", dbRelPath),
			slog.String("target", resolvedDir))
		return nil, true, nil
	}

	nextStack := cloneObservedDirStack(dirStack)
	nextStack[resolvedDir] = struct{}{}

	return nextStack, false, nil
}

func (o *localObserver) walkFollowedDirectory(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
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
		o.logger.Debug("skipping symlinked directory with unresolved target",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		recordUnreadableObservedBoundary(skipped, dbRelPath, err, unreadableSymlinkTargetDetail)
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
		currentRows,
		events,
		jobs,
		skipped,
		scanStartNano,
		nextStack,
		"while following symlinked directory",
	)
}

func (o *localObserver) walkObservedDirectory(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
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
		o.logger.Debug("resolve observed directory failed",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		recordUnreadableObservedBoundary(skipped, dbRelPath, err, unreadableDirectoryDetail)
		return nil
	}

	return o.walkObservedEntries(
		ctx,
		fsPath,
		dbRelPath,
		observed,
		currentRows,
		events,
		jobs,
		skipped,
		scanStartNano,
		nextStack,
		"during observed walk",
	)
}

func (o *localObserver) walkObservedEntries(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
	stage string,
) error {
	entries, err := localpath.ReadDir(fsPath)
	if err != nil {
		o.logger.Debug("read dir failed "+stage,
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		recordUnreadableObservedBoundary(skipped, dbRelPath, err, unreadableDirectoryDetail)
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
			currentRows,
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

func (o *localObserver) walkObservedEntry(
	ctx context.Context,
	parentFsPath string,
	parentRelPath string,
	entry os.DirEntry,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
	stage string,
) error {
	entryName := nfcNormalize(entry.Name())
	entryRelPath := joinObservedPath(parentRelPath, entryName)
	entryFsPath := filepath.Join(parentFsPath, entry.Name())

	if entry.Type()&fs.ModeSymlink != 0 {
		if !newContentFilter(o.filterConfig).ShouldFollowSymlinks() {
			o.logger.Debug("skipping symlink",
				slog.String("path", entryRelPath))
			return nil
		}

		return o.processSymlinkPath(
			ctx,
			entryFsPath,
			entryRelPath,
			entryName,
			observed,
			currentRows,
			events,
			jobs,
			skipped,
			scanStartNano,
			dirStack,
		)
	}

	kind := dirEntryKind(entry)
	if skipItem := shouldObserveWithFilter(
		entryName,
		entryRelPath,
		kind,
		o.filterConfig,
		o.protectedRoots,
		o.observationRules,
	); skipItem != nil {
		if skipItem.Reason != "" {
			*skipped = append(*skipped, *skipItem)
		}

		return nil
	}

	info, err := entry.Info()
	if err != nil {
		o.logger.Debug("stat failed "+stage,
			slog.String("path", entryRelPath),
			slog.String("error", err.Error()))
		recordUnreadableObservedBoundary(skipped, entryRelPath, err, unreadableDirectoryDetail)
		return nil
	}

	if err := o.processObservedInfo(
		entryFsPath,
		entryRelPath,
		entryName,
		info,
		kind,
		observed,
		currentRows,
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
			currentRows,
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
	// entries counts the directory entries seen under watched directories.
	// The kqueue backend opens one descriptor for every entry of a watched
	// directory, files and subdirectories alike (a watched subdirectory's own
	// watch reuses its entry's descriptor), so entries plus the root's own
	// descriptor projects what watching will hold.
	entries int
}

type watchAddSession struct {
	added []string
}

func newWatchAddSession() *watchAddSession {
	return &watchAddSession{}
}

func (s *watchAddSession) record(path string) {
	s.added = append(s.added, path)
}

func (o *localObserver) addObservedDirWatches(
	ctx context.Context,
	watcher fsWatcher,
	fsPath string,
	dbRelPath string,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
	session *watchAddSession,
) error {
	if ctx.Err() != nil {
		return fmt.Errorf("watch setup context: %w", ctx.Err())
	}

	// Per-directory watch setup failures are Debug: unreadable folders are
	// already recorded by the scanner as skipped items, and the setup
	// summary counts failures.
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.logger.Debug("stat failed during watch setup",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(dbRelPath)
		o.logger.Debug("skipping symlink in watch setup",
			slog.String("path", fsPath))
		return nil
	}

	o.forgetExcludedSymlink(dbRelPath)

	if !info.IsDir() {
		return nil
	}

	if dbRelPath != "." {
		name := nfcNormalize(filepath.Base(fsPath))
		if shouldObserveWithFilter(name, dbRelPath, observedKindDir, o.filterConfig, o.protectedRoots, o.observationRules) != nil {
			return nil
		}
	}

	if addErr := o.addObservedWatch(watcher, fsPath, info, counts, session); addErr != nil {
		return addErr
	}

	nextStack, skipChildren, err := o.nextObservedDirStack(
		dirStack,
		fsPath,
		dbRelPath,
		"skipping symlinked directory cycle in watch setup",
	)
	if err != nil {
		o.logger.Debug("failed to resolve watched directory",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	if skipChildren {
		return nil
	}

	return o.addObservedDirChildrenWatches(ctx, watcher, fsPath, dbRelPath, counts, nextStack, session)
}

func (o *localObserver) addObservedWatch(
	watcher fsWatcher,
	fsPath string,
	info os.FileInfo,
	counts *watchSetupCounts,
	session *watchAddSession,
) error {
	cleanPath := filepath.Clean(fsPath)
	alreadyWatched := o.watchedDirs.has(cleanPath)

	addErr := watcher.Add(fsPath)
	if addErr == nil {
		counts.watched++
		o.watchedDirs.add(cleanPath, info)
		if session != nil && !alreadyWatched {
			session.record(cleanPath)
		}
		return nil
	}

	if cause, ok := classifyWatchAddError(addErr); ok {
		// Debug: the caller falls back to periodic scans and reports the
		// limit once with the fix.
		o.logger.Debug("watch resource limit reached during setup",
			slog.String("path", fsPath),
			slog.Int("watches_added", counts.watched),
			slog.String("error", addErr.Error()),
		)

		return &localWatchUnavailableError{cause: cause, err: addErr}
	}

	counts.failed++
	o.logger.Debug("failed to add watch",
		slog.String("path", fsPath),
		slog.String("error", addErr.Error()))
	return nil
}

func (o *localObserver) addObservedDirChildrenWatches(
	ctx context.Context,
	watcher fsWatcher,
	fsPath string,
	dbRelPath string,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
	session *watchAddSession,
) error {
	entries, err := localpath.ReadDir(fsPath)
	if err != nil {
		o.logger.Debug("read dir failed during watch setup",
			slog.String("path", fsPath),
			slog.String("error", err.Error()))
		return nil
	}

	counts.entries += len(entries)
	if err := o.checkWatchDescriptorBudget(counts); err != nil {
		return err
	}

	for _, entry := range entries {
		if ctx.Err() != nil {
			return fmt.Errorf("watch setup context: %w", ctx.Err())
		}

		if err := o.addObservedChildWatch(ctx, watcher, fsPath, dbRelPath, entry, counts, dirStack, session); err != nil {
			return err
		}
	}

	return nil
}

// checkWatchDescriptorBudget stops watch setup before kqueue would hold more
// descriptors than the budget (D-079). kqueue keeps one open descriptor per
// watched directory and per file inside one, so watching a large tree used
// to exhaust the process's open-file limit; hashing, uploads and SQLite then
// failed with EMFILE while parts of the tree silently went unwatched. Over
// budget, the runtime falls back to periodic full scans instead. A budget of
// zero means the backend holds no per-watch descriptors (inotify).
func (o *localObserver) checkWatchDescriptorBudget(counts *watchSetupCounts) error {
	if o.descriptorBudget <= 0 {
		return nil
	}

	projected := counts.entries + 1 // +1: the sync root's own watch
	if projected <= o.descriptorBudget {
		return nil
	}

	return &localWatchUnavailableError{
		cause: watchCauseDescriptorBudget,
		err: fmt.Errorf("watching needs more than %d open files; the budget is %d (half the open-file limit)",
			projected, o.descriptorBudget),
	}
}

func (o *localObserver) addObservedChildWatch(
	ctx context.Context,
	watcher fsWatcher,
	parentFsPath string,
	parentRelPath string,
	entry os.DirEntry,
	counts *watchSetupCounts,
	dirStack map[string]struct{},
	session *watchAddSession,
) error {
	childFsPath := filepath.Join(parentFsPath, entry.Name())
	childName := nfcNormalize(entry.Name())
	childRelPath := joinObservedPath(parentRelPath, childName)

	if entry.Type()&fs.ModeSymlink != 0 {
		if !newContentFilter(o.filterConfig).ShouldFollowSymlinks() {
			o.rememberExcludedSymlink(childRelPath)
			return nil
		}

		return o.addObservedDirWatches(ctx, watcher, childFsPath, childRelPath, counts, dirStack, session)
	}

	o.forgetExcludedSymlink(childRelPath)

	if !entry.IsDir() {
		return nil
	}

	if shouldObserveWithFilter(childName, childRelPath, observedKindDir, o.filterConfig, o.protectedRoots, o.observationRules) != nil {
		return nil
	}

	return o.addObservedDirWatches(ctx, watcher, childFsPath, childRelPath, counts, dirStack, session)
}

func (o *localObserver) processSymlinkPath(
	ctx context.Context,
	fsPath string,
	dbRelPath string,
	name string,
	observed map[string]bool,
	currentRows map[string]localStateRow,
	events *[]changeEvent,
	jobs *[]hashJob,
	skipped *[]skippedItem,
	scanStartNano int64,
	dirStack map[string]struct{},
) error {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.logger.Debug("skipping symlink with unreadable target",
			slog.String("path", dbRelPath),
			slog.String("error", err.Error()))
		if isSymlink {
			recordUnreadableObservedBoundary(skipped, dbRelPath, err, unreadableSymlinkTargetDetail)
		} else {
			recordUnreadableObservedBoundary(skipped, dbRelPath, err, unreadableDirectoryDetail)
		}
		return nil
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.logger.Debug("skipping symlink",
			slog.String("path", dbRelPath))
		return nil
	}

	o.forgetExcludedSymlink(dbRelPath)

	kind := observedKindFromInfo(info)
	if skipItem := shouldObserveWithFilter(name, dbRelPath, kind, o.filterConfig, o.protectedRoots, o.observationRules); skipItem != nil {
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
		currentRows,
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
		currentRows,
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
