// Package sync owns the single mounted content-root runtime, including local observation.
//
// Contents:
//   - LocalObserver struct + NewLocalObserver constructor
//   - FsWatcher interface + fsnotifyWrapper adapter
//   - Watch() entry point + AddWatchesRecursive
//   - Event channel management (TrySend, DroppedEvents, LastActivity)
//
// Related files:
//   - observer_local_handlers.go:  watch event loop + fsnotify event handlers
//   - observer_local_collisions.go: case collision detection helpers (cache, peers)
//   - scanner.go:                   FullScan, walk/hash/filter logic
package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Constants for the local observer (watch mode).
const (
	safetyScanInterval = 5 * time.Minute

	// defaultWriteCoalesceCooldown is the per-path quiescence window for
	// write coalescing (B-107). Multiple Write events within this window are
	// coalesced into a single hash + emit.
	defaultWriteCoalesceCooldown = 500 * time.Millisecond

	// HashRequestBufSize is the buffer for the timer → watchLoop channel.
	// Timer callbacks must not block; 256 handles bursts like `git checkout`.
	HashRequestBufSize = 256
)

// MaxCoalesceRetries caps the number of re-schedule attempts in hashAndEmit
// when errFileChangedDuringHash is returned. Prevents infinite retry if a file
// is being written to continuously.
const MaxCoalesceRetries = 3

// HashRequest is sent from timer callbacks to the watchLoop goroutine when a
// write coalesce timer fires and the file should be hashed (B-107).
type HashRequest struct {
	FsPath    string
	DbRelPath string
	Name      string
	Retries   int // number of re-schedules already attempted
}

// localWatchState is the single mutable owner for one LocalObserver watch
// session. The surrounding LocalObserver keeps immutable dependencies and
// long-lived configuration; timer queues, scope generations, watch
// registrations, and collision caches stay grouped here so watch-mode state
// does not sprawl across unrelated fields.
type localWatchState struct {
	// Write coalescing fields (B-107). Single watch-loop owner.
	PendingTimers map[string]syncTimer // per-path timers; watchLoop-only (no mutex needed)
	HashRequests  chan HashRequest     // timer callback → watchLoop

	// dirNameCache caches lowercase→original name mappings per directory for
	// O(1) case collision lookups. Built lazily on first check; invalidated
	// by Create/Delete events. Single-goroutine access (watchLoop) — no mutex.
	DirNameCache map[string]map[string][]string // dirPath → lowName → []originalNames

	// recentLocalDeletes tracks dbRelPaths of files deleted locally in the
	// current watch session. Used to suppress false-positive baseline
	// collisions during case-only renames: the Delete event removes the
	// old name but baseline isn't updated until the action executes (async).
	// Cleared on safety scan. Single-goroutine access (watchLoop).
	RecentLocalDeletes map[string]struct{}

	// collisionPeers tracks N-way collision relationships detected in watch
	// mode. When a collider is deleted, all surviving peers are re-emitted
	// via handleCreate (which re-checks and re-records any remaining collisions).
	// Cleared on safety scan (authoritative DetectCaseCollisions replaces this).
	// Single-goroutine access (watchLoop) — no mutex.
	CollisionPeers map[string]map[string]struct{} // dbRelPath → set of peer dbRelPaths

	// excludedSymlinkPaths tracks alias paths that local observation excluded
	// under the built-in symlink safety rules. Delete detection consults this set so a
	// silently excluded symlink does not later reappear as a synthetic delete.
	// Paths stay recorded until the same alias is observed as a real file/dir
	// again or the observer is recreated. Single-goroutine access only.
	excludedSymlinkPaths map[string]struct{}

	// watchedDirs tracks fsnotify registrations owned by this observer. The
	// watch loop is the single writer, so no mutex is needed.
	watchedDirs map[string]struct{}
}

