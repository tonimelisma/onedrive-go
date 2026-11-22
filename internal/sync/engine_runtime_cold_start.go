package sync

import (
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// A fresh plan opens into its backlog slowly (R-6.8.25); the slow start's
// state and arithmetic are the scheduler's (syncsched/cold_start.go). The
// flow's side below reports what changes and picks what to dispatch.

// handOverColdStart is the flow's side of a block activation: the scope
// owns admission to what it blocks from now on (R-6.8.24), so the cold
// start stops gating it. Said once per target at Debug.
func (flow *engineFlow) handOverColdStart(key syncdomain.ScopeKey) {
	for _, target := range flow.sched.HandOverColdStart(key) {
		flow.deps.emit(syncdomain.EngineDebugEvent{
			Type:     syncdomain.EngineDebugEventColdStartHandedOver,
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
func (flow *engineFlow) noteColdStartSuccess(ta *syncdomain.TrackedAction) {
	allowance := flow.sched.NoteSuccess(ta)
	if allowance == 0 {
		return
	}
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:  syncdomain.EngineDebugEventColdStartAllowanceRaised,
		Note:  syncsched.ColdStartTarget(ta),
		Count: allowance,
	})
	flow.deps.logger.Debug("slow start: raising the in-flight allowance",
		slog.String("target", syncsched.ColdStartTarget(ta)),
		slog.Int("allowance", allowance),
		slog.Int("ceiling", flow.sched.ColdStartCeiling()),
	)
}

// nextDispatchable picks the outbox entry to hand to a worker next: the
// first, in outbox order, that is due (its admission instant has passed,
// see TrackedAction.AdmitAt) and whose target has room under the slow-start
// allowance. It returns -1 when none is, together with the earliest
// admission instant still ahead, or the zero time when every held-back
// entry is waiting on in-flight work rather than on the clock.
//
// Dependency order is not starved: an action whose dependents are waiting
// counts as one in flight, nothing more, and an entry behind the head is
// taken when the head's target is full and its own is not. The scan is
// linear in the outbox; the outbox is a plan's ready frontier, and the
// common case stops at its head.
func (flow *engineFlow) nextDispatchable(outbox []*syncdomain.TrackedAction, now time.Time) (int, time.Time) {
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
		if flow.sched.ColdStartAdmits(ta) {
			return i, time.Time{}
		}
	}

	return -1, nextAdmitAt
}

// markDispatched records that outbox entry i is going to a worker and
// removes it from the outbox, which is returned, order kept.
func (flow *engineFlow) markDispatched(outbox []*syncdomain.TrackedAction, i int) []*syncdomain.TrackedAction {
	ta := outbox[i]
	flow.sched.Running(ta)
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type: syncdomain.EngineDebugEventActionDispatched,
		Path: ta.Action.Path,
		Note: syncsched.ColdStartTarget(ta),
	})

	return append(outbox[:i], outbox[i+1:]...)
}
