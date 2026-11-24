// The local observer's watch-mode event handlers.
//
// Contents:
//   - watchLoop:     main select loop (fsnotify events, errors, safety scan, ctx)
//   - HandleFsEvent: fsnotify event → handler routing
//   - handleCreate:  new file/dir → hash + row patch + recursive watch add
//   - HandleDelete:  file/dir removal → subtree removal patch + remove watches
//
// Related files:
//   - observer_local.go:            LocalObserver struct, constructor, Watch() entry point
//   - observer_local_new_dir.go:    a created directory's registration and contents scan
//   - observer_local_write_hash.go: handleWrite's debounced hash and HashAndEmit
//   - scanner.go:                   FullScan, walk/filter logic (hashing in scanner_hash.go)

package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// watchLoop is the main select loop for Watch(). It processes fsnotify events,
// watcher errors, safety scan ticks, and context cancellation.
//
// Go's select statement picks a random ready case when multiple are ready
// simultaneously. This means watcher events and safety scan ticks have no
// guaranteed priority ordering. This is intentional — the safety scan
// (every 5 minutes) provides eventual consistency for any events missed
// or dropped by fsnotify, regardless of select scheduling order.
func (o *LocalObserver) watchLoop(
	ctx context.Context, watcher FsWatcher, tree *synctree.Root,
) error {
	syncRoot := tree.Path()
	interval := o.EffectiveSafetyScanInterval()

	ticker := o.clock.NewTicker(interval)
	defer syncdomain.StopTicker(ticker)
	tickCh := syncdomain.TickerChan(ticker)

	errBackoff := retry.NewBackoff(retry.WatchLocalPolicy(), o.clock)

	for {
		select {
		case <-ctx.Done():
			return nil

		case fsEvent, ok := <-watcher.Events():
			if !ok {
				return nil
			}

			o.HandleFsEvent(ctx, fsEvent, watcher, tree)
			if o.watchUnavailable != nil {
				return o.watchUnavailable
			}

			// Successful event resets error backoff.
			errBackoff.Reset()

		case req := <-o.HashRequests:
			// Deferred hash from write coalesce timer (B-107).
			o.HashAndEmit(ctx, tree, req)

		case <-o.recoveryScanRequests:
			o.armRecoveryScan()

		case gen := <-o.recoveryScanDue:
			if err := o.runDueRecoveryScan(ctx, tree, gen); err != nil {
				return err
			}

		case watchErr, ok := <-watcher.Errors():
			if !ok {
				return nil
			}

			if stop, err := o.handleWatcherError(ctx, watchErr, errBackoff, syncRoot); stop {
				return err
			}

		case <-tickCh:
			if _, err := o.rescanUnlessRootGone(ctx, tree); err != nil {
				return err
			}
			errBackoff.Reset()
		}
	}
}

// maxRewatchPerScan bounds how many missing watches one full scan re-adds, so
// a scan after a large unwatched tree appeared does not stall the watch loop
// on thousands of Add calls; the rest are added by later scans.
const maxRewatchPerScan = 256

// rewatchUnwatchedDirectories adds watches for directories a full scan found
// that have none. Event-driven registration misses directories created while
// events were lost (a queue overflow) and directories whose Add failed
// (ENOSPC, ENOMEM, EACCES); before this they stayed unwatched for the rest of
// the session, every change in them waiting for the next safety scan. A
// failed Add is not remembered: the next full scan tries again. A resource
// limit ends the pass early.
func (o *LocalObserver) rewatchUnwatchedDirectories(tree *synctree.Root, rows []syncdomain.LocalStateRow) {
	if o.activeWatcher == nil || tree == nil {
		return
	}

	dirs := []string{"."}
	for i := range rows {
		if rows[i].ItemType == syncdomain.ItemTypeFolder {
			dirs = append(dirs, rows[i].Path)
		}
	}
	// Parents before children: a child's events only arrive once its
	// parent is watched.
	slices.SortFunc(dirs, func(a, b string) int {
		if depth := strings.Count(a, "/") - strings.Count(b, "/"); depth != 0 {
			return depth
		}
		return strings.Compare(a, b)
	})

	added := 0
	for _, rel := range dirs {
		if added >= maxRewatchPerScan {
			return
		}
		// Rows carry the stored NFC path; the watch is registered on the
		// spelling on disk, as the walk registered it (R-2.13.2).
		fsPath := tree.Path()
		if rel != "." {
			fsPath = filepath.Join(fsPath, tree.Spelling(filepath.FromSlash(rel)))
		}
		if o.watchedDirs.has(fsPath) {
			continue
		}

		if addErr := o.activeWatcher.Add(fsPath); addErr != nil {
			o.reportWatchAddFailure("failed to re-add watch after full scan", rel, addErr)
			if _, isLimit := classifyWatchAddError(addErr); isLimit {
				return
			}
			continue
		}
		info, _, statErr := statObservedPath(fsPath)
		if statErr != nil {
			info = nil
		}
		o.watchedDirs.add(fsPath, info)
		added++
	}
}

