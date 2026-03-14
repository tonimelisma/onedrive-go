// observer_local_handlers.go — Watch-mode event processing for LocalObserver.
//
// Contents:
//   - watchLoop:    main select loop (fsnotify events, errors, safety scan, ctx)
//   - handleEvent:  fsnotify event → ChangeEvent routing
//   - handleCreate: new file/dir → hash + emit + recursive watch add
//   - handleWrite:  file modification → debounced hash + emit
//   - handleRemove: file/dir removal → emit delete + remove watches
//   - hashAndEmit:  hash computation with retry + event emission
//
// Related files:
//   - observer_local.go:            LocalObserver struct, constructor, Watch() entry point
//   - observer_local_collisions.go: case collision detection helpers (cache, peers)
//   - scanner.go:                   FullScan, walk/hash/filter logic
package sync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/retry"
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
	ctx context.Context, watcher FsWatcher, syncRoot string, events chan<- ChangeEvent,
) error {
	interval := o.safetyScanInterval
	if interval == 0 {
		interval = safetyScanInterval
	}

	tickCh, tickStop := o.safetyTickFunc(interval)
	defer tickStop()

	errBackoff := retry.NewBackoff(retry.WatchLocal)

	for {
		select {
		case <-ctx.Done():
			return nil

		case fsEvent, ok := <-watcher.Events():
			if !ok {
				return nil
			}

			o.handleFsEvent(ctx, fsEvent, watcher, syncRoot, events)

			// Successful event resets error backoff.
			errBackoff.Reset()

		case req := <-o.hashRequests:
			// Deferred hash from write coalesce timer (B-107).
			o.hashAndEmit(ctx, req, events)

		case watchErr, ok := <-watcher.Errors():
			if !ok {
				return nil
			}

			delay := errBackoff.Next()
			o.logger.Warn("filesystem watcher error",
				slog.String("error", watchErr.Error()),
				slog.Duration("backoff", delay),
			)

			// Exponential backoff prevents tight loop under sustained errors
			// (e.g., kernel buffer overflow).
			if sleepErr := o.sleepFunc(ctx, delay); sleepErr != nil {
				return nil
			}

			// After watcher error, check if sync root still exists (B-113).
			// A deleted root means the watcher is watching nothing.
			if !syncRootExists(syncRoot) {
				o.logger.Error("sync root deleted, stopping watch",
					slog.String("sync_root", syncRoot))

				return ErrSyncRootDeleted
			}

		case <-tickCh:
			// Check if sync root still exists before running safety scan (B-113).
			if !syncRootExists(syncRoot) {
				o.logger.Error("sync root deleted, stopping watch",
					slog.String("sync_root", syncRoot))

				return ErrSyncRootDeleted
			}

			o.runSafetyScan(ctx, syncRoot, events)
			errBackoff.Reset()
		}
	}
}

// handleFsEvent processes a single fsnotify event and sends the appropriate
// ChangeEvent to the output channel.
func (o *LocalObserver) handleFsEvent(
	ctx context.Context, fsEvent fsnotify.Event, watcher FsWatcher,
	syncRoot string, events chan<- ChangeEvent,
) {
	// Ignore chmod events — mode changes are not synced.
	if fsEvent.Has(fsnotify.Chmod) && !fsEvent.Has(fsnotify.Create) && !fsEvent.Has(fsnotify.Write) {
		return
	}

	relPath, err := filepath.Rel(syncRoot, fsEvent.Name)
	if err != nil {
		o.logger.Warn("failed to compute relative path",
			slog.String("path", fsEvent.Name), slog.String("error", err.Error()))

		return
	}

	dbRelPath := nfcNormalize(filepath.ToSlash(relPath))
	name := nfcNormalize(filepath.Base(fsEvent.Name))

	// Unified observation filter (Stage 1: name + path length).
	// Watch handlers don't collect SkippedItems — the safety scan (FullScan
	// every 5 min) catches them for recording to sync_failures.
	if skip := shouldObserve(name, dbRelPath); skip != nil {
		if skip.Reason != "" {
			o.logger.Debug("watch: skipping file",
				slog.String("path", dbRelPath),
				slog.String("reason", skip.Reason))
		}

		return
	}

	switch {
	case fsEvent.Has(fsnotify.Create):
		o.handleCreate(ctx, fsEvent.Name, dbRelPath, name, watcher, events)

	case fsEvent.Has(fsnotify.Write):
		o.handleWrite(fsEvent.Name, dbRelPath, name)

	case fsEvent.Has(fsnotify.Remove) || fsEvent.Has(fsnotify.Rename):
		// Pass the original filesystem path (fsEvent.Name) rather than
		// reconstructing from syncRoot + dbRelPath. The NFC-normalized
		// dbRelPath may differ from the filesystem's encoding (NFD on
		// macOS HFS+), causing watcher.Remove() to silently fail.
		o.handleDelete(ctx, watcher, fsEvent.Name, dbRelPath, name, events)
	}
}

