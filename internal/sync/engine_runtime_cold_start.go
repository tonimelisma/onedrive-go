package sync

import (
	"log/slog"
	"time"
)

// A fresh plan opens into its backlog slowly (R-6.8.25). Measured on a
// personal OneDrive on 2026-11-01: a cold start with eight transfer workers
// and a backlog of small uploads made 19 requests a second and drew its first
// 429 within a minute, before any block scope existed whose ramp could have
// paced it (Microsoft's documented business budget is about 10 requests a
// second per user; personal limits are unpublished). TCP's answer to
// "unknown conditions" is slow start (RFC 5681 §3.1), and so is this one: a
// per-target in-flight allowance that starts at coldStartInitialAllowance
// and doubles per round of successful completions up to the worker pool
// size. The first 429 hands the target to the scope machinery of R-6.8.24,
// which owns the ramp from there; a target that already has an active
// throttle scope when the plan starts is the scope's from the start. The
// allowance is runtime state of one plan and starts over with the next.
//
// The watch-loop and one-shot dispatch pickers (nextDispatchable) consult it
// before handing an action to a worker; the scheduler, the single owner of
// dispatch state, keeps it.

// coldStartInitialAllowance is how many of one target's actions a fresh plan
// lets fly before any has completed: enough to overlap one request's latency,
// few enough that a limit found at once costs one 429.
const coldStartInitialAllowance = 2

// coldStartRamp is one target's position in the slow start of the current
// plan. A target is the drive a request goes to (Action.ThrottleTargetKey),
// the unit Microsoft meters and the unit a throttle scope blocks.
type coldStartRamp struct {
	// allowance is how many of the target's actions may be in flight.
	allowance int
	// inFlight counts the target's actions handed to a worker and not yet
	// finished; markRunning and markFinished keep it.
	inFlight int
	// successes counts successful completions in the current round; a round
	// of `allowance` successes doubles the allowance.
	successes int
	// handedOver is set once a block scope owns admission to the target:
	// the allowance no longer gates, the scope's ramp does.
	handedOver bool
}

// coldStartTarget is the ramp key of an action: its throttle target, or ""
// for an action that sends nothing to a drive, which no ramp gates.
func coldStartTarget(ta *trackedAction) string {
	if ta == nil {
		return ""
	}

	return ta.Action.ThrottleTargetKey()
}

// resetColdStart starts the slow start over for a fresh plan, with the
// worker pool size as the ceiling the allowance doubles toward.
func (s *actionScheduler) resetColdStart(ceiling int) {
	s.coldStartCeiling = ceiling
	s.coldStart = make(map[string]*coldStartRamp)
	s.coldStartServiceHandedOver = false
}

// coldStartRampFor returns the target's ramp, creating it at the initial
// allowance (bounded by the ceiling) on first use. A service-wide scope
// that already took the plan over makes every new ramp handed over too.
func (s *actionScheduler) coldStartRampFor(target string) *coldStartRamp {
	if ramp, ok := s.coldStart[target]; ok {
		return ramp
	}
	ramp := &coldStartRamp{allowance: min(coldStartInitialAllowance, s.coldStartCeiling)}
	if ramp.allowance < 1 {
		ramp.allowance = 1
	}
	if s.coldStartServiceHandedOver {
		ramp.handedOver = true
		ramp.allowance = s.coldStartCeiling
	}
	if s.coldStart == nil {
		s.coldStart = make(map[string]*coldStartRamp)
	}
	s.coldStart[target] = ramp

	return ramp
}

// coldStartAdmits reports whether the target's allowance has room for one
// more action in flight. A scheduler with no ceiling (no plan installed) and
// an action with no target are never gated.
func (s *actionScheduler) coldStartAdmits(ta *trackedAction) bool {
	target := coldStartTarget(ta)
	if target == "" || s.coldStartCeiling <= 0 {
		return true
	}
	ramp := s.coldStartRampFor(target)

	return ramp.handedOver || ramp.inFlight < ramp.allowance
}

// noteColdStartSuccess counts a successful completion of an action a worker
// ran toward the target's round and reports the raised allowance when the
// round completes (zero when it did not, or the ramp is over).
func (s *actionScheduler) noteColdStartSuccess(ta *trackedAction) int {
	target := coldStartTarget(ta)
	if target == "" || s.coldStartCeiling <= 0 {
		return 0
	}
	ramp := s.coldStartRampFor(target)
	if ramp.handedOver || ramp.allowance >= s.coldStartCeiling {
		return 0
	}
	ramp.successes++
	if ramp.successes < ramp.allowance {
		return 0
	}
	ramp.successes = 0
	ramp.allowance = min(ramp.allowance*2, s.coldStartCeiling)

	return ramp.allowance
}

