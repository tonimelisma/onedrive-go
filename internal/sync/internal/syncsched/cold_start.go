package syncsched

import "github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"

// A fresh plan opens into its backlog slowly (R-6.8.25). Measured on a
// personal OneDrive on 2026-11-01: a cold start with eight transfer workers
// and a backlog of small uploads made 19 requests a second and drew its first
// 429 within a minute, before any block scope existed whose ramp could have
// paced it (Microsoft's documented business budget is about 10 requests a
// second per user; personal limits are unpublished). TCP's answer to
// "unknown conditions" is slow start (RFC 5681 §3.1), and so is this one: a
// per-target in-flight allowance that starts at ColdStartInitialAllowance
// and doubles per round of successful completions up to the worker pool
// size. The first 429 hands the target to the scope machinery of R-6.8.24,
// which owns the ramp from there; a target that already has an active
// throttle scope when the plan starts is the scope's from the start. The
// allowance is runtime state of one plan and starts over with the next.
//
// The watch-loop and one-shot dispatch pickers consult it (ColdStartAdmits)
// before handing an action to a worker.

// ColdStartInitialAllowance is how many of one target's actions a fresh plan
// lets fly before any has completed: enough to overlap one request's latency,
// few enough that a limit found at once costs one 429.
const ColdStartInitialAllowance = 2

// coldStartRamp is one target's position in the slow start of the current
// plan. A target is the drive a request goes to (Action.ThrottleTargetKey),
// the unit Microsoft meters and the unit a throttle scope blocks.
type coldStartRamp struct {
	// allowance is how many of the target's actions may be in flight.
	allowance int
	// inFlight counts the target's actions handed to a worker and not yet
	// finished; Running and Finished keep it.
	inFlight int
	// successes counts successful completions in the current round; a round
	// of `allowance` successes doubles the allowance.
	successes int
	// handedOver is set once a block scope owns admission to the target:
	// the allowance no longer gates, the scope's ramp does.
	handedOver bool
}

// ColdStartTarget is the ramp key of an action: its throttle target, or ""
// for an action that sends nothing to a drive, which no ramp gates.
func ColdStartTarget(ta *syncdomain.TrackedAction) string {
	if ta == nil {
		return ""
	}

	return ta.Action.ThrottleTargetKey()
}

// ResetColdStart starts the slow start over for a fresh plan, with the
// worker pool size as the ceiling the allowance doubles toward.
func (s *Scheduler) ResetColdStart(ceiling int) {
	s.coldStartCeiling = ceiling
	s.coldStart = make(map[string]*coldStartRamp)
	s.coldStartServiceHandedOver = false
}

// ColdStartCeiling is the worker pool size the slow start doubles toward;
// zero until a plan is installed.
func (s *Scheduler) ColdStartCeiling() int {
	return s.coldStartCeiling
}

// coldStartRampFor returns the target's ramp, creating it at the initial
// allowance (bounded by the ceiling) on first use. A service-wide scope
// that already took the plan over makes every new ramp handed over too.
func (s *Scheduler) coldStartRampFor(target string) *coldStartRamp {
	if ramp, ok := s.coldStart[target]; ok {
		return ramp
	}
	ramp := &coldStartRamp{allowance: min(ColdStartInitialAllowance, s.coldStartCeiling)}
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

// ColdStartAdmits reports whether the target's allowance has room for one
// more action in flight. A scheduler with no ceiling (no plan installed) and
// an action with no target are never gated.
func (s *Scheduler) ColdStartAdmits(ta *syncdomain.TrackedAction) bool {
	target := ColdStartTarget(ta)
	if target == "" || s.coldStartCeiling <= 0 {
		return true
	}
	ramp := s.coldStartRampFor(target)

	return ramp.handedOver || ramp.inFlight < ramp.allowance
}

// NoteSuccess counts a successful completion of an action a worker ran
// toward the target's round and reports the raised allowance when the round
// completes (zero when it did not, or the ramp is over).
func (s *Scheduler) NoteSuccess(ta *syncdomain.TrackedAction) int {
	target := ColdStartTarget(ta)
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

// HandOverColdStart gives admission to every target key blocks to the scope:
// a throttle scope takes its one drive, a service scope takes every target,
// present and future. It returns the targets whose ramps were gating until
// now, so the caller can say so once; other scope kinds say nothing about
// request rate and change nothing.
func (s *Scheduler) HandOverColdStart(key syncdomain.ScopeKey) []string {
	if s.coldStartCeiling <= 0 {
		return nil
	}
	var handed []string
	switch key.Kind {
	case syncdomain.ScopeThrottleTarget:
		if ramp := s.coldStartRampFor(key.ThrottleTargetKey()); !ramp.handedOver {
			ramp.handedOver = true
			ramp.allowance = s.coldStartCeiling
			handed = append(handed, key.ThrottleTargetKey())
		}
	case syncdomain.ScopeService:
		s.coldStartServiceHandedOver = true
		for target, ramp := range s.coldStart {
			if ramp.handedOver {
				continue
			}
			ramp.handedOver = true
			ramp.allowance = s.coldStartCeiling
			handed = append(handed, target)
		}
	case syncdomain.ScopeQuotaOwn, syncdomain.ScopePermDirRead, syncdomain.ScopePermDirWrite, syncdomain.ScopePermRemoteRead,
		syncdomain.ScopePermRemoteWrite, syncdomain.ScopeDiskLocal, syncdomain.ScopeLocalReadOnly, syncdomain.ScopeDeleteSafety:
	}

	return handed
}