// runDueRecoveryScan runs the recovery scan a fired timer asked for, unless
// the timer's generation is stale.
func (o *LocalObserver) runDueRecoveryScan(
	ctx context.Context, tree *synctree.Root, gen uint64,
) error {
	if !o.takeRecoveryScanDue(gen) {
		return nil
	}

	o.logger.Debug("running recovery scan after missed local changes")
	scanned, err := o.rescanUnlessRootGone(ctx, tree)
	if err != nil {
		return err
	}
	if scanned {
		// A deferred scan is not a recovery scan that ended: it must not
		// feed the storm pacing, which doubles the next delay.
		o.finishRecoveryScan()
	}

	return nil
}

// rescanUnlessRootGone runs a full rescan, or reports the sync root gone
// (B-113): a deleted root means the watcher is watching nothing. It reports
// whether a scan ran.
func (o *LocalObserver) rescanUnlessRootGone(ctx context.Context, tree *synctree.Root) (bool, error) {
	if !SyncRootExists(tree.Path()) {
		// Debug: the returned error is reported once by the watch runtime.
		o.logger.Debug("sync root deleted, stopping watch",
			slog.String("sync_root", tree.Path()))

		return false, fmt.Errorf("%w: %w", syncdomain.ErrMountRootUnavailable, errSyncRootDeleted)
	}

	scanned := o.runSafetyScan(ctx, tree)
	if o.watchUnavailable != nil {
		return scanned, o.watchUnavailable
	}

	return scanned, nil
}

// handleWatcherError marks local truth suspect, arms a recovery rescan and
// backs off. It reports stop=true when the watch loop must return err (nil
// on shutdown).
func (o *LocalObserver) handleWatcherError(
	ctx context.Context, watchErr error, errBackoff *retry.Backoff, syncRoot string,
) (bool, error) {
	// Self-healing: local truth is marked for a rescan. The first error of a
	// streak is Info; repeats under sustained kernel queue overflow are Debug.
	delay := errBackoff.Next()
	watcherErrLevel := slog.LevelDebug
	if errBackoff.Consecutive() <= 1 {
		watcherErrLevel = slog.LevelInfo
	}
	msg := "filesystem watcher error; rescanning the sync folder"
	if errors.Is(watchErr, fsnotify.ErrEventOverflow) {
		// IN_Q_OVERFLOW: the kernel discarded events, so changes are known
		// to be missing, not merely possible.
		msg = "too many local changes at once for the system to report; rescanning the sync folder"
	}
	o.logger.Log(ctx, watcherErrLevel, msg,
		slog.Any("error", watchErr),
		slog.Duration("backoff", delay),
	)
	o.TrySendLocalTruthSuspect(ctx, syncdomain.LocalTruthRecoveryWatcherError)
	o.armRecoveryScan()

	// Exponential backoff prevents a tight loop under sustained errors
	// (e.g., kernel buffer overflow).
	if sleepErr := o.clock.Sleep(ctx, delay); sleepErr != nil {
		// The sleep ends early only on cancellation, which is shutdown, a
		// clean stop rather than a watch failure.
		return true, nil //nolint:nilerr // cancellation is a clean stop
	}

	// After a watcher error, check that the sync root still exists (B-113).
	if !SyncRootExists(syncRoot) {
		o.logger.Debug("sync root deleted, stopping watch",
			slog.String("sync_root", syncRoot))

		return true, fmt.Errorf("%w: %w", syncdomain.ErrMountRootUnavailable, errSyncRootDeleted)
	}

	return false, nil
}

