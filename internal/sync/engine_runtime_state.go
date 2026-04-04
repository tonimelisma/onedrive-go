package sync

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func (rt *watchRuntime) phase() watchRuntimePhase {
	return rt.watchRuntimeState.phase
}

func (rt *watchRuntime) isDraining() bool {
	return rt.phase() == watchRuntimePhaseDraining
}

func (rt *watchRuntime) enterDraining() bool {
	if rt.watchRuntimeState.phase == watchRuntimePhaseDraining {
		return false
	}

	rt.watchRuntimeState.phase = watchRuntimePhaseDraining

	return true
}

func (rt *watchRuntime) replaceActiveScopes(blocks []synctypes.ScopeBlock) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = rt.activeScopes[:0]
	rt.activeScopes = append(rt.activeScopes, blocks...)
}

func (rt *watchRuntime) upsertActiveScope(block *synctypes.ScopeBlock) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = syncdispatch.UpsertScope(rt.activeScopes, block)
}

func (rt *watchRuntime) removeActiveScope(key synctypes.ScopeKey) {
	rt.activeScopesMu.Lock()
	defer rt.activeScopesMu.Unlock()

	rt.activeScopes = syncdispatch.RemoveScope(rt.activeScopes, key)
}

func (rt *watchRuntime) lookupActiveScope(key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return syncdispatch.LookupScope(rt.activeScopes, key)
}

func (rt *watchRuntime) hasActiveScope(key synctypes.ScopeKey) bool {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return syncdispatch.HasScope(rt.activeScopes, key)
}

func (rt *watchRuntime) findBlockingScope(ta *synctypes.TrackedAction) synctypes.ScopeKey {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return syncdispatch.FindBlockingScope(rt.activeScopes, ta)
}

func (rt *watchRuntime) activeScopeKeys() []synctypes.ScopeKey {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	return syncdispatch.ScopeKeys(rt.activeScopes)
}

func (rt *watchRuntime) snapshotActiveScopes() []synctypes.ScopeBlock {
	rt.activeScopesMu.RLock()
	defer rt.activeScopesMu.RUnlock()

	blocks := make([]synctypes.ScopeBlock, len(rt.activeScopes))
	copy(blocks, rt.activeScopes)

	return blocks
}

func (rt *watchRuntime) earliestTrialAt() (time.Time, bool) {
	return syncdispatch.EarliestTrialAt(rt.snapshotActiveScopes())
}

func (rt *watchRuntime) dueTrials(now time.Time) []synctypes.ScopeKey {
	return syncdispatch.DueTrials(rt.snapshotActiveScopes(), now)
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