// handleCreate processes a Create event: stat, hash (files), add watch (dirs).
func (o *LocalObserver) handleCreate(
	ctx context.Context, fsPath, dbRelPath, name string,
	watcher FsWatcher, events chan<- ChangeEvent,
) {
	info, err := os.Stat(fsPath)
	if err != nil {
		// File may have been removed immediately after creation.
		o.logger.Debug("stat failed for created path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Early rejection for case collisions in watch mode (R-2.12.2).
	// Applies to both files and directories — OneDrive's case-insensitive
	// namespace cannot host both Xyz/ and xyz in the same parent.
	// The authoritative check is FullScan's post-walk detectCaseCollisions;
	// this rejects obvious collisions between safety scans.
	if collidingName, hasCollision := o.hasCaseCollisionCached(filepath.Dir(fsPath), name, filepath.Dir(dbRelPath)); hasCollision {
		// Track peer for re-emission when the collider is deleted.
		peerRelPath := o.buildPeerRelPath(dbRelPath, collidingName)
		o.addCollisionPeer(dbRelPath, peerRelPath)

		o.logger.Debug("case collision detected in watch mode, skipping event",
			slog.String("path", dbRelPath),
			slog.String("collides_with", collidingName))

		return
	}

	ev := ChangeEvent{
		Source: SourceLocal,
		Type:   ChangeCreate,
		Path:   dbRelPath,
		Name:   name,
		Size:   info.Size(),
		Mtime:  info.ModTime().UnixNano(),
	}

	if info.IsDir() {
		ev.ItemType = ItemTypeFolder

		if addErr := watcher.Add(fsPath); addErr != nil {
			o.logger.Warn("failed to add watch on new directory",
				slog.String("path", dbRelPath), slog.String("error", addErr.Error()))
		}

		// Scan directory contents for files created before the watch was
		// registered. Duplicates from fsnotify are harmless — the buffer's
		// per-path deduplication handles them.
		o.scanNewDirectory(ctx, fsPath, dbRelPath, watcher, events)
	} else {
		// Stage 2 observation filter: file size check (requires stat).
		if o.isOversizedFile(info.Size(), dbRelPath) {
			return
		}

		ev.ItemType = ItemTypeFile
		ev.Hash = o.stableHashOrEmpty(fsPath, dbRelPath)
	}

	o.trySend(ctx, events, &ev)

	// Update directory name cache so subsequent collision checks see this entry.
	o.updateDirNameCache(filepath.Dir(fsPath), name)
}

// stableHashOrEmpty computes a stable hash for a file, returning an empty
// string on any failure. Extracted to deduplicate identical hash-failure
// handling in handleCreate and scanNewDirectory. Both callers emit events
// with empty hashes on failure because Create events and directory scans
// have no guaranteed follow-up event (B-203).
func (o *LocalObserver) stableHashOrEmpty(fsPath, dbRelPath string) string {
	hash, err := computeStableHash(fsPath)
	if err != nil {
		if errors.Is(err, errFileChangedDuringHash) {
			o.logger.Debug("file metadata still settling, emitting with empty hash",
				slog.String("path", dbRelPath))
		} else {
			o.logger.Warn("hash failed, emitting event with empty hash",
				slog.String("path", dbRelPath), slog.String("error", err.Error()))
		}

		return ""
	}

	return hash
}

// scanNewDirectory walks a newly-created directory and emits ChangeCreate
// events for any files already present. This catches files created between
// the directory's creation and the fsnotify watch registration.
func (o *LocalObserver) scanNewDirectory(
	ctx context.Context, dirPath, dirRelPath string,
	watcher FsWatcher, events chan<- ChangeEvent,
) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		o.logger.Debug("scan new directory failed",
			slog.String("path", dirRelPath), slog.String("error", err.Error()))

		return
	}

	// Pre-populate directory name cache from entries we just read,
	// avoiding a redundant os.ReadDir in hasCaseCollisionCached.
	o.populateDirNameCache(dirPath, entries)

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}

		entryName := nfcNormalize(entry.Name())
		entryRelPath := dirRelPath + "/" + entryName

		// Unified observation filter (Stage 1).
		if shouldObserve(entryName, entryRelPath) != nil {
			continue
		}

		entryFsPath := filepath.Join(dirPath, entry.Name())

		// Early rejection for case collisions in directory scan (R-2.12.2).
		// Applies to both files and directories — checked before branching.
		if collidingName, hasCollision := o.hasCaseCollisionCached(dirPath, entryName, dirRelPath); hasCollision {
			o.logger.Debug("case collision detected in directory scan, skipping",
				slog.String("path", entryRelPath),
				slog.String("collides_with", collidingName))

			continue
		}

		// Recurse into subdirectories: add watch and scan contents.
		if entry.IsDir() {
			if addErr := watcher.Add(entryFsPath); addErr != nil {
				o.logger.Warn("failed to add watch on nested directory",
					slog.String("path", entryRelPath), slog.String("error", addErr.Error()))
			}

			dirEv := ChangeEvent{
				Source:   SourceLocal,
				Type:     ChangeCreate,
				Path:     entryRelPath,
				Name:     entryName,
				ItemType: ItemTypeFolder,
			}

			o.trySend(ctx, events, &dirEv)

			o.scanNewDirectory(ctx, entryFsPath, entryRelPath, watcher, events)

			continue
		}

		info, statErr := entry.Info()
		if statErr != nil {
			o.logger.Debug("stat failed during directory scan",
				slog.String("path", entryRelPath), slog.String("error", statErr.Error()))

			continue
		}

		// Stage 2 observation filter: file size check (requires stat).
		if o.isOversizedFile(info.Size(), entryRelPath) {
			continue
		}

		fileEv := ChangeEvent{
			Source:   SourceLocal,
			Type:     ChangeCreate,
			Path:     entryRelPath,
			Name:     entryName,
			ItemType: ItemTypeFile,
			Size:     info.Size(),
			Hash:     o.stableHashOrEmpty(entryFsPath, entryRelPath),
			Mtime:    info.ModTime().UnixNano(),
		}

		o.trySend(ctx, events, &fileEv)
	}
}

