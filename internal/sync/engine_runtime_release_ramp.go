package sync

import (
	"context"
	"fmt"
	"log/slog"
)

// Re-admission after a timed block scope ramps instead of flooding
// (R-6.8.24). Measured on a live account on 2026-11-01: a successful trial
// released a 10,000-upload backlog at once, the workers drained it through
// 429 after 429, and the next trial repeated the flood -- 22-28 requests a
// second, thousands of 429s per five minutes, about ten files uploaded. The
// ramp bounds what one trial lets through, and a 429 inside it re-blocks.

const (
	// releaseRampMinimumBatch is the first batch after a successful trial
	// when fewer transfer workers are configured: enough to keep a small
	// pool busy, few enough that a 429 costs little.
	releaseRampMinimumBatch = 4

	// releaseRampMaxBatch caps the doubling. In practice a batch this size
	// is the whole backlog; the cap only keeps the arithmetic bounded.
	releaseRampMaxBatch = 1 << 16
)

// nextReleaseAllowance is the ramp policy: the size of the next batch a scope
// on probation releases, given the transfer worker count, the batch it last
// released (zero when the scope is not on probation yet) and the slow-start
// threshold its last 429 left (zero when none has). The first batch is the
// worker count, at least releaseRampMinimumBatch; each batch that completed
// without a further 429 doubles the next, up to the threshold; from there
// the batch grows by one. Pure doubling overshoots the limit by up to 2x on
// every cycle, and each overshoot costs a full Retry-After plus Microsoft's
// penalty for requests made while throttled (RFC 5681 §3.1 eq. 4 is the
// same remedy).
func nextReleaseAllowance(transferWorkers, lastAllowance, slowStartThreshold int) int {
	if lastAllowance <= 0 {
		return firstReleaseAllowance(transferWorkers)
	}
	if lastAllowance >= releaseRampMaxBatch {
		return releaseRampMaxBatch
	}
	doubled := min(lastAllowance*2, releaseRampMaxBatch)
	if slowStartThreshold <= 0 || doubled <= slowStartThreshold {
		return doubled
	}
	if lastAllowance < slowStartThreshold {
		return slowStartThreshold
	}

	return lastAllowance + 1
}

func firstReleaseAllowance(transferWorkers int) int {
	return max(transferWorkers, releaseRampMinimumBatch)
}

// slowStartThresholdAfterThrottle is the ramp's memory after a 429: half the
// batch that drew it, at least the first batch. A 429 that interrupted no
// ramp (the probe itself, or an episode's first failures) says nothing new
// and keeps the previous threshold.
func slowStartThresholdAfterThrottle(transferWorkers, allowanceAtThrottle, previous int) int {
	if allowanceAtThrottle <= 0 {
		return previous
	}

	return max(allowanceAtThrottle/2, firstReleaseAllowance(transferWorkers))
}

// admittedWorkOwner is the runtime that holds admitted, not yet dispatched
// work: the watch loop's outbox or the one-shot results loop's. A block scope
// that activates takes back the admitted work it blocks through this seam;
// the owner hands those actions over and forgets them, and the flow holds
// them under the scope. Only the loop goroutine that owns the outbox calls
// it, from inside a completion it is applying.
type admittedWorkOwner interface {
	takeAdmittedWorkBlockedBy(key ScopeKey) []*trackedAction
}

// noAdmittedWork is the owner of a flow that has no outbox of its own.
type noAdmittedWork struct{}

func (noAdmittedWork) takeAdmittedWorkBlockedBy(ScopeKey) []*trackedAction { return nil }

