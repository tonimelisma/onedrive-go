package sync

import (
	"context"
	"sort"
	"time"
)

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

func (rt *watchRuntime) beginSyncStatusBatch(startedAt time.Time) {
	if rt.syncBatch.active {
		return
	}

	rt.syncBatch = watchSyncBatchState{
		active:        true,
		startedAt:     startedAt,
		succeededBase: rt.succeeded,
		failedBase:    rt.failed,
		errorBase:     len(rt.syncErrors),
	}
}

func (rt *watchRuntime) clearSyncStatusBatch() {
	rt.syncBatch = watchSyncBatchState{}
}

func (rt *watchRuntime) finishSyncStatusBatch(ctx context.Context, mode Mode) {
	if !rt.syncBatch.active {
		return
	}

	update := &SyncStatusUpdate{
		SyncedAt:  rt.engine.nowFunc(),
		Duration:  rt.engine.since(rt.syncBatch.startedAt),
		Succeeded: rt.succeeded - rt.syncBatch.succeededBase,
		Failed:    rt.failed - rt.syncBatch.failedBase,
	}
	if rt.syncBatch.errorBase < len(rt.syncErrors) {
		update.Errors = append(update.Errors, rt.syncErrors[rt.syncBatch.errorBase:]...)
	}

	rt.clearSyncStatusBatch()
	rt.engine.writeSyncStatusBestEffort(ctx, mode, false, update)
}

func (rt *watchRuntime) maybeFinishSyncStatusBatch(
	ctx context.Context,
	mode Mode,
	outbox []*TrackedAction,
) {
	if len(outbox) > 0 || rt.runningCount > 0 || rt.hasDueHeldWork(rt.engine.nowFunc()) {
		return
	}

	rt.finishSyncStatusBatch(ctx, mode)
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

func (flow *engineFlow) initializePreparedRuntime(prepared *preparedCurrentActionPlan) {
	flow.retryRowsByKey = make(map[RetryWorkKey]RetryWorkRow, len(prepared.retryRows))
	for i := range prepared.retryRows {
		row := prepared.retryRows[i]
		flow.retryRowsByKey[retryWorkKeyForRetryWork(&row)] = row
	}

	flow.heldByKey = make(map[RetryWorkKey]*heldAction)
	flow.heldScopeOrder = make(map[ScopeKey][]RetryWorkKey)
	flow.queuedByID = make(map[int64]struct{})
	flow.runningByID = make(map[int64]struct{})
	flow.runningCount = 0
	flow.nextHeldOrder = 0

	activeScopes := make([]ActiveScope, 0, len(prepared.blockScopes))
	for i := range prepared.blockScopes {
		if prepared.blockScopes[i] == nil {
			continue
		}
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(prepared.blockScopes[i]))
	}
	flow.replaceActiveScopes(activeScopes)
	if flow.scopeState == nil {
		flow.scopeState = NewScopeState(flow.engine.nowFunc, flow.engine.logger)
	}
}

func (flow *engineFlow) markQueued(ta *TrackedAction) {
	if ta == nil {
		return
	}
	flow.queuedByID[ta.ID] = struct{}{}
}

func (flow *engineFlow) markRunning(ta *TrackedAction) {
	if ta == nil {
		return
	}
	delete(flow.queuedByID, ta.ID)
	if _, ok := flow.runningByID[ta.ID]; ok {
		return
	}
	flow.runningByID[ta.ID] = struct{}{}
	flow.runningCount++
}

func (flow *engineFlow) markFinished(ta *TrackedAction) {
	if ta == nil {
		return
	}
	if _, ok := flow.runningByID[ta.ID]; ok {
		delete(flow.runningByID, ta.ID)
		if flow.runningCount > 0 {
			flow.runningCount--
		}
	}
	delete(flow.queuedByID, ta.ID)
}

func (flow *engineFlow) holdAction(ta *TrackedAction, reason heldReason, scopeKey ScopeKey, nextRetry time.Time) {
	if ta == nil {
		return
	}

	flow.markFinished(ta)
	key := retryWorkKeyForAction(&ta.Action)
	flow.nextHeldOrder++
	held := &heldAction{
		Tracked:   ta,
		Reason:    reason,
		ScopeKey:  scopeKey,
		NextRetry: nextRetry,
		HeldOrder: flow.nextHeldOrder,
	}
	flow.heldByKey[key] = held
	if !scopeKey.IsZero() {
		flow.heldScopeOrder[scopeKey] = append(flow.heldScopeOrder[scopeKey], key)
	}
}

func (flow *engineFlow) releaseHeldAction(key RetryWorkKey) *TrackedAction {
	held, ok := flow.heldByKey[key]
	if !ok || held == nil {
		return nil
	}

	delete(flow.heldByKey, key)
	if !held.ScopeKey.IsZero() {
		keys := flow.heldScopeOrder[held.ScopeKey]
		filtered := keys[:0]
		for _, existing := range keys {
			if existing != key {
				filtered = append(filtered, existing)
			}
		}
		if len(filtered) == 0 {
			delete(flow.heldScopeOrder, held.ScopeKey)
		} else {
			flow.heldScopeOrder[held.ScopeKey] = filtered
		}
	}

	return held.Tracked
}

func (flow *engineFlow) hasDueHeldWork(now time.Time) bool {
	for _, held := range flow.heldByKey {
		if held == nil {
			continue
		}
		if held.Reason == heldReasonRetry && !held.NextRetry.After(now) {
			return true
		}
	}

	for _, scope := range flow.snapshotActiveScopes() {
		if scope.NextTrialAt.After(now) {
			continue
		}
		if len(flow.heldScopeOrder[scope.Key]) > 0 {
			return true
		}
	}

	return false
}

