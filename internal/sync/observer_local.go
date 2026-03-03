package sync

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ErrSyncRootDeleted is returned when the sync root directory has been deleted
// or become inaccessible while a watch was running.
var ErrSyncRootDeleted = errors.New("sync: sync root directory deleted or inaccessible")

// ErrWatchLimitExhausted is returned when the inotify watch limit is
// exhausted (Linux ENOSPC). The engine falls back to periodic full scans.
var ErrWatchLimitExhausted = errors.New("sync: inotify watch limit exhausted")

// Constants for the local observer (watch mode).
const (
	safetyScanInterval  = 5 * time.Minute
	watchErrInitBackoff = 1 * time.Second
	watchErrMaxBackoff  = 30 * time.Second
	watchErrBackoffMult = 2

	// defaultWriteCoalesceCooldown is the per-path quiescence window for
	// write coalescing (B-107). Multiple Write events within this window are
	// coalesced into a single hash + emit.
	defaultWriteCoalesceCooldown = 500 * time.Millisecond

	// hashRequestBufSize is the buffer for the timer → watchLoop channel.
	// Timer callbacks must not block; 256 handles bursts like `git checkout`.
	hashRequestBufSize = 256
)

// maxCoalesceRetries caps the number of re-schedule attempts in hashAndEmit
// when errFileChangedDuringHash is returned. Prevents infinite retry if a file
// is being written to continuously.
const maxCoalesceRetries = 3

// hashRequest is sent from timer callbacks to the watchLoop goroutine when a
// write coalesce timer fires and the file should be hashed (B-107).
type hashRequest struct {
	fsPath    string
	dbRelPath string
	name      string
	retries   int // number of re-schedules already attempted
}

// FsWatcher abstracts filesystem event monitoring. Satisfied by
// *fsnotify.Watcher; tests inject a mock implementation.
type FsWatcher interface {
	Add(name string) error
	Remove(name string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

// fsnotifyWrapper adapts *fsnotify.Watcher to the FsWatcher interface.
// fsnotify exposes Events and Errors as public fields, not methods.
type fsnotifyWrapper struct {
	w *fsnotify.Watcher
}

func (fw *fsnotifyWrapper) Add(name string) error         { return fw.w.Add(name) }
func (fw *fsnotifyWrapper) Remove(name string) error      { return fw.w.Remove(name) }
func (fw *fsnotifyWrapper) Close() error                  { return fw.w.Close() }
func (fw *fsnotifyWrapper) Events() <-chan fsnotify.Event { return fw.w.Events }
func (fw *fsnotifyWrapper) Errors() <-chan error          { return fw.w.Errors }

// LocalObserver walks the local filesystem and produces []ChangeEvent by
// comparing each entry against the in-memory baseline. Stateless — syncRoot
// is a parameter of FullScan, allowing reuse across cycles.
type LocalObserver struct {
	baseline           *Baseline
	logger             *slog.Logger
	checkWorkers       int // parallel hash goroutine limit for FullScan (0 → defaultCheckWorkers)
	watcherFactory     func() (FsWatcher, error)
	droppedEvents      atomic.Int64                                     // events dropped by trySend due to full channel
	lastActivityNano   atomic.Int64                                     // liveness: updated on each event emit (B-125)
	sleepFunc          func(ctx context.Context, d time.Duration) error // injectable for testing
	safetyTickFunc     func(d time.Duration) (<-chan time.Time, func()) // injectable for testing; returns tick channel + stop func
	safetyScanInterval time.Duration                                    // 0 → default (5 minutes); configurable (B-099)

	// Write coalescing fields (B-107). Initialized in Watch(), not the
	// constructor, since FullScan doesn't use coalescing.
	writeCoalesceCooldown time.Duration          // 0 → defaultWriteCoalesceCooldown; injectable for tests
	pendingTimers         map[string]*time.Timer // per-path timers; watchLoop-only (no mutex needed)
	hashRequests          chan hashRequest       // timer callback → watchLoop
}

// NewLocalObserver creates a LocalObserver. checkWorkers controls the number
// of parallel goroutines used for file hashing during FullScan (0 → default 4).
// The baseline must be loaded (from BaselineManager.Load); it is read-only
// during observation.
func NewLocalObserver(baseline *Baseline, logger *slog.Logger, checkWorkers int) *LocalObserver {
	return &LocalObserver{
		baseline:     baseline,
		logger:       logger,
		checkWorkers: checkWorkers,
		sleepFunc:    timeSleep,
		safetyTickFunc: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		watcherFactory: func() (FsWatcher, error) {
			w, err := fsnotify.NewWatcher()
			if err != nil {
				return nil, err
			}
			return &fsnotifyWrapper{w: w}, nil
		},
	}
}

// trySend sends a ChangeEvent to the events channel without blocking. If the
// channel is full, the event is dropped and logged at Warn. The safety scan
// (every 5 minutes) catches any dropped events, providing eventual consistency.
func (o *LocalObserver) trySend(ctx context.Context, events chan<- ChangeEvent, ev *ChangeEvent) {
	select {
	case events <- *ev:
		o.recordActivity()
	case <-ctx.Done():
	default:
		o.droppedEvents.Add(1)
		o.logger.Warn("event channel full, dropping event (safety scan will catch up)",
			slog.String("path", ev.Path),
			slog.String("type", ev.Type.String()),
		)
	}
}

// DroppedEvents returns the cumulative number of events dropped by trySend
// due to a full channel. Production code uses ResetDroppedEvents for per-cycle
// reporting; this accessor is retained for tests and diagnostics.
func (o *LocalObserver) DroppedEvents() int64 {
	return o.droppedEvents.Load()
}

// ResetDroppedEvents atomically reads and resets the drop counter to zero.
// Returns the number of events dropped since the last reset. Used by the
// engine to log per-cycle drops without double-counting across cycles.
func (o *LocalObserver) ResetDroppedEvents() int64 {
	return o.droppedEvents.Swap(0)
}

// LastActivity returns the time of the most recent event emission.
// Returns zero time if no events have been emitted. Thread-safe (B-125).
func (o *LocalObserver) LastActivity() time.Time {
	nano := o.lastActivityNano.Load()
	if nano == 0 {
		return time.Time{}
	}

	return time.Unix(0, nano)
}

// recordActivity updates the liveness timestamp to now.
func (o *LocalObserver) recordActivity() {
	o.lastActivityNano.Store(time.Now().UnixNano())
}

// estimateDirCount returns the estimated number of directories that will need
// inotify watches. Counts ItemTypeFolder entries in baseline plus one for the
// sync root itself.
func (o *LocalObserver) estimateDirCount() int {
	count := 1 // sync root always needs a watch

	o.baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
		if entry.ItemType == ItemTypeFolder {
			count++
		}
	})

	return count
}

