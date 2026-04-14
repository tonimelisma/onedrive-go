package sync

import (
	"sync"

	"github.com/tonimelisma/onedrive-go/internal/syncscope"
)

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. The Engine itself remains an immutable dependency
// container; run-scoped state lives here instead.
type engineFlow struct {
	engine *Engine
	watch  *watchRuntime

	depGraph   *DepGraph
	dispatchCh chan *TrackedAction
	shortcuts  []Shortcut

	succeeded  int
	failed     int
	syncErrors []error
	summaries  []failureSummaryEntry

	runID string
}

func newEngineFlow(engine *Engine) engineFlow {
	flow := engineFlow{
		engine: engine,
		runID:  engine.nextRuntimeRunID(),
	}

	return flow
}

func (f *engineFlow) setShortcuts(shortcuts []Shortcut) {
	f.shortcuts = shortcuts
}

func (f *engineFlow) getShortcuts() []Shortcut {
	return f.shortcuts
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
	// owner, but tests and repair paths can observe or adjust the working set
	// while timers are being re-armed.
	activeScopesMu sync.RWMutex

	// Active scope blocks owned by the watch control flow. The slice is tiny
	// (usually 0-5 entries), so linear scans keep the logic simple and avoid a
	// second mirrored subsystem.
	activeScopes []ScopeBlock

	// Scope detection — sliding window failure tracking.
	scopeState *ScopeState

	// Monotonic action ID counter owned by the watch control flow. Prevents
	// ID collisions across batches without introducing cross-goroutine sync.
	nextActionID int64

	// userIntentPending makes durable user-intent admission level-triggered at
	// the watch-loop boundary. Wakes can arrive while ordinary outbox work is
	// still draining; this flag ensures the next user-intent pass is not lost.
	userIntentPending bool
}

type watchObservationState struct {
	// scopeMu guards scopeSnapshot so observer goroutines can read the current
	// effective scope while the watch loop remains the single writer.
	scopeMu sync.RWMutex

	// Event buffer — watch-loop retry/trial work injects events via buf.Add().
	buf *Buffer

	// Delete safety protection: rolling counter + durable held-delete state.
	deleteCounter *DeleteCounter

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs       *RemoteObserver
	localObs        *LocalObserver
	scopeSnapshot   syncscope.Snapshot
	scopeGeneration int64
	scopeChanges    chan syncscope.Change

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
}

type watchReconcileState struct {
	// Deduplication: caches the last visible-issue summary signature and
	// per-scope shared-folder child-set signatures for watch summaries.
	lastSummaryTotal     int
	lastSummarySignature string
	lastRemoteBlocked    map[ScopeKey]string

	// Full reconciliation is started by the watch loop and hands its result
	// back over reconcileResults. The loop owns reconcileActive and applies the
	// returned events/shortcut snapshot on receipt.
	reconcileActive  bool
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
	scopeChangeChannelBuf                       = 8
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
			reconcileResults:  make(chan reconcileResult, 1),
		},
	}
	rt.scopeChanges = make(chan syncscope.Change, scopeChangeChannelBuf)
	rt.watch = rt

	return rt
}
