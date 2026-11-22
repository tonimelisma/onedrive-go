package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Re-admission after a timed block scope ramps instead of flooding
// (R-6.8.24); the ramp's arithmetic is the scheduler's
// (syncsched/release_ramp.go).

// admittedWorkOwner is the runtime that holds admitted, not yet dispatched
// work: the watch loop's outbox or the one-shot results loop's. A block scope
// that activates takes back the admitted work it blocks through this seam;
// the owner hands those actions over and forgets them, and the flow holds
// them under the scope. Only the loop goroutine that owns the outbox calls
// it, from inside a completion it is applying.
type admittedWorkOwner interface {
	takeAdmittedWorkBlockedBy(key syncdomain.ScopeKey) []*syncdomain.TrackedAction
}

// noAdmittedWork is the owner of a flow that has no outbox of its own.
type noAdmittedWork struct{}

func (noAdmittedWork) takeAdmittedWorkBlockedBy(syncdomain.ScopeKey) []*syncdomain.TrackedAction {
	return nil
}

// takeOutboxBlockedBy splits an outbox into the actions key blocks, which are
// returned, and the rest, which is the new outbox. Order is kept on both
// sides.
func takeOutboxBlockedBy(outbox []*syncdomain.TrackedAction, key syncdomain.ScopeKey) (taken, kept []*syncdomain.TrackedAction) {
	for _, ta := range outbox {
		if ta != nil && syncsched.ScopeBlocksTrackedAction(key, ta, ta.Action.ThrottleTargetKey()) {
			taken = append(taken, ta)
			continue
		}
		kept = append(kept, ta)
	}

	return taken, kept
}

// retireAdmittedWorkBlockedBy takes back the admitted work key blocks and
// holds it under key, persisted blocked, through the ordinary scope-hold
// admission path. Work a worker already took is not reachable here; it
// finishes and its outcome is classified normally.
func (flow *engineFlow) retireAdmittedWorkBlockedBy(ctx context.Context, key syncdomain.ScopeKey) error {
	taken := flow.admitted.takeAdmittedWorkBlockedBy(key)
	if len(taken) == 0 {
		return nil
	}

	decisions := make([]syncsched.Admission, 0, len(taken))
	for _, ta := range taken {
		decisions = append(decisions, syncsched.Admission{
			Action:       ta,
			Kind:         syncsched.HoldForScope,
			ScopeKey:     key,
			RetryWorkKey: syncdomain.RetryWorkKeyForAction(&ta.Action),
		})
	}
	if _, err := flow.applyAdmissionDecisions(ctx, decisions); err != nil {
		return fmt.Errorf("retire %d admitted actions under %s: %w", len(taken), key.String(), err)
	}

	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventAdmittedWorkRetired,
		ScopeKey: key,
		Count:    len(taken),
	})
	flow.deps.logger.Debug("holding admitted work the activated scope blocks",
		slog.String("scope_key", key.String()),
		slog.Int("retired", len(taken)),
	)

	return nil
}

// noteProbationMemberFinished records that a member of the scope's current
// batch finished other than with a success (held for an ordinary retry,
// superseded). Nothing is released for it: a batch whose last member failed
// waits for the next due trial, one interval out, which resets the count and
// continues the ramp from its success.
func (flow *engineFlow) noteProbationMemberFinished(key syncdomain.ScopeKey, generation uint64) {
	scope, active := flow.sched.Scope(key)
	if !active || scope.Generation != generation || scope.ReleaseAllowance == 0 {
		return
	}
	scope.BatchOutstanding = max(scope.BatchOutstanding-1, 0)
	flow.sched.PutScope(&scope)
}

