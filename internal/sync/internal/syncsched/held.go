package syncsched

import (
	"sort"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Held is one action waiting for a retry deadline or a blocked scope.
type Held struct {
	Tracked   *syncdomain.TrackedAction
	Reason    HeldReason
	ScopeKey  syncdomain.ScopeKey
	NextRetry time.Time
	HeldOrder uint64
}

// HeldReason is why an action is held.
type HeldReason string

const (
	// HeldRetry is held until its retry deadline.
	HeldRetry HeldReason = "retry"
	// HeldScope is held until its block scope's trial or release.
	HeldScope HeldReason = "scope"
)

// Hold takes ta out of the queued and running sets and holds it under its
// retry-work key, for reason, under scope (zero for none) until nextRetry
// (zero for none). Holding a key again replaces the earlier hold.
func (s *Scheduler) Hold(ta *syncdomain.TrackedAction, reason HeldReason, scope syncdomain.ScopeKey, nextRetry time.Time) {
	if ta == nil {
		return
	}

	s.Finished(ta)
	ta.AdmitAt = time.Time{}
	key := syncdomain.RetryWorkKeyForAction(&ta.Action)
	s.nextHeldOrder++
	s.held[key] = &Held{
		Tracked:   ta,
		Reason:    reason,
		ScopeKey:  scope,
		NextRetry: nextRetry,
		HeldOrder: s.nextHeldOrder,
	}
	if !scope.IsZero() {
		s.heldByScope[scope] = append(s.heldByScope[scope], key)
	}
}

// Release takes the action held under key out of the held work and returns
// it; nil when nothing is held under key.
func (s *Scheduler) Release(key syncdomain.RetryWorkKey) *syncdomain.TrackedAction {
	held, ok := s.held[key]
	if !ok || held == nil {
		return nil
	}

	delete(s.held, key)
	if !held.ScopeKey.IsZero() {
		keys := s.heldByScope[held.ScopeKey]
		filtered := keys[:0]
		for _, existing := range keys {
			if existing != key {
				filtered = append(filtered, existing)
			}
		}
		if len(filtered) == 0 {
			delete(s.heldByScope, held.ScopeKey)
		} else {
			s.heldByScope[held.ScopeKey] = filtered
		}
	}

	return held.Tracked
}

// HeldWork is the hold under key, if any.
func (s *Scheduler) HeldWork(key syncdomain.RetryWorkKey) (Held, bool) {
	held, ok := s.held[key]
	if !ok || held == nil {
		return Held{}, false
	}

	return *held, true
}

// HeldCount is how many actions are held.
func (s *Scheduler) HeldCount() int {
	return len(s.held)
}

// HeldKeys is every retry-work key held, in no particular order.
func (s *Scheduler) HeldKeys() []syncdomain.RetryWorkKey {
	keys := make([]syncdomain.RetryWorkKey, 0, len(s.held))
	for key := range s.held {
		keys = append(keys, key)
	}

	return keys
}

// HeldUnderScope is how many actions are held under scope.
func (s *Scheduler) HeldUnderScope(scope syncdomain.ScopeKey) int {
	return len(s.heldByScope[scope])
}

// EarliestHeldRetryAt is the soonest retry deadline of the work held for a
// retry; ok is false when none is.
func (s *Scheduler) EarliestHeldRetryAt() (time.Time, bool) {
	var earliest time.Time
	found := false

	for _, held := range s.held {
		if held == nil || held.Reason != HeldRetry || held.NextRetry.IsZero() {
			continue
		}
		if !found || held.NextRetry.Before(earliest) {
			earliest = held.NextRetry
			found = true
		}
	}

	return earliest, found
}

// HasDueHeldWork reports whether a held retry is due at now, or an active
// scope whose trial is due holds work.
func (s *Scheduler) HasDueHeldWork(now time.Time) bool {
	for _, held := range s.held {
		if held == nil {
			continue
		}
		if held.Reason == HeldRetry && !held.NextRetry.After(now) {
			return true
		}
	}

	for _, scope := range s.Scopes() {
		if scope.NextTrialAt.After(now) {
			continue
		}
		if len(s.heldByScope[scope.Key]) > 0 {
			return true
		}
	}

	return false
}

// DueRetries is the keys of the held retries due at now, soonest deadline
// first and in held order among equal ones.
func (s *Scheduler) DueRetries(now time.Time) []syncdomain.RetryWorkKey {
	keys := make([]syncdomain.RetryWorkKey, 0)
	for key, held := range s.held {
		if held == nil || held.Reason != HeldRetry || held.NextRetry.After(now) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		left := s.held[keys[i]]
		right := s.held[keys[j]]
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

// DueTrials is, for each active scope whose trial is due at now (soonest
// first, then by key), the first action held under it: the one its trial
// runs.
func (s *Scheduler) DueTrials(now time.Time) []syncdomain.RetryWorkKey {
	activeScopes := s.Scopes()
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
		for _, key := range s.heldByScope[scope.Key] {
			if held := s.held[key]; held != nil && held.Reason == HeldScope {
				keys = append(keys, key)
				break
			}
		}
	}

	return keys
}

// HeldBatch is the first limit actions held under scope, in held order: a
// release ramp's next batch.
func (s *Scheduler) HeldBatch(scope syncdomain.ScopeKey, limit int) []syncdomain.RetryWorkKey {
	batch := make([]syncdomain.RetryWorkKey, 0, limit)
	for _, work := range s.heldByScope[scope] {
		if len(batch) == limit {
			break
		}
		if held := s.held[work]; held != nil && held.Reason == HeldScope {
			batch = append(batch, work)
		}
	}

	return batch
}

// IsHeld reports whether an action is held under key.
func (s *Scheduler) IsHeld(key syncdomain.RetryWorkKey) bool {
	_, ok := s.HeldWork(key)
	return ok
}