// handleWrite processes a Write event by scheduling a deferred hash after a
// cooldown period (B-107 write coalescing). Rapid saves (IDE auto-save) trigger
// multiple Write events per file; coalescing ensures only one hash + emit per
// quiescence window. Emission is routed through the hashRequests channel →
// hashAndEmit (which has ctx and events from the watchLoop).
//
// Stale baseline interaction (B-116): handleWrite reads the live baseline
// (RWMutex-protected, updated in-place by CommitOutcome). If an action is
// in-flight (dispatched to workers but not yet committed to baseline), the safety scan
// may re-emit an event for something already being processed. This is safe:
// processBatch deduplicates via HasInFlight + CancelByPath (B-122).
func (o *LocalObserver) handleWrite(fsPath, dbRelPath, name string) {
	info, err := os.Stat(fsPath)
	if err != nil {
		o.logger.Debug("stat failed for modified path",
			slog.String("path", dbRelPath), slog.String("error", err.Error()))

		return
	}

	// Ignore directory write events — folder mtime changes are noise.
	if info.IsDir() {
		return
	}

	cooldown := o.writeCoalesceCooldown
	if cooldown == 0 {
		cooldown = defaultWriteCoalesceCooldown
	}

	// Cancel existing timer for this path (B-107 coalescing).
	if timer, ok := o.pendingTimers[dbRelPath]; ok {
		timer.Stop()
	}

	// Schedule deferred hash after cooldown. Timer callback sends to
	// hashRequests channel (non-blocking); watchLoop picks it up via select.
	req := hashRequest{fsPath: fsPath, dbRelPath: dbRelPath, name: name}
	o.pendingTimers[dbRelPath] = time.AfterFunc(cooldown, func() {
		select {
		case o.hashRequests <- req:
		default:
			o.droppedRetries.Add(1)
			o.logger.Debug("hash request dropped, channel full (safety scan will catch up)",
				slog.String("path", dbRelPath))
		}
	})
}

