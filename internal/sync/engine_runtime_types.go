package sync

import (
	"sync"
	"time"
)

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. The Engine itself remains an immutable dependency
// container; run-scoped state lives here instead.
type engineFlow struct {
	engine *Engine
	watch  *watchRuntime

	depGraph   *DepGraph
	dispatchCh chan *TrackedAction

	activeScopesMu sync.RWMutex
	activeScopes   []ActiveScope
	scopeState     *ScopeState
	nextActionID   int64

	retryRowsByKey map[RetryWorkKey]RetryWorkRow
	heldByKey      map[RetryWorkKey]*heldAction
	heldScopeOrder map[ScopeKey][]RetryWorkKey
	queuedByID     map[int64]struct{}
	runningByID    map[int64]struct{}
	runningCount   int
	nextHeldOrder  uint64

	succeeded  int
	failed     int
	syncErrors []error
	summaries  []failureSummaryEntry

	scopeCtrl scopeController
	runID     string
}

type heldReason string

const (
	heldReasonRetry heldReason = "retry"
	heldReasonScope heldReason = "scope"
)

type heldAction struct {
	Tracked   *TrackedAction
	Reason    heldReason
	ScopeKey  ScopeKey
	NextRetry time.Time
	HeldOrder uint64
}

func newEngineFlow(engine *Engine) *engineFlow {
	flow := &engineFlow{
		engine:         engine,
		runID:          engine.nextRuntimeRunID(),
		retryRowsByKey: make(map[RetryWorkKey]RetryWorkRow),
		heldByKey:      make(map[RetryWorkKey]*heldAction),
		heldScopeOrder: make(map[ScopeKey][]RetryWorkKey),
		queuedByID:     make(map[int64]struct{}),
		runningByID:    make(map[int64]struct{}),
	}
	flow.initPolicyControllers()

	return flow
}

type oneShotRunner struct {
	*engineFlow
}

func newOneShotRunner(engine *Engine) *oneShotRunner {
	return &oneShotRunner{
		engineFlow: newEngineFlow(engine),
	}
}

type watchLoopState struct {
	phase           watchRuntimePhase
	outbox          []*TrackedAction
	replanPending   bool
	pendingDirty    DirtyBatch
	hasPendingDirty bool
}

type watchRuntimeState struct {
	loop watchLoopState

	// syncBatch tracks the current best-effort watch batch so status updates
	// are written only after the loop has exhausted all currently admissible
	// work and returned to quiescence.
	syncBatch watchSyncBatchState
}

type watchSyncBatchState struct {
	active        bool
	startedAt     time.Time
	succeededBase int
	failedBase    int
	errorBase     int
}

type watchObservationState struct {
	// Dirty debounce buffer. Local and remote observers mark paths or full
	// refresh requests here; the watch loop refreshes snapshots and replans
	// from SQLite current truth after the debounce window closes.
	dirtyBuf *DirtyBuffer

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *RemoteObserver
	localObs  *LocalObserver

	// Socket.IO wake source lifecycle, when enabled for full-drive watch.
	socketIOWakeStop chan struct{}
	socketIOWakeDone chan struct{}
}

type watchTimerState struct {
	// timerMu guards trialTimer and retryTimer pointers. Timer callbacks only
	// signal channels; they never mutate loop-owned state directly.
	timerMu sync.RWMutex

	// Trial and retry timers are armed by the watch loop. The channels stay
	// persistent so timer callbacks only signal the loop; they never mutate
	// loop-owned state directly.
	trialTimer syncTimer
	trialCh    chan struct{}

	// Retry timer — watch loop retrier sweeps retry_work on each tick.
	retryTimer   syncTimer
	retryTimerCh chan struct{} // persistent, buffered(1)
}

type watchSummaryState struct {
	// Deduplication: caches the last watch-condition signature and per-scope
	// shared-folder child-set signatures for watch summaries.
	lastSummarySignature string
	lastRemoteBlocked    map[ScopeKey]string
}

type watchRefreshState struct {
	// Full remote refresh is started by the watch loop and hands one
	// loop-applied remote observation batch back over refreshResults. The loop
	// owns refreshActive, durable apply, and dirty marking on receipt.
	refreshActive  bool
	refreshTimer   syncTimer
	refreshCh      chan time.Time
	refreshResults chan remoteRefreshResult

	// afterRefreshCommit is a test-only hook called after the watch loop has
	// durably applied a full-refresh batch but before it marks the resulting
	// dirty work. Nil in production.
	afterRefreshCommit func()
}

// watchRuntime owns all mutable watch-mode state. It is created by RunWatch
// and discarded when the watch session ends.
type watchRuntime struct {
	*engineFlow
	watchRuntimeState
	watchObservationState
	watchTimerState
	watchSummaryState
	watchRefreshState
}

type watchRuntimePhase string

const (
	watchRuntimePhaseRunning  watchRuntimePhase = "running"
	watchRuntimePhaseDraining watchRuntimePhase = "draining"
)

func newWatchRuntime(engine *Engine) *watchRuntime {
	rt := &watchRuntime{
		engineFlow: newEngineFlow(engine),
		watchRuntimeState: watchRuntimeState{
			loop: watchLoopState{
				phase: watchRuntimePhaseRunning,
			},
		},
		watchTimerState: watchTimerState{
			trialCh:      make(chan struct{}, 1),
			retryTimerCh: make(chan struct{}, 1),
		},
		watchSummaryState: watchSummaryState{
			lastRemoteBlocked: make(map[ScopeKey]string),
		},
		watchRefreshState: watchRefreshState{
			refreshCh:      make(chan time.Time, 1),
			refreshResults: make(chan remoteRefreshResult, 1),
		},
	}
	rt.watch = rt

	return rt
}
