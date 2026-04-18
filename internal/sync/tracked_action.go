package sync

import "context"

// TrackedAction pairs an Action with an ID and a per-action cancel function.
// Workers pull TrackedActions from the ready channel. The ID is a sequential
// counter (assigned by the engine) used as a unique key for the graph's
// internal maps.
type TrackedAction struct {
	Action Action
	ID     int64
	Cancel context.CancelFunc

	// IsTrial marks this action as a scope trial — a real action dispatched
	// from the held queue to test whether a blocked scope has recovered (R-2.10.5).
	IsTrial bool

	// TrialScopeKey identifies which scope this trial is testing. Set by
	// DispatchTrial, propagated through ActionCompletion so the engine knows
	// which scope to release on trial success.
	TrialScopeKey ScopeKey
}