// releaseScopeBatch is what a successful trial of key releases: the next ramp
// batch of the scope's held work, dispatched as trials of the scope so that
// admission passes them while the scope still blocks everything else and
// their outcomes drive the ramp. A member's success only counts its batch
// down; the batch's last member releases the next, doubled one (releasing on
// every success cascaded 4+8+16+... admissions off five successes). The
// store marks exactly those rows ready and deletes the scope row with the
// last blocked ones; the ledger keeps the scope on probation with the batch
// size as its allowance until then. A trial from before a re-block (its
// generation is stale) or of a scope that is already gone releases nothing:
// the drive just said it is still limited, or the backlog is already free.
func (flow *engineFlow) releaseScopeBatch(
	ctx context.Context,
	key syncdomain.ScopeKey,
	generation uint64,
) ([]*syncdomain.TrackedAction, error) {
	scope, active := flow.sched.Scope(key)
	if !active || scope.Generation != generation {
		flow.deps.logger.Debug("trial succeeded for a scope that was re-blocked or released meanwhile; releasing nothing",
			slog.String("scope_key", key.String()),
			slog.Bool("active", active),
		)
		return nil, nil
	}
	if scope.ReleaseAllowance > 0 {
		scope.BatchOutstanding = max(scope.BatchOutstanding-1, 0)
		if scope.BatchOutstanding > 0 {
			flow.sched.PutScope(&scope)
			return nil, nil
		}
	}

	now := flow.deps.now()
	allowance := syncsched.NextReleaseAllowance(flow.cfg.transferWorkers, scope.ReleaseAllowance, scope.SlowStartThreshold)
	batchKeys := flow.sched.HeldBatch(key, allowance)

	_, removed, err := flow.retryWork.releaseScope(ctx, flow.deps.store, key, batchKeys, now)
	if err != nil {
		return nil, fmt.Errorf("sync: releasing %d blocked actions under %s: %w", len(batchKeys), key.String(), err)
	}

	batch := make([]*syncdomain.TrackedAction, 0, len(batchKeys))
	for _, work := range batchKeys {
		ta := flow.sched.Release(work)
		if ta == nil {
			continue
		}
		ta.IsTrial = true
		ta.TrialScopeKey = key
		ta.TrialScopeGeneration = scope.Generation
		ta.AdmitAt = now.Add(flow.deps.clock.Jitter(syncsched.ReleaseBatchAdmissionWindow))
		batch = append(batch, ta)
	}

	if removed {
		flow.sched.DropScope(key)
		flow.deps.emit(syncdomain.EngineDebugEvent{
			Type:     syncdomain.EngineDebugEventScopeReleased,
			ScopeKey: key,
			Count:    len(batch),
		})
		flow.deps.logger.Info("Resumed syncing: "+key.Humanize(),
			slog.String("scope_key", key.String()),
			slog.Int("released", len(batch)),
		)
		flow.mustAssertReleasedScope(ctx, key, "release scope")
	} else {
		if len(batch) > 0 {
			scope.ReleaseAllowance = allowance
			scope.BatchOutstanding = len(batch)
		}
		// A probation that stalls (every member superseded, a replan rebuilt
		// the runtime) recovers by an ordinary trial one interval out.
		scope.NextTrialAt = now.Add(scope.TrialInterval)
		if err := flow.persistActiveScope(ctx, &scope); err != nil {
			return batch, fmt.Errorf("keep scope %s on probation: %w", key.String(), err)
		}
		flow.deps.emit(syncdomain.EngineDebugEvent{
			Type:     syncdomain.EngineDebugEventScopeBatchReleased,
			ScopeKey: key,
			Count:    len(batch),
		})
		flow.deps.logger.Debug("trial succeeded; resuming a batch of the blocked work",
			slog.String("scope_key", key.String()),
			slog.Int("released", len(batch)),
			slog.Int("allowance", allowance),
			slog.Int("slow_start_threshold", scope.SlowStartThreshold),
			slog.Int("still_held", flow.sched.HeldUnderScope(key)),
		)
	}
	flow.signals.armTrialTimer()
	flow.mustAssertInvariants(ctx, "release scope batch")

	if len(batch) == 0 {
		return nil, nil
	}

	return flow.admitReady(ctx, batch)
}
