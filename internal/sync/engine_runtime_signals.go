package sync

// runtimeSignals is how completion, admission, and scope decisions ask the
// runtime to do something later.
//
// One-shot and watch differ in exactly one way that these decisions can
// observe: whether there is a future. A one-shot pass returns when its work
// settles, so nothing armed during it could ever fire, and marking truth dirty
// has no reader. Watch keeps the runtime alive across timer ticks and
// observation batches, so all five requests mean something.
//
// This was previously expressed by passing `watch *watchRuntime` into
// engineFlow methods and nil-checking it at nineteen sites -- a parent taking
// its own child as a nullable parameter, since watchRuntime embeds *engineFlow.
// The no-op implementation states the same thing as a fact about the domain
// rather than as an absence to be checked for.
type runtimeSignals interface {
	// armHeldTimers asks the runtime to schedule the next due held retry or
	// trial, whichever comes first.
	armHeldTimers()
	// armRetryTimer asks the runtime to schedule the next due held retry.
	armRetryTimer()
	// armTrialTimer asks the runtime to schedule the next due scope trial.
	armTrialTimer()
	// kickDueHeldRelease asks the runtime to release already-due held
	// retry work immediately rather than waiting for the next tick.
	kickDueHeldRelease()
	// markReplanNeeded reports that committed truth changed under the current
	// plan, so the runtime should replan from it.
	markReplanNeeded()
}

// noRuntimeSignals is the one-shot implementation. Every method is a no-op
// because a one-shot pass has no future in which a timer could fire or a
// replan could be consumed.
type noRuntimeSignals struct{}

func (noRuntimeSignals) armHeldTimers()      {}
func (noRuntimeSignals) armRetryTimer()      {}
func (noRuntimeSignals) armTrialTimer()      {}
func (noRuntimeSignals) kickDueHeldRelease() {}
func (noRuntimeSignals) markReplanNeeded()   {}

// watchInspector is the debug-only view the invariant pass needs into watch
// state. It is separate from runtimeSignals because the two have different
// lifetimes: signals are a production path, while these reads happen only when
// engine.assertInvariants is enabled, which production leaves off.
type watchInspector interface {
	phase() watchRuntimePhase
	isDraining() bool
	hasRetryTimer() bool
	hasTrialTimer() bool
	hasActiveRefresh() bool
}

// noWatchInspector reports the invariants a one-shot runtime trivially
// satisfies: it never drains, arms no watch timers, and runs no refresh.
type noWatchInspector struct{}

func (noWatchInspector) phase() watchRuntimePhase { return watchRuntimePhaseRunning }
func (noWatchInspector) isDraining() bool         { return false }
func (noWatchInspector) hasRetryTimer() bool      { return false }
func (noWatchInspector) hasTrialTimer() bool      { return false }
func (noWatchInspector) hasActiveRefresh() bool   { return false }
