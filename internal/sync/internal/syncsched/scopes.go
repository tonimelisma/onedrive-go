package syncsched

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// ReplaceScopes makes blocks the active scopes.
func (s *Scheduler) ReplaceScopes(blocks []syncdomain.ActiveScope) {
	s.checkOwner()

	s.active = s.active[:0]
	s.active = append(s.active, blocks...)
}

// PutScope adds block, or replaces the active scope with its key.
func (s *Scheduler) PutScope(block *syncdomain.ActiveScope) {
	s.checkOwner()

	s.active = UpsertScope(s.active, block)
}

// DropScope removes the active scope with key, if any.
func (s *Scheduler) DropScope(key syncdomain.ScopeKey) {
	s.checkOwner()

	s.active = RemoveScope(s.active, key)
}

// Scope is the active scope with key, if any.
func (s *Scheduler) Scope(key syncdomain.ScopeKey) (syncdomain.ActiveScope, bool) {
	s.checkOwner()

	return LookupScope(s.active, key)
}

// HasScope reports whether a scope with key is active.
func (s *Scheduler) HasScope(key syncdomain.ScopeKey) bool {
	s.checkOwner()

	return HasScope(s.active, key)
}

// Scopes is a copy of the active scopes.
func (s *Scheduler) Scopes() []syncdomain.ActiveScope {
	s.checkOwner()

	blocks := make([]syncdomain.ActiveScope, len(s.active))
	copy(blocks, s.active)

	return blocks
}

// BlockingScope is the active scope that blocks ta, the most specific one
// when several do; zero when none does.
func (s *Scheduler) BlockingScope(ta *syncdomain.TrackedAction) syncdomain.ScopeKey {
	s.checkOwner()

	return FindBlockingScope(s.active, ta)
}

// EarliestPauseTrialAt is the soonest next trial among the active scopes
// that are a server pause still blocking work: a target throttle (429) or
// the service scope (503), with no release allowance yet. Permission and
// quota scopes are not pauses: waiting does not clear them.
func (s *Scheduler) EarliestPauseTrialAt() (time.Time, bool) {
	s.checkOwner()

	var earliest time.Time
	found := false
	for i := range s.active {
		scope := &s.active[i]
		if !IsPauseScope(scope.Key) || scope.ReleaseAllowance > 0 {
			continue
		}
		if !found || scope.NextTrialAt.Before(earliest) {
			earliest = scope.NextTrialAt
			found = true
		}
	}

	return earliest, found
}

// IsPauseScope reports whether a scope is one OneDrive asked for with a
// Retry-After: the drive's throttle or the service outage scope.
func IsPauseScope(key syncdomain.ScopeKey) bool {
	return key.IsThrottleTarget() || key.IsGlobal()
}

// ObserveFailure feeds a completion into scope detection and reports whether
// a block scope should activate. Before any plan is installed there are no
// windows and nothing activates.
func (s *Scheduler) ObserveFailure(r *syncdomain.ActionCompletion) syncdomain.ScopeUpdateResult {
	if s.windows == nil {
		return syncdomain.ScopeUpdateResult{}
	}

	return s.windows.UpdateScope(r)
}

// ObserveSuccess records a success, which resets scope detection's windows.
func (s *Scheduler) ObserveSuccess() {
	if s.windows != nil {
		s.windows.RecordSuccess()
	}
}
