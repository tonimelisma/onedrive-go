// Watch-mode handling of file writes: the debounced, churn-aware hash
// scheduling and HashAndEmit.

package syncobserve

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// handleWrite processes a Write event by scheduling a deferred hash after a
// cooldown period (B-107 write coalescing). Rapid saves (IDE auto-save) trigger
// multiple Write events per file; coalescing ensures only one hash + emit per
// quiescence window. Emission is routed through the hashRequests channel →
// HashAndEmit (which has the watchLoop's ctx).
//
// Stale baseline interaction (B-116): handleWrite reads the live baseline
// (RWMutex-protected, updated in-place by CommitMutation). If an action is
// in-flight (dispatched to workers but not yet committed to baseline), the safety scan
// may re-emit an event for something already being processed. This is safe:
// the watch loop turns that event into a fresh steady-state replan instead of
// trying to mutate the currently running graph in place.
func (o *LocalObserver) handleWrite(_ *synctree.Root, fsPath, dbRelPath string) {
	info, isSymlink, err := statObservedPath(fsPath)
	if err != nil {
		o.logger.Debug("stat failed for modified path",
			slog.String("path", dbRelPath), slog.Any("error", err))

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

	o.scheduleWriteHash(fsPath, dbRelPath)
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
func (o *LocalObserver) scheduleWriteHash(fsPath, dbRelPath string) {
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

	o.scheduleDeferredHash(fsPath, dbRelPath, 0, delay)
}

func (o *LocalObserver) writeCooldown() time.Duration {
	if o.hooks.WriteCoalesceCooldown == 0 {
		return defaultWriteCoalesceCooldown
	}

	return o.hooks.WriteCoalesceCooldown
}

// scheduleDeferredHash arms a timer that hands a hash request to the watch
// loop. Timer callbacks must not block or touch watch-loop state, so a full
// channel drops the request and asks for a recovery rescan instead.
func (o *LocalObserver) scheduleDeferredHash(fsPath, dbRelPath string, retries int, delay time.Duration) {
	req := HashRequest{
		FsPath:    fsPath,
		DbRelPath: dbRelPath,
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
	// lastHash is the hash last emitted, so a re-emit of unchanged content
	// does not count as churn.
	lastHash string
	// backoff is the minimum delay before the next hash; zero while the
	// path is stable.
	backoff time.Duration
	warned  bool
}

// writeChurnFor returns the churn record for path, forgetting a path that has
// been quiet long enough to count as stable again.
func (o *LocalObserver) writeChurnFor(path string, now time.Time) *pathWriteChurn {
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

// recordWriteEmit notes that a file's content was emitted with hash.
//
// A stable path becomes unstable after unstableWriteEmits changes within
// unstableWriteWindow. From then on every emit of content that differs from
// the previous emit doubles the backoff, up to unstableWriteMaxBackoff. (A
// window count alone could never double it: with a one-minute backoff three
// emits never fit in the two-minute window, so it stayed pinned at the
// initial value.) writeChurnFor resets the path once it has been quiet for
// twice its backoff.
func (o *LocalObserver) recordWriteEmit(path, hash string) {
	now := o.clock.Now()
	churn := o.writeChurnFor(path, now)
	contentChanged := churn.lastEmit.IsZero() || hash == "" || hash != churn.lastHash
	churn.lastEmit = now
	churn.lastHash = hash

	if churn.backoff > 0 {
		if contentChanged {
			o.escalateWriteBackoff(path, churn)
		}
		return
	}

	if churn.windowStart.IsZero() || now.Sub(churn.windowStart) > unstableWriteWindow {
		churn.windowStart = now
		churn.emits = 0
	}
	churn.emits++
	if churn.emits >= unstableWriteEmits {
		churn.windowStart = now
		churn.emits = 0
		o.escalateWriteBackoff(path, churn)
	}
}

// escalateWriteBackoff marks path unstable, or doubles its backoff, up to the
// cap, and says so once per path.
func (o *LocalObserver) escalateWriteBackoff(path string, churn *pathWriteChurn) {
	churn.backoff = min(max(2*churn.backoff, unstableWriteInitialBackoff), unstableWriteMaxBackoff)
	if churn.warned {
		return
	}

	churn.warned = true
	// Info: self-managing; the file still syncs, only less often.
	o.logger.Info("A file keeps changing; it will be uploaded less often until it settles. "+
		"If it is a log or database, consider excluding it with ignored_paths",
		slog.String("path", path),
		slog.Duration("next_check_after", churn.backoff),
	)
}

// deferStillChangingHash re-arms the hash of a file that kept changing
// through every coalescing retry, after an escalated backoff.
func (o *LocalObserver) deferStillChangingHash(req HashRequest) {
	now := o.clock.Now()
	churn := o.writeChurnFor(req.DbRelPath, now)
	o.escalateWriteBackoff(req.DbRelPath, churn)
	churn.firstPendingAt = now
	o.scheduleDeferredHash(req.FsPath, req.DbRelPath, 0, churn.backoff)
}

// pruneWriteChurn drops records of paths that are stable and idle, so the
// map does not grow with every file ever written in a long session.
func (o *LocalObserver) pruneWriteChurn(now time.Time) {
	for path, churn := range o.writeChurn {
		if churn.firstPendingAt.IsZero() && churn.isQuiet(now) {
			delete(o.writeChurn, path)
		}
	}
}

// HashAndEmit is called from the watchLoop when a write coalesce timer fires.
// It hashes the file and sends its row if the content differs from the
// baseline. Runs in the watchLoop goroutine (same thread as handleWrite).
func (o *LocalObserver) HashAndEmit(ctx context.Context, tree *synctree.Root, req HashRequest) {
	req = o.prepareDeferredHashRequest(req)

	info, isSymlink, err := statObservedPath(req.FsPath)
	if err != nil {
		o.logger.Debug("stat failed for deferred hash",
			slog.String("path", req.DbRelPath), slog.Any("error", err))

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

	hash, err := computeStableHashWith(ctx, req.FsPath, o.hooks.HashFile)
	if err != nil {
		if o.retryDeferredHashOnChange(req, err) {
			return
		}

		if errors.Is(err, errFileChangedDuringHash) {
			// Still being written after every retry. An empty-hash row
			// would only plan an upload of a file in mid-write, so the path
			// stays pending: it is marked unstable and checked again after
			// the backoff.
			o.logger.Debug("hash retries exhausted; file still changing, checking again after backoff",
				slog.String("path", req.DbRelPath),
				slog.Int("retries", req.Retries),
				slog.Int("max_retries", maxCoalesceRetries),
			)
			o.deferStillChangingHash(req)
			return
		}

		o.logger.Debug("hash failed for deferred write, emitting with empty hash",
			slog.String("path", req.DbRelPath), slog.Any("error", err))
	} else {
		// Check baseline — if hash matches, the write was a no-op.
		if existing, ok := o.baseline.GetByPath(req.DbRelPath); ok && existing.LocalHash == hash {
			return
		}
	}

	ev := syncdomain.LocalEntry{
		Path:     req.DbRelPath,
		ItemType: syncdomain.ItemTypeFile,
		Size:     info.Size(),
		Hash:     hash,
		Mtime:    info.ModTime().UnixNano(),
	}
	if identity, ok := synctree.IdentityFromFileInfo(info); ok {
		ev.LocalDevice = identity.Device
		ev.LocalInode = identity.Inode
		ev.LocalBirthTime = identity.BirthTime
		ev.LocalHasIdentity = true
	}
	ev.LocalCtime = syncdomain.FileChangeTime(info)

	o.recordWriteEmit(req.DbRelPath, hash)
	o.TrySend(ctx, &ev)
}

func (o *LocalObserver) prepareDeferredHashRequest(req HashRequest) HashRequest {
	delete(o.PendingTimers, req.DbRelPath)
	if churn, ok := o.writeChurn[req.DbRelPath]; ok {
		churn.firstPendingAt = time.Time{}
	}

	return req
}

func (o *LocalObserver) retryDeferredHashOnChange(req HashRequest, err error) bool {
	if !errors.Is(err, errFileChangedDuringHash) || req.Retries >= maxCoalesceRetries {
		return false
	}

	o.logger.Debug("file changed during deferred hash, re-scheduling",
		slog.String("path", req.DbRelPath),
		slog.Int("retry", req.Retries+1))

	o.scheduleDeferredHash(req.FsPath, req.DbRelPath, req.Retries+1, o.writeCooldown())

	return true
}
