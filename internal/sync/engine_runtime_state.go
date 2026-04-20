package sync

import "time"

func (rt *watchRuntime) phase() watchRuntimePhase {
	return rt.loop.phase
}

func (rt *watchRuntime) isDraining() bool {
	return rt.phase() == watchRuntimePhaseDraining
}

func (rt *watchRuntime) enterDraining() bool {
	if rt.loop.phase == watchRuntimePhaseDraining {
		return false
	}

	rt.loop.phase = watchRuntimePhaseDraining

	return true
}

func (rt *watchRuntime) currentOutbox() []*TrackedAction {
	return rt.loop.outbox
}

func (rt *watchRuntime) replaceOutbox(outbox []*TrackedAction) {
	if len(outbox) == 0 {
		rt.loop.outbox = nil
		return
	}

	rt.loop.outbox = append(rt.loop.outbox[:0], outbox...)
}

func (rt *watchRuntime) appendOutbox(actions []*TrackedAction) {
	if len(actions) == 0 {
		return
	}

	rt.loop.outbox = append(rt.loop.outbox, actions...)
}

func (rt *watchRuntime) consumeOutboxHead() {
	if len(rt.loop.outbox) == 0 {
		return
	}

	rt.loop.outbox = rt.loop.outbox[1:]
}

func (rt *watchRuntime) replaceActiveScopes(blocks []ActiveScope) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = rt.activeScopes[:0]
	rt.activeScopes = append(rt.activeScopes, blocks...)
}

func (rt *watchRuntime) upsertActiveScope(block *ActiveScope) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = UpsertScope(rt.activeScopes, block)
}

func (rt *watchRuntime) removeActiveScope(key ScopeKey) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = RemoveScope(rt.activeScopes, key)
}

func (rt *watchRuntime) lookupActiveScope(key ScopeKey) (ActiveScope, bool) {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return LookupScope(rt.activeScopes, key)
}

func (rt *watchRuntime) hasActiveScope(key ScopeKey) bool {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return HasScope(rt.activeScopes, key)
}

func (rt *watchRuntime) findBlockingScope(ta *TrackedAction) ScopeKey {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return FindBlockingScope(rt.activeScopes, ta)
}

func (rt *watchRuntime) activeScopeKeys() []ScopeKey {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return ScopeKeys(rt.activeScopes)
}

func (rt *watchRuntime) snapshotActiveScopes() []ActiveScope {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	blocks := make([]ActiveScope, len(rt.activeScopes))
	copy(blocks, rt.activeScopes)

	return blocks
}

func (rt *watchRuntime) earliestTrialAt() (time.Time, bool) {
	return EarliestTrialAt(rt.snapshotActiveScopes())
}

func (rt *watchRuntime) dueTrials(now time.Time) []ScopeKey {
	return DueTrials(rt.snapshotActiveScopes(), now)
}

func (rt *watchRuntime) resetTrialTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.trialTimer != nil {
		rt.trialTimer.Stop()
		rt.trialTimer = nil
	}

	rt.trialTimer = next
}

func (rt *watchRuntime) hasTrialTimer() bool {
	rt.timerMu.RLock()
	defer rt.timerMu.RUnlock()

	return rt.trialTimer != nil
}

func (rt *watchRuntime) resetRetryTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.retryTimer != nil {
		rt.retryTimer.Stop()
		rt.retryTimer = nil
	}

	rt.retryTimer = next
}

func (rt *watchRuntime) hasRetryTimer() bool {
	rt.timerMu.RLock()
	defer rt.timerMu.RUnlock()

	return rt.retryTimer != nil
}

func (rt *watchRuntime) resetReconcileTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.reconcileTimer != nil {
		rt.reconcileTimer.Stop()
		rt.reconcileTimer = nil
	}

	rt.reconcileTimer = next
}
