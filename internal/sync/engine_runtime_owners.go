package sync

import (
	stdsync "sync"
	"time"
)

// The four owners below split engineFlow's mutable state by authority. Each
// answers one question about a run, and each is written and read by the single
// runtime goroutine that owns the run.
//
// Splitting them does not change the concurrency model: one goroutine still
// owns all four. What changes is that the compiler now shows which owner a
// method touches, so "this decision is about scheduling" or "this decision is
// about retry timing" is visible in the code rather than inferred from field
// names on a struct with several dozen of them.

// actionScheduler owns dependency-ready dispatch: which actions exist, which
// are queued, and which are running.
type actionScheduler struct {
	graph        *depGraph
	dispatchCh   chan *trackedAction
	nextActionID int64
	queuedByID   map[int64]struct{}
	runningByID  map[int64]struct{}
	runningCount int
}

func newActionScheduler() actionScheduler {
	return actionScheduler{
		queuedByID:  make(map[int64]struct{}),
		runningByID: make(map[int64]struct{}),
	}
}

// retryLedger owns exact held work: the durable retry rows loaded for this run,
// the actions currently held, and their release ordering.
type retryLedger struct {
	rowsByKey     map[RetryWorkKey]RetryWorkRow
	heldByKey     map[RetryWorkKey]*heldAction
	heldByScope   map[ScopeKey][]RetryWorkKey
	nextHeldOrder uint64
}

func newRetryLedger() retryLedger {
	return retryLedger{
		rowsByKey:   make(map[RetryWorkKey]RetryWorkRow),
		heldByKey:   make(map[RetryWorkKey]*heldAction),
		heldByScope: make(map[ScopeKey][]RetryWorkKey),
	}
}

// scopeLedger owns the active shared blockers for this run.
type scopeLedger struct {
	// mu guards active and nothing else. It is the one field here read outside
	// the runtime goroutine, by admission checks and status snapshots.
	mu     stdsync.RWMutex
	active []activeScope

	// state is owned by the runtime goroutine alone and is never taken under mu.
	state *scopeState
}

// runResults owns what the run will report when it finishes.
type runResults struct {
	succeeded int
	failed    int
	errors    []error
	summaries []failureSummaryEntry
}

// heldAction is one action waiting for a retry deadline or a blocked scope.
type heldAction struct {
	Tracked   *trackedAction
	Reason    heldReason
	ScopeKey  ScopeKey
	NextRetry time.Time
	HeldOrder uint64
}

type heldReason string

const (
	heldReasonRetry heldReason = "retry"
	heldReasonScope heldReason = "scope"
)

func (s *actionScheduler) trackedActionForCompletion(r *actionCompletion) *trackedAction {
	if r == nil || s.graph == nil {
		return nil
	}

	ta, ok := s.graph.Get(r.ActionID)
	if !ok {
		return nil
	}

	return ta
}

func (s *actionScheduler) markQueued(ta *trackedAction) {
	if ta == nil {
		return
	}
	s.queuedByID[ta.ID] = struct{}{}
}

func (s *actionScheduler) markRunning(ta *trackedAction) {
	if ta == nil {
		return
	}
	delete(s.queuedByID, ta.ID)
	if _, ok := s.runningByID[ta.ID]; ok {
		return
	}
	s.runningByID[ta.ID] = struct{}{}
	s.runningCount++
}

func (s *actionScheduler) markFinished(ta *trackedAction) {
	if ta == nil {
		return
	}
	if _, ok := s.runningByID[ta.ID]; ok {
		delete(s.runningByID, ta.ID)
		if s.runningCount > 0 {
			s.runningCount--
		}
	}
	delete(s.queuedByID, ta.ID)
}

func (s *actionScheduler) allocatePlanActionIDs(count int) []int64 {
	actionIDs := make([]int64, count)
	baseID := s.nextActionID
	s.nextActionID += int64(count)

	for i := 0; i < count; i++ {
		actionIDs[i] = baseID + int64(i)
	}

	return actionIDs
}

func (l *retryLedger) releaseHeldAction(key RetryWorkKey) *trackedAction {
	held, ok := l.heldByKey[key]
	if !ok || held == nil {
		return nil
	}

	delete(l.heldByKey, key)
	if !held.ScopeKey.IsZero() {
		keys := l.heldByScope[held.ScopeKey]
		filtered := keys[:0]
		for _, existing := range keys {
			if existing != key {
				filtered = append(filtered, existing)
			}
		}
		if len(filtered) == 0 {
			delete(l.heldByScope, held.ScopeKey)
		} else {
			l.heldByScope[held.ScopeKey] = filtered
		}
	}

	return held.Tracked
}

func (l *retryLedger) earliestHeldRetryAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, held := range l.heldByKey {
		if held == nil || held.Reason != heldReasonRetry || held.NextRetry.IsZero() {
			continue
		}
		if !found || held.NextRetry.Before(earliest) {
			earliest = held.NextRetry
			found = true
		}
	}

	return earliest, found
}

func (l *retryLedger) applyPersistedRetryAdmission(
	now time.Time,
	ta *trackedAction,
	decision *admissionDecision,
) {
	if ta == nil || decision == nil {
		return
	}

	row, ok := l.rowsByKey[decision.RetryWorkKey]
	if !ok || (!decision.ClearScopeKey.IsZero() && row.ScopeKey == decision.ClearScopeKey) {
		return
	}

	switch {
	case row.Blocked && !row.ScopeKey.IsZero() &&
		(!ta.IsTrial || ta.TrialScopeKey.IsZero() || row.ScopeKey != ta.TrialScopeKey):
		decision.Kind = admissionHoldScope
		decision.ScopeKey = row.ScopeKey
	case row.NextRetryAt > 0:
		nextRetryAt := time.Unix(0, row.NextRetryAt)
		if nextRetryAt.After(now) {
			decision.Kind = admissionHoldRetry
			decision.NextRetryAt = nextRetryAt
		}
	}
}

func (l *scopeLedger) replaceActiveScopes(blocks []activeScope) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.active = l.active[:0]
	l.active = append(l.active, blocks...)
}

func (l *scopeLedger) upsertActiveScope(block *activeScope) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.active = upsertScope(l.active, block)
}

func (l *scopeLedger) removeActiveScope(key ScopeKey) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.active = removeScope(l.active, key)
}

func (l *scopeLedger) lookupActiveScope(key ScopeKey) (activeScope, bool) {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return lookupScope(l.active, key)
}

func (l *scopeLedger) hasActiveScope(key ScopeKey) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return hasScope(l.active, key)
}

func (l *scopeLedger) findBlockingScope(ta *trackedAction) ScopeKey {
	l.mu.RLock()
	defer l.mu.RUnlock()

	return findBlockingScope(l.active, ta)
}

func (l *scopeLedger) snapshotActiveScopes() []activeScope {
	l.mu.RLock()
	defer l.mu.RUnlock()

	blocks := make([]activeScope, len(l.active))
	copy(blocks, l.active)

	return blocks
}
