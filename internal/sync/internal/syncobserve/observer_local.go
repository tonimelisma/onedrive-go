// The local observer of the mounted content-root runtime: its type, the
// filesystem watcher seam and the watch entry point.
//
// Contents:
//   - LocalObserver struct + NewLocalObserver constructor
//   - FsWatcher interface + fsnotifyWrapper adapter
//   - Watch() entry point + AddWatchesRecursive
//   - Observation sending (TrySend, TrySendLocalObservationBatch, DroppedEvents)
//
// Related files:
//   - observer_local_handlers.go:  watch event loop + fsnotify event handlers
//   - observer_local_collisions.go: case collision detection helpers (cache, peers)
//   - scanner.go:                   FullScan, walk/hash/filter logic

package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
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

	// HashRequestBufSize is the buffer for the timer → watchLoop channel.
	// Timer callbacks must not block; 256 handles bursts like `git checkout`.
	HashRequestBufSize = 256
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
// when ErrFileChangedDuringHash is returned. Prevents infinite retry if a file
// is being written to continuously.
const maxCoalesceRetries = 3

// HashRequest is sent from timer callbacks to the watchLoop goroutine when a
// write coalesce timer fires and the file should be hashed (B-107).
type HashRequest struct {
	FsPath    string
	DbRelPath string
	Retries   int // number of re-schedules already attempted
}

// localWatchState is the single mutable owner for one LocalObserver watch
// session. The surrounding LocalObserver keeps immutable dependencies and
// long-lived configuration; timer queues, scope generations, watch
// registrations, and collision caches stay grouped here so watch-mode state
// does not sprawl across unrelated fields.
type localWatchState struct {
	// Write coalescing fields (B-107). Single watch-loop owner.
	PendingTimers map[string]syncdomain.SyncTimer // per-path timers; watchLoop-only (no mutex needed)
	HashRequests  chan HashRequest                // timer callback → watchLoop

	// writeChurn holds per-path write history for coalescing deadlines and
	// unstable-file backoff (D-081b). Watch-loop only; pruned at safety scans.
	writeChurn map[string]*pathWriteChurn

	// excludedSymlinkPaths tracks alias paths that local observation excluded
	// under the built-in symlink safety rules. Delete detection consults this set so a
	// silently excluded symlink does not later reappear as a synthetic delete.
	// Paths stay recorded until the same alias is observed as a real file/dir
	// again or the observer is recreated. Single-goroutine access only.
	excludedSymlinkPaths map[string]struct{}

	// watchedDirs tracks fsnotify registrations owned by this observer. The
	// watch loop is the single writer, so no mutex is needed.
	watchedDirs watchRegistry

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
	// generation when it fires. Both are buffered(1): one pending signal is
	// the whole message. The rest is watch-loop only: recoveryScanGen is
	// bumped whenever the armed timer is canceled, so a timer that fires
	// after Stop (a real timer can) delivers a stale generation that is
	// ignored rather than a token that triggers an extra scan.
	recoveryScanRequests chan struct{}
	recoveryScanDue      chan uint64
	recoveryScanTimer    syncdomain.SyncTimer
	recoveryScanGen      uint64

	// Recovery-scan pacing (see recoveryScanDelayNow). lastFullScanDuration
	// is how long the most recent full scan took; lastRecoveryDelay and
	// lastRecoveryScanEnd let a request arriving soon after a recovery scan
	// double the next delay.
	lastFullScanDuration time.Duration
	lastRecoveryDelay    time.Duration
	lastRecoveryScanEnd  time.Time
}

func newLocalWatchState() localWatchState {
	return localWatchState{
		PendingTimers:        make(map[string]syncdomain.SyncTimer),
		HashRequests:         make(chan HashRequest, HashRequestBufSize),
		excludedSymlinkPaths: make(map[string]struct{}),
		recoveryScanRequests: make(chan struct{}, 1),
		recoveryScanDue:      make(chan uint64, 1),
	}
}

// WatchUnavailableCause names the operating-system limit that left watch
// mode without the watches it needs. Each cause has its own operator fix, so
// the one-time warning names the cause rather than a generic failure.
type WatchUnavailableCause int

