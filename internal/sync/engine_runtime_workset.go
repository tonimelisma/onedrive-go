package sync

import (
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

func (flow *engineFlow) initializeRuntimeState(runtime *runtimePlan) {
	flow.retryWork.install(runtime.RetryRows)

	flow.retries.heldByKey = make(map[syncdomain.RetryWorkKey]*heldAction)
	flow.retries.heldByScope = make(map[syncdomain.ScopeKey][]syncdomain.RetryWorkKey)
	flow.sched.queuedByID = make(map[int64]struct{})
	flow.sched.runningByID = make(map[int64]struct{})
	flow.sched.runningCount = 0
	flow.sched.resetColdStart(flow.cfg.poolSize)
	flow.retries.nextHeldOrder = 0

	activeScopes := make([]syncdomain.ActiveScope, 0, len(runtime.BlockScopes))
	for i := range runtime.BlockScopes {
		if runtime.BlockScopes[i] == nil {
			continue
		}
		activeScopes = append(activeScopes, syncdomain.ActiveScopeFromBlockScopeRow(runtime.BlockScopes[i]))
	}
	flow.scopes.replaceActiveScopes(activeScopes)
	// A target already under a throttle (or service) scope when the plan
	// starts is the scope's to ramp (R-6.8.24); the slow start is for a
	// target nothing is known about (R-6.8.25).
	for i := range activeScopes {
		flow.handOverColdStart(activeScopes[i].Key)
	}
	if flow.scopes.state == nil {
		flow.scopes.state = syncexec.NewScopeState(flow.deps.now, flow.deps.logger)
	}
}

func (flow *engineFlow) holdAction(ta *syncdomain.TrackedAction, reason heldReason, scopeKey syncdomain.ScopeKey, nextRetry time.Time) {
	if ta == nil {
		return
	}

	flow.sched.markFinished(ta)
	ta.AdmitAt = time.Time{}
	key := syncdomain.RetryWorkKeyForAction(&ta.Action)
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

func (flow *engineFlow) dueRetryKeys(now time.Time) []syncdomain.RetryWorkKey {
	keys := make([]syncdomain.RetryWorkKey, 0)
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

func (flow *engineFlow) dueTrialKeys(now time.Time) []syncdomain.RetryWorkKey {
	activeScopes := flow.scopes.snapshotActiveScopes()
	sort.Slice(activeScopes, func(i, j int) bool {
		if !activeScopes[i].NextTrialAt.Equal(activeScopes[j].NextTrialAt) {
			return activeScopes[i].NextTrialAt.Before(activeScopes[j].NextTrialAt)
		}
		return activeScopes[i].Key.String() < activeScopes[j].Key.String()
	})

	var keys []syncdomain.RetryWorkKey
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
