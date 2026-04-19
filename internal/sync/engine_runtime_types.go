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

	succeeded  int
	failed     int
	syncErrors []error
	summaries  []failureSummaryEntry

	scopeCtrl scopeController
	runID     string
}

func newEngineFlow(engine *Engine) engineFlow {
	flow := engineFlow{
		engine: engine,
		runID:  engine.nextRuntimeRunID(),
	}
	flow.initPolicyControllers()

	return flow
}

type oneShotRunner struct {
	engineFlow
}

func newOneShotRunner(engine *Engine) *oneShotRunner {
	return &oneShotRunner{
		engineFlow: newEngineFlow(engine),
	}
}

type watchLoopState struct {
	phase  watchRuntimePhase
	outbox []*TrackedAction
}

type watchRuntimeState struct {
	loop watchLoopState

	// activeScopesMu guards activeScopes. The watch loop remains the logical
	// owner, but tests and startup normalization can observe or adjust the
	// working set while timers are being re-armed.
	activeScopesMu sync.RWMutex

	// Active block scopes owned by the watch control flow. The slice is tiny
	// (usually 0-5 entries), so linear scans keep the logic simple and avoid a
	// second mirrored subsystem.
	activeScopes []BlockScope

	// Scope detection — sliding window failure tracking.
	scopeState *ScopeState

	// Monotonic action ID counter owned by the watch control flow. Prevents
	// ID collisions across batches without introducing cross-goroutine sync.
	nextActionID int64
}

type watchObservationState struct {
	// Dirty debounce buffer. Local and remote observers mark paths or full
	// refresh requests here; the watch loop refreshes snapshots and replans
	// from SQLite current truth after the debounce window closes.
	dirtyBuf *DirtyBuffer

	// Cross-connection SQLite commit detector for watch recheck ticks.
	lastDataVersion int64

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

	// Retry timer — watch loop retrier sweeps sync_failures on each tick.
	retryTimer   syncTimer
	retryTimerCh chan struct{} // persistent, buffered(1)

	// Throttling: tracks last recheckPermissions call time (R-2.10.9).
	lastPermRecheck time.Time
}

type watchReconcileState struct {
	// Deduplication: caches the last visible-issue summary signature and
	// per-scope shared-folder child-set signatures for watch summaries.
	lastSummaryTotal     int
	lastSummarySignature string
	lastRemoteBlocked    map[ScopeKey]string

	// Full reconciliation is started by the watch loop and hands its result
	// back over reconcileResults. The loop owns reconcileActive and applies the
	// returned events on receipt.
	reconcileActive  bool
	reconcileTimer   syncTimer
	reconcileCh      chan time.Time
	reconcileResults chan reconcileResult

	// afterReconcileCommit is a test-only hook called after CommitObservation
	// succeeds in runFullReconciliationAsync. Nil in production. Allows tests
	// to inject actions (e.g. context cancellation) at an otherwise unreachable
	// point between commit and buffer feeding.
	afterReconcileCommit func()
}

// watchRuntime owns all mutable watch-mode state. It is created by RunWatch
// and discarded when the watch session ends.
type watchRuntime struct {
	engineFlow
	watchRuntimeState
	watchObservationState
	watchTimerState
	watchReconcileState
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
		watchReconcileState: watchReconcileState{
			lastRemoteBlocked: make(map[ScopeKey]string),
			reconcileCh:       make(chan time.Time, 1),
			reconcileResults:  make(chan reconcileResult, 1),
		},
	}
	rt.watch = rt

	return rt
}