// HandleFsEvent processes a single fsnotify event and hands it to the handler
// for its operation, which sends what it observes as a patch (TrySend).
func (o *LocalObserver) HandleFsEvent(
	ctx context.Context, fsEvent fsnotify.Event, watcher FsWatcher,
	tree *synctree.Root,
) {
	// Ignore chmod events — mode changes are not synced.
	if fsEvent.Has(fsnotify.Chmod) && !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Write) {
		return
	}

	relPath, err := tree.Rel(fsEvent.Name)
	if err != nil {
		o.logger.Debug("failed to compute relative path",
			slog.String("path", fsEvent.Name), slog.Any("error", err))

		return
	}
	if relPath == "." {
		o.logger.Debug("watch: ignoring sync root lifecycle event",
			slog.String("path", fsEvent.Name))
		return
	}

	dbRelPath := syncdomain.StoredPathOf(relPath)
	name := namekey.Stored(filepath.Base(fsEvent.Name))

	if reservation, ok := syncshortcut.ProtectedRootPathReservation(dbRelPath, o.protectedRoots); ok {
		if reservation.Path == dbRelPath {
			o.reportProtectedRootEvent(syncshortcut.ProtectedRootEvent{
				Type:         syncshortcut.ProtectedRootEventPathReserved,
				Path:         dbRelPath,
				ReservedPath: reservation.Path,
				MountID:      reservation.MountID,
				BindingID:    reservation.BindingID,
			})
		}
		return
	}

	// Unified observation filter (Stage 1: name + path length).
	// Watch handlers don't collect SkippedItems — the safety scan (FullScan
	// every 5 min) catches them for observation-issue persistence.
	if skip := shouldObserveWithFilter(
		name,
		dbRelPath,
		syncdomain.ObservedKindUnknown,
		o.filterConfig,
		o.protectedRoots,
		o.observationRules,
	); skip != nil {
		if skip.Reason != "" {
			o.logger.Debug("watch: skipping file",
				slog.String("path", dbRelPath),
				slog.String("reason", string(skip.Reason)))
		}

		return
	}

	switch {
	case fsEvent.Has(fsnotify.Create):
		o.handleCreate(ctx, tree, fsEvent.Name, dbRelPath, name, watcher)

	case fsEvent.Has(fsnotify.Write):
		o.handleWrite(tree, fsEvent.Name, dbRelPath)

	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		// Pass the original filesystem path (fsEvent.Name) rather than
		// reconstructing from syncRoot + dbRelPath. The NFC-normalized
		// dbRelPath may differ from the filesystem's encoding (NFD on
		// macOS HFS+), causing watcher.Remove() to silently fail.
		o.HandleDelete(ctx, watcher, tree, fsEvent.Name, dbRelPath, name)
	}
}

// handleCreate processes a Create event: stat, hash (files), add watch (dirs).
func (o *LocalObserver) handleCreate(
	ctx context.Context, tree *synctree.Root,
	fsPath, dbRelPath, name string,
	watcher FsWatcher,
) {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		// File may have been removed immediately after creation.
		o.logger.Debug("stat failed for created path",
			slog.String("path", dbRelPath), slog.Any("error", err))

		return
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(dbRelPath)
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info)
		return
	}

	o.forgetExcludedSymlink(dbRelPath)

	if isSpecialFile(info) {
		// FIFOs, sockets and devices are never synced (see processObservedInfo).
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info)
		return
	}

	if protectedRoot, ok := syncshortcut.ProtectedRootIdentityReservation(dbRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(syncshortcut.ProtectedRootEvent{
			Type:         syncshortcut.ProtectedRootEventIdentityMatch,
			Path:         dbRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		return
	}

	if skip := shouldObserveWithFilter(name, dbRelPath, infoKind(info), o.filterConfig, o.protectedRoots, o.observationRules); skip != nil {
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info)
		return
	}

	ev := syncdomain.LocalEntry{
		Path:  dbRelPath,
		Size:  info.Size(),
		Mtime: info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		ev.LocalDevice = identity.Device
		ev.LocalInode = identity.Inode
		ev.LocalBirthTime = identity.BirthTime
		ev.LocalHasIdentity = true
	}
	ev.LocalCtime = syncdomain.FileChangeTime(info)

	if info.IsDir() {
		o.handleDirectoryCreate(ctx, tree, fsPath, dbRelPath, info, &ev, watcher)
		return
	}

	// Stage 2 observation filter: file size check (requires stat).
	if o.IsOversizedFile(info.Size(), dbRelPath) {
		return
	}

	ev.ItemType = syncdomain.ItemTypeFile
	ev.Hash = o.watchCreateHash(ctx, tree, fsPath, dbRelPath, info)

	o.TrySend(ctx, &ev)
}