func (flow *engineFlow) dueRetryKeys(now time.Time) []RetryWorkKey {
	keys := make([]RetryWorkKey, 0)
	for key, held := range flow.heldByKey {
		if held == nil || held.Reason != heldReasonRetry || held.NextRetry.After(now) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := flow.heldByKey[keys[i]]
		right := flow.heldByKey[keys[j]]
		if left == nil || right == nil {
			return keys[i].Path < keys[j].Path
		}
		if !left.NextRetry.Equal(right.NextRetry) {
			return left.NextRetry.Before(right.NextRetry)
		}
		return left.HeldOrder < right.HeldOrder
	})
	return keys
}

func (flow *engineFlow) dueTrialKeys(now time.Time) []RetryWorkKey {
	activeScopes := flow.snapshotActiveScopes()
	sort.Slice(activeScopes, func(i, j int) bool {
		if !activeScopes[i].NextTrialAt.Equal(activeScopes[j].NextTrialAt) {
			return activeScopes[i].NextTrialAt.Before(activeScopes[j].NextTrialAt)
		}
		return activeScopes[i].Key.String() < activeScopes[j].Key.String()
	})

	var keys []RetryWorkKey
	for _, scope := range activeScopes {
		if scope.NextTrialAt.After(now) {
			continue
		}
		for _, key := range flow.heldScopeOrder[scope.Key] {
			if held := flow.heldByKey[key]; held != nil && held.Reason == heldReasonScope {
				keys = append(keys, key)
				break
			}
		}
	}

	return keys
}

func (flow *engineFlow) releaseHeldScope(scopeKey ScopeKey) {
	keys := append([]RetryWorkKey(nil), flow.heldScopeOrder[scopeKey]...)
	for _, key := range keys {
		held := flow.heldByKey[key]
		if held == nil || held.Reason != heldReasonScope {
			continue
		}
		held.Reason = heldReasonRetry
		held.ScopeKey = ScopeKey{}
		held.NextRetry = flow.engine.nowFunc()
	}
	delete(flow.heldScopeOrder, scopeKey)
}

func (flow *engineFlow) replaceActiveScopes(blocks []ActiveScope) {
	flow.activeScopesMu.Lock()
	defer flow.activeScopesMu.Unlock()

	flow.activeScopes = flow.activeScopes[:0]
	flow.activeScopes = append(flow.activeScopes, blocks...)
}

func (rt *watchRuntime) replaceActiveScopes(blocks []ActiveScope) {
	rt.engineFlow.replaceActiveScopes(blocks)
}

func (flow *engineFlow) upsertActiveScope(block *ActiveScope) {
	flow.activeScopesMu.Lock()
	defer flow.activeScopesMu.Unlock()

	flow.activeScopes = UpsertScope(flow.activeScopes, block)
}

func (rt *watchRuntime) upsertActiveScope(block *ActiveScope) {
	rt.engineFlow.upsertActiveScope(block)
}

func (flow *engineFlow) removeActiveScope(key ScopeKey) {
	flow.activeScopesMu.Lock()
	defer flow.activeScopesMu.Unlock()

	flow.activeScopes = RemoveScope(flow.activeScopes, key)
}

func (flow *engineFlow) lookupActiveScope(key ScopeKey) (ActiveScope, bool) {
	flow.activeScopesMu.RLock()
	defer flow.activeScopesMu.RUnlock()

	return LookupScope(flow.activeScopes, key)
}

func (rt *watchRuntime) lookupActiveScope(key ScopeKey) (ActiveScope, bool) {
	return rt.engineFlow.lookupActiveScope(key)
}

func (flow *engineFlow) hasActiveScope(key ScopeKey) bool {
	flow.activeScopesMu.RLock()
	defer flow.activeScopesMu.RUnlock()

	return HasScope(flow.activeScopes, key)
}

func (rt *watchRuntime) hasActiveScope(key ScopeKey) bool {
	return rt.engineFlow.hasActiveScope(key)
}

func (flow *engineFlow) findBlockingScope(ta *TrackedAction) ScopeKey {
	flow.activeScopesMu.RLock()
	defer flow.activeScopesMu.RUnlock()

	return FindBlockingScope(flow.activeScopes, ta)
}

func (flow *engineFlow) snapshotActiveScopes() []ActiveScope {
	flow.activeScopesMu.RLock()
	defer flow.activeScopesMu.RUnlock()

	blocks := make([]ActiveScope, len(flow.activeScopes))
	copy(blocks, flow.activeScopes)

	return blocks
}

func (rt *watchRuntime) snapshotActiveScopes() []ActiveScope {
	return rt.engineFlow.snapshotActiveScopes()
}

func (rt *watchRuntime) earliestTrialAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, scope := range rt.snapshotActiveScopes() {
		if len(rt.heldScopeOrder[scope.Key]) == 0 {
			continue
		}
		if !found || scope.NextTrialAt.Before(earliest) {
			earliest = scope.NextTrialAt
			found = true
		}
	}

	return earliest, found
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

func (rt *watchRuntime) resetRefreshTimer(next syncTimer) {
	rt.timerMu.Lock()
	defer rt.timerMu.Unlock()

	if rt.refreshTimer != nil {
		rt.refreshTimer.Stop()
		rt.refreshTimer = nil
	}

	rt.refreshTimer = next
}