const (
	// watchCauseWatchLimit: inotify max_user_watches is exhausted (ENOSPC).
	watchCauseWatchLimit WatchUnavailableCause = iota + 1
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
// It matches ErrLocalWatchUnavailable with errors.Is.
type localWatchUnavailableError struct {
	cause WatchUnavailableCause
	err   error
}

func (e *localWatchUnavailableError) Error() string {
	if e.err == nil {
		return syncdomain.ErrLocalWatchUnavailable.Error()
	}

	return syncdomain.ErrLocalWatchUnavailable.Error() + ": " + e.err.Error()
}

func (e *localWatchUnavailableError) Unwrap() error { return e.err }

func (e *localWatchUnavailableError) Is(target error) bool {
	return target == syncdomain.ErrLocalWatchUnavailable
}

// WatchUnavailableCauseOf returns the cause carried by err, if any.
func WatchUnavailableCauseOf(err error) (WatchUnavailableCause, bool) {
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
func classifyWatchAddError(err error) (WatchUnavailableCause, bool) {
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
func classifyWatcherCreateError(err error) (WatchUnavailableCause, bool) {
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

// WatchUnavailableReason is the user-facing description of a cause.
func WatchUnavailableReason(cause WatchUnavailableCause) string {
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

// WatchUnavailableFix is the operator action that clears a cause.
func WatchUnavailableFix(cause WatchUnavailableCause) string {
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
func watchCauseStopsWatching(cause WatchUnavailableCause) bool {
	return cause == watchCauseFileLimit
}

// watchedDir is one directory registration: the identity the directory had
// when it was registered, so a repeated Create of the same directory can be
// told apart from a new directory at the same path.
type watchedDir struct {
	identity    synctree.FileIdentity
	hasIdentity bool
}

// watchRegistry is the observer's record of its fsnotify registrations, keyed
// by cleaned filesystem path, with a parent-to-children index so a directory's
// registered subtree is found in time proportional to the subtree rather than
// to every watch (a linear scan over all watches ran on every directory create
// and delete). The index also links unregistered intermediate directories
// (one whose own Add failed) so registered descendants below them stay
// reachable. Watch-loop goroutine only; the zero value is ready to use.
type watchRegistry struct {
	dirs     map[string]watchedDir
	children map[string]map[string]struct{}
}

func (r *watchRegistry) add(path string, info os.FileInfo) {
	path = filepath.Clean(path)
	if r.dirs == nil {
		r.dirs = make(map[string]watchedDir)
		r.children = make(map[string]map[string]struct{})
	}

	entry := watchedDir{}
	if info != nil {
		entry.identity, entry.hasIdentity = synctree.IdentityFromFileInfo(info)
	}
	r.dirs[path] = entry

	// Link the path to its ancestors until an existing link is found.
	for child := path; ; {
		parent := filepath.Dir(child)
		if parent == child {
			return
		}
		siblings := r.children[parent]
		if siblings == nil {
			siblings = make(map[string]struct{})
			r.children[parent] = siblings
		}
		if _, linked := siblings[child]; linked {
			return
		}
		siblings[child] = struct{}{}
		child = parent
	}
}

func (r *watchRegistry) get(path string) (watchedDir, bool) {
	entry, ok := r.dirs[filepath.Clean(path)]
	return entry, ok
}

func (r *watchRegistry) has(path string) bool {
	_, ok := r.get(path)
	return ok
}

func (r *watchRegistry) remove(path string) {
	path = filepath.Clean(path)
	delete(r.dirs, path)
	r.unlinkIfUnused(path)
}

// unlinkIfUnused drops path's link from its parent when nothing registered
// remains at or below it, and repeats upward.
func (r *watchRegistry) unlinkIfUnused(path string) {
	for {
		if _, registered := r.dirs[path]; registered || len(r.children[path]) > 0 {
			return
		}
		delete(r.children, path)
		parent := filepath.Dir(path)
		if parent == path {
			return
		}
		siblings := r.children[parent]
		if _, linked := siblings[path]; !linked {
			return
		}
		delete(siblings, path)
		path = parent
	}
}

// descendants returns the registered directories strictly beneath path,
// children before their parents (post-order), siblings in path order.
func (r *watchRegistry) descendants(path string) []string {
	var out []string
	var walk func(string)
	walk = func(dir string) {
		kids := make([]string, 0, len(r.children[dir]))
		for kid := range r.children[dir] {
			kids = append(kids, kid)
		}
		slices.Sort(kids)
		for _, kid := range kids {
			walk(kid)
			if _, registered := r.dirs[kid]; registered {
				out = append(out, kid)
			}
		}
	}
	walk(filepath.Clean(path))

	return out
}

// paths lists every registered directory, for tests and diagnostics.
func (r *watchRegistry) paths() []string {
	out := make([]string, 0, len(r.dirs))
	for path := range r.dirs {
		out = append(out, path)
	}
	slices.Sort(out)

	return out
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

// LocalObserver observes the local filesystem: full scans that return every
// row, and watch handlers that send patches to the batch channel. It compares
// entries against the in-memory baseline only to skip re-reading unchanged
// content. Stateless — syncRoot is a parameter of FullScan, allowing reuse
// across passes.
type LocalObserver struct {
	baseline syncdomain.BaselineView
	logger   *slog.Logger
	// hashCache, when set, is the durable record of hashes a full scan
	// computed for files the baseline did not know (R-2.2.8): consulted
	// before a new file is hashed, fed as the hash phase goes. Nil (a dry
	// run, or a test) means every new file is hashed and nothing is kept.
	hashCache        syncdomain.LocalHashCache
	checkWorkers     int // parallel hash goroutine limit for FullScan (0 → defaultCheckWorkers)
	filterConfig     syncdomain.ContentFilterConfig
	protectedRoots   []syncshortcut.ProtectedRoot
	observationRules syncdomain.LocalObservationRules
	expectedRoot     *syncdomain.SyncRootIdentity
	// scanAvailability is FullScan's per-scan state for recording synced
	// paths that are unavailable rather than absent (scanner_unavailable.go).
	// Set and cleared by FullScan; nil outside a scan.
	scanAvailability *scanAvailability
	// scanNames collects the on-disk names a full scan accepts, for its
	// collision phase; nil outside a FullScan.
	scanNames ScanNames
	// watchIdentityLookup caches the identity index the watch handlers use
	// to recognize a synced item renamed to a skipped name; rebuilt every
	// watchIdentityIndexMaxAge. Touched only by the goroutine running Watch.
	// mountBoundaryFlags are the folders recorded as crossed mount points:
	// set by the engine before a scan, and by the watch runtime after each
	// snapshot it commits (SetMountBoundaryFlags). A scan reads the set once,
	// at its start; the set is replaced whole, never changed in place.
	mountBoundaryFlags   atomic.Pointer[map[string]struct{}]
	watchIdentityLookup  *scanAvailability
	watchIdentityBuiltAt time.Time
	protectedRootEvents  syncshortcut.ProtectedRootEventSink
	// hooks are the observer's test seams; the zero value is production.
	hooks Hooks

	// debugEvents is the engine's debug event sink, set by the watch runtime
	// before Watch; nil reports nothing.
	debugEvents func(syncdomain.EngineDebugEvent)

	// descriptorBudget caps the open descriptors watch setup may commit to
	// watches (kqueue); 0 means the backend uses none per watch (inotify).
	descriptorBudget int

	// filesystemKind identifies the sync root's filesystem so a network or
	// FUSE root, whose remote-side changes produce no local events, gets a
	// warning and a shorter safety-scan interval. Injectable for tests.
	filesystemKind func(path string) (filesystemInfo, error)

	droppedEvents  atomic.Int64 // observations dropped on a full batch channel
	droppedRetries atomic.Int64 // hash requests dropped due to full channel

	// reportedWatchCauses and safetyScanFailing let the watch loop report a
	// persistent condition once instead of per directory or per scan.
	// Both are touched only by the goroutine running Watch.
	reportedWatchCauses map[WatchUnavailableCause]struct{}
	safetyScanFailing   bool

	// activeWatcher is the watcher of the running Watch call, so full scans
	// can re-add watches the event path missed. Watch-loop goroutine only;
	// nil outside Watch.
	activeWatcher FsWatcher

	// rootFilesystemChecked keeps the root-filesystem warning to one per
	// observer when Watch is re-entered after a fallback period.
	rootFilesystemChecked bool

	// watchUnavailable is set by an event handler when a mid-session watch
	// failure means watching must stop (kqueue ran out of descriptors). The
	// watch loop returns it so the runtime falls back to periodic scans and
	// the watcher's descriptors are released for transfers and SQLite.
	// Watch-loop goroutine only.
	watchUnavailable error

	safetyScanInterval time.Duration // 0 → default (5 minutes); configurable (B-099)

	// clock is the observer's time capability: safety-scan ticks, retry
	// sleeps, and write-coalesce timers all read the same clock, so a test
	// cannot advance one without the others.
	clock syncdomain.SyncClock

	startupSafetyScan bool // engine watch mode closes the bootstrap-to-fsnotify startup gap

	// localBatchCh forwards engine-applied local observation batches. The
	// observer never writes SQLite directly; the watch runtime owns durable
	// application of these batches.
	localBatchCh chan<- LocalObservation

	// localWatchState owns all watch-loop mutable state. It is embedded so
	// same-package tests can still reach the existing fields directly while the
	// runtime contract stays grouped under one owner.
	localWatchState
}

// NewLocalObserver creates a LocalObserver. checkWorkers controls the number
// of parallel goroutines used for file hashing during FullScan (0 → default 4).
// The baseline must be loaded (from SyncStore.Load); it is read-only
// during observation.
func NewLocalObserver(baseline syncdomain.BaselineView, logger *slog.Logger, checkWorkers int) *LocalObserver {
	return &LocalObserver{
		baseline:         baseline,
		logger:           logger,
		checkWorkers:     checkWorkers,
		clock:            syncdomain.RealClock{},
		localWatchState:  newLocalWatchState(),
		descriptorBudget: watchDescriptorBudget(),
		filesystemKind:   localFilesystemKind,
	}
}

// SetLocalObservationBatchChannel sets the channel that carries local
// observation batches to the watch runtime (see localBatchCh). Must be called
// before Watch.
func (o *LocalObserver) SetLocalObservationBatchChannel(ch chan<- LocalObservation) {
	o.localBatchCh = ch
}

// SetFilterConfig installs per-drive sync content filters. The observer copies
// slices so later config mutations cannot silently change an already-running
// watch/scanner.
func (o *LocalObserver) SetFilterConfig(cfg syncdomain.ContentFilterConfig) {
	o.filterConfig = syncdomain.ContentFilterConfig{
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
func (o *LocalObserver) SetProtectedRoots(roots []syncshortcut.ProtectedRoot) {
	o.protectedRoots = append([]syncshortcut.ProtectedRoot(nil), roots...)
}

// SetProtectedRootEventSink installs the parent watch-runtime notification sink
// used for protected-root lifecycle facts. The observer never mutates those
// roots; it only reports that the parent engine should reconcile.
func (o *LocalObserver) SetProtectedRootEventSink(sink syncshortcut.ProtectedRootEventSink) {
	o.protectedRootEvents = sink
}

// SetObservationRules installs platform-derived local validation rules. These
// stay separate from user-configured filter knobs so drive semantics do not
// get conflated with local exclusions.
func (o *LocalObserver) SetObservationRules(rules syncdomain.LocalObservationRules) {
	o.observationRules = rules
}

// SetLocalHashCache installs the durable hash cache a full scan consults and
// feeds (R-2.2.8).
func (o *LocalObserver) SetLocalHashCache(cache syncdomain.LocalHashCache) {
	o.hashCache = cache
}

// SetMountBoundaryFlags installs the folders previously recorded as crossed
// mount points.
func (o *LocalObserver) SetMountBoundaryFlags(paths []string) {
	flags := make(map[string]struct{}, len(paths))
	for _, flagged := range paths {
		flags[flagged] = struct{}{}
	}
	o.mountBoundaryFlags.Store(&flags)
}

// SetExpectedRootIdentity validates scans against one fixed identity.
func (o *LocalObserver) SetExpectedRootIdentity(identity *synctree.RootIdentity) {
	if identity == nil {
		o.expectedRoot = nil
		return
	}
	o.expectedRoot = syncdomain.FixedSyncRootIdentity(identity)
}

// SetSyncRootIdentity validates scans against the engine's shared identity,
// so a binding made while the observer runs is enforced at once.
func (o *LocalObserver) SetSyncRootIdentity(identity *syncdomain.SyncRootIdentity) {
	o.expectedRoot = identity
}

func (o *LocalObserver) reportProtectedRootEvent(event syncshortcut.ProtectedRootEvent) {
	if o == nil || o.protectedRootEvents == nil {
		return
	}
	o.protectedRootEvents(event)
}

func (o *LocalObserver) recordPendingTimer(path string, timer syncdomain.SyncTimer) {
	if o.PendingTimers == nil {
		o.PendingTimers = make(map[string]syncdomain.SyncTimer)
	}

	o.PendingTimers[path] = timer
}

func (o *LocalObserver) cancelPendingTimer(path string) {
	if timer, ok := o.PendingTimers[path]; ok {
		timer.Stop()
		delete(o.PendingTimers, path)
	}
	if churn, ok := o.writeChurn[path]; ok {
		churn.firstPendingAt = time.Time{}
	}
}

// SetClock replaces the observer's time capability (see clock). Must be
// called before the observer scans or watches.
func (o *LocalObserver) SetClock(clock syncdomain.SyncClock) {
	o.clock = clock
}

// SetDebugEvents installs the engine's debug event sink (see debugEvents).
// Must be called before Watch.
func (o *LocalObserver) SetDebugEvents(emit func(syncdomain.EngineDebugEvent)) {
	o.debugEvents = emit
}

// SetSafetyScan sets the healthy safety-scan interval and whether Watch runs
// one safety scan at startup, closing the gap between the bootstrap scan and
// the first watch event. Must be called before Watch.
func (o *LocalObserver) SetSafetyScan(interval time.Duration, atStartup bool) {
	o.safetyScanInterval = interval
	o.startupSafetyScan = atStartup
}

// SetHooks installs the observer's test seams. The engine passes its own
// through unchanged; production's are the zero value. Must be called before
// the observer scans or watches.
func (o *LocalObserver) SetHooks(hooks Hooks) {
	o.hooks = hooks
}

// TrySend hands what a watch handler observed for one path to the runtime as
// a patch, without blocking (TrySendLocalObservationBatch).
func (o *LocalObserver) TrySend(ctx context.Context, ev *syncdomain.ChangeEvent) {
	if ev != nil {
		batch := localObservationBatchForEvent(ev)
		o.TrySendLocalObservationBatch(ctx, &batch)
	}
}

// TrySendLocalObservationBatch sends batch to the runtime without blocking.
// On a full channel the batch is dropped, counted and logged at Debug (a bulk
// copy can drop thousands), and a recovery scan is requested, which replaces
// local truth with a full snapshot.
func (o *LocalObserver) TrySendLocalObservationBatch(ctx context.Context, batch *LocalObservation) {
	if o.localBatchCh == nil {
		return
	}

	select {
	case o.localBatchCh <- *batch:
	case <-ctx.Done():
	default:
		o.droppedEvents.Add(1)
		o.requestRecoveryScan()
		o.logger.Debug("local observation batch channel full, marking local truth suspect on maintenance",
			slog.String("kind", batch.Kind.String()),
			slog.Int("rows", len(batch.Rows)),
			slog.Int("deleted_prefixes", len(batch.DeletedPrefixes)),
		)
	}
}

func (o *LocalObserver) TrySendLocalTruthSuspect(ctx context.Context, reason string) {
	o.TrySendLocalObservationBatch(ctx, &LocalObservation{
		Kind:           LocalTruthSuspect,
		RecoveryReason: reason,
	})
}

// TrySendFullLocalSnapshot hands a full scan to the runtime as one snapshot:
// its rows, its whole finding set (the empty set too, because a scan that
// skipped nothing has proven every earlier finding gone) and the mount points
// it crossed, which the runtime commits together. A snapshot dropped on a
// full channel asks for a recovery scan, as every dropped observation does.
//
// The snapshot carries no dirty hint of its own: the runtime replans when the
// commit changes local truth (R-2.8.17) or the persisted finding set. The
// scan's events are relative to the baseline, not to local_state, so they
// count every change already committed and planned. Treating them as a dirty
// hint retired the admitted plan at every safety scan during a backlog
// (R-2.8.21) and, with a replan slower than the safety interval, starved
// dispatch entirely.
func (o *LocalObserver) TrySendFullLocalSnapshot(ctx context.Context, result ScanResult) {
	o.TrySendLocalObservationBatch(ctx, &LocalObservation{
		Kind:          LocalSnapshot,
		Rows:          BuildLocalStateRows(result),
		Findings:      result.Skipped,
		CrossedMounts: result.CrossedMounts,
	})
}

// localBatchChannelFull reports whether the runtime has not taken the
// observations already sent: a full snapshot sent now would be dropped.
func (o *LocalObserver) localBatchChannelFull() bool {
	return o.localBatchCh != nil && len(o.localBatchCh) == cap(o.localBatchCh)
}

// requestRecoveryScan asks the watch loop for an early full rescan. Safe from
// any goroutine: it only signals. A request while one is pending coalesces.
func (o *LocalObserver) requestRecoveryScan() {
	select {
	case o.recoveryScanRequests <- struct{}{}:
	default:
	}
}

// armRecoveryScan starts the recovery-scan timer unless one is already
// armed. Watch-loop goroutine only.
func (o *LocalObserver) armRecoveryScan() {
	if o.recoveryScanTimer != nil || o.recoveryScanDue == nil {
		return
	}

	delay := o.recoveryScanDelayNow()
	due := o.recoveryScanDue
	gen := o.recoveryScanGen
	o.recoveryScanTimer = o.clock.AfterFunc(delay, func() {
		select {
		case due <- gen:
		default:
		}
	})
}

// recoveryScanDelayNow paces recovery scans so they cannot run back to back on
// a large tree under a sustained event storm: the delay is at least
// recoveryScanDelay and twice the last full scan's duration, and doubles when
// the request comes in while the previous recovery delay has not yet passed
// since the last recovery scan ended (the storm continues). It never exceeds
// the safety-scan interval, which would run the scan anyway.
func (o *LocalObserver) recoveryScanDelayNow() time.Duration {
	delay := max(recoveryScanDelay, 2*o.lastFullScanDuration)
	if !o.lastRecoveryScanEnd.IsZero() && o.clock.Now().Sub(o.lastRecoveryScanEnd) < o.lastRecoveryDelay {
		delay = max(delay, 2*o.lastRecoveryDelay)
	}
	delay = min(delay, o.EffectiveSafetyScanInterval())
	o.lastRecoveryDelay = delay

	return delay
}

// takeRecoveryScanDue reports whether a fired recovery timer's generation is
// the armed one, and disarms it. Watch-loop goroutine only.
func (o *LocalObserver) takeRecoveryScanDue(gen uint64) bool {
	if gen != o.recoveryScanGen || o.recoveryScanTimer == nil {
		return false
	}
	o.recoveryScanTimer = nil

	return true
}

// finishRecoveryScan records when a recovery scan ended, for pacing.
func (o *LocalObserver) finishRecoveryScan() {
	o.lastRecoveryScanEnd = o.clock.Now()
}

// cancelRecoveryScan disarms a pending recovery scan, including one whose
// timer already fired. Watch-loop goroutine only.
func (o *LocalObserver) cancelRecoveryScan() {
	if o.recoveryScanTimer != nil {
		o.recoveryScanTimer.Stop()
		o.recoveryScanTimer = nil
	}
	o.recoveryScanGen++

	select {
	case <-o.recoveryScanDue:
	default:
	}
}

// DroppedEvents returns the cumulative number of observations dropped on a
// full batch channel. Production code uses ResetDroppedEvents for per-pass
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

// RestoreDropped adds counts taken with ResetDroppedEvents and
// ResetDroppedRetries back, so a recovery that failed is retried on the next
// maintenance tick.
func (o *LocalObserver) RestoreDropped(events, retries int64) {
	o.droppedEvents.Add(events)
	o.droppedRetries.Add(retries)
}

// DroppedRetries returns the cumulative number of hash requests dropped
// because the HashRequests channel was full. Safety scans catch these.
func (o *LocalObserver) DroppedRetries() int64 {
	return o.droppedRetries.Load()
}

func (o *LocalObserver) ResetDroppedRetries() int64 {
	return o.droppedRetries.Swap(0)
}

// EstimateDirCount returns the estimated number of directories that will need
// inotify watches. Counts ItemTypeFolder entries in baseline plus one for the
// sync root itself.
func (o *LocalObserver) EstimateDirCount() int {
	count := 1 // sync root always needs a watch

	o.baseline.ForEachPath(func(_ string, entry syncdomain.BaselineEntry) {
		if entry.ItemType == syncdomain.ItemTypeFolder {
			count++
		}
	})

	return count
}

// Watch monitors the local filesystem for changes using fsnotify and sends
// what it observes to the batch channel (SetLocalObservationBatchChannel). It
// blocks until the context is canceled, returning nil. A periodic safety scan
// (every 5 minutes) catches any events that fsnotify may miss (e.g., during
// brief watcher gaps or platform edge cases).
//
// An observation sent to a full batch channel is dropped and counted, and a
// recovery scan is requested, which replaces local truth with a full snapshot
// (B-114).
func (o *LocalObserver) Watch(ctx context.Context, tree *synctree.Root) error {
	syncRoot := tree.Path()
	o.logger.Info("local observer starting watch",
		slog.String("sync_root", syncRoot),
	)

	// Watch can run again after a fallback period: registrations and the
	// stop reason belong to the previous watcher.
	o.watchedDirs = watchRegistry{}
	o.watchUnavailable = nil

	watcher, err := o.newWatcher()
	if err != nil {
		if cause, ok := classifyWatcherCreateError(err); ok {
			return &localWatchUnavailableError{cause: cause, err: fmt.Errorf("create filesystem watcher: %w", err)}
		}

		return fmt.Errorf("create filesystem watcher: %w", err)
	}
	defer watcher.Close() //nolint:errcheck // watcher shutdown releases OS resources and reports nothing a caller can act on

	defer o.cancelPendingTimers()
	defer o.cancelRecoveryScan()

	o.activeWatcher = watcher
	defer func() { o.activeWatcher = nil }()

	// Pre-flight capacity check (Linux only; no-op on other platforms).
	checkInotifyCapacity(o.EstimateDirCount(), o.logger)
	if !o.rootFilesystemChecked {
		o.rootFilesystemChecked = true
		o.adaptToRootFilesystem(syncRoot)
	}

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
		o.runSafetyScan(ctx, tree)
		if o.watchUnavailable != nil {
			return o.watchUnavailable
		}
	}
	if o.hooks.BeforeWatchLoop != nil {
		o.hooks.BeforeWatchLoop()
	}

	return o.watchLoop(ctx, watcher, tree)
}

// filesystemInfo describes the filesystem holding the sync root.
type filesystemInfo struct {
	// kind is the platform's name for the filesystem type ("nfs", "cifs").
	kind string
	// remote is true for network and FUSE filesystems: changes made by other
	// clients of the share, or by the FUSE server, generate no local watch
	// events, so only rescans observe them.
	remote bool
}

// networkSafetyScanInterval is the safety-scan interval on a network or FUSE
// sync root (D-081c). The default five minutes relied on events that such a
// root never delivers for changes made elsewhere; a full scan of an unchanged
// tree is stat-only, so scanning more often costs metadata reads, not
// content reads.
const networkSafetyScanInterval = 2 * time.Minute

// adaptToRootFilesystem warns once and shortens the safety-scan interval when
// the sync root is on a network or FUSE filesystem.
func (o *LocalObserver) adaptToRootFilesystem(syncRoot string) {
	if o.filesystemKind == nil {
		return
	}

	info, err := o.filesystemKind(syncRoot)
	if err != nil {
		o.logger.Debug("could not identify the sync folder's filesystem",
			slog.Any("error", err))
		return
	}
	if !info.remote {
		return
	}

	// Only the default interval is shortened: a non-default interval was
	// chosen deliberately, and it is never lengthened.
	if o.EffectiveSafetyScanInterval() == safetyScanInterval {
		o.safetyScanInterval = networkSafetyScanInterval
	}

	o.logger.Warn("The sync folder is on a network or FUSE filesystem ("+info.kind+"). "+
		"Changes that reach it without going through this computer may not be reported as they happen; "+
		"they are noticed by the periodic rescan, every "+WaitText(o.EffectiveSafetyScanInterval())+".",
		slog.String("filesystem", info.kind),
	)
}

func (o *LocalObserver) EffectiveSafetyScanInterval() time.Duration {
	if o.safetyScanInterval == 0 {
		return safetyScanInterval
	}

	return o.safetyScanInterval
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

// isFatalWatchSetupError reports a watch setup that cannot go on: watching is
// unavailable, or the watch's run is ending.
func isFatalWatchSetupError(ctx context.Context, err error) bool {
	return errors.Is(err, syncdomain.ErrLocalWatchUnavailable) || syncdomain.RunEnded(ctx, err)
}

func (o *LocalObserver) rollbackAddedWatches(watcher FsWatcher, session *watchAddSession) {
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
				slog.Any("error", err))
		}

		o.watchedDirs.remove(path)
	}
}

// AddWatchesRecursive walks the sync root and adds a watch on every directory.
func (o *LocalObserver) AddWatchesRecursive(ctx context.Context, watcher FsWatcher, tree *synctree.Root) error {
	syncRoot := tree.Path()
	counts := &watchSetupCounts{}
	session := newWatchAddSession()
	err := o.addObservedDirWatches(ctx, watcher, syncRoot, ".", counts, make(map[string]struct{}), session)
	if isFatalWatchSetupError(ctx, err) {
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