// hashAndEmit is called from the watchLoop when a write coalesce timer fires.
// It hashes the file and emits a ChangeModify event if the content differs
// from the baseline. Runs in the watchLoop goroutine (same thread as handleWrite).
func (o *LocalObserver) hashAndEmit(ctx context.Context, req hashRequest, events chan<- ChangeEvent) {
	delete(o.pendingTimers, req.dbRelPath)

	info, err := os.Stat(req.fsPath)
	if err != nil {
		o.logger.Debug("stat failed for deferred hash",
			slog.String("path", req.dbRelPath), slog.String("error", err.Error()))

		return
	}

	if info.IsDir() {
		return
	}

	// Stage 2 observation filter: file size check (requires stat).
	if o.isOversizedFile(info.Size(), req.dbRelPath) {
		return
	}

	// Early rejection for case collisions in write coalesce (R-2.12.2).
	// The authoritative check is FullScan's detectCaseCollisions; this
	// rejects obvious collisions between safety scans.
	if collidingName, hasCollision := o.hasCaseCollisionCached(
		filepath.Dir(req.fsPath), filepath.Base(req.fsPath), filepath.Dir(req.dbRelPath),
	); hasCollision {
		o.logger.Debug("case collision detected for modified file, skipping event",
			slog.String("path", req.dbRelPath),
			slog.String("collides_with", collidingName))

		return
	}

	hash, err := computeStableHash(req.fsPath)
	if err != nil {
		if errors.Is(err, errFileChangedDuringHash) && req.retries < maxCoalesceRetries {
			// File still changing — re-schedule with incremented retry count.
			// If another Write event arrives, handleWrite resets the timer anyway.
			o.logger.Debug("file changed during deferred hash, re-scheduling",
				slog.String("path", req.dbRelPath),
				slog.Int("retry", req.retries+1))

			cooldown := o.writeCoalesceCooldown
			if cooldown == 0 {
				cooldown = defaultWriteCoalesceCooldown
			}

			req2 := req // copy for closure
			req2.retries++
			o.pendingTimers[req.dbRelPath] = time.AfterFunc(cooldown, func() {
				select {
				case o.hashRequests <- req2:
				default:
					o.droppedRetries.Add(1)
					o.logger.Debug("hash retry dropped, channel full (safety scan will catch up)",
						slog.String("path", req.dbRelPath),
						slog.Int("retry", req2.retries))
				}
			})

			return
		}

		// Distinguish retry exhaustion from generic hash failures for
		// observability — helps diagnose continuously-written files.
		if errors.Is(err, errFileChangedDuringHash) {
			o.logger.Warn("hash retries exhausted, emitting with empty hash",
				slog.String("path", req.dbRelPath),
				slog.Int("retries", req.retries),
				slog.Int("max_retries", maxCoalesceRetries),
			)
		} else {
			o.logger.Warn("hash failed for deferred write, emitting with empty hash",
				slog.String("path", req.dbRelPath), slog.String("error", err.Error()))
		}
	} else {
		// Check baseline — if hash matches, the write was a no-op.
		if existing, ok := o.baseline.GetByPath(req.dbRelPath); ok && existing.LocalHash == hash {
			return
		}
	}

	ev := ChangeEvent{
		Source:   SourceLocal,
		Type:     ChangeModify,
		Path:     req.dbRelPath,
		Name:     req.name,
		ItemType: ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}

	o.trySend(ctx, events, &ev)
}

