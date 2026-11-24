package sync

import (
	"context"
	"fmt"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

// admitReady applies exact retry/block-scope admission to a ready action set.
// Dependency readiness is already satisfied; admission decides whether the
// action dispatches now or is held as exact work for later release. A
// stopping run admits nothing (isStoppingRun): the ready set comes back as it
// is, for the caller to complete as shutdown, with no freshness read and no
// held row written for work that will not run.
func (flow *engineFlow) admitReady(
	ctx context.Context,
	ready []*syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	if isStoppingRun(ctx) {
		return ready, nil
	}

	decisions := flow.decideAdmission(flow.deps.now(), ready)
	filtered, ownedOnError, err := flow.filterFreshAdmissionDecisions(ctx, decisions)
	if err != nil {
		return ownedOnError, err
	}

	return flow.applyAdmissionDecisions(ctx, filtered)
}

func (flow *engineFlow) filterFreshAdmissionDecisions(
	ctx context.Context,
	decisions []syncsched.Admission,
) ([]syncsched.Admission, []*syncdomain.TrackedAction, error) {
	filtered := make([]syncsched.Admission, 0, len(decisions))
	for i := range decisions {
		decision := decisions[i]
		ta := decision.Action
		if ta == nil {
			continue
		}
		if decision.Kind != syncsched.AdmitNow {
			filtered = append(filtered, decision)
			continue
		}

		freshness, err := syncexec.EvaluateActionFreshnessFromStore(ctx, executorStoreOf(flow.deps.store), &ta.Action)
		if err != nil {
			return filtered, append(admissionDispatchActions(filtered), ta), err
		}
		if freshness.Fresh {
			filtered = append(filtered, decisions[i])
			continue
		}

		completionErr := fmt.Errorf("%w: %s", syncdomain.ErrActionPreconditionChanged, freshness.Reason)
		completion := syncexec.CompletionFromTrackedAction(ta, nil, completionErr)
		flow.deps.perf.RecordSuperseded(perf.SupersededSourceEngineAdmission, 1)
		if err := flow.applySupersededCompletion(ctx, ta, &completion, "admission stale action"); err != nil {
			return filtered, admissionDispatchActions(filtered), err
		}
	}

	return filtered, nil, nil
}

func admissionDispatchActions(decisions []syncsched.Admission) []*syncdomain.TrackedAction {
	actions := make([]*syncdomain.TrackedAction, 0, len(decisions))
	for i := range decisions {
		if decisions[i].Kind == syncsched.AdmitNow && decisions[i].Action != nil {
			actions = append(actions, decisions[i].Action)
		}
	}

	return actions
}

func (flow *engineFlow) decideAdmission(
	now time.Time,
	ready []*syncdomain.TrackedAction,
) []syncsched.Admission {
	decisions := make([]syncsched.Admission, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}

		decisions = append(decisions, flow.sched.Admit(now, ta, flow.retryWork.row))
	}

	return decisions
}

func (flow *engineFlow) applyAdmissionDecisions(
	ctx context.Context,
	decisions []syncsched.Admission,
) ([]*syncdomain.TrackedAction, error) {
	var dispatch []*syncdomain.TrackedAction

	for i := range decisions {
		decision := &decisions[i]
		ta := decision.Action
		if ta == nil {
			continue
		}

		if err := flow.applyAdmissionScopeClear(ctx, decision); err != nil {
			return dispatch, err
		}

		switch decision.Kind {
		case syncsched.AdmitNow:
			flow.sched.Queued(ta)
			dispatch = append(dispatch, ta)
		case syncsched.HoldForRetry:
			flow.sched.Hold(ta, syncsched.HeldRetry, syncdomain.ScopeKey{}, decision.NextRetryAt)
		case syncsched.HoldForScope:
			if err := flow.persistHeldScopeDecision(ctx, ta, decision); err != nil {
				return dispatch, err
			}
			flow.sched.Hold(ta, syncsched.HeldScope, decision.ScopeKey, time.Time{})
		}
	}

	flow.signals.armHeldTimers()

	return dispatch, nil
}

func (flow *engineFlow) applyAdmissionScopeClear(
	ctx context.Context,
	decision *syncsched.Admission,
) error {
	if decision == nil || decision.ClearScopeKey.IsZero() {
		return nil
	}

	return flow.clearBlockedRetryWorkForScope(ctx, decision.RetryWorkKey, decision.ClearScopeKey)
}

func (flow *engineFlow) persistHeldScopeDecision(
	ctx context.Context,
	ta *syncdomain.TrackedAction,
	decision *syncsched.Admission,
) error {
	if ta == nil || decision == nil || decision.ScopeKey.IsZero() {
		return nil
	}

	row, ok := flow.retryWork.row(decision.RetryWorkKey)
	if ok && row.Blocked && row.ScopeKey == decision.ScopeKey {
		return nil
	}

	work := decision.RetryWorkKey
	if work == (syncdomain.RetryWorkKey{}) {
		work = syncdomain.RetryWorkKeyForAction(&ta.Action)
	}
	if err := flow.persistBlockedRetryWork(ctx, work, decision.ScopeKey); err != nil {
		return err
	}
	if !ok || !row.Blocked || row.ScopeKey.IsZero() || row.ScopeKey == decision.ScopeKey {
		return nil
	}

	// The work left the scope it was blocked under: a trial of that scope
	// that a higher-priority scope also blocks. Left with no blocked work, the
	// old scope is dead state with nothing to trial; it is discarded, and the
	// failure that made it is detected again if it persists.
	return flow.discardScopeWithoutBlockedWork(ctx, row.ScopeKey)
}

// discardScopeWithoutBlockedWork discards an active scope that no blocked
// retry work names any more.
func (flow *engineFlow) discardScopeWithoutBlockedWork(ctx context.Context, scopeKey syncdomain.ScopeKey) error {
	if !flow.sched.HasScope(scopeKey) {
		return nil
	}
	hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil || hasBlockedWork {
		return err
	}

	return flow.discardScope(ctx, scopeKey)
}

// executorStoreOf is the executor's view of store. A missing store stays
// missing: a nil *SyncStore would otherwise become a non-nil ExecutorStore,
// and the freshness check skips only when it has no store at all.
func executorStoreOf(store *syncstore.SyncStore) syncexec.ExecutorStore {
	if store == nil {
		return nil
	}

	return store
}
