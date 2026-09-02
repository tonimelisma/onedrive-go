package sync

import (
	"sort"
	"time"
)

func (flow *engineFlow) initializeRuntimeState(runtime *runtimePlan) {
	flow.retries.rowsByKey = make(map[RetryWorkKey]RetryWorkRow, len(runtime.RetryRows))
	for i := range runtime.RetryRows {
		row := runtime.RetryRows[i]
		flow.retries.rowsByKey[row.WorkKey()] = row
	}

	flow.retries.heldByKey = make(map[RetryWorkKey]*heldAction)
	flow.retries.heldByScope = make(map[ScopeKey][]RetryWorkKey)
	flow.sched.queuedByID = make(map[int64]struct{})
	flow.sched.runningByID = make(map[int64]struct{})
	flow.sched.runningCount = 0
	flow.retries.nextHeldOrder = 0

	activeScopes := make([]activeScope, 0, len(runtime.BlockScopes))
	for i := range runtime.BlockScopes {
		if runtime.BlockScopes[i] == nil {
			continue
		}
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(runtime.BlockScopes[i]))
	}
	flow.scopes.replaceActiveScopes(activeScopes)
	if flow.scopes.state == nil {
		flow.scopes.state = newScopeState(flow.engine.nowFunc, flow.deps.logger)
	}
}

func (flow *engineFlow) holdAction(ta *trackedAction, reason heldReason, scopeKey ScopeKey, nextRetry time.Time) {
	if ta == nil {
		return
	}

	flow.sched.markFinished(ta)
	key := retryWorkKeyForAction(&ta.Action)
	flow.retries.nextHeldOrder++
	held := &heldAction{
		Tracked:   ta,
		Reason:    reason,
		ScopeKey:  scopeKey,
		NextRetry: nextRetry,
		HeldOrder: flow.retries.nextHeldOrder,
	}
	flow.retries.heldByKey[key] = held
	if !scopeKey.IsZero() {
		flow.retries.heldByScope[scopeKey] = append(flow.retries.heldByScope[scopeKey], key)
	}
}

func (flow *engineFlow) hasDueHeldWork(now time.Time) bool {
	for _, held := range flow.retries.heldByKey {
		if held == nil {
			continue
		}
		if held.Reason == heldReasonRetry && !held.NextRetry.After(now) {
			return true
		}
	}

	for _, scope := range flow.scopes.snapshotActiveScopes() {
		if scope.NextTrialAt.After(now) {
			continue
		}
		if len(flow.retries.heldByScope[scope.Key]) > 0 {
			return true
		}
	}

	return false
}

func (flow *engineFlow) dueRetryKeys(now time.Time) []RetryWorkKey {
	keys := make([]RetryWorkKey, 0)
	for key, held := range flow.retries.heldByKey {
		if held == nil || held.Reason != heldReasonRetry || held.NextRetry.After(now) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := flow.retries.heldByKey[keys[i]]
		right := flow.retries.heldByKey[keys[j]]
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
	activeScopes := flow.scopes.snapshotActiveScopes()
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
		for _, key := range flow.retries.heldByScope[scope.Key] {
			if held := flow.retries.heldByKey[key]; held != nil && held.Reason == heldReasonScope {
				keys = append(keys, key)
				break
			}
		}
	}

	return keys
}

func (flow *engineFlow) releaseHeldScope(scopeKey ScopeKey) {
	keys := append([]RetryWorkKey(nil), flow.retries.heldByScope[scopeKey]...)
	now := flow.deps.now()
	for _, key := range keys {
		held := flow.retries.heldByKey[key]
		if held == nil || held.Reason != heldReasonScope {
			continue
		}
		if row, ok := flow.retries.rowsByKey[key]; ok && row.Blocked && row.ScopeKey == scopeKey {
			row.Blocked = false
			row.NextRetryAt = now.UnixNano()
			flow.retries.rowsByKey[key] = row
		}
		held.Reason = heldReasonRetry
		held.ScopeKey = ScopeKey{}
		held.NextRetry = now
	}
	delete(flow.retries.heldByScope, scopeKey)
}
