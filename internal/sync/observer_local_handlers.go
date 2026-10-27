// Package sync owns the single mounted content-root runtime, including local observation
// event handlers.
//
// Contents:
//   - watchLoop:    main select loop (fsnotify events, errors, safety scan, ctx)
//   - handleEvent:  fsnotify event → ChangeEvent routing
//   - handleCreate: new file/dir → hash + emit + recursive watch add
//   - handleWrite:  file modification → debounced hash + emit
//   - handleRemove: file/dir removal → emit delete + remove watches
//   - HashAndEmit:  hash computation with retry + event emission
//
// Related files:
//   - observer_local.go:            LocalObserver struct, constructor, Watch() entry point
//   - observer_local_collisions.go: case collision detection helpers (cache, peers)
//   - scanner.go:                   FullScan, walk/hash/filter logic
package sync

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

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/retry"
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
func (o *localObserver) watchLoop(
	ctx context.Context, watcher fsWatcher, tree *synctree.Root, events chan<- changeEvent,
) error {
	syncRoot := tree.Path()
	interval := o.safetyScanInterval
	if interval == 0 {
		interval = safetyScanInterval
	}

	ticker := o.clock.NewTicker(interval)
	defer stopTicker(ticker)
	tickCh := tickerChan(ticker)

	errBackoff := retry.NewBackoff(retry.WatchLocalPolicy())

	for {
		select {
		case <-ctx.Done():
			return nil

		case fsEvent, ok := <-watcher.Events():
			if !ok {
				return nil
			}

			o.HandleFsEvent(ctx, fsEvent, watcher, tree, events)
			if o.watchUnavailable != nil {
				return o.watchUnavailable
			}

			// Successful event resets error backoff.
			errBackoff.Reset()

		case req := <-o.HashRequests:
			// Deferred hash from write coalesce timer (B-107).
			o.HashAndEmit(ctx, tree, req, events)

		case <-o.recoveryScanRequests:
			o.armRecoveryScan()

		case <-o.recoveryScanDue:
			o.recoveryScanTimer = nil
			if !syncRootExists(syncRoot) {
				o.logger.Debug("sync root deleted, stopping watch",
					slog.String("sync_root", syncRoot))

				return fmt.Errorf("%w: %w", ErrMountRootUnavailable, errSyncRootDeleted)
			}

			o.logger.Debug("running recovery scan after missed local changes")
			o.runSafetyScan(ctx, tree, events)

		case watchErr, ok := <-watcher.Errors():
			if !ok {
				return nil
			}

			// Self-healing: local truth is marked for a rescan. The first
			// error of a streak is Info; repeats under sustained kernel
			// queue overflow are Debug.
			delay := errBackoff.Next()
			watcherErrLevel := slog.LevelDebug
			if errBackoff.Consecutive() <= 1 {
				watcherErrLevel = slog.LevelInfo
			}
			msg := "filesystem watcher error; rescanning the sync folder"
			if errors.Is(watchErr, fsnotify.ErrEventOverflow) {
				// IN_Q_OVERFLOW: the kernel discarded events, so changes
				// are known to be missing, not merely possible.
				msg = "too many local changes at once for the system to report; rescanning the sync folder"
			}
			o.logger.Log(ctx, watcherErrLevel, msg,
				slog.String("error", watchErr.Error()),
				slog.Duration("backoff", delay),
			)
			o.TrySendLocalTruthSuspect(ctx, localTruthRecoveryWatcherError)
			o.armRecoveryScan()

			// Exponential backoff prevents tight loop under sustained errors
			// (e.g., kernel buffer overflow).
			if sleepErr := o.clock.Sleep(ctx, delay); sleepErr != nil {
				return nil
			}

			// After watcher error, check if sync root still exists (B-113).
			// A deleted root means the watcher is watching nothing.
			if !syncRootExists(syncRoot) {
				// Debug: the returned error is reported once by the watch
				// runtime.
				o.logger.Debug("sync root deleted, stopping watch",
					slog.String("sync_root", syncRoot))

				return fmt.Errorf("%w: %w", ErrMountRootUnavailable, errSyncRootDeleted)
			}

		case <-tickCh:
			// Check if sync root still exists before running safety scan (B-113).
			if !syncRootExists(syncRoot) {
				o.logger.Debug("sync root deleted, stopping watch",
					slog.String("sync_root", syncRoot))

				return fmt.Errorf("%w: %w", ErrMountRootUnavailable, errSyncRootDeleted)
			}

			o.runSafetyScan(ctx, tree, events)
			errBackoff.Reset()
		}
	}
}

