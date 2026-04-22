package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

func (flow *engineFlow) activateScope(ctx context.Context, watch *watchRuntime, block *ActiveScope) error {
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

	flow.upsertActiveScope(block)
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, watch, "activate scope")

	return nil
}

func (flow *engineFlow) extendScopeTrial(
	ctx context.Context,
	watch *watchRuntime,
	scopeKey ScopeKey,
	retryAfter time.Duration,
) error {
	block, ok := flow.lookupActiveScope(scopeKey)
	if !ok {
		return nil
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
	if err := flow.activateScope(ctx, watch, &block); err != nil {
		return fmt.Errorf("extend trial interval for %s: %w", scopeKey.String(), err)
	}

	if watch != nil {
		watch.armTrialTimer()
	}

	return nil
}

func (flow *engineFlow) rearmScopeTrial(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) error {
	block, ok := flow.lookupActiveScope(scopeKey)
	if !ok {
		return nil
	}
	if block.TrialInterval <= 0 {
		return nil
	}

	block.NextTrialAt = flow.engine.nowFunc().Add(block.TrialInterval)
	if err := flow.activateScope(ctx, watch, &block); err != nil {
		return fmt.Errorf("rearm trial interval for %s: %w", scopeKey.String(), err)
	}

	flow.engine.logger.Debug("rearming trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("interval", block.TrialInterval),
	)

	if watch != nil {
		watch.armTrialTimer()
	}

	return nil
}

func (flow *engineFlow) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	rows, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		return false, fmt.Errorf("sync: checking blocked retry work for scope %s: %w", scopeKey.String(), err)
	}

	for i := range rows {
		if rows[i].ScopeKey == scopeKey {
			return true, nil
		}
	}

	return false, nil
}

func (flow *engineFlow) rearmOrDiscardScope(ctx context.Context, watch *watchRuntime, scopeKey ScopeKey) error {
	if scopeKey.IsZero() {
		return nil
	}

	hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		return fmt.Errorf("rearm or discard scope %s: %w", scopeKey.String(), err)
	}

	switch decideTimedBlockScopeAction(hasBlockedWork) {
	case timedBlockScopeKeep:
		return flow.rearmScopeTrial(ctx, watch, scopeKey)
	case timedBlockScopeDiscard:
		return flow.discardScope(ctx, watch, scopeKey)
	}

	return nil
}

// feedScopeDetection feeds an action completion into scope detection sliding
// windows. If a threshold is crossed, creates a block scope. Called directly
// from the normal processActionCompletion switch — never called for trial
// results because a live scope already owns the blocker lifecycle.
func (flow *engineFlow) feedScopeDetection(ctx context.Context, watch *watchRuntime, r *ActionCompletion) error {
	if flow.scopeState == nil {
		return nil
	}

	if r.HTTPStatus == 0 {
		return nil
	}

	sr := flow.scopeState.UpdateScope(r)
	if sr.Block {
		return flow.applyBlockScope(ctx, watch, sr)
	}

	return nil
}

// applyBlockScope persists and activates a new block scope using the same
// timing policy as trial extension and rearm.
func (flow *engineFlow) applyBlockScope(ctx context.Context, watch *watchRuntime, sr ScopeUpdateResult) error {
	now := flow.engine.nowFunc()
	interval := computeTrialInterval(sr.RetryAfter)

	block := &ActiveScope{
		Key:           sr.ScopeKey,
		BlockedAt:     now,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}
	if err := flow.activateScope(ctx, watch, block); err != nil {
		return fmt.Errorf("apply block scope %s: %w", sr.ScopeKey.String(), err)
	}

	flow.engine.logger.Warn("block scope active — actions blocked",
		slog.String("scope_key", sr.ScopeKey.String()),
		slog.String("condition_type", sr.ConditionType),
		slog.Duration("trial_interval", interval),
	)

	if watch != nil {
		watch.armTrialTimer()
	}

	return nil
}

// releaseScope atomically removes the block scope and makes blocked retry work
// under that scope eligible to run again.
func (flow *engineFlow) releaseScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if err := flow.engine.baseline.ReleaseScope(ctx, key, flow.engine.nowFunc()); err != nil {
		return fmt.Errorf("sync: releasing scope %s: %w", key.String(), err)
	}

	flow.removeActiveScope(key)
	if watch != nil {
		flow.engine.emitDebugEvent(engineDebugEvent{
			Type:     engineDebugEventScopeReleased,
			ScopeKey: key,
		})
		watch.kickRetryHeldReleaseNow()
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
func (flow *engineFlow) discardScope(ctx context.Context, watch *watchRuntime, key ScopeKey) error {
	if err := flow.engine.baseline.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	flow.removeActiveScope(key)
	if watch != nil {
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