// watchCreateHash returns the content hash for a file a watch create (or a
// new-directory scan) observed, without letting a long read hold back the
// row of a moved file.
//
// A rename is observed as the delete of the old path, sent at once, and a
// create of the new path. Hashing the new path synchronously meant a large
// file's row reached the runtime only after the read finished; once that
// took longer than the replan debounce, the replan saw only the delete half
// and planned a remote delete instead of a move, losing the item's identity,
// versions and sharing (D-080). So when the file's filesystem identity
// matches a synced item at another path:
//   - unchanged size and mtime mean the same content: the synced hash is
//     reused and no read happens at all (a renamed tree is not re-read);
//   - otherwise the row goes out now with an unknown hash, which the move
//     pairing accepts on equal mtime, and the hash follows as a deferred
//     hash request.
//
// Every other create is hashed synchronously, as before: an unknown hash
// there could only make a replan plan an upload or a conflict copy early.
func (o *LocalObserver) watchCreateHash(
	ctx context.Context, tree *synctree.Root, fsPath, dbRelPath string, info os.FileInfo,
) string {
	if identity, ok := synctree.IdentityFromFileInfo(info); ok && !hasOtherHardLinks(info) {
		entry := o.watchIdentityIndex().baselineEntryWithIdentity(o.baseline, identity)
		// An identity match is only a move when the synced path is gone.
		// If it still exists the inode was freed and reused (the synced
		// file was replaced and the index is stale): the new file is
		// unrelated content and is hashed like any create.
		if entry != nil && entry.Path != dbRelPath && entry.ItemType == syncdomain.ItemTypeFile &&
			syncedPathGone(tree, entry.Path) {
			if CanReuseBaselineHash(info, entry, o.clock.Now().UnixNano()) {
				return entry.LocalHash
			}

			o.writeChurnFor(dbRelPath, o.clock.Now()).firstPendingAt = o.clock.Now()
			o.scheduleDeferredHash(fsPath, dbRelPath, 0, o.writeCooldown())
			return ""
		}
	}

	return o.stableHashOrEmpty(ctx, fsPath, dbRelPath)
}

// syncedPathGone reports whether the synced path is confirmed absent now,
// under any spelling on disk of the stored path (R-2.13.2). Any other answer
// (present, or unreadable) is not proof of a move.
func syncedPathGone(tree *synctree.Root, dbRelPath string) bool {
	if tree == nil {
		return false
	}
	_, err := tree.Lstat(tree.Spelling(dbRelPath))

	return errors.Is(err, fs.ErrNotExist)
}

// stableHashOrEmpty computes a stable hash for a file, returning an empty
// string on any failure. Extracted to deduplicate identical hash-failure
// handling in handleCreate and scanNewDirectory. Both callers emit events
// with empty hashes on failure because Create events and directory scans
// have no guaranteed follow-up event (B-203).
func (o *LocalObserver) stableHashOrEmpty(ctx context.Context, fsPath, dbRelPath string) string {
	hash, err := computeStableHashWith(ctx, fsPath, o.hooks.HashFile)
	if err != nil {
		if errors.Is(err, errFileChangedDuringHash) {
			o.logger.Debug("file metadata still settling, emitting with empty hash",
				slog.String("path", dbRelPath))
		} else {
			// Debug: an unreadable file fails its upload, which the
			// console reports; this would repeat on every write.
			o.logger.Debug("hash failed, emitting event with empty hash",
				slog.String("path", dbRelPath), slog.Any("error", err))
		}

		return ""
	}

	return hash
}

