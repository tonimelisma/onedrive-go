package syncsched

import (
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// AdmissionKind is what admission decided for a ready action.
type AdmissionKind int

const (
	// AdmitNow dispatches the action.
	AdmitNow AdmissionKind = iota
	// HoldForRetry holds it until NextRetryAt.
	HoldForRetry
	// HoldForScope holds it under ScopeKey.
	HoldForScope
)

// Admission is admission's decision for one ready action. ClearScopeKey is
// the scope a trial no longer belongs to, whose blocked row the caller
// clears; RetryWorkKey is the action's retry-work key.
type Admission struct {
	Action        *syncdomain.TrackedAction
	Kind          AdmissionKind
	ScopeKey      syncdomain.ScopeKey
	NextRetryAt   time.Time
	ClearScopeKey syncdomain.ScopeKey
	RetryWorkKey  syncdomain.RetryWorkKey
}

// Admit decides, at now, whether a dependency-ready action dispatches or is
// held: its durable retry row (row, which reports the row the run holds for
// a key) holds it under the scope that blocked it or until its retry
// deadline, and an active scope that blocks it holds it under that scope. A
// trial passes the scope it is the trial of.
func (s *Scheduler) Admit(
	now time.Time,
	ta *syncdomain.TrackedAction,
	row func(syncdomain.RetryWorkKey) (syncdomain.RetryWorkRow, bool),
) Admission {
	decision := Admission{
		Action:       ta,
		Kind:         AdmitNow,
		RetryWorkKey: syncdomain.RetryWorkKeyForAction(&ta.Action),
	}
	if ta.IsTrial && !ta.TrialScopeKey.IsZero() &&
		!ta.TrialScopeKey.BlocksAction(ta.Action.Path, ta.Action.ThrottleTargetKey(), ta.Action.Type) {
		decision.ClearScopeKey = ta.TrialScopeKey
	}

	admitPersisted(now, ta, &decision, row)
	s.admitActiveScope(ta, &decision)

	return decision
}

func admitPersisted(
	now time.Time,
	ta *syncdomain.TrackedAction,
	decision *Admission,
	rowFor func(syncdomain.RetryWorkKey) (syncdomain.RetryWorkRow, bool),
) {
	if rowFor == nil {
		return
	}
	row, ok := rowFor(decision.RetryWorkKey)
	if !ok || (!decision.ClearScopeKey.IsZero() && row.ScopeKey == decision.ClearScopeKey) {
		return
	}

	switch {
	case row.Blocked && !row.ScopeKey.IsZero() &&
		(!ta.IsTrial || ta.TrialScopeKey.IsZero() || row.ScopeKey != ta.TrialScopeKey):
		decision.Kind = HoldForScope
		decision.ScopeKey = row.ScopeKey
	case syncdomain.StoredTimeIsSet(row.NextRetryAt):
		nextRetryAt := time.Unix(0, row.NextRetryAt)
		if nextRetryAt.After(now) {
			decision.Kind = HoldForRetry
			decision.NextRetryAt = nextRetryAt
		}
	}
}

func (s *Scheduler) admitActiveScope(ta *syncdomain.TrackedAction, decision *Admission) {
	if decision.Kind != AdmitNow {
		return
	}

	scopeKey := s.BlockingScope(ta)
	if scopeKey.IsZero() {
		return
	}
	if ta.IsTrial && !ta.TrialScopeKey.IsZero() && scopeKey == ta.TrialScopeKey {
		return
	}

	decision.Kind = HoldForScope
	decision.ScopeKey = scopeKey
}