// handOverColdStart gives admission to every target key blocks to the scope:
// a throttle scope takes its one drive, a service scope takes every target,
// present and future. It returns the targets whose ramps were gating until
// now, so the caller can say so once; other scope kinds say nothing about
// request rate and change nothing.
func (s *actionScheduler) handOverColdStart(key ScopeKey) []string {
	if s.coldStartCeiling <= 0 {
		return nil
	}
	var handed []string
	switch key.Kind {
	case ScopeThrottleTarget:
		if ramp := s.coldStartRampFor(key.ThrottleTargetKey()); !ramp.handedOver {
			ramp.handedOver = true
			ramp.allowance = s.coldStartCeiling
			handed = append(handed, key.ThrottleTargetKey())
		}
	case ScopeService:
		s.coldStartServiceHandedOver = true
		for target, ramp := range s.coldStart {
			if ramp.handedOver {
				continue
			}
			ramp.handedOver = true
			ramp.allowance = s.coldStartCeiling
			handed = append(handed, target)
		}
	case ScopeQuotaOwn, ScopePermDirRead, ScopePermDirWrite, ScopePermRemoteRead,
		ScopePermRemoteWrite, ScopeDiskLocal, ScopeLocalReadOnly, ScopeDeleteSafety:
	}

	return handed
}

// handOverColdStart is the flow's side of a block activation: the scope
// owns admission to what it blocks from now on (R-6.8.24), so the cold
// start stops gating it. Said once per target at Debug.
func (flow *engineFlow) handOverColdStart(key ScopeKey) {
	for _, target := range flow.sched.handOverColdStart(key) {
		flow.deps.emit(engineDebugEvent{
			Type:     engineDebugEventColdStartHandedOver,
			ScopeKey: key,
			Note:     target,
		})
		flow.deps.logger.Debug("slow start handed over to the block scope",
			slog.String("scope_key", key.String()),
			slog.String("target", target),
		)
	}
}

// noteColdStartSuccess counts a worker-run action's success toward its
// target's slow-start round and reports a raised allowance at Debug.
func (flow *engineFlow) noteColdStartSuccess(ta *trackedAction) {
	allowance := flow.sched.noteColdStartSuccess(ta)
	if allowance == 0 {
		return
	}
	flow.deps.emit(engineDebugEvent{
		Type:  engineDebugEventColdStartAllowanceRaised,
		Note:  coldStartTarget(ta),
		Count: allowance,
	})
	flow.deps.logger.Debug("slow start: raising the in-flight allowance",
		slog.String("target", coldStartTarget(ta)),
		slog.Int("allowance", allowance),
		slog.Int("ceiling", flow.sched.coldStartCeiling),
	)
}

// nextDispatchable picks the outbox entry to hand to a worker next: the
// first, in outbox order, that is due (its admission instant has passed,
// see trackedAction.AdmitAt) and whose target has room under the slow-start
// allowance. It returns -1 when none is, together with the earliest
// admission instant still ahead, or the zero time when every held-back
// entry is waiting on in-flight work rather than on the clock.
//
// Dependency order is not starved: an action whose dependents are waiting
// counts as one in flight, nothing more, and an entry behind the head is
// taken when the head's target is full and its own is not. The scan is
// linear in the outbox; the outbox is a plan's ready frontier, and the
// common case stops at its head.
func (flow *engineFlow) nextDispatchable(outbox []*trackedAction, now time.Time) (int, time.Time) {
	var nextAdmitAt time.Time
	for i, ta := range outbox {
		if ta == nil {
			continue
		}
		if ta.AdmitAt.After(now) {
			if nextAdmitAt.IsZero() || ta.AdmitAt.Before(nextAdmitAt) {
				nextAdmitAt = ta.AdmitAt
			}
			continue
		}
		if flow.sched.coldStartAdmits(ta) {
			return i, time.Time{}
		}
	}

	return -1, nextAdmitAt
}

// markDispatched records that outbox entry i is going to a worker and
// removes it from the outbox, which is returned, order kept.
func (flow *engineFlow) markDispatched(outbox []*trackedAction, i int) []*trackedAction {
	ta := outbox[i]
	flow.sched.markRunning(ta)
	flow.deps.emit(engineDebugEvent{
		Type: engineDebugEventActionDispatched,
		Path: ta.Action.Path,
		Note: coldStartTarget(ta),
	})

	return append(outbox[:i], outbox[i+1:]...)
}