// Watch monitors the local filesystem for changes using fsnotify and sends
// events to the provided channel. It blocks until the context is canceled,
// returning nil. A periodic safety scan (every 5 minutes) catches any events
// that fsnotify may miss (e.g., during brief watcher gaps or platform edge
// cases). Returns ErrNosyncGuard if the .nosync guard file is present.
//
// Channel sizing (B-114): The events channel should be buffered (recommended
// size: 256). An unbuffered channel blocks on every event. If the channel is
// full, trySend drops the event and increments the drop counter — the safety
// scan provides eventual consistency for any dropped events.
func (o *LocalObserver) Watch(ctx context.Context, syncRoot string, events chan<- ChangeEvent) error {
	o.logger.Info("local observer starting watch",
		slog.String("sync_root", syncRoot),
	)

	// Guard: abort if .nosync file is present.
	if _, err := os.Stat(filepath.Join(syncRoot, nosyncFileName)); err == nil {
		o.logger.Warn("nosync guard file detected, aborting watch",
			slog.String("sync_root", syncRoot))

		return ErrNosyncGuard
	}

	watcher, err := o.watcherFactory()
	if err != nil {
		return fmt.Errorf("sync: creating filesystem watcher: %w", err)
	}
	defer watcher.Close()

	// Initialize write coalescing (B-107). These are watch-only structures;
	// FullScan doesn't use coalescing.
	o.pendingTimers = make(map[string]*time.Timer)
	o.hashRequests = make(chan hashRequest, hashRequestBufSize)

	defer o.cancelPendingTimers()

	// Pre-flight capacity check (Linux only; no-op on other platforms).
	checkInotifyCapacity(o.estimateDirCount(), o.logger)

	// Walk the sync root to add watches on all existing directories.
	if walkErr := o.addWatchesRecursive(watcher, syncRoot); walkErr != nil {
		if errors.Is(walkErr, ErrWatchLimitExhausted) {
			return ErrWatchLimitExhausted
		}

		return fmt.Errorf("sync: adding initial watches: %w", walkErr)
	}

	return o.watchLoop(ctx, watcher, syncRoot, events)
}

// cancelPendingTimers stops and clears all pending write coalesce timers.
// Called on watchLoop exit to prevent timer callbacks sending to a closed channel.
//
// Deleting map entries during range iteration is safe in Go — the spec
// guarantees that entries added during iteration may or may not be visited,
// and deletion of unvisited entries is well-defined. See go.dev/ref/spec#For_range.
func (o *LocalObserver) cancelPendingTimers() {
	for path, timer := range o.pendingTimers {
		timer.Stop()
		delete(o.pendingTimers, path)
	}
}

// addWatchesRecursive walks the sync root and adds a watch on every directory.
func (o *LocalObserver) addWatchesRecursive(watcher FsWatcher, syncRoot string) error {
	var watched, failed int

	err := filepath.WalkDir(syncRoot, func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			o.logger.Warn("walk error during watch setup",
				slog.String("path", fsPath), slog.String("error", walkErr.Error()))

			return skipEntry(d)
		}

		if !d.IsDir() {
			return nil
		}

		// Symlinked directories cannot be reliably watched (the watcher
		// follows the real path, not the symlink) and are excluded from
		// sync. Log a warning so the user knows why the directory is
		// skipped (B-120).
		if d.Type()&fs.ModeSymlink != 0 {
			o.logger.Warn("skipping symlinked directory in watch setup",
				slog.String("path", fsPath))

			return filepath.SkipDir
		}

		name := d.Name()
		if fsPath != syncRoot && (isAlwaysExcluded(name) || !isValidOneDriveName(name)) {
			return filepath.SkipDir
		}

		if addErr := watcher.Add(fsPath); addErr != nil {
			if isWatchLimitError(addErr) {
				o.logger.Error("inotify watch limit exhausted",
					slog.String("path", fsPath),
					slog.Int("watches_added", watched),
				)

				return ErrWatchLimitExhausted
			}

			failed++
			o.logger.Warn("failed to add watch",
				slog.String("path", fsPath), slog.String("error", addErr.Error()))
		} else {
			watched++
		}

		return nil
	})

	// Use Info when failures occurred (operator needs to know), Debug otherwise.
	logLevel := slog.LevelDebug
	if failed > 0 {
		logLevel = slog.LevelInfo
	}

	o.logger.Log(context.Background(), logLevel, "watch setup complete",
		slog.Int("watches_added", watched),
		slog.Int("watches_failed", failed),
	)

	return err
}