// takeOutboxBlockedBy splits an outbox into the actions key blocks, which are
// returned, and the rest, which is the new outbox. Order is kept on both
// sides.
func takeOutboxBlockedBy(outbox []*trackedAction, key ScopeKey) (taken, kept []*trackedAction) {
	for _, ta := range outbox {
		if ta != nil && scopeBlocksTrackedAction(key, ta, ta.Action.ThrottleTargetKey()) {
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
func (flow *engineFlow) retireAdmittedWorkBlockedBy(ctx context.Context, key ScopeKey) error {
	taken := flow.admitted.takeAdmittedWorkBlockedBy(key)
	if len(taken) == 0 {
		return nil
	}

	decisions := make([]admissionDecision, 0, len(taken))
	for _, ta := range taken {
		decisions = append(decisions, admissionDecision{
			Action:       ta,
			Kind:         admissionHoldScope,
			ScopeKey:     key,
			RetryWorkKey: retryWorkKeyForAction(&ta.Action),
		})
	}
	if _, err := flow.applyAdmissionDecisions(ctx, decisions); err != nil {
		return fmt.Errorf("retire %d admitted actions under %s: %w", len(taken), key.String(), err)
	}

	flow.deps.emit(engineDebugEvent{
		Type:     engineDebugEventAdmittedWorkRetired,
		ScopeKey: key,
		Count:    len(taken),
	})
	flow.deps.logger.Debug("holding admitted work the activated scope blocks",
		slog.String("scope_key", key.String()),
		slog.Int("retired", len(taken)),
	)

	return nil
}

// heldScopeBatch is the first `limit` held entries of a scope in held order:
// the next ramp batch.
func (flow *engineFlow) heldScopeBatch(key ScopeKey, limit int) []RetryWorkKey {
	batch := make([]RetryWorkKey, 0, limit)
	for _, work := range flow.retries.heldByScope[key] {
		if len(batch) == limit {
			break
		}
		if held := flow.retries.heldByKey[work]; held != nil && held.Reason == heldReasonScope {
			batch = append(batch, work)
		}
	}

	return batch
}

// noteProbationMemberFinished records that a member of the scope's current
// batch finished other than with a success (held for an ordinary retry,
// superseded). Nothing is released for it: a batch whose last member failed
// waits for the next due trial, one interval out, which resets the count and
// continues the ramp from its success.
func (flow *engineFlow) noteProbationMemberFinished(key ScopeKey, generation uint64) {
	scope, active := flow.scopes.lookupActiveScope(key)
	if !active || scope.Generation != generation || scope.ReleaseAllowance == 0 {
		return
	}
	scope.BatchOutstanding = max(scope.BatchOutstanding-1, 0)
	flow.scopes.upsertActiveScope(&scope)
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
	key ScopeKey,
	generation uint64,
) ([]*trackedAction, error) {
	scope, active := flow.scopes.lookupActiveScope(key)
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
			flow.scopes.upsertActiveScope(&scope)
			return nil, nil
		}
	}

	now := flow.deps.now()
	allowance := nextReleaseAllowance(flow.engine.transferWorkers, scope.ReleaseAllowance, scope.SlowStartThreshold)
	batchKeys := flow.heldScopeBatch(key, allowance)

	removed, err := flow.deps.store.ReleaseScopeWork(ctx, key, batchKeys, now)
	if err != nil {
		return nil, fmt.Errorf("sync: releasing %d blocked actions under %s: %w", len(batchKeys), key.String(), err)
	}

	batch := make([]*trackedAction, 0, len(batchKeys))
	for _, work := range batchKeys {
		ta := flow.retries.releaseHeldAction(work)
		if ta == nil {
			continue
		}
		if row, ok := flow.retries.rowsByKey[work]; ok {
			row.Blocked = false
			row.NextRetryAt = now.UnixNano()
			flow.retries.rowsByKey[work] = row
		}
		ta.IsTrial = true
		ta.TrialScopeKey = key
		ta.TrialScopeGeneration = scope.Generation
		batch = append(batch, ta)
	}

	if removed {
		flow.scopes.removeActiveScope(key)
		flow.deps.emit(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
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
		flow.deps.emit(engineDebugEvent{
			Type:     engineDebugEventScopeBatchReleased,
			ScopeKey: key,
			Count:    len(batch),
		})
		flow.deps.logger.Debug("trial succeeded; resuming a batch of the blocked work",
			slog.String("scope_key", key.String()),
			slog.Int("released", len(batch)),
			slog.Int("allowance", allowance),
			slog.Int("slow_start_threshold", scope.SlowStartThreshold),
			slog.Int("still_held", len(flow.retries.heldByScope[key])),
		)
	}
	flow.signals.armTrialTimer()
	flow.mustAssertInvariants(ctx, "release scope batch")

	if len(batch) == 0 {
		return nil, nil
	}

	return flow.admitReady(ctx, batch)
}
