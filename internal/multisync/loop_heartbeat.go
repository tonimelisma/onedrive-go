package multisync

import (
	"sync/atomic"
	"time"
)

const (
	// loopHeartbeatInterval is how often an idle watch loop beats.
	loopHeartbeatInterval = 15 * time.Second
	// loopSilenceLimit is how long the loop may go without a beat outside a
	// declared long step before it counts as hung.
	loopSilenceLimit = 4 * loopHeartbeatInterval
	// watchStartupStepLimit holds the heartbeat while RunWatch starts its
	// runners; systemd does not enforce the watchdog before READY=1, and the
	// unit's TimeoutStartSec covers start-up.
	watchStartupStepLimit = 5 * time.Minute
	// watchRunnerStartStepLimit is the limit declared while one runner
	// starts. Starting has no bound of its own yet (only the transport's
	// timeouts hold it); this is the limit the heartbeat observes, not a
	// deadline anything enforces.
	watchRunnerStartStepLimit = 5 * time.Minute
	// watchLoopStepLimit is the limit declared while the loop applies a
	// config reload, a drive's event or a pass over the drive set. That work
	// is local: the config file and its lock (each change waits at most
	// config's lock timeout), the drive set computed in memory, removed
	// shortcuts' state files and the parent stores they are acknowledged to,
	// and lines to the console and the service manager. The drives it starts
	// or stops declare their own steps inside it. Like the runner start's,
	// it is the limit the heartbeat observes, not a deadline anything
	// enforces; past it the watchdog stops vouching for the loop.
	watchLoopStepLimit = 5 * time.Minute
)

// loopHeartbeat records the RunWatch loop's progress for the watchdog
// goroutine. The loop writes it; any goroutine reads it.
//
// The invariant the watchdog relies on: the loop never blocks outside a step
// it declared with beatFor, except for calls that return well within
// loopSilenceLimit. A loop silent past its limit is hung, and the service
// manager restarts the process (the keepalive stops); a false alarm costs a
// restart, never data.
type loopHeartbeat struct {
	now   func() time.Time
	state atomic.Pointer[heartbeatState]
}

// heartbeatState is one beat, stored as a whole so a reader never pairs one
// beat's deadline with another beat's time. A state is never changed once
// stored; a step's end stores a new one.
type heartbeatState struct {
	last     time.Time // when the loop beat
	deadline time.Time // when its silence stops counting as progress
	step     string    // the innermost long step it is in, empty when none
	// outer is the step this one was declared inside, restored when it
	// ends; nil at the loop's top level.
	outer *heartbeatState
	// finished is set once RunWatch has returned: there is no loop left to
	// judge, and what the process does on its way out is bounded by the
	// service manager's stop timeout, not by the watchdog.
	finished bool
}

func newLoopHeartbeat(now func() time.Time) *loopHeartbeat {
	h := &loopHeartbeat{now: now}
	h.beat()

	return h
}

// beat records progress outside any long step: the loop is at the top of an
// iteration, so every step it declared before has ended.
func (h *loopHeartbeat) beat() {
	if h == nil {
		return
	}
	now := h.now()
	h.state.Store(&heartbeatState{last: now, deadline: now.Add(loopSilenceLimit)})
}

// beatFor records progress on entering a step that may hold the loop for up
// to limit, which is the bound that holds the step, and returns the function
// that ends it. A step the loop leaves only at the top of its next iteration
// need not be ended: the next beat ends it. Ending a step declared inside
// another restores the outer step, whose remaining work then has at least
// the ordinary limit, so a long inner step never makes the outer one read as
// overdue the moment it returns.
func (h *loopHeartbeat) beatFor(step string, limit time.Duration) (end func()) {
	if h == nil {
		return func() {}
	}
	if limit < loopSilenceLimit {
		limit = loopSilenceLimit
	}
	now := h.now()
	var outer *heartbeatState
	if current := h.state.Load(); current != nil && current.step != "" {
		outer = current
	}
	entered := &heartbeatState{last: now, deadline: now.Add(limit), step: step, outer: outer}
	h.state.Store(entered)

	return func() { h.endStep(entered) }
}

// endStep leaves the step entered stored, unless a later beat has already
// replaced it.
func (h *loopHeartbeat) endStep(entered *heartbeatState) {
	now := h.now()
	restored := &heartbeatState{last: now, deadline: now.Add(loopSilenceLimit)}
	if outer := entered.outer; outer != nil {
		restored.step = outer.step
		restored.outer = outer.outer
		if outer.deadline.After(restored.deadline) {
			restored.deadline = outer.deadline
		}
	}
	h.state.CompareAndSwap(entered, restored)
}

// finish records that the loop has returned. RunWatch calls it last, after
// its drain, whose runner stops declare their own steps.
func (h *loopHeartbeat) finish() {
	if h == nil {
		return
	}
	h.state.Store(&heartbeatState{last: h.now(), finished: true})
}

// alive reports whether the loop beat within its current deadline, the step
// it is in, and how long it has been silent.
func (h *loopHeartbeat) alive() (ok bool, step string, silent time.Duration) {
	if h == nil {
		return true, "", 0
	}
	state := h.state.Load()
	if state == nil || state.finished {
		return true, "", 0
	}
	now := h.now()
	silent = now.Sub(state.last)

	return !now.After(state.deadline), state.step, silent
}

// LoopAlive reports whether the watch loop made progress recently, the long
// step it is in (empty when none), and how long it has been silent.
func (o *Orchestrator) LoopAlive() (ok bool, step string, silent time.Duration) {
	return o.heartbeat.alive()
}