// HandleDelete processes a Remove/Rename event. For directories, also removes
// the fsnotify watch to prevent resource leaks (macOS/kqueue doesn't auto-clean).
//
// fsPath is the original filesystem path from fsEvent.Name — NOT reconstructed
// from syncRoot + dbRelPath. On macOS HFS+, fsnotify delivers NFD-encoded
// paths while dbRelPath is NFC-normalized. Using the original fsPath for
// watcher.Remove() ensures the removal matches the path registered by fsnotify.
func (o *LocalObserver) HandleDelete(
	ctx context.Context, watcher FsWatcher, tree *synctree.Root, fsPath, dbRelPath, name string,
) {
	// A Remove or Rename event names a path, not an inode, and arrives late.
	// A lone MOVED_FROM, an atomic save (rename over the path) or an event
	// attributed to the wrong name would otherwise become a delete of a file
	// that is still there, and then a delete on OneDrive (D-011). The path
	// is only deleted when it is actually gone; a replacement arrives as its
	// own create or write event.
	if localPathStillPresent(tree, dbRelPath, fsPath) {
		o.logger.Debug("ignoring delete event for a path that still exists",
			slog.String("path", dbRelPath))
		return
	}

	// Clean up write coalesce timer for deleted path (B-107).
	o.cancelPendingTimer(dbRelPath)

	if o.hasExcludedSymlinkAncestor(dbRelPath) {
		if _, ok := o.baseline.GetByPath(dbRelPath); !ok {
			o.forgetExcludedSymlink(dbRelPath)
		}

		return
	}

	// Remove the watches of a deleted directory to prevent resource leaks
	// (B-112): the watch set says what was a directory. Every watch below
	// the path goes, even when the directory's own Add failed, and then the
	// directory's own. Linux inotify auto-cleans, but macOS kqueue may not.
	// Uses fsPath directly instead of reconstructing from syncRoot +
	// dbRelPath to avoid NFC/NFD mismatch on macOS HFS+ (B-312).
	if o.watchedDirs.has(fsPath) {
		o.forgetWatchedSubtree(watcher, fsPath, dbRelPath)
	} else {
		o.forgetWatchedDescendants(watcher, fsPath, dbRelPath)
	}

	o.TrySend(ctx, &syncdomain.LocalEntry{Path: dbRelPath, IsDeleted: true})
}

// rescanIfSkippedEntryHidesSyncedItem runs a safety scan now when a created
// entry sync skips is a synced item in disguise: it occupies a synced path
// (a symlink or special file replacing it), or it is a synced item renamed to
// a name sync skips (recognized by filesystem identity). The rename's delete
// half has already reached the runtime; the scan records the synced path as
// unavailable (scanner_unavailable.go) within the same debounce window, so
// the replan sees an unavailable path instead of a deleted one (D-004, R-6.2.14).
func (o *LocalObserver) rescanIfSkippedEntryHidesSyncedItem(
	ctx context.Context,
	tree *synctree.Root,
	dbRelPath string,
	info os.FileInfo,
) {
	hides := false
	if _, synced := o.baseline.GetByPath(dbRelPath); synced {
		hides = true
	} else if identity, ok := synctree.IdentityFromFileInfo(info); ok && !hasOtherHardLinks(info) {
		if entry := o.watchIdentityIndex().baselineEntryWithIdentity(o.baseline, identity); entry != nil && entry.Path != dbRelPath {
			hides = true
		}
	}
	if !hides || tree == nil {
		return
	}

	o.logger.Debug("a synced item became something sync skips; rescanning",
		slog.String("path", dbRelPath))
	o.runSafetyScan(ctx, tree)
}

// watchIdentityIndexMaxAge bounds how stale the watch handler's
// identity-to-baseline index may be. Skipped creates are frequent (editor
// swap files, .DS_Store), so the index is rebuilt at most this often rather
// than per event; an item synced within the window and then renamed out of
// reach is caught by the next safety scan instead.
const watchIdentityIndexMaxAge = time.Minute

func (o *LocalObserver) watchIdentityIndex() *scanAvailability {
	now := o.clock.Now()
	if o.watchIdentityLookup == nil || now.Sub(o.watchIdentityBuiltAt) > watchIdentityIndexMaxAge {
		o.watchIdentityLookup = newScanAvailability(0)
		o.watchIdentityBuiltAt = now
	}
	return o.watchIdentityLookup
}

