package sync

import (
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncobserve"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// engineFlow holds mutable per-run execution state shared by one-shot and
// watch coordinators. The Engine itself remains an immutable dependency
// container; run-scoped state lives here instead.
type engineFlow struct {
	engine *Engine

	depGraph  *syncdispatch.DepGraph
	readyCh   chan *synctypes.TrackedAction
	shortcuts []synctypes.Shortcut

	succeeded  int
	failed     int
	syncErrors []error

	scopeCtrl    scopeController
	shortcutCtrl shortcutCoordinator
}

func newEngineFlow(engine *Engine) engineFlow {
	flow := engineFlow{engine: engine}
	flow.initPolicyControllers()

	return flow
}

func (f *engineFlow) setShortcuts(shortcuts []synctypes.Shortcut) {
	f.shortcuts = shortcuts
}

func (f *engineFlow) getShortcuts() []synctypes.Shortcut {
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

type watchRuntimeState struct {
	// activeScopesMu guards activeScopes. The watch loop remains the logical
	// owner, but tests and repair paths can observe or adjust the working set
	// while timers are being re-armed.
	activeScopesMu sync.RWMutex

	// Active scope blocks owned by the watch control flow. The slice is tiny
	// (usually 0-5 entries), so linear scans keep the logic simple and avoid a
	// second mirrored subsystem.
	activeScopes []synctypes.ScopeBlock

	// Scope detection — sliding window failure tracking.
	scopeState *syncdispatch.ScopeState

	// Monotonic action ID counter owned by the watch control flow. Prevents
	// ID collisions across batches without introducing cross-goroutine sync.
	nextActionID int64
}

type watchObservationState struct {
	// Event buffer — watch-loop retry/trial work injects events via buf.Add().
	buf *syncobserve.Buffer

	// Big-delete protection: rolling counter + external change detection.
	// deleteCounter is nil even in watch mode when force=true.
	deleteCounter   *syncdispatch.DeleteCounter
	lastDataVersion int64

	// Observer references — set in startObservers, nil'd on shutdown.
	remoteObs *syncobserve.RemoteObserver
	localObs  *syncobserve.LocalObserver
}

type watchTimerState struct {
	// timerMu guards trialTimer and retryTimer pointers. Timer callbacks only
	// signal channels; they never mutate loop-owned state directly.
	timerMu sync.RWMutex

	// Trial and retry timers are armed by the watch loop. The channels stay
	// persistent so timer callbacks only signal the loop; they never mutate
	// loop-owned state directly.
	trialTimer *time.Timer
	trialCh    chan struct{}

	// Retry timer — watch loop retrier sweeps sync_failures on each tick.
	retryTimer   *time.Timer
	retryTimerCh chan struct{} // persistent, buffered(1)

	// Throttling: tracks last recheckPermissions call time (R-2.10.9).
	lastPermRecheck time.Time
}

type watchReconcileState struct {
	// Deduplication: caches last actionable issue count for watch summaries.
	lastSummaryTotal int

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

func newWatchRuntime(engine *Engine) *watchRuntime {
	return &watchRuntime{
		engineFlow: newEngineFlow(engine),
		watchTimerState: watchTimerState{
			trialCh:      make(chan struct{}, 1),
			retryTimerCh: make(chan struct{}, 1),
		},
		watchReconcileState: watchReconcileState{
			reconcileResults: make(chan reconcileResult, 1),
		},
	}
}
