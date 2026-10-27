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
	"syscall"
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

	// recoveryScanDelay is how long after the first missed-change signal the
	// recovery rescan runs. Long enough for an event storm that overflowed
	// the kernel queue to pass, short enough that missed changes are picked
	// up long before the five-minute safety scan.
	recoveryScanDelay = 10 * time.Second

	// hashRequestBufSize is the buffer for the timer → watchLoop channel.
	// Timer callbacks must not block; 256 handles bursts like `git checkout`.
	hashRequestBufSize = 256
)

// Write coalescing limits (see scheduleWriteHash).
const (
	// maxWriteCoalesceWait bounds how long writes that keep arriving can
	// postpone the hash of a path.
	maxWriteCoalesceWait = 30 * time.Second

	// A path that emits unstableWriteEmits content changes within
	// unstableWriteWindow is unstable: its next hash waits
	// unstableWriteInitialBackoff, doubling per further burst up to
	// unstableWriteMaxBackoff.
	unstableWriteEmits          = 3
	unstableWriteWindow         = 2 * time.Minute
	unstableWriteInitialBackoff = time.Minute
	unstableWriteMaxBackoff     = 30 * time.Minute
)

// maxCoalesceRetries caps the number of re-schedule attempts in hashAndEmit
// when errFileChangedDuringHash is returned. Prevents infinite retry if a file
// is being written to continuously.
const maxCoalesceRetries = 3

// hashRequest is sent from timer callbacks to the watchLoop goroutine when a
// write coalesce timer fires and the file should be hashed (B-107).
type hashRequest struct {
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
	HashRequests  chan hashRequest     // timer callback → watchLoop

	// writeChurn holds per-path write history for coalescing deadlines and
	// unstable-file backoff (D-081b). Watch-loop only; pruned at safety scans.
	writeChurn map[string]*pathWriteChurn

	// dirNameCache caches namespace key→original name mappings per directory
	// for O(1) collision lookups. Built lazily on first check; invalidated
	// by Create/Delete events. Single-goroutine access (watchLoop) — no mutex.
	DirNameCache map[string]map[string][]string // dirPath → namekey.Key → []originalNames

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

	// Recovery scan (R-2.8.13). When observation is known to have missed
	// changes -- a kernel queue overflow, a watcher error, a dropped batch or
	// hash request -- marking local truth suspect alone left the missed
	// changes unobserved until the next periodic safety scan, up to five
	// minutes later. A recovery request arms one timer; when it fires the
	// watch loop runs a full rescan. Requests while armed coalesce, so a
	// sustained storm costs one rescan per delay, not one per drop.
	//
	// recoveryScanRequests carries requests from timer callbacks (which must
	// not touch watch-loop state); recoveryScanDue carries the armed timer's
	// firing. Both are buffered(1): one pending signal is the whole message.
	// recoveryScanTimer is watch-loop only.
	recoveryScanRequests chan struct{}
	recoveryScanDue      chan struct{}
	recoveryScanTimer    syncTimer
}

func newLocalWatchState() localWatchState {
	return localWatchState{
		PendingTimers:        make(map[string]syncTimer),
		HashRequests:         make(chan hashRequest, hashRequestBufSize),
		DirNameCache:         make(map[string]map[string][]string),
		RecentLocalDeletes:   make(map[string]struct{}),
		CollisionPeers:       make(map[string]map[string]struct{}),
		excludedSymlinkPaths: make(map[string]struct{}),
		watchedDirs:          make(map[string]struct{}),
		recoveryScanRequests: make(chan struct{}, 1),
		recoveryScanDue:      make(chan struct{}, 1),
	}
}

// watchUnavailableCause names the operating-system limit that left watch
// mode without the watches it needs. Each cause has its own operator fix, so
// the one-time warning names the cause rather than a generic failure.
type watchUnavailableCause int