// localPathStillPresent reports whether the stored path of the entry at
// fsPath still names an entry: the exact name, or another spelling of it in
// Unicode form. On a case-insensitive volume Lstat also finds a
// differently-cased sibling, which is the other half of a case-only rename,
// so the parent listing decides.
//
// The event's own path is the one looked up, not the stored NFC dbRelPath:
// on a byte-exact filesystem an NFD-named file is absent under its NFC
// spelling, and an atomic save of it read as a delete (R-2.13.2). A removed
// Unicode-form twin leaves the other spelling, which still holds the stored
// path's row, so its delete is no delete of that path.
func localPathStillPresent(tree *synctree.Root, dbRelPath, fsPath string) bool {
	if tree == nil {
		return false
	}
	rel, err := tree.Rel(fsPath)
	if err != nil {
		rel = tree.Spelling(dbRelPath)
	}
	if _, statErr := tree.Lstat(rel); statErr != nil && isASCIIName(filepath.Base(fsPath)) {
		// An ASCII name has no other Unicode form a twin could carry, apart
		// from canonical singletons such as KELVIN SIGN for K, which this
		// accepts missing: a missing ASCII name is gone, and a bulk delete
		// lists no folder per removed file.
		return false
	}
	entries, err := tree.ReadDir(filepath.Dir(rel))
	if err != nil {
		return false
	}
	// Present means an entry of that stored form is there: one, or a
	// collision of two.
	match := namekey.FindStored(entries, namekey.Stored(filepath.Base(fsPath)), func(entry *os.DirEntry) string {
		return (*entry).Name()
	})

	return match.Found() || match.Ambiguous
}

// isASCIIName reports whether name holds only ASCII characters.
func isASCIIName(name string) bool {
	for i := 0; i < len(name); i++ {
		if name[i] >= utf8.RuneSelf {
			return false
		}
	}

	return true
}

// forgetWatchedSubtree removes the watch on a deleted or renamed-away
// directory and on every watched directory beneath it, deepest first.
//
// Removing only the directory's own watch was wrong after a rename (D-077):
// inotify watches follow the inode, and fsnotify keys its registrations by
// path, so a descendant watch kept its old path. When the renamed tree was
// re-added under its new name, fsnotify's register found the existing
// descriptor and returned the old registration, and every later event under
// the moved tree carried the old path (and a tree moved out of the sync root
// kept reporting changes as if it were still inside). Dropping the whole
// subtree makes the re-add register fresh paths.
func (o *LocalObserver) forgetWatchedSubtree(watcher FsWatcher, fsPath, dbRelPath string) {
	o.forgetWatchedDescendants(watcher, fsPath, dbRelPath)

	// The directory itself is removed by its original event path, not the
	// cleaned map key: on macOS HFS+ the NFD spelling must match what
	// fsnotify registered (B-312). Linux inotify auto-cleans a deleted
	// directory, but macOS kqueue may not (B-112).
	o.removeWatch(watcher, fsPath, dbRelPath)
}

// forgetWatchedDescendants removes every watch strictly beneath fsPath,
// deepest first.
func (o *LocalObserver) forgetWatchedDescendants(watcher FsWatcher, fsPath, dbRelPath string) {
	for _, watched := range o.watchedDirs.descendants(fsPath) {
		o.removeWatch(watcher, watched, dbRelPath)
	}
}

// isRegisteredSameDirectory reports whether fsPath is already watched as the
// same directory (same device and inode) that info describes.
func (o *LocalObserver) isRegisteredSameDirectory(fsPath string, info os.FileInfo) bool {
	registered, ok := o.watchedDirs.get(fsPath)
	if !ok || !registered.hasIdentity {
		return false
	}
	identity, ok := synctree.IdentityFromFileInfo(info)

	return ok && synctree.SameIdentity(identity, registered.identity)
}

func (o *LocalObserver) removeWatch(watcher FsWatcher, fsPath, dbRelPath string) {
	if rmErr := watcher.Remove(fsPath); rmErr != nil {
		// Debug: the kernel often removed the watch already (benign).
		o.logger.Debug("watch removal for deleted directory",
			slog.String("path", dbRelPath),
			slog.String("watch", fsPath),
			slog.Any("error", rmErr),
		)
	}

	o.watchedDirs.remove(fsPath)
}

