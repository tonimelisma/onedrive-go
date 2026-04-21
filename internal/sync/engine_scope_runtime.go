package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (controller *scopeController) activateScope(ctx context.Context, watch *watchRuntime, block *ActiveScope) error {
	flow := controller.flow

	if block == nil {
		return fmt.Errorf("sync: activating scope: missing block")
	}

	persisted, err := blockScopeRowFromActiveScope(*block)
	if err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if err := flow.engine.baseline.UpsertBlockScope(ctx, persisted); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	if watch != nil {
		watch.upsertActiveScope(block)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, watch, "activate scope")

	return nil
}

func (controller *scopeController) extendScopeTrial(
	ctx context.Context,
	watch *watchRuntime,
	scopeKey ScopeKey,
	retryAfter time.Duration,
) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := watch.lookupActiveScope(scopeKey)
	if !ok {
		return
	}

	newInterval := computeTrialInterval(retryAfter)
	nextAt := flow.engine.nowFunc().Add(newInterval)

	flow.engine.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	block.NextTrialAt = nextAt
	block.TrialInterval = newInterval
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("extendScopeTrial: failed to persist interval extension",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	watch.armTrialTimer()
}

func (controller *scopeController) rearmScopeTrial(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) {
	flow := controller.flow

	if watch == nil {
		return
	}

	block, ok := watch.lookupActiveScope(scopeKey)
	if !ok {
		return
	}
	if block.TrialInterval <= 0 {
		return
	}

	block.NextTrialAt = flow.engine.nowFunc().Add(block.TrialInterval)
	if err := controller.activateScope(ctx, watch, &block); err != nil {
		flow.engine.logger.Warn("rearmScopeTrial: failed to persist rearmed interval",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Debug("rearming trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("interval", block.TrialInterval),
	)

	watch.armTrialTimer()
}

func (controller *scopeController) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	_, found, err := controller.flow.engine.baseline.PickRetryTrialCandidate(ctx, scopeKey)
	if err != nil {
		return false, fmt.Errorf("sync: checking blocked retry work for scope %s: %w", scopeKey.String(), err)
	}

	return found, nil
}

func (controller *scopeController) rearmOrDiscardScope(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) {
	if scopeKey.IsZero() {
		return
	}

	flow := controller.flow
	hasBlockedWork, err := controller.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		flow.engine.logger.Warn("rearmOrDiscardScope: failed to check blocked retry work",
			slog.String("scope_key", scopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	switch decideTimedBlockScopeAction(hasBlockedWork) {
	case timedBlockScopeKeep:
		controller.rearmScopeTrial(ctx, watch, scopeKey)
	case timedBlockScopeDiscard:
		if err := controller.discardScope(ctx, watch, scopeKey); err != nil {
			flow.engine.logger.Warn("rearmOrDiscardScope: failed to discard empty scope",
				slog.String("scope_key", scopeKey.String()),
				slog.String("error", err.Error()),
			)
		}
	}
}

// feedScopeDetection feeds an action completion into scope detection sliding
// windows. If a threshold is crossed, creates a block scope. Called directly
// from the normal processActionCompletion switch — never called for trial
// results because a live scope already owns the blocker lifecycle.
func (controller *scopeController) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *ActionCompletion) {
	if watch == nil {
		return
	}

	if r.HTTPStatus == 0 {
		return
	}

	sr := watch.scopeState.UpdateScope(r)
	if sr.Block {
		controller.applyBlockScope(ctx, watch, sr)
	}
}

// applyBlockScope persists and activates a new block scope using the same
// timing policy as trial extension and rearm.
func (controller *scopeController) applyBlockScope(ctx context.Context, watch *watchRuntime, sr ScopeUpdateResult) {
	flow := controller.flow

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(sr.RetryAfter)

	block := &ActiveScope{
		Key:           sr.ScopeKey,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}
	if err := controller.activateScope(ctx, watch, block); err != nil {
		flow.engine.logger.Warn("applyBlockScope: failed to persist block scope",
			slog.String("scope_key", sr.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
		return
	}

	flow.engine.logger.Warn("block scope active — actions blocked",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("condition_type", sr.ConditionType),
		slog.Duration("trial_interval", interval),
	)

	if watch != nil {
		watch.armTrialTimer()
	}
}

// releaseScope atomically removes the block scope and makes blocked retry work
// under that scope eligible to run again.
func (controller *scopeController) releaseScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
		watch.kickRetrySweepNow()
		watch.armTrialTimer()
	} else {
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
	}

	flow.engine.logger.Info("block scope cleared — blocked work released",
		slog.String("scope_key", key.String()),
	)

	flow.mustAssertReleasedScope(ctx, watch, key, "release scope")
	flow.mustAssertInvariants(ctx, watch, "release scope")

	return nil
}

// discardScope atomically removes the block scope and deletes blocked retry
// work tied to it. Used when the blocked subtree itself disappears.
func (controller *scopeController) discardScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	flow := controller.flow

	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	if watch != nil {
		watch.removeActiveScope(key)
		watch.armTrialTimer()
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, watch, key, "discard scope")
	flow.mustAssertInvariants(ctx, watch, "discard scope")

	return nil
}

func (controller *scopeController) applyTrialReclassification(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	bl *Baseline,
) {
	if decision.PermissionFlow != permissionFlowNone {
		if permOutcome, handled := controller.decidePermissionOutcome(ctx, decision, r, bl); handled {
			controller.clearBlockedRetryWorkForScope(ctx, retryWorkKeyForCompletion(r), r.TrialScopeKey)
			controller.applyPermissionOutcome(ctx, watch, decision.PermissionFlow, &permOutcome)
		}
		return
	}

	if decision.Class == errclass.ClassBlockScopeingTransient && decision.ScopeKey == SKDiskLocal() {
		if controller.rehomeBlockedRetryWork(ctx, r, decision.ScopeKey) {
			controller.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      decision.ScopeKey,
				ConditionType: decision.ScopeKey.ConditionType(),
			})
		}
	}
}

func (controller *scopeController) clearBlockedRetryWork(
	ctx context.Context,
	row *RetryWorkRow,
	caller string,
) {
	if row == nil {
		return
	}

	work := retryWorkKeyForRetryWork(row)

	if err := controller.flow.engine.baseline.ClearBlockedRetryWork(ctx, work, row.ScopeKey); err != nil {
		controller.flow.engine.logger.Debug(caller+": failed to clear blocked retry work",
			slog.String("path", row.Path),
			slog.String("scope_key", row.ScopeKey.String()),
			slog.String("error", err.Error()),
		)
	}
}