const (
	// watchCauseWatchLimit: inotify max_user_watches is exhausted (ENOSPC).
	watchCauseWatchLimit watchUnavailableCause = iota + 1
	// watchCauseInstanceLimit: inotify max_user_instances is exhausted
	// (EMFILE from inotify_init1 on Linux).
	watchCauseInstanceLimit
	// watchCauseFileLimit: the process or system open-file limit is reached
	// (EMFILE or ENFILE). On kqueue every watched directory holds a descriptor.
	watchCauseFileLimit
	// watchCauseKernelMemory: the kernel could not allocate a watch (ENOMEM).
	watchCauseKernelMemory
	// watchCauseDescriptorBudget: watching the tree with kqueue would use more
	// than the descriptor budget left for hashing, transfers and SQLite.
	watchCauseDescriptorBudget
)

// localWatchUnavailableError carries the cause of a watch failure from the
// observer to the watch runtime, which falls back to periodic full scans.
// It matches errLocalWatchUnavailable with errors.Is.
type localWatchUnavailableError struct {
	cause watchUnavailableCause
	err   error
}

func (e *localWatchUnavailableError) Error() string {
	if e.err == nil {
		return errLocalWatchUnavailable.Error()
	}

	return errLocalWatchUnavailable.Error() + ": " + e.err.Error()
}

func (e *localWatchUnavailableError) Unwrap() error { return e.err }

func (e *localWatchUnavailableError) Is(target error) bool {
	return target == errLocalWatchUnavailable
}

// watchUnavailableCauseOf returns the cause carried by err, if any.
func watchUnavailableCauseOf(err error) (watchUnavailableCause, bool) {
	var unavailable *localWatchUnavailableError
	if !errors.As(err, &unavailable) {
		return 0, false
	}

	return unavailable.cause, true
}

// classifyWatchAddError maps a failed watcher.Add to the resource limit it
// hit. Any other error (EACCES on one directory, a vanished path) is a
// per-directory failure that the safety scan covers, not a reason to stop
// watching. Before this, only ENOSPC was recognized: EMFILE, ENFILE and ENOMEM
// were logged at Debug and the directories silently went unwatched.
func classifyWatchAddError(err error) (watchUnavailableCause, bool) {
	switch {
	case errors.Is(err, syscall.ENOSPC):
		return watchCauseWatchLimit, true
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return watchCauseFileLimit, true
	case errors.Is(err, syscall.ENOMEM):
		return watchCauseKernelMemory, true
	default:
		return 0, false
	}
}

// classifyWatcherCreateError maps a failed watcher creation to its limit.
// EMFILE from inotify_init1 means max_user_instances on Linux, not the
// open-file limit, so that case is platform-owned (watchCreateEMFILECause).
// Before this, any creation failure ended local observation for the session.
func classifyWatcherCreateError(err error) (watchUnavailableCause, bool) {
	switch {
	case errors.Is(err, syscall.EMFILE):
		return watchCreateEMFILECause, true
	case errors.Is(err, syscall.ENFILE):
		return watchCauseFileLimit, true
	case errors.Is(err, syscall.ENOMEM):
		return watchCauseKernelMemory, true
	default:
		return 0, false
	}
}

// watchUnavailableReason is the user-facing description of a cause.
func watchUnavailableReason(cause watchUnavailableCause) string {
	switch cause {
	case watchCauseWatchLimit:
		return "the system inotify watch limit is reached"
	case watchCauseInstanceLimit:
		return "the system inotify instance limit or the open-file limit is reached"
	case watchCauseFileLimit:
		return "the open-file limit is reached"
	case watchCauseKernelMemory:
		return "the system is out of memory for file watches"
	case watchCauseDescriptorBudget:
		return "watching every folder would use too many open files"
	default:
		return "the system refused to watch the folder"
	}
}

// watchUnavailableFix is the operator action that clears a cause.
func watchUnavailableFix(cause watchUnavailableCause) string {
	switch cause {
	case watchCauseWatchLimit:
		return "sudo sysctl -w fs.inotify.max_user_watches=524288"
	case watchCauseInstanceLimit:
		return "sudo sysctl -w fs.inotify.max_user_instances=1024, or raise the open-file limit (ulimit -n)"
	case watchCauseFileLimit, watchCauseDescriptorBudget:
		return "raise the open-file limit (ulimit -n, or LimitNOFILE= for a service), or exclude large folders with ignored_dirs"
	case watchCauseKernelMemory:
		return "free memory, or exclude large folders with ignored_dirs"
	default:
		return "check the system log"
	}
}