func newLocalWatchState() localWatchState {
	return localWatchState{
		PendingTimers:        make(map[string]syncTimer),
		HashRequests:         make(chan HashRequest, HashRequestBufSize),
		DirNameCache:         make(map[string]map[string][]string),
		RecentLocalDeletes:   make(map[string]struct{}),
		CollisionPeers:       make(map[string]map[string]struct{}),
		excludedSymlinkPaths: make(map[string]struct{}),
		watchedDirs:          make(map[string]struct{}),
	}
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

func (fw *fsnotifyWrapper) Add(name string) error {
	if err := fw.w.Add(name); err != nil {
		return fmt.Errorf("add watch %q: %w", name, err)
	}

	return nil
}

func (fw *fsnotifyWrapper) Remove(name string) error {
	if err := fw.w.Remove(name); err != nil {
		return fmt.Errorf("remove watch %q: %w", name, err)
	}

	return nil
}

func (fw *fsnotifyWrapper) Close() error {
	if err := fw.w.Close(); err != nil {
		return fmt.Errorf("close watcher: %w", err)
	}

	return nil
}

func (fw *fsnotifyWrapper) Events() <-chan fsnotify.Event { return fw.w.Events }
func (fw *fsnotifyWrapper) Errors() <-chan error          { return fw.w.Errors }

// LocalObserver walks the local filesystem and produces []ChangeEvent by
// comparing each entry against the in-memory baseline. Stateless — syncRoot
// is a parameter of FullScan, allowing reuse across passes.
type LocalObserver struct {
	Baseline           *Baseline
	Logger             *slog.Logger
	checkWorkers       int // parallel hash goroutine limit for FullScan (0 → defaultCheckWorkers)
	filterConfig       LocalFilterConfig
	observationRules   LocalObservationRules
	expectedRootID     *synctree.FileIdentity
	managedRootEvents  ManagedRootEventSink
	WatcherFactory     func() (FsWatcher, error)
	droppedEvents      atomic.Int64                                     // events dropped by TrySend due to full channel
	droppedRetries     atomic.Int64                                     // hash requests dropped due to full channel
	lastActivityNano   atomic.Int64                                     // liveness: updated on each event emit (B-125)
	SleepFunc          func(ctx context.Context, d time.Duration) error // injectable for testing
	SafetyTickFunc     func(d time.Duration) (<-chan time.Time, func()) // injectable for testing; returns tick channel + stop func
	safetyScanInterval time.Duration                                    // 0 → default (5 minutes); configurable (B-099)

	// hashFunc computes the QuickXorHash of a file. Injectable for testing
	// (e.g., to simulate panics in the hash phase).
	HashFunc func(path string) (string, error)

	AfterFunc       func(delay time.Duration, fn func()) syncTimer // injectable for deterministic tests
	AfterSafetyScan func()                                         // test hook: called after safety-scan state reset completes

	WriteCoalesceCooldown time.Duration // 0 → defaultWriteCoalesceCooldown; injectable for tests
	StartupSafetyScan     bool          // engine watch mode closes the bootstrap-to-fsnotify startup gap

	// skippedCh forwards SkippedItems from safety scans to the engine for
	// observation-issue persistence. Nil disables forwarding (pre-existing behavior).
	// Set via SetSkippedChannel before Watch.
	skippedCh chan<- []SkippedItem

	// localWatchState owns all watch-loop mutable state. It is embedded so
	// same-package tests can still reach the existing fields directly while the
	// runtime contract stays grouped under one owner.
	localWatchState
}

// NewLocalObserver creates a LocalObserver. checkWorkers controls the number
// of parallel goroutines used for file hashing during FullScan (0 → default 4).
// The baseline must be loaded (from SyncStore.Load); it is read-only
// during observation.
func NewLocalObserver(baseline *Baseline, logger *slog.Logger, checkWorkers int) *LocalObserver {
	return &LocalObserver{
		Baseline:        baseline,
		Logger:          logger,
		checkWorkers:    checkWorkers,
		HashFunc:        driveops.ComputeQuickXorHash,
		AfterFunc:       realAfterFunc,
		SleepFunc:       TimeSleep,
		localWatchState: newLocalWatchState(),
		SafetyTickFunc: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		WatcherFactory: func() (FsWatcher, error) {
			w, err := fsnotify.NewWatcher()
			if err != nil {
				return nil, fmt.Errorf("create fsnotify watcher: %w", err)
			}
			return &fsnotifyWrapper{w: w}, nil
		},
	}
}

// SetSkippedChannel sets the channel for forwarding SkippedItems from safety
// scans to the engine. Must be called before Watch. Nil disables forwarding.
func (o *LocalObserver) SetSkippedChannel(ch chan<- []SkippedItem) {
	o.skippedCh = ch
}

// SetFilterConfig installs user-configured local observation filters. The
// observer copies the slices so later config mutations cannot silently change
// an already-running watch/scanner.
func (o *LocalObserver) SetFilterConfig(cfg LocalFilterConfig) {
	o.filterConfig = LocalFilterConfig{
		SkipDotfiles: cfg.SkipDotfiles,
		SkipSymlinks: cfg.SkipSymlinks,
		SkipDirs:     append([]string(nil), cfg.SkipDirs...),
		SkipFiles:    append([]string(nil), cfg.SkipFiles...),
		ManagedRoots: append([]ManagedRootReservation(nil), cfg.ManagedRoots...),
	}
}

// SetManagedRootEventSink installs the parent watch-runtime notification sink
// used for managed-root lifecycle facts. The observer never mutates those
// roots; it only reports that the parent engine should reconcile.
func (o *LocalObserver) SetManagedRootEventSink(sink ManagedRootEventSink) {
	o.managedRootEvents = sink
}

// SetObservationRules installs platform-derived local validation rules. These
// stay separate from user-configured filter knobs so drive semantics do not
// get conflated with local exclusions.
func (o *LocalObserver) SetObservationRules(rules LocalObservationRules) {
	o.observationRules = rules
}

func (o *LocalObserver) SetExpectedRootIdentity(identity *synctree.FileIdentity) {
	if identity == nil {
		o.expectedRootID = nil
		return
	}
	next := *identity
	o.expectedRootID = &next
}

func (o *LocalObserver) reportManagedRootEvent(event ManagedRootEvent) {
	if o == nil || o.managedRootEvents == nil {
		return
	}
	o.managedRootEvents(event)
}

func (o *LocalObserver) recordPendingTimer(path string, timer syncTimer) {
	if o.PendingTimers == nil {
		o.PendingTimers = make(map[string]syncTimer)
	}

	o.PendingTimers[path] = timer
}

func (o *LocalObserver) cancelPendingTimer(path string) {
	if timer, ok := o.PendingTimers[path]; ok {
		timer.Stop()
		delete(o.PendingTimers, path)
	}
}

// SetWatcherFactory overrides the default fsnotify watcher factory. Used by
// tests to inject a mock factory that simulates inotify watch limit exhaustion
// (ENOSPC) or other platform-specific failure modes.
func (o *LocalObserver) SetWatcherFactory(fn func() (FsWatcher, error)) {
	o.WatcherFactory = fn
}

// TrySend sends a ChangeEvent to the events channel without blocking. If the
// channel is full, the event is dropped and logged at Warn. The safety scan
// (every 5 minutes) catches any dropped events, providing eventual consistency.
func (o *LocalObserver) TrySend(ctx context.Context, events chan<- ChangeEvent, ev *ChangeEvent) {
	select {
	case events <- *ev:
		o.recordActivity()
	case <-ctx.Done():
	default:
		o.droppedEvents.Add(1)
		o.Logger.Warn("event channel full, dropping event (safety scan will catch up)",
			slog.String("path", ev.Path),
			slog.String("type", ev.Type.String()),
		)
	}
}

// DroppedEvents returns the cumulative number of events dropped by TrySend
// due to a full channel. Production code uses ResetDroppedEvents for per-pass
// reporting; this accessor is retained for tests and diagnostics.
func (o *LocalObserver) DroppedEvents() int64 {
	return o.droppedEvents.Load()
}

// ResetDroppedEvents atomically reads and resets the drop counter to zero.
// Returns the number of events dropped since the last reset. Used by the
// engine to log per-pass drops without double-counting across passes.
func (o *LocalObserver) ResetDroppedEvents() int64 {
	return o.droppedEvents.Swap(0)
}

// DroppedRetries returns the cumulative number of hash requests dropped
// because the HashRequests channel was full. Safety scans catch these.
func (o *LocalObserver) DroppedRetries() int64 {
	return o.droppedRetries.Load()
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

// EstimateDirCount returns the estimated number of directories that will need
// inotify watches. Counts ItemTypeFolder entries in baseline plus one for the
// sync root itself.
func (o *LocalObserver) EstimateDirCount() int {
	count := 1 // sync root always needs a watch

	o.Baseline.ForEachPath(func(_ string, entry *BaselineEntry) {
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
// full, TrySend drops the event and increments the drop counter — the safety
// scan provides eventual consistency for any dropped events.
func (o *LocalObserver) Watch(ctx context.Context, tree *synctree.Root, events chan<- ChangeEvent) error {
	syncRoot := tree.Path()
	o.Logger.Info("local observer starting watch",
		slog.String("sync_root", syncRoot),
	)

	// Guard: abort if .nosync file is present.
	if _, err := tree.Stat(nosyncFileName); err == nil {
		o.Logger.Warn("nosync guard file detected, aborting watch",
			slog.String("sync_root", syncRoot))

		return ErrNosyncGuard
	}

	watcher, err := o.WatcherFactory()
	if err != nil {
		return fmt.Errorf("sync: creating filesystem watcher: %w", err)
	}
	defer watcher.Close()

	defer o.cancelPendingTimers()

	// Pre-flight capacity check (Linux only; no-op on other platforms).
	CheckInotifyCapacity(o.EstimateDirCount(), o.Logger)

	// Walk the sync root to add watches on all existing directories.
	if walkErr := o.AddWatchesRecursive(ctx, watcher, tree); walkErr != nil {
		if errors.Is(walkErr, ErrWatchLimitExhausted) {
			return ErrWatchLimitExhausted
		}

		return fmt.Errorf("sync: adding initial watches: %w", walkErr)
	}
	if o.StartupSafetyScan {
		// Close the startup gap between the bootstrap local scan and the point at
		// which fsnotify watches are fully installed. Any local change that lands in
		// that window is observed here; subsequent changes are covered by the
		// installed watcher or the periodic safety scan.
		o.runSafetyScan(ctx, tree, events)
	}

	return o.watchLoop(ctx, watcher, tree, events)
}

// cancelPendingTimers stops and clears all pending write coalesce timers.
// Called on watchLoop exit to prevent timer callbacks sending to a closed channel.
//
// Deleting map entries during range iteration is safe in Go — the spec
// guarantees that entries added during iteration may or may not be visited,
// and deletion of unvisited entries is well-defined. See go.dev/ref/spec#For_range.
func (o *LocalObserver) cancelPendingTimers() {
	for path, timer := range o.PendingTimers {
		timer.Stop()
		delete(o.PendingTimers, path)
	}
}

func isFatalWatchSetupError(err error) bool {
	return errors.Is(err, ErrWatchLimitExhausted) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (o *LocalObserver) rollbackAddedWatches(watcher FsWatcher, session *watchAddSession) {
	if session == nil || len(session.added) == 0 {
		return
	}

	for i := len(session.added) - 1; i >= 0; i-- {
		path := session.added[i]
		if err := watcher.Remove(path); err != nil {
			o.Logger.Warn("watch setup rollback: failed to remove watch",
				slog.String("path", path),
				slog.String("error", err.Error()))
		}

		delete(o.watchedDirs, path)
	}
}

// AddWatchesRecursive walks the sync root and adds a watch on every directory.
func (o *LocalObserver) AddWatchesRecursive(ctx context.Context, watcher FsWatcher, tree *synctree.Root) error {
	syncRoot := tree.Path()
	counts := &watchSetupCounts{}
	session := newWatchAddSession()
	err := o.addObservedDirWatches(ctx, watcher, syncRoot, ".", counts, make(map[string]struct{}), session)
	if err != nil && isFatalWatchSetupError(err) {
		o.rollbackAddedWatches(watcher, session)
	}

	// Use Info when failures occurred (operator needs to know), Debug otherwise.
	logLevel := slog.LevelDebug
	if counts.failed > 0 {
		logLevel = slog.LevelInfo
	}

	o.Logger.Log(ctx, logLevel, "watch setup complete",
		slog.Int("watches_added", counts.watched),
		slog.Int("watches_failed", counts.failed),
	)

	if err != nil {
		return fmt.Errorf("walk watch setup: %w", err)
	}

	return nil
}
