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

	depGraph   *depGraph
	dispatchCh chan *trackedAction

	// activeScopesMu guards activeScopes and nothing else. The fields that
	// follow it are owned by the single runtime goroutine; only the scope
	// slice is read concurrently, by admission checks and status snapshots.
	activeScopesMu sync.RWMutex
	activeScopes   []activeScope
	scopeState     *scopeState
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

	runID string
}

type heldReason string

const (
	heldReasonRetry heldReason = "retry"
	heldReasonScope heldReason = "scope"
)

type heldAction struct {
	Tracked   *trackedAction
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