// reportWatchAddFailure reports a directory the watcher could not add. Once
// a resource limit is reached every new directory fails the same way, so each
// cause is warned once with its fix; other failures are per-directory Debug
// lines (the safety scan still covers those folders). A descriptor-limit
// failure also records watchUnavailable so the watch loop stops watching.
func (o *LocalObserver) reportWatchAddFailure(msg, dbRelPath string, addErr error) {
	cause, ok := classifyWatchAddError(addErr)
	if !ok {
		o.logger.Debug(msg, slog.String("path", dbRelPath), slog.Any("error", addErr))
		return
	}

	if watchCauseStopsWatching(cause) {
		if o.watchUnavailable == nil {
			o.watchUnavailable = &localWatchUnavailableError{cause: cause, err: addErr}
		}

		return
	}

	if _, reported := o.reportedWatchCauses[cause]; reported {
		return
	}
	if o.reportedWatchCauses == nil {
		o.reportedWatchCauses = make(map[WatchUnavailableCause]struct{})
	}
	o.reportedWatchCauses[cause] = struct{}{}
	o.logger.Warn("Cannot watch new folders for changes ("+WatchUnavailableReason(cause)+"); "+
		"changes in new folders will be noticed only by the periodic rescan. "+
		"To fix: "+WatchUnavailableFix(cause),
		slog.String("path", dbRelPath),
	)
}

// runSafetyScan performs a full filesystem scan as a safety net for events
// fsnotify may have missed, and sends it to the runtime as one snapshot
// (TrySendFullLocalSnapshot): its rows, and its finding set, from which the
// engine records new findings and clears the ones this scan no longer
// reports. It reports whether the scan ran.
//
// While the runtime has not taken the observations already sent (its loop
// is inside a long replan), the scan's snapshot would only be dropped, and
// the drop would ask for yet another recovery scan: a runtime blocked for
// ten minutes cost six full scans of 11,000 entries that delivered nothing
// (R-2.8.21). So the scan is deferred instead: one recovery request keeps a
// paced timer armed until the channel has room, and the first scan after
// that covers everything the deferred ones would have.
func (o *LocalObserver) runSafetyScan(ctx context.Context, tree *synctree.Root) bool {
	if o.localBatchChannelFull() {
		o.logger.Debug("deferring full scan: the sync runtime has not taken the previous observations yet")
		o.requestRecoveryScan()
		return false
	}

	o.logger.Debug("running safety scan")

	// This scan reads the whole tree, so it covers anything a pending
	// recovery scan was armed for. Requests made after this point (a batch
	// this scan fails to deliver) re-arm it.
	o.cancelRecoveryScan()

	start := o.clock.Now()

	result, err := o.FullScan(ctx, tree)
	o.lastFullScanDuration = o.clock.Now().Sub(start)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// Shutdown interrupted the scan: not a failure.
			o.logger.Debug("safety scan canceled", slog.Any("error", err))
		case o.safetyScanFailing:
			o.logger.Debug("safety scan failed again", slog.Any("error", err))
		default:
			o.safetyScanFailing = true
			o.logger.Warn("Could not rescan the sync folder; will retry", slog.Any("error", err))
		}
		o.TrySendLocalTruthSuspect(ctx, syncdomain.LocalTruthRecoveryFullScanFailed)
		return true
	}
	o.safetyScanFailing = false

	o.rewatchUnwatchedDirectories(tree, result.Rows)
	o.TrySendFullLocalSnapshot(ctx, result)

	o.pruneWriteChurn(o.clock.Now())
	if o.hooks.AfterSafetyScan != nil {
		o.hooks.AfterSafetyScan()
	}

	// Log timing and resource counts for operational visibility (B-101).
	elapsed := o.lastFullScanDuration
	o.logger.Info("safety scan complete",
		slog.Duration("elapsed", elapsed),
		slog.Int("rows", len(result.Rows)),
		slog.Int("skipped", len(result.Skipped)),
		slog.Int("baseline_entries", o.baseline.Len()),
	)

	return true
}

// infoKind classifies a stat result for the observation filter.
func infoKind(info os.FileInfo) syncdomain.ObservedKind {
	if info.IsDir() {
		return syncdomain.ObservedKindDir
	}

	return syncdomain.ObservedKindFile
}

// errSyncRootDeleted is returned when the sync root directory has been deleted
// or become inaccessible while a watch was running.
var errSyncRootDeleted = errors.New("it was deleted or can't be read")
