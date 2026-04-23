package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// loadActiveScopes refreshes watch runtime scope state from the persisted
// block_scopes table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
func (flow *engineFlow) loadActiveScopes(ctx context.Context, watch *watchRuntime) error {
	if watch == nil {
		return nil
	}

	blocks, err := flow.engine.baseline.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	activeScopes := make([]ActiveScope, 0, len(blocks))
	for i := range blocks {
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(blocks[i]))
	}
	watch.replaceActiveScopes(activeScopes)

	return nil
}

// normalizePersistedScopes removes stale persisted scopes before any admission
// begins. block_scopes now owns only timed shared blockers for blocked work,
// so a persisted scope with no blocked retry_work is dead state and must be
// discarded immediately on startup.
func (flow *engineFlow) normalizePersistedScopes(
	ctx context.Context,
	watch *watchRuntime,
) error {
	blocks, listScopeErr := flow.engine.baseline.ListBlockScopes(ctx)
	if listScopeErr != nil {
		return fmt.Errorf("sync: listing block scopes: %w", listScopeErr)
	}

	blockedRetries, err := flow.engine.baseline.ListBlockedRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing blocked retry_work rows: %w", err)
	}
	for _, step := range planPersistedScopeNormalization(blocks, blockedRetries) {
		if err := flow.dropStartupScopeRow(ctx, step.Key, step.Note); err != nil {
			return err
		}
	}

	flow.mustAssertInvariants(ctx, watch, "normalize persisted scopes")

	return nil
}

func (flow *engineFlow) dropStartupScopeRow(ctx context.Context, key ScopeKey, note string) error {
	if err := flow.engine.baseline.DeleteBlockScope(ctx, key); err != nil {
		return fmt.Errorf("sync: deleting startup scope %s: %w", key.String(), err)
	}
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

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
// from the normal applyRuntimeCompletionStage switch — never called for trial
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

func (flow *engineFlow) transitionTrialScopeToPersistedBlock(
	ctx context.Context,
	watch *watchRuntime,
	from ScopeKey,
	to ScopeKey,
	conditionType string,
	retryAfter time.Duration,
) error {
	if to.IsZero() {
		return nil
	}

	now := flow.engine.nowFunc()
	interval := computeTrialInterval(retryAfter)
	block := &ActiveScope{
		Key:           to,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
	}
	persisted, err := blockScopeRowFromActiveScope(*block)
	if err != nil {
		return fmt.Errorf("transition blocked scope %s -> %s: %w", from.String(), to.String(), err)
	}
	if err := flow.engine.baseline.UpsertBlockScope(ctx, persisted); err != nil {
		return fmt.Errorf("transition blocked scope %s -> %s: %w", from.String(), to.String(), err)
	}

	flow.upsertActiveScope(block)
	flow.engine.emitDebugEvent(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: to,
	})

	if from != to && !from.IsZero() {
		hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, from)
		if err != nil {
			return fmt.Errorf("transition blocked scope %s -> %s: check old scope blocked work: %w", from.String(), to.String(), err)
		}
		if hasBlockedWork {
			if err := flow.rearmScopeTrial(ctx, watch, from); err != nil {
				return fmt.Errorf("transition blocked scope %s -> %s: rearm old scope: %w", from.String(), to.String(), err)
			}
		} else {
			if err := flow.engine.baseline.DiscardScope(ctx, from); err != nil {
				return fmt.Errorf("transition blocked scope %s -> %s: discard old scope: %w", from.String(), to.String(), err)
			}
			flow.removeActiveScope(from)
			flow.engine.emitDebugEvent(engineDebugEvent{
				Type:     engineDebugEventScopeDiscarded,
				ScopeKey: from,
			})
		}
	}

	flow.engine.logger.Warn("block scope active — actions blocked",
		slog.String("scope_key", to.String()),
		slog.String("condition_type", conditionType),
		slog.Duration("trial_interval", interval),
	)

	flow.mustAssertInvariants(ctx, watch, "transition blocked scope")
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

func (flow *engineFlow) persistBlockedRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	if scopeKey.IsZero() {
		return fmt.Errorf("persist blocked retry_work for %s: missing scope key", work.Path)
	}

	row, err := flow.engine.baseline.RecordBlockedRetryWork(ctx, work, scopeKey)
	if err != nil {
		return fmt.Errorf("persist blocked retry_work for %s under %s: %w", work.Path, scopeKey.String(), err)
	}
	if row == nil {
		return fmt.Errorf(
			"persist blocked retry_work for %s under %s: missing persisted row",
			work.Path,
			scopeKey.String(),
		)
	}

	flow.retryRowsByKey[row.WorkKey()] = *row
	return nil
}

func (flow *engineFlow) clearBlockedRetryWorkForScope(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	if scopeKey.IsZero() {
		return nil
	}

	if err := flow.engine.baseline.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		return fmt.Errorf("clear blocked retry_work for %s under %s: %w", work.Path, scopeKey.String(), err)
	}
	if row, ok := flow.retryRowsByKey[work]; ok && row.Blocked && row.ScopeKey == scopeKey {
		delete(flow.retryRowsByKey, work)
	}

	return nil
}

// recordBlockedRetryWork records retry_work for an action that is currently
// blocked by an active scope. Blocked rows have no retry timing until the
// scope is released or trialed.
func (flow *engineFlow) recordBlockedRetryWork(ctx context.Context, action *Action, scopeKey ScopeKey) error {
	return flow.persistBlockedRetryWork(ctx, retryWorkKeyForAction(action), scopeKey)
}

func (flow *engineFlow) holdActionFromPersistedRetryState(
	current *TrackedAction,
	work RetryWorkKey,
) error {
	if current == nil {
		return nil
	}

	row, ok := flow.retryRowsByKey[work]
	if !ok {
		return fmt.Errorf("hold action %s from persisted retry state: missing retry_work row", work.Path)
	}
	if row.Blocked {
		flow.holdAction(current, heldReasonScope, row.ScopeKey, time.Time{})
		return nil
	}

	nextRetry := time.Time{}
	if row.NextRetryAt > 0 {
		nextRetry = time.Unix(0, row.NextRetryAt)
	}
	flow.holdAction(current, heldReasonRetry, ScopeKey{}, nextRetry)
	return nil
}

func (flow *engineFlow) holdActionUnderScope(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	if current == nil {
		return nil
	}

	if err := flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), scopeKey); err != nil {
		return err
	}
	flow.holdAction(current, heldReasonScope, scopeKey, time.Time{})
	if watch != nil {
		watch.armHeldTimers()
	}
	return nil
}

func (flow *engineFlow) rehomeBlockedRetryWork(
	ctx context.Context,
	r *ActionCompletion,
	scopeKey ScopeKey,
) error {
	return flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), scopeKey)
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
	watch *watchRuntime,
) ([]*TrackedAction, error) {
	now := flow.engine.nowFunc()
	var ready []*TrackedAction

	for _, key := range flow.dueRetryKeys(now) {
		if ta := flow.releaseHeldAction(key); ta != nil {
			ta.IsTrial = false
			ta.TrialScopeKey = ScopeKey{}
			ready = append(ready, ta)
		}
	}

	for _, key := range flow.dueTrialKeys(now) {
		held := flow.heldByKey[key]
		if held == nil {
			continue
		}

		ta := flow.releaseHeldAction(key)
		if ta == nil {
			continue
		}
		ta.IsTrial = true
		ta.TrialScopeKey = held.ScopeKey
		ready = append(ready, ta)
	}

	if len(ready) == 0 {
		return nil, nil
	}

	return flow.admitReady(ctx, watch, ready)
}
