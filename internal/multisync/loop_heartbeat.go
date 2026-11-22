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
)

// loopHeartbeat records the RunWatch loop's progress for the watchdog
// goroutine. The loop writes it; any goroutine reads it.
type loopHeartbeat struct {
	now   func() time.Time
	state atomic.Pointer[heartbeatState]
}

// heartbeatState is one beat, stored as a whole so a reader never pairs one
// beat's limit with another beat's time.
type heartbeatState struct {
	last  time.Time     // when the loop beat
	limit time.Duration // how long it may stay silent after that beat
	step  string        // the long step it declared, empty when none
}

func newLoopHeartbeat(now func() time.Time) *loopHeartbeat {
	h := &loopHeartbeat{now: now}
	h.beat()

	return h
}

// beat records progress outside any long step.
func (h *loopHeartbeat) beat() {
	h.beatFor("", loopSilenceLimit)
}

// beatFor records progress on entering a step that may hold the loop for up
// to limit, which is the bound that holds the step.
func (h *loopHeartbeat) beatFor(step string, limit time.Duration) {
	if h == nil {
		return
	}
	if limit < loopSilenceLimit {
		limit = loopSilenceLimit
	}
	h.state.Store(&heartbeatState{last: h.now(), limit: limit, step: step})
}

// alive reports whether the loop beat within its current limit, the step it
// last declared, and how long it has been silent.
func (h *loopHeartbeat) alive() (ok bool, step string, silent time.Duration) {
	if h == nil {
		return true, "", 0
	}
	state := h.state.Load()
	if state == nil {
		return true, "", 0
	}
	silent = h.now().Sub(state.last)

	return silent <= state.limit, state.step, silent
}

// LoopAlive reports whether the watch loop made progress recently, the long
// step it is in (empty when none), and how long it has been silent.
func (o *Orchestrator) LoopAlive() (ok bool, step string, silent time.Duration) {
	return o.heartbeat.alive()
}