// HandleFsEvent processes a single fsnotify event and sends the appropriate
// ChangeEvent to the output channel.
func (o *localObserver) HandleFsEvent(
	ctx context.Context, fsEvent fsnotify.Event, watcher fsWatcher,
	tree *synctree.Root, events chan<- changeEvent,
) {
	// Ignore chmod events — mode changes are not synced.
	if fsEvent.Has(fsnotify.Chmod) && !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Write) {
		return
	}

	relPath, err := tree.Rel(fsEvent.Name)
	if err != nil {
		o.logger.Debug("failed to compute relative path",
			slog.String("path", fsEvent.Name), slog.String("error", err.Error()))

		return
	}
	if relPath == "." {
		o.logger.Debug("watch: ignoring sync root lifecycle event",
			slog.String("path", fsEvent.Name))
		return
	}

	dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(fsEvent.Name))

	if reservation, ok := protectedRootPathReservation(dbRelPath, o.protectedRoots); ok {
		if reservation.Path == dbRelPath {
			o.reportProtectedRootEvent(protectedRootEvent{
				Type:         protectedRootEventPathReserved,
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
		observedKindUnknown,
		o.filterConfig,
		o.protectedRoots,
		o.observationRules,
	); skip != nil {
		if skip.Reason != "" {
			o.logger.Debug("watch: skipping file",
				slog.String("path", dbRelPath),
				slog.String("reason", skip.Reason))
		}

		return
	}

	switch {
	case fsEvent.Has(fsnotify.Create):
		o.handleCreate(ctx, tree, fsEvent.Name, dbRelPath, name, watcher, events)

	case fsEvent.Has(fsnotify.Write):
		o.handleWrite(tree, fsEvent.Name, dbRelPath, name)

	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		// Pass the original filesystem path (fsEvent.Name) rather than
		// reconstructing from syncRoot + dbRelPath. The NFC-normalized
		// dbRelPath may differ from the filesystem's encoding (NFD on
		// macOS HFS+), causing watcher.Remove() to silently fail.
		o.HandleDelete(ctx, watcher, tree, fsEvent.Name, dbRelPath, name, events)
	}
}

// handleCreate processes a Create event: stat, hash (files), add watch (dirs).
func (o *localObserver) handleCreate(
	ctx context.Context, tree *synctree.Root,
	fsPath, dbRelPath, name string,
	watcher fsWatcher, events chan<- changeEvent,
) {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		// File may have been removed immediately after creation.
		o.logger.Debug("stat failed for created path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(dbRelPath)
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info, events)
		return
	}

	o.forgetExcludedSymlink(dbRelPath)

	if isSpecialFile(info) {
		// FIFOs, sockets and devices are never synced (see processObservedInfo).
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info, events)
		return
	}

	if protectedRoot, ok := protectedRootIdentityReservation(dbRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(protectedRootEvent{
			Type:         protectedRootEventIdentityMatch,
			Path:         dbRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		return
	}

	if skip := shouldObserveWithFilter(name, dbRelPath, infoKind(info), o.filterConfig, o.protectedRoots, o.observationRules); skip != nil {
		o.rescanIfSkippedEntryHidesSyncedItem(ctx, tree, dbRelPath, info, events)
		return
	}

	// Early rejection for case collisions in watch mode (R-2.12.2).
	// Applies to both files and directories — OneDrive's case-insensitive
	// namespace cannot host both Xyz/ and xyz in the same parent.
	// The authoritative check is FullScan's post-walk DetectCaseCollisions;
	// this rejects obvious collisions between safety scans.
	if collidingName, hasCollision := o.HasCaseCollisionCached(tree, filepath.Dir(fsPath), name, filepath.Dir(dbRelPath)); hasCollision {
		// Track peer for re-emission when the collider is deleted.
		peerRelPath := o.buildPeerRelPath(dbRelPath, collidingName)
		o.AddCollisionPeer(dbRelPath, peerRelPath)

		o.logger.Debug("case collision detected in watch mode, skipping event",
			slog.String("path", dbRelPath),
			slog.String("collides_with", collidingName))

		return
	}

	ev := changeEvent{
		Source: SourceLocal,
		Type:   changeCreate,
		Path:   dbRelPath,
		Name:   name,
		Size:   info.Size(),
		Mtime:  info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		ev.LocalDevice = identity.Device
		ev.LocalInode = identity.Inode
		ev.LocalHasIdentity = true
	}

	if info.IsDir() {
		ev.ItemType = ItemTypeFolder

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
			if o.watchedDirs == nil {
				o.watchedDirs = make(map[string]struct{})
			}
			o.watchedDirs[filepath.Clean(fsPath)] = struct{}{}
		}

		// The directory's own row goes out before its contents are scanned.
		// When it is the destination half of a folder rename, its identity
		// pairs the move; emitting it only after hashing a large tree left
		// the replan seeing just the delete half (D-080).
		o.TrySend(ctx, events, &ev)
		o.updateDirNameCache(filepath.Dir(fsPath), name)

		// Scan directory contents for files created before the watch was
		// registered. Duplicates from fsnotify are harmless — the buffer's
		// per-path deduplication handles them.
		o.scanNewDirectory(ctx, tree, fsPath, dbRelPath, watcher, events)

		return
	}

	// Stage 2 observation filter: file size check (requires stat).
	if o.IsOversizedFile(info.Size(), dbRelPath) {
		return
	}

	ev.ItemType = ItemTypeFile
	ev.Hash = o.watchCreateHash(fsPath, dbRelPath, name, info)

	o.TrySend(ctx, events, &ev)

	// Update directory name cache so subsequent collision checks see this entry.
	o.updateDirNameCache(filepath.Dir(fsPath), name)
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
func (o *localObserver) watchCreateHash(fsPath, dbRelPath, name string, info os.FileInfo) string {
	if identity, ok := synctree.IdentityFromFileInfo(info); ok && !hasOtherHardLinks(info) {
		entry := o.watchIdentityIndex().baselineEntryWithIdentity(o.baseline, identity)
		if entry != nil && entry.Path != dbRelPath && entry.ItemType == ItemTypeFile {
			if canReuseBaselineHash(info, entry, o.clock.Now().UnixNano()) {
				return entry.LocalHash
			}

			o.scheduleDeferredHash(fsPath, dbRelPath, name, 0, o.writeCooldown())
			return ""
		}
	}

	return o.stableHashOrEmpty(fsPath, dbRelPath)
}

// stableHashOrEmpty computes a stable hash for a file, returning an empty
// string on any failure. Extracted to deduplicate identical hash-failure
// handling in handleCreate and scanNewDirectory. Both callers emit events
// with empty hashes on failure because Create events and directory scans
// have no guaranteed follow-up event (B-203).
func (o *localObserver) stableHashOrEmpty(fsPath, dbRelPath string) string {
	hash, err := computeStableHashWith(fsPath, o.hashFunc)
	if err != nil {
		if errors.Is(err, errFileChangedDuringHash) {
			o.logger.Debug("file metadata still settling, emitting with empty hash",
				slog.String("path", dbRelPath))
		} else {
			// Debug: an unreadable file fails its upload, which the
			// console reports; this would repeat on every write.
			o.logger.Debug("hash failed, emitting event with empty hash",
				slog.String("path", dbRelPath), slog.String("error", err.Error()))
		}

		return ""
	}

	return hash
}

// scanNewDirectory walks a newly-created directory and emits ChangeCreate
// events for any files already present. This catches files created between
// the directory's creation and the fsnotify watch registration.
func (o *localObserver) scanNewDirectory(
	ctx context.Context, tree *synctree.Root, dirPath, dirRelPath string,
	watcher fsWatcher, events chan<- changeEvent,
) {
	entries, err := localpath.ReadDir(dirPath)
	if err != nil {
		o.logger.Debug("scan new directory failed",
			slog.String("path", dirRelPath), slog.String("error", err.Error()))

		return
	}

	// Pre-populate directory name cache from entries we just read,
	// avoiding a redundant os.ReadDir in HasCaseCollisionCached.
	o.populateDirNameCache(dirPath, entries)

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		o.scanNewDirectoryEntry(ctx, tree, dirPath, dirRelPath, entry, watcher, events)
	}
}

func (o *localObserver) scanNewDirectoryEntry(
	ctx context.Context,
	tree *synctree.Root,
	dirPath string,
	dirRelPath string,
	entry os.DirEntry,
	watcher fsWatcher,
	events chan<- changeEvent,
) {
	entryName := nfcNormalize(entry.Name())
	entryRelPath := dirRelPath + "/" + entryName
	entryFsPath := filepath.Join(dirPath, entry.Name())

	info, kind, ok := o.scanNewDirectoryEntryInfo(entry, entryFsPath, entryRelPath)
	if !ok {
		return
	}

	if shouldObserveWithFilter(entryName, entryRelPath, kind, o.filterConfig, o.protectedRoots, o.observationRules) != nil {
		return
	}

	if protectedRoot, ok := protectedRootIdentityReservation(entryRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(protectedRootEvent{
			Type:         protectedRootEventIdentityMatch,
			Path:         entryRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		return
	}

	if collidingName, hasCollision := o.HasCaseCollisionCached(tree, dirPath, entryName, dirRelPath); hasCollision {
		o.logger.Debug("case collision detected in directory scan, skipping",
			slog.String("path", entryRelPath),
			slog.String("collides_with", collidingName))
		return
	}

	if kind == observedKindDir {
		o.scanNewDirectoryChildDir(ctx, tree, entryFsPath, entryRelPath, entryName, watcher, events)
		return
	}

	if o.IsOversizedFile(info.Size(), entryRelPath) {
		return
	}

	fileEv := changeEvent{
		Source:   SourceLocal,
		Type:     changeCreate,
		Path:     entryRelPath,
		Name:     entryName,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     o.watchCreateHash(entryFsPath, entryRelPath, entryName, info),
		Mtime:    info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		fileEv.LocalDevice = identity.Device
		fileEv.LocalInode = identity.Inode
		fileEv.LocalHasIdentity = true
	}

	o.TrySend(ctx, events, &fileEv)
}

func (o *localObserver) scanNewDirectoryEntryInfo(
	entry os.DirEntry,
	entryFsPath string,
	entryRelPath string,
) (os.FileInfo, observedKind, bool) {
	if entry.Type()&fs.ModeSymlink != 0 {
		info, isSymlink, err := statObservedPath(entryFsPath)
		if err != nil {
			o.logger.Debug("stat failed during directory scan",
				slog.String("path", entryRelPath), slog.String("error", err.Error()))
			return nil, observedKindUnknown, false
		}

		if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
			o.rememberExcludedSymlink(entryRelPath)
			return nil, observedKindUnknown, false
		}

		o.forgetExcludedSymlink(entryRelPath)
		if isSpecialFile(info) {
			return nil, observedKindUnknown, false
		}
		return info, observedKindFromInfo(info), true
	}

	o.forgetExcludedSymlink(entryRelPath)

	info, err := entry.Info()
	if err != nil {
		o.logger.Debug("stat failed during directory scan",
			slog.String("path", entryRelPath), slog.String("error", err.Error()))
		return nil, observedKindUnknown, false
	}
	if isSpecialFile(info) {
		return nil, observedKindUnknown, false
	}

	return info, dirEntryKind(entry), true
}

func (o *localObserver) scanNewDirectoryChildDir(
	ctx context.Context,
	tree *synctree.Root,
	entryFsPath string,
	entryRelPath string,
	entryName string,
	watcher fsWatcher,
	events chan<- changeEvent,
) {
	if addErr := watcher.Add(entryFsPath); addErr != nil {
		o.reportWatchAddFailure("failed to add watch on nested directory", entryRelPath, addErr)
	} else {
		if o.watchedDirs == nil {
			o.watchedDirs = make(map[string]struct{})
		}
		o.watchedDirs[filepath.Clean(entryFsPath)] = struct{}{}
	}

	dirEv := changeEvent{
		Source:   SourceLocal,
		Type:     changeCreate,
		Path:     entryRelPath,
		Name:     entryName,
		ItemType: ItemTypeFolder,
	}
	if info, _, err := statObservedPath(entryFsPath); err == nil {
		dirEv.Size = info.Size()
		dirEv.Mtime = info.ModTime().UnixNano()
		if identity, ok := synctree.IdentityFromFileInfo(info); ok {
			dirEv.LocalDevice = identity.Device
			dirEv.LocalInode = identity.Inode
			dirEv.LocalHasIdentity = true
		}
	}

	o.TrySend(ctx, events, &dirEv)
	o.scanNewDirectory(ctx, tree, entryFsPath, entryRelPath, watcher, events)
}

// handleWrite processes a Write event by scheduling a deferred hash after a
// cooldown period (B-107 write coalescing). Rapid saves (IDE auto-save) trigger
// multiple Write events per file; coalescing ensures only one hash + emit per
// quiescence window. Emission is routed through the hashRequests channel →
// HashAndEmit (which has ctx and events from the watchLoop).
//
// Stale baseline interaction (B-116): handleWrite reads the live baseline
// (RWMutex-protected, updated in-place by CommitMutation). If an action is
// in-flight (dispatched to workers but not yet committed to baseline), the safety scan
// may re-emit an event for something already being processed. This is safe:
// the watch loop turns that event into a fresh steady-state replan instead of
// trying to mutate the currently running graph in place.
func (o *localObserver) handleWrite(_ *synctree.Root, fsPath, dbRelPath, name string) {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.logger.Debug("stat failed for modified path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(dbRelPath)
		return
	}

	o.forgetExcludedSymlink(dbRelPath)

	// Ignore directory write events — folder mtime changes are noise.
	if info.IsDir() {
		return
	}
	if isSpecialFile(info) {
		return // FIFOs, sockets and devices are never synced (see processObservedInfo).
	}

	o.scheduleWriteHash(fsPath, dbRelPath, name)
}

// scheduleWriteHash (re)arms the coalescing timer for a written file.
//
// Each Write restarts the quiet-period timer (B-107), so an IDE's burst of
// saves costs one hash. Two limits keep that from going wrong:
//   - a path is hashed at most maxWriteCoalesceWait after its first pending
//     write: a file written more often than the quiet period used to restart
//     the timer forever and was never observed between safety scans;
//   - a path that keeps producing new content is marked unstable and backs
//     off exponentially (D-081b): without it a log or database written every
//     second was hashed, uploaded and superseded every second.
func (o *localObserver) scheduleWriteHash(fsPath, dbRelPath, name string) {
	now := o.clock.Now()
	churn := o.writeChurnFor(dbRelPath, now)

	if timer, pending := o.PendingTimers[dbRelPath]; pending {
		if churn.backoff > 0 || now.Sub(churn.firstPendingAt) >= maxWriteCoalesceWait {
			// Keep the armed deadline: the hash happens on schedule even
			// while writes continue.
			return
		}
		// Restart the quiet period but keep firstPendingAt: the window
		// stays open.
		timer.Stop()
		delete(o.PendingTimers, dbRelPath)
	} else {
		churn.firstPendingAt = now
	}

	delay := o.writeCooldown()
	if churn.backoff > delay {
		delay = churn.backoff
	}

	o.scheduleDeferredHash(fsPath, dbRelPath, name, 0, delay)
}

func (o *localObserver) writeCooldown() time.Duration {
	if o.writeCoalesceCooldown == 0 {
		return defaultWriteCoalesceCooldown
	}

	return o.writeCoalesceCooldown
}

// scheduleDeferredHash arms a timer that hands a hash request to the watch
// loop. Timer callbacks must not block or touch watch-loop state, so a full
// channel drops the request and asks for a recovery rescan instead.
func (o *localObserver) scheduleDeferredHash(fsPath, dbRelPath, name string, retries int, delay time.Duration) {
	req := hashRequest{
		FsPath:    fsPath,
		DbRelPath: dbRelPath,
		Name:      name,
		Retries:   retries,
	}
	requests := o.HashRequests
	timer := o.clock.AfterFunc(delay, func() {
		select {
		case requests <- req:
		default:
			o.droppedRetries.Add(1)
			o.requestRecoveryScan()
			o.logger.Debug("hash request dropped, channel full (recovery scan will catch up)",
				slog.String("path", dbRelPath),
				slog.Int("retry", retries))
		}
	})
	o.recordPendingTimer(dbRelPath, timer)
}

// pathWriteChurn is the write history of one path, used to keep a file that
// never stops changing from being re-hashed and re-uploaded on every write.
// Watch-loop goroutine only.
type pathWriteChurn struct {
	// firstPendingAt is when the current coalescing window opened; zero
	// when no hash is pending for the path.
	firstPendingAt time.Time
	// windowStart and emits count content changes emitted recently.
	windowStart time.Time
	emits       int
	lastEmit    time.Time
	// backoff is the minimum delay before the next hash; zero while the
	// path is stable.
	backoff time.Duration
	warned  bool
}

// writeChurnFor returns the churn record for path, forgetting a path that has
// been quiet long enough to count as stable again.
func (o *localObserver) writeChurnFor(path string, now time.Time) *pathWriteChurn {
	if o.writeChurn == nil {
		o.writeChurn = make(map[string]*pathWriteChurn)
	}

	churn, ok := o.writeChurn[path]
	if ok && churn.firstPendingAt.IsZero() && churn.isQuiet(now) {
		if churn.backoff > 0 {
			o.logger.Debug("file settled; watching it normally again", slog.String("path", path))
		}
		ok = false
	}
	if !ok {
		churn = &pathWriteChurn{}
		o.writeChurn[path] = churn
	}

	return churn
}

// isQuiet reports whether no change was emitted for long enough that the
// path no longer counts as unstable: twice its current backoff, and at least
// the window that detects instability.
func (c *pathWriteChurn) isQuiet(now time.Time) bool {
	quiet := max(unstableWriteWindow, 2*c.backoff)
	return c.lastEmit.IsZero() || now.Sub(c.lastEmit) >= quiet
}

// recordWriteEmit notes that a changed file was emitted. Too many changes in
// one window mark the path unstable and double its backoff, up to the cap.
func (o *localObserver) recordWriteEmit(path string) {
	now := o.clock.Now()
	churn := o.writeChurnFor(path, now)
	if churn.windowStart.IsZero() || now.Sub(churn.windowStart) > unstableWriteWindow {
		churn.windowStart = now
		churn.emits = 0
	}
	churn.emits++
	churn.lastEmit = now
	if churn.emits < unstableWriteEmits {
		return
	}

	churn.windowStart = now
	churn.emits = 0
	churn.backoff = min(max(2*churn.backoff, unstableWriteInitialBackoff), unstableWriteMaxBackoff)
	if !churn.warned {
		churn.warned = true
		// Info: self-managing; the file still syncs, only less often.
		o.logger.Info("A file keeps changing; it will be uploaded less often until it settles. "+
			"If it is a log or database, consider excluding it with ignored_paths",
			slog.String("path", path),
			slog.Duration("next_check_after", churn.backoff),
		)
	}
}

// pruneWriteChurn drops records of paths that are stable and idle, so the
// map does not grow with every file ever written in a long session.
func (o *localObserver) pruneWriteChurn(now time.Time) {
	for path, churn := range o.writeChurn {
		if churn.firstPendingAt.IsZero() && churn.isQuiet(now) {
			delete(o.writeChurn, path)
		}
	}
}

// HashAndEmit is called from the watchLoop when a write coalesce timer fires.
// It hashes the file and emits a ChangeModify event if the content differs
// from the baseline. Runs in the watchLoop goroutine (same thread as handleWrite).
func (o *localObserver) HashAndEmit(ctx context.Context, tree *synctree.Root, req hashRequest, events chan<- changeEvent) {
	req = o.prepareDeferredHashRequest(req)

	info, isSymlink, err := statObservedPath(req.FsPath)
	if err != nil {
		o.logger.Debug("stat failed for deferred hash",
			slog.String("path", req.DbRelPath), slog.String("error", err.Error()))

		return
	}

	if shouldSkipObservedSymlink(isSymlink, o.filterConfig) {
		o.rememberExcludedSymlink(req.DbRelPath)
		return
	}

	o.forgetExcludedSymlink(req.DbRelPath)

	if info.IsDir() {
		return
	}

	if isSpecialFile(info) {
		return // FIFOs, sockets and devices are never synced (see processObservedInfo).
	}

	// Stage 2 observation filter: file size check (requires stat).
	if o.IsOversizedFile(info.Size(), req.DbRelPath) {
		return
	}

	// Early rejection for case collisions in write coalesce (R-2.12.2).
	// The authoritative check is FullScan's DetectCaseCollisions; this
	// rejects obvious collisions between safety scans.
	if collidingName, hasCollision := o.HasCaseCollisionCached(
		tree, filepath.Dir(req.FsPath), filepath.Base(req.FsPath), filepath.Dir(req.DbRelPath),
	); hasCollision {
		o.logger.Debug("case collision detected for modified file, skipping event",
			slog.String("path", req.DbRelPath),
			slog.String("collides_with", collidingName))

		return
	}

	hash, err := computeStableHashWith(req.FsPath, o.hashFunc)
	if err != nil {
		if o.retryDeferredHashOnChange(req, err) {
			return
		}

		// Distinguish retry exhaustion from generic hash failures for
		// observability — helps diagnose continuously-written files.
		if errors.Is(err, errFileChangedDuringHash) {
			o.logger.Debug("hash retries exhausted, emitting with empty hash",
				slog.String("path", req.DbRelPath),
				slog.Int("retries", req.Retries),
				slog.Int("max_retries", maxCoalesceRetries),
			)
		} else {
			o.logger.Debug("hash failed for deferred write, emitting with empty hash",
				slog.String("path", req.DbRelPath), slog.String("error", err.Error()))
		}
	} else {
		// Check baseline — if hash matches, the write was a no-op.
		if existing, ok := o.baseline.GetByPath(req.DbRelPath); ok && existing.LocalHash == hash {
			return
		}
	}

	ev := changeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     req.DbRelPath,
		Name:     req.Name,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		ev.LocalDevice = identity.Device
		ev.LocalInode = identity.Inode
		ev.LocalHasIdentity = true
	}

	o.recordWriteEmit(req.DbRelPath)
	o.TrySend(ctx, events, &ev)
}

func (o *localObserver) prepareDeferredHashRequest(req hashRequest) hashRequest {
	delete(o.PendingTimers, req.DbRelPath)
	if churn, ok := o.writeChurn[req.DbRelPath]; ok {
		churn.firstPendingAt = time.Time{}
	}

	return req
}

func (o *localObserver) retryDeferredHashOnChange(req hashRequest, err error) bool {
	if !errors.Is(err, errFileChangedDuringHash) || req.Retries >= maxCoalesceRetries {
		return false
	}

	o.logger.Debug("file changed during deferred hash, re-scheduling",
		slog.String("path", req.DbRelPath),
		slog.Int("retry", req.Retries+1))

	o.scheduleDeferredHash(req.FsPath, req.DbRelPath, req.Name, req.Retries+1, o.writeCooldown())

	return true
}

// HandleDelete processes a Remove/Rename event. For directories, also removes
// the fsnotify watch to prevent resource leaks (macOS/kqueue doesn't auto-clean).
//
// fsPath is the original filesystem path from fsEvent.Name — NOT reconstructed
// from syncRoot + dbRelPath. On macOS HFS+, fsnotify delivers NFD-encoded
// paths while dbRelPath is NFC-normalized. Using the original fsPath for
// watcher.Remove() ensures the removal matches the path registered by fsnotify.
func (o *localObserver) HandleDelete(
	ctx context.Context, watcher fsWatcher, tree *synctree.Root, fsPath, dbRelPath, name string,
	events chan<- changeEvent,
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

	// Invalidate directory name cache so collision checks see the deletion.
	o.removeDirNameCache(filepath.Dir(fsPath), name)

	if o.hasExcludedSymlinkAncestor(dbRelPath) {
		if _, ok := o.baseline.GetByPath(dbRelPath); !ok {
			o.forgetExcludedSymlink(dbRelPath)
		}

		return
	}

	// Track recent deletion so baseline cross-check doesn't false-positive
	// on case-only renames (File.txt → file.txt). The baseline is updated
	// asynchronously after the delete action executes.
	if o.RecentLocalDeletes != nil {
		o.RecentLocalDeletes[dbRelPath] = struct{}{}
	}

	// Re-emit surviving collision peers — when a user resolves a case collision
	// by deleting one file, the surviving peers should sync immediately instead
	// of waiting for the next safety scan (up to 5 minutes). Each re-emitted
	// handleCreate re-checks HasCaseCollisionCached — if collisions remain
	// among the surviving peers, they are re-recorded and stay blocked.
	if peers := o.removeCollisionPeersFor(dbRelPath); len(peers) > 0 {
		for peerPath := range peers {
			peerFsPath := filepath.Join(filepath.Dir(fsPath), filepath.Base(peerPath))
			peerName := filepath.Base(peerPath)
			o.handleCreate(ctx, tree, peerFsPath, peerPath, peerName, watcher, events)
		}
	}

	resolvedType := ItemTypeFile

	if existing, ok := o.baseline.GetByPath(dbRelPath); ok {
		resolvedType = existing.ItemType
	} else if _, ok := o.watchedDirs[filepath.Clean(fsPath)]; ok {
		resolvedType = ItemTypeFolder
	}

	// Remove watch for deleted directories to prevent resource leaks (B-112).
	// Linux inotify auto-cleans, but macOS kqueue may not. Safe to call even
	// if the watch was already removed (Remove returns a benign error).
	// Uses fsPath directly instead of reconstructing from syncRoot + dbRelPath
	// to avoid NFC/NFD mismatch on macOS HFS+ (B-312).
	if resolvedType == ItemTypeFolder {
		o.forgetWatchedSubtree(watcher, fsPath, dbRelPath)
	}

	ev := changeEvent{
		Source:    SourceLocal,
		Type:      ChangeDelete,
		Path:      dbRelPath,
		Name:      name,
		ItemType:  resolvedType,
		IsDeleted: true,
	}

	o.TrySend(ctx, events, &ev)
}

// rescanIfSkippedEntryHidesSyncedItem runs a safety scan now when a created
// entry sync skips is a synced item in disguise: it occupies a synced path
// (a symlink or special file replacing it), or it is a synced item renamed to
// a name sync skips (recognized by filesystem identity). The rename's delete
// half has already reached the runtime; the scan records the synced path as
// unavailable (scanner_unavailable.go) within the same debounce window, so
// the replan sees an unavailable path instead of a deleted one (D-004, P-5).
func (o *localObserver) rescanIfSkippedEntryHidesSyncedItem(
	ctx context.Context,
	tree *synctree.Root,
	dbRelPath string,
	info os.FileInfo,
	events chan<- changeEvent,
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
	o.runSafetyScan(ctx, tree, events)
}

// watchIdentityIndexMaxAge bounds how stale the watch handler's
// identity-to-baseline index may be. Skipped creates are frequent (editor
// swap files, .DS_Store), so the index is rebuilt at most this often rather
// than per event; an item synced within the window and then renamed out of
// reach is caught by the next safety scan instead.
const watchIdentityIndexMaxAge = time.Minute

func (o *localObserver) watchIdentityIndex() *scanAvailability {
	now := o.clock.Now()
	if o.watchIdentityLookup == nil || now.Sub(o.watchIdentityBuiltAt) > watchIdentityIndexMaxAge {
		o.watchIdentityLookup = newScanAvailability(0)
		o.watchIdentityBuiltAt = now
	}
	return o.watchIdentityLookup
}

// localPathStillPresent reports whether the exact name at fsPath exists. On a
// case-insensitive volume Lstat also finds a differently-cased sibling, which
// is the other half of a case-only rename, so the parent listing decides.
func localPathStillPresent(tree *synctree.Root, dbRelPath, fsPath string) bool {
	if tree == nil {
		return false
	}
	if _, err := tree.Lstat(dbRelPath); err != nil {
		return false
	}
	entries, err := tree.ReadDir(filepath.Dir(dbRelPath))
	if err != nil {
		return false
	}
	want := filepath.Base(fsPath)
	wantNFC := nfcNormalize(want)
	for _, entry := range entries {
		if entry.Name() == want || nfcNormalize(entry.Name()) == wantNFC {
			return true
		}
	}
	return false
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
func (o *localObserver) forgetWatchedSubtree(watcher fsWatcher, fsPath, dbRelPath string) {
	o.forgetWatchedDescendants(watcher, fsPath, dbRelPath)

	// The directory itself is removed by its original event path, not the
	// cleaned map key: on macOS HFS+ the NFD spelling must match what
	// fsnotify registered (B-312). Linux inotify auto-cleans a deleted
	// directory, but macOS kqueue may not (B-112).
	o.removeWatch(watcher, fsPath, dbRelPath)
}

// forgetWatchedDescendants removes every watch strictly beneath fsPath,
// deepest first.
func (o *localObserver) forgetWatchedDescendants(watcher fsWatcher, fsPath, dbRelPath string) {
	root := filepath.Clean(fsPath)
	prefix := root + string(filepath.Separator)
	descendants := make([]string, 0)
	for watched := range o.watchedDirs {
		if strings.HasPrefix(watched, prefix) {
			descendants = append(descendants, watched)
		}
	}
	// Deepest first, so a parent is never removed while a child under it is
	// still registered. Ties are broken by path for a deterministic order.
	slices.SortFunc(descendants, func(a, b string) int {
		if depth := strings.Count(b, string(filepath.Separator)) - strings.Count(a, string(filepath.Separator)); depth != 0 {
			return depth
		}
		return strings.Compare(a, b)
	})

	for _, watched := range descendants {
		o.removeWatch(watcher, watched, dbRelPath)
	}
}

func (o *localObserver) removeWatch(watcher fsWatcher, fsPath, dbRelPath string) {
	if rmErr := watcher.Remove(fsPath); rmErr != nil {
		// Debug: the kernel often removed the watch already (benign).
		o.logger.Debug("watch removal for deleted directory",
			slog.String("path", dbRelPath),
			slog.String("watch", fsPath),
			slog.String("error", rmErr.Error()),
		)
	}

	delete(o.watchedDirs, filepath.Clean(fsPath))
}

// reportWatchAddFailure reports a directory the watcher could not add. Once
// a resource limit is reached every new directory fails the same way, so each
// cause is warned once with its fix; other failures are per-directory Debug
// lines (the safety scan still covers those folders). A descriptor-limit
// failure also records watchUnavailable so the watch loop stops watching.
func (o *localObserver) reportWatchAddFailure(msg, dbRelPath string, addErr error) {
	cause, ok := classifyWatchAddError(addErr)
	if !ok {
		o.logger.Debug(msg, slog.String("path", dbRelPath), slog.String("error", addErr.Error()))
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
		o.reportedWatchCauses = make(map[watchUnavailableCause]struct{})
	}
	o.reportedWatchCauses[cause] = struct{}{}
	o.logger.Warn("Cannot watch new folders for changes ("+watchUnavailableReason(cause)+"); "+
		"changes in new folders will be noticed only by the periodic rescan. "+
		"To fix: "+watchUnavailableFix(cause),
		slog.String("path", dbRelPath),
	)
}

// runSafetyScan performs a full filesystem scan as a safety net, sending any
// detected changes to the events channel. This catches events that fsnotify
// may have missed. Skipped items are logged at DEBUG — the engine's primary
// scan handles observation-issue persistence.
func (o *localObserver) runSafetyScan(ctx context.Context, tree *synctree.Root, events chan<- changeEvent) {
	o.logger.Debug("running safety scan")

	// This scan reads the whole tree, so it covers anything a pending
	// recovery scan was armed for. Requests made after this point (a batch
	// this scan fails to deliver) re-arm it.
	o.cancelRecoveryScan()

	start := time.Now()

	result, err := o.FullScan(ctx, tree)
	if err != nil {
		switch {
		case ctx.Err() != nil:
			// Shutdown interrupted the scan: not a failure.
			o.logger.Debug("safety scan canceled", slog.String("error", err.Error()))
		case o.safetyScanFailing:
			o.logger.Debug("safety scan failed again", slog.String("error", err.Error()))
		default:
			o.safetyScanFailing = true
			o.logger.Warn("Could not rescan the sync folder; will retry", slog.String("error", err.Error()))
		}
		o.TrySendLocalTruthSuspect(ctx, localTruthRecoveryFullScanFailed)
		return
	}
	o.safetyScanFailing = false

	o.TrySendFullLocalSnapshot(ctx, result)

	for i := range result.Events {
		o.TrySendChangeEventOnly(ctx, events, &result.Events[i])

		if ctx.Err() != nil {
			return
		}
	}

	if len(result.Skipped) > 0 {
		if o.skippedCh != nil {
			// Forward SkippedItems to the engine for observation-issue persistence.
			select {
			case o.skippedCh <- result.Skipped:
			default:
				o.logger.Debug("skipped items channel full, will catch on next scan",
					slog.Int("count", len(result.Skipped)))
			}
		} else {
			o.logger.Debug("safety scan: skipped items",
				slog.Int("count", len(result.Skipped)))
		}
	}

	// Clear directory name cache, collision peers, and recent deletes —
	// safety scan rebuilds state from scratch, so any cached state may be stale.
	o.DirNameCache = make(map[string]map[string][]string)
	o.CollisionPeers = make(map[string]map[string]struct{})
	o.RecentLocalDeletes = make(map[string]struct{})
	o.pruneWriteChurn(o.clock.Now())
	if o.afterSafetyScan != nil {
		o.afterSafetyScan()
	}

	// Log timing and resource counts for operational visibility (B-101).
	elapsed := time.Since(start)
	o.logger.Info("safety scan complete",
		slog.Duration("elapsed", elapsed),
		slog.Int("events", len(result.Events)),
		slog.Int("skipped", len(result.Skipped)),
		slog.Int("baseline_entries", o.baseline.Len()),
	)
}

// infoKind classifies a stat result for the observation filter.
func infoKind(info os.FileInfo) observedKind {
	if info.IsDir() {
		return observedKindDir
	}

	return observedKindFile
}