// watchCauseStopsWatching reports whether a mid-session watch failure must
// end watching altogether. A descriptor-limit failure does: on kqueue every
// watched directory holds a descriptor, and continuing to hold them starves
// hashing, uploads and SQLite. The other causes leave existing watches
// healthy, so only new folders fall back to the safety scan.
func watchCauseStopsWatching(cause watchUnavailableCause) bool {
	return cause == watchCauseFileLimit
}

// fsWatcher abstracts filesystem event monitoring. Satisfied by
// *fsnotify.Watcher; tests inject a mock implementation.
type fsWatcher interface {
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

// localObserver walks the local filesystem and produces []ChangeEvent by
// comparing each entry against the in-memory baseline. Stateless — syncRoot
// is a parameter of FullScan, allowing reuse across passes.
type localObserver struct {
	baseline         *Baseline
	logger           *slog.Logger
	checkWorkers     int // parallel hash goroutine limit for FullScan (0 → defaultCheckWorkers)
	filterConfig     ContentFilterConfig
	protectedRoots   []ProtectedRoot
	observationRules LocalObservationRules
	expectedRoot     *syncRootIdentity
	// scanAvailability is FullScan's per-scan state for recording synced
	// paths that are unavailable rather than absent (scanner_unavailable.go).
	// Set and cleared by FullScan; nil outside a scan.
	scanAvailability *scanAvailability
	// watchIdentityLookup caches the identity index the watch handlers use
	// to recognize a synced item renamed to a skipped name; rebuilt every
	// watchIdentityIndexMaxAge. Touched only by the goroutine running Watch.
	// mountBoundaryFlags are the folders recorded as crossed mount points;
	// set by the engine before a scan (SetMountBoundaryFlags).
	mountBoundaryFlags   map[string]struct{}
	watchIdentityLookup  *scanAvailability
	watchIdentityBuiltAt time.Time
	protectedRootEvents  protectedRootEventSink
	watcherFactory       func() (fsWatcher, error)
	droppedEvents        atomic.Int64 // events dropped by TrySend due to full channel
	droppedRetries       atomic.Int64 // hash requests dropped due to full channel
	lastActivityNano     atomic.Int64 // liveness: updated on each event emit (B-125)

	// reportedWatchCauses and safetyScanFailing let the watch loop report a
	// persistent condition once instead of per directory or per scan.
	// Both are touched only by the goroutine running Watch.
	reportedWatchCauses map[watchUnavailableCause]struct{}
	safetyScanFailing   bool

	// watchUnavailable is set by an event handler when a mid-session watch
	// failure means watching must stop (kqueue ran out of descriptors). The
	// watch loop returns it so the runtime falls back to periodic scans and
	// the watcher's descriptors are released for transfers and SQLite.
	// Watch-loop goroutine only.
	watchUnavailable error

	safetyScanInterval time.Duration // 0 → default (5 minutes); configurable (B-099)

	// hashFunc computes the QuickXorHash of a file. Injectable for testing
	// (e.g., to simulate panics in the hash phase).
	hashFunc func(path string) (string, error)

	// clock is the observer's time capability: safety-scan ticks, retry
	// sleeps, and write-coalesce timers all read the same clock, so a test
	// cannot advance one without the others.
	clock           syncClock
	afterSafetyScan func() // test hook: called after safety-scan state reset completes

	writeCoalesceCooldown time.Duration // 0 → defaultWriteCoalesceCooldown; injectable for tests
	startupSafetyScan     bool          // engine watch mode closes the bootstrap-to-fsnotify startup gap

	// skippedCh forwards SkippedItems from safety scans to the engine for
	// observation-issue persistence. Nil disables forwarding (pre-existing behavior).
	// Set via SetSkippedChannel before Watch.
	skippedCh chan<- []skippedItem

	// localBatchCh forwards engine-applied local observation batches. The
	// observer never writes SQLite directly; the watch runtime owns durable
	// application of these batches.
	localBatchCh chan<- localObservationBatch

	// localWatchState owns all watch-loop mutable state. It is embedded so
	// same-package tests can still reach the existing fields directly while the
	// runtime contract stays grouped under one owner.
	localWatchState
}

// newLocalObserver creates a LocalObserver. checkWorkers controls the number
// of parallel goroutines used for file hashing during FullScan (0 → default 4).
// The baseline must be loaded (from SyncStore.Load); it is read-only
// during observation.
func newLocalObserver(baseline *Baseline, logger *slog.Logger, checkWorkers int) *localObserver {
	return &localObserver{
		baseline:        baseline,
		logger:          logger,
		checkWorkers:    checkWorkers,
		hashFunc:        driveops.ComputeQuickXorHash,
		clock:           realClock{},
		localWatchState: newLocalWatchState(),
		watcherFactory: func() (fsWatcher, error) {
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
func (o *localObserver) SetSkippedChannel(ch chan<- []skippedItem) {
	o.skippedCh = ch
}

func (o *localObserver) SetLocalObservationBatchChannel(ch chan<- localObservationBatch) {
	o.localBatchCh = ch
}

// SetFilterConfig installs per-drive sync content filters. The observer copies
// slices so later config mutations cannot silently change an already-running
// watch/scanner.
func (o *localObserver) SetFilterConfig(cfg ContentFilterConfig) {
	o.filterConfig = ContentFilterConfig{
		IgnoredDirs:      append([]string(nil), cfg.IgnoredDirs...),
		IncludedDirs:     append([]string(nil), cfg.IncludedDirs...),
		IgnoredPaths:     append([]string(nil), cfg.IgnoredPaths...),
		IgnoreDotfiles:   cfg.IgnoreDotfiles,
		IgnoreJunkFiles:  cfg.IgnoreJunkFiles,
		FollowSymlinks:   cfg.FollowSymlinks,
		CrossFilesystems: cfg.CrossFilesystems,
	}
}

// SetProtectedRoots installs engine-derived shortcut boundary protection. The
// roots are derived from parent shortcut_roots state, not user filter config.
func (o *localObserver) SetProtectedRoots(roots []ProtectedRoot) {
	o.protectedRoots = append([]ProtectedRoot(nil), roots...)
}

// SetProtectedRootEventSink installs the parent watch-runtime notification sink
// used for protected-root lifecycle facts. The observer never mutates those
// roots; it only reports that the parent engine should reconcile.
func (o *localObserver) SetProtectedRootEventSink(sink protectedRootEventSink) {
	o.protectedRootEvents = sink
}

// SetObservationRules installs platform-derived local validation rules. These
// stay separate from user-configured filter knobs so drive semantics do not
// get conflated with local exclusions.
func (o *localObserver) SetObservationRules(rules LocalObservationRules) {
	o.observationRules = rules
}

// SetMountBoundaryFlags installs the folders previously recorded as crossed
// mount points.
func (o *localObserver) SetMountBoundaryFlags(paths []string) {
	o.mountBoundaryFlags = make(map[string]struct{}, len(paths))
	for _, flagged := range paths {
		o.mountBoundaryFlags[flagged] = struct{}{}
	}
}

// SetExpectedRootIdentity validates scans against one fixed identity.
func (o *localObserver) SetExpectedRootIdentity(identity *synctree.RootIdentity) {
	if identity == nil {
		o.expectedRoot = nil
		return
	}
	o.expectedRoot = fixedSyncRootIdentity(identity)
}

// SetSyncRootIdentity validates scans against the engine's shared identity,
// so a binding made while the observer runs is enforced at once.
func (o *localObserver) SetSyncRootIdentity(identity *syncRootIdentity) {
	o.expectedRoot = identity
}

func (o *localObserver) reportProtectedRootEvent(event protectedRootEvent) {
	if o == nil || o.protectedRootEvents == nil {
		return
	}
	o.protectedRootEvents(event)
}

func (o *localObserver) recordPendingTimer(path string, timer syncTimer) {
	if o.PendingTimers == nil {
		o.PendingTimers = make(map[string]syncTimer)
	}

	o.PendingTimers[path] = timer
}

func (o *localObserver) cancelPendingTimer(path string) {
	if timer, ok := o.PendingTimers[path]; ok {
		timer.Stop()
		delete(o.PendingTimers, path)
	}
	if churn, ok := o.writeChurn[path]; ok {
		churn.firstPendingAt = time.Time{}
	}
}

// SetWatcherFactory overrides the default fsnotify watcher factory. Used by
// tests to inject a mock factory that simulates inotify watch limit exhaustion
// (ENOSPC) or other platform-specific failure modes.
func (o *localObserver) SetWatcherFactory(fn func() (fsWatcher, error)) {
	o.watcherFactory = fn
}

// TrySend sends a ChangeEvent to the events channel without blocking. If the
// channel is full, the event is dropped and logged at Warn. The safety scan
// (every 5 minutes) catches any dropped events, providing eventual consistency.
func (o *localObserver) TrySend(ctx context.Context, events chan<- changeEvent, ev *changeEvent) {
	if ev != nil {
		o.TrySendLocalObservationBatch(ctx, localObservationBatchForEvent(ev))
	}
	o.TrySendChangeEventOnly(ctx, events, ev)
}

func (o *localObserver) TrySendChangeEventOnly(ctx context.Context, events chan<- changeEvent, ev *changeEvent) {
	if events == nil || ev == nil {
		return
	}

	select {
	case events <- *ev:
		o.recordActivity()
	case <-ctx.Done():
	default:
		// Debug per event: a bulk copy can drop thousands. droppedEvents
		// aggregates them and the maintenance tick schedules one rescan.
		o.droppedEvents.Add(1)
		o.requestRecoveryScan()
		o.logger.Debug("event channel full, dropping event (recovery scan will catch up)",
			slog.String("path", ev.Path),
			slog.String("type", ev.Type.String()),
		)
	}
}

func (o *localObserver) TrySendLocalObservationBatch(ctx context.Context, batch localObservationBatch) {
	if o.localBatchCh == nil {
		return
	}

	select {
	case o.localBatchCh <- batch:
		o.recordActivity()
	case <-ctx.Done():
	default:
		o.droppedEvents.Add(1)
		o.requestRecoveryScan()
		o.logger.Debug("local observation batch channel full, marking local truth suspect on maintenance",
			slog.Bool("full_snapshot", batch.fullSnapshot),
			slog.Int("rows", len(batch.rows)),
			slog.Int("deleted_paths", len(batch.deletedPaths)),
			slog.Int("deleted_prefixes", len(batch.deletedPrefixes)),
		)
	}
}

func (o *localObserver) TrySendLocalTruthSuspect(ctx context.Context, reason string) {
	o.TrySendLocalObservationBatch(ctx, localObservationBatch{
		markSuspect:    true,
		recoveryReason: reason,
	})
}

func (o *localObserver) TrySendFullLocalSnapshot(ctx context.Context, result scanResult) {
	o.TrySendLocalObservationBatch(ctx, localObservationBatch{
		fullSnapshot: true,
		rows:         buildLocalStateRows(result),
		dirty:        len(result.Events) > 0 || len(result.Skipped) > 0,
	})
}

// requestRecoveryScan asks the watch loop for an early full rescan. Safe from
// any goroutine: it only signals. A request while one is pending coalesces.
func (o *localObserver) requestRecoveryScan() {
	select {
	case o.recoveryScanRequests <- struct{}{}:
	default:
	}
}

// armRecoveryScan starts the recovery-scan timer unless one is already
// armed. Watch-loop goroutine only.
func (o *localObserver) armRecoveryScan() {
	if o.recoveryScanTimer != nil || o.recoveryScanDue == nil {
		return
	}

	due := o.recoveryScanDue
	o.recoveryScanTimer = o.clock.AfterFunc(recoveryScanDelay, func() {
		select {
		case due <- struct{}{}:
		default:
		}
	})
}

// cancelRecoveryScan disarms a pending recovery scan, including one whose
// timer already fired. Watch-loop goroutine only.
func (o *localObserver) cancelRecoveryScan() {
	if o.recoveryScanTimer != nil {
		o.recoveryScanTimer.Stop()
		o.recoveryScanTimer = nil
	}

	select {
	case <-o.recoveryScanDue:
	default:
	}
}

// DroppedEvents returns the cumulative number of events dropped by TrySend
// due to a full channel. Production code uses ResetDroppedEvents for per-pass
// reporting; this accessor is retained for tests and diagnostics.
func (o *localObserver) DroppedEvents() int64 {
	return o.droppedEvents.Load()
}

// ResetDroppedEvents atomically reads and resets the drop counter to zero.
// Returns the number of events dropped since the last reset. Used by the
// engine to log per-pass drops without double-counting across passes.
func (o *localObserver) ResetDroppedEvents() int64 {
	return o.droppedEvents.Swap(0)
}

// DroppedRetries returns the cumulative number of hash requests dropped
// because the HashRequests channel was full. Safety scans catch these.
func (o *localObserver) DroppedRetries() int64 {
	return o.droppedRetries.Load()
}

func (o *localObserver) ResetDroppedRetries() int64 {
	return o.droppedRetries.Swap(0)
}

// LastActivity returns the time of the most recent event emission.
// Returns zero time if no events have been emitted. Thread-safe (B-125).
func (o *localObserver) LastActivity() time.Time {
	nano := o.lastActivityNano.Load()
	if nano == 0 {
		return time.Time{}
	}

	return time.Unix(0, nano)
}

// recordActivity updates the liveness timestamp to now.
func (o *localObserver) recordActivity() {
	o.lastActivityNano.Store(time.Now().UnixNano())
}

// EstimateDirCount returns the estimated number of directories that will need
// inotify watches. Counts ItemTypeFolder entries in baseline plus one for the
// sync root itself.
func (o *localObserver) EstimateDirCount() int {
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
// cases).
//
// Channel sizing (B-114): The events channel should be buffered (recommended
// size: 256). An unbuffered channel blocks on every event. If the channel is
// full, TrySend drops the event and increments the drop counter — the safety
// scan provides eventual consistency for any dropped events.
func (o *localObserver) Watch(ctx context.Context, tree *synctree.Root, events chan<- changeEvent) error {
	syncRoot := tree.Path()
	o.logger.Info("local observer starting watch",
		slog.String("sync_root", syncRoot),
	)

	watcher, err := o.watcherFactory()
	if err != nil {
		if cause, ok := classifyWatcherCreateError(err); ok {
			return &localWatchUnavailableError{cause: cause, err: fmt.Errorf("create filesystem watcher: %w", err)}
		}

		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer watcher.Close() //nolint:errcheck // watcher shutdown releases OS resources and reports nothing a caller can act on

	defer o.cancelPendingTimers()
	defer o.cancelRecoveryScan()

	// Pre-flight capacity check (Linux only; no-op on other platforms).
	checkInotifyCapacity(o.EstimateDirCount(), o.logger)

	// Walk the sync root to add watches on all existing directories.
	if walkErr := o.AddWatchesRecursive(ctx, watcher, tree); walkErr != nil {
		// A resource-limit failure keeps its typed cause so the watch runtime
		// can name the limit in its one-time warning.
		return fmt.Errorf("add initial watches: %w", walkErr)
	}
	if o.startupSafetyScan {
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
func (o *localObserver) cancelPendingTimers() {
	for path, timer := range o.PendingTimers {
		timer.Stop()
		delete(o.PendingTimers, path)
	}
}

func isFatalWatchSetupError(err error) bool {
	return errors.Is(err, errLocalWatchUnavailable) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (o *localObserver) rollbackAddedWatches(watcher fsWatcher, session *watchAddSession) {
	if session == nil || len(session.added) == 0 {
		return
	}

	for i := len(session.added) - 1; i >= 0; i-- {
		path := session.added[i]
		if err := watcher.Remove(path); err != nil {
			// Debug: rollback also runs on shutdown, and the watcher is
			// being discarded either way.
			o.logger.Debug("watch setup rollback: failed to remove watch",
				slog.String("path", path),
				slog.String("error", err.Error()))
		}

		delete(o.watchedDirs, path)
	}
}

// AddWatchesRecursive walks the sync root and adds a watch on every directory.
func (o *localObserver) AddWatchesRecursive(ctx context.Context, watcher fsWatcher, tree *synctree.Root) error {
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

	o.logger.Log(ctx, logLevel, "watch setup complete",
		slog.Int("watches_added", counts.watched),
		slog.Int("watches_failed", counts.failed),
	)

	if err != nil {
		return fmt.Errorf("walk watch setup: %w", err)
	}

	return nil
}