// handleDelete processes a Remove/Rename event. For directories, also removes
// the fsnotify watch to prevent resource leaks (macOS/kqueue doesn't auto-clean).
//
// fsPath is the original filesystem path from fsEvent.Name — NOT reconstructed
// from syncRoot + dbRelPath. On macOS HFS+, fsnotify delivers NFD-encoded
// paths while dbRelPath is NFC-normalized. Using the original fsPath for
// watcher.Remove() ensures the removal matches the path registered by fsnotify.
func (o *LocalObserver) handleDelete(
	ctx context.Context, watcher FsWatcher, fsPath, dbRelPath, name string,
	events chan<- ChangeEvent,
) {
	// Clean up write coalesce timer for deleted path (B-107).
	if timer, ok := o.pendingTimers[dbRelPath]; ok {
		timer.Stop()
		delete(o.pendingTimers, dbRelPath)
	}

	// Invalidate directory name cache so collision checks see the deletion.
	o.removeDirNameCache(filepath.Dir(fsPath), name)

	// Track recent deletion so baseline cross-check doesn't false-positive
	// on case-only renames (File.txt → file.txt). The baseline is updated
	// asynchronously after the delete action executes.
	if o.recentLocalDeletes != nil {
		o.recentLocalDeletes[dbRelPath] = struct{}{}
	}

	// Re-emit surviving collision peers — when a user resolves a case collision
	// by deleting one file, the surviving peers should sync immediately instead
	// of waiting for the next safety scan (up to 5 minutes). Each re-emitted
	// handleCreate re-checks hasCaseCollisionCached — if collisions remain
	// among the surviving peers, they are re-recorded and stay blocked.
	if peers := o.removeCollisionPeersFor(dbRelPath); len(peers) > 0 {
		for peerPath := range peers {
			peerFsPath := filepath.Join(filepath.Dir(fsPath), filepath.Base(peerPath))
			peerName := filepath.Base(peerPath)
			o.handleCreate(ctx, peerFsPath, peerPath, peerName, watcher, events)
		}
	}

	itemType := ItemTypeFile

	if existing, ok := o.baseline.GetByPath(dbRelPath); ok {
		itemType = existing.ItemType
	}

	// Remove watch for deleted directories to prevent resource leaks (B-112).
	// Linux inotify auto-cleans, but macOS kqueue may not. Safe to call even
	// if the watch was already removed (Remove returns a benign error).
	// Uses fsPath directly instead of reconstructing from syncRoot + dbRelPath
	// to avoid NFC/NFD mismatch on macOS HFS+ (B-312).
	if itemType == ItemTypeFolder {
		if rmErr := watcher.Remove(fsPath); rmErr != nil {
			o.logger.Debug("watch removal for deleted directory",
				slog.String("path", dbRelPath),
				slog.String("error", rmErr.Error()),
			)
		}
	}

	ev := ChangeEvent{
		Source:    SourceLocal,
		Type:      ChangeDelete,
		Path:      dbRelPath,
		Name:      name,
		ItemType:  itemType,
		IsDeleted: true,
	}

	o.trySend(ctx, events, &ev)
}

// runSafetyScan performs a full filesystem scan as a safety net, sending any
// detected changes to the events channel. This catches events that fsnotify
// may have missed. Skipped items are logged at DEBUG — the engine's primary
// scan handles recording to sync_failures.
func (o *LocalObserver) runSafetyScan(ctx context.Context, syncRoot string, events chan<- ChangeEvent) {
	o.logger.Debug("running safety scan")

	start := time.Now()

	result, err := o.FullScan(ctx, syncRoot)
	if err != nil {
		o.logger.Warn("safety scan failed", slog.String("error", err.Error()))
		return
	}

	for i := range result.Events {
		o.trySend(ctx, events, &result.Events[i])

		if ctx.Err() != nil {
			return
		}
	}

	if len(result.Skipped) > 0 {
		if o.skippedCh != nil {
			// Forward SkippedItems to the engine for recording in sync_failures.
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
	o.dirNameCache = make(map[string]map[string][]string)
	o.collisionPeers = make(map[string]map[string]struct{})
	o.recentLocalDeletes = make(map[string]struct{})

	// Log timing and resource counts for operational visibility (B-101).
	elapsed := time.Since(start)
	o.logger.Info("safety scan complete",
		slog.Duration("elapsed", elapsed),
		slog.Int("events", len(result.Events)),
		slog.Int("skipped", len(result.Skipped)),
		slog.Int("baseline_entries", o.baseline.Len()),
	)
}
