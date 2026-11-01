package sync

import (
	"context"
	"time"
)

// trackedAction pairs an Action with an ID and a per-action cancel function.
// Workers pull TrackedActions from the ready channel. The ID is a sequential
// counter (assigned by the engine) used as a unique key for the graph's
// internal maps.
type trackedAction struct {
	Action Action
	ID     int64
	Cancel context.CancelFunc

	// IsTrial marks this action as a scope trial — a real action dispatched
	// from blocked retry work to test whether a blocked scope has recovered (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies which scope this trial is testing. Set by
	// DispatchTrial, propagated through ActionCompletion so the engine knows
	// which scope to release on trial success.
	TrialScopeKey ScopeKey

	// TrialScopeGeneration is the scope's Generation when this trial was
	// released. A success from an earlier generation (the scope re-blocked
	// while it ran) is an ordinary success and releases nothing.
	TrialScopeGeneration uint64

	// AdmitAt is the engine-clock instant before which the dispatch loops
	// keep this admitted action in the outbox: a member of a released ramp
	// batch is spread over releaseBatchAdmissionWindow rather than sent in
	// the instant of the release (R-6.8.24). Zero means now. Cleared when
	// the action is dispatched or held.
	AdmitAt time.Time
}
