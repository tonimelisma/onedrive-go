package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// loadActiveScopes refreshes watch runtime scope state from the persisted
// block_scopes table. The store remains the restart/recovery record; watch
// mode keeps only the current working set in memory.
//
// This is watch-only by construction: one-shot initializes its scope working
// set from the current plan instead. It sits on watchRuntime rather than
// taking a nullable one, which is what the old nil guard was standing in for.
func (rt *watchRuntime) loadActiveScopes(ctx context.Context) error {
	flow := rt.engineFlow

	blocks, err := flow.deps.store.ListBlockScopes(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing active scopes: %w", err)
	}

	activeScopes := make([]activeScope, 0, len(blocks))
	for i := range blocks {
		activeScopes = append(activeScopes, activeScopeFromBlockScopeRow(blocks[i]))
	}
	flow.scopes.replaceActiveScopes(activeScopes)

	return nil
}

// normalizePersistedScopes removes stale persisted scopes before any admission
// begins. block_scopes now owns only timed shared blockers for blocked work,
// so a persisted scope with no blocked retry_work is dead state and must be
// discarded immediately on startup.
func (flow *engineFlow) normalizePersistedScopes(
	ctx context.Context,
) error {
	blocks, listScopeErr := flow.deps.store.ListBlockScopes(ctx)
	if listScopeErr != nil {
		return fmt.Errorf("sync: listing block scopes: %w", listScopeErr)
	}

	blockedRetries, err := flow.deps.store.ListBlockedRetryWork(ctx)
	if err != nil {
		return fmt.Errorf("sync: listing blocked retry_work rows: %w", err)
	}
	for _, step := range planPersistedScopeNormalization(blocks, blockedRetries) {
		if err := flow.dropStartupScopeRow(ctx, step.Key, step.Note); err != nil {
			return err
		}
	}

	flow.mustAssertInvariants(ctx, "normalize persisted scopes")

	return nil
}

func (flow *engineFlow) dropStartupScopeRow(ctx context.Context, key ScopeKey, note string) error {
	if err := flow.deps.store.DeleteBlockScope(ctx, key); err != nil {
		return fmt.Errorf("sync: deleting startup scope %s: %w", key.String(), err)
	}
	flow.deps.emit(engineDebugEvent{
		Type:     engineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (flow *engineFlow) activateScope(ctx context.Context, block *activeScope) error {
	if block == nil {
		return fmt.Errorf("sync: activating scope: missing block")
	}
	if err := flow.persistActiveScope(ctx, block); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	flow.deps.emit(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, "activate scope")

	return nil
}

// persistActiveScope writes a scope's timing to both authorities, the store
// row and the ledger, without announcing an activation.
func (flow *engineFlow) persistActiveScope(ctx context.Context, block *activeScope) error {
	persisted, err := blockScopeRowFromActiveScope(*block)
	if err != nil {
		return err
	}
	if err := flow.deps.store.UpsertBlockScope(ctx, persisted); err != nil {
		return err
	}
	flow.scopes.upsertActiveScope(block)

	return nil
}

// newBlockActivation is the ledger shape of a (re)activation of key: one
// generation past the active scope's, with the ramp's memory of where the
// activating 429 stopped it, or the first generation of a new scope. The
// trial never moves earlier than a deadline the scope already holds
// (latestTrialDeadline).
func (flow *engineFlow) newBlockActivation(key ScopeKey, interval time.Duration, now time.Time) *activeScope {
	block := &activeScope{
		Key:           key,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
		Generation:    1,
	}
	if current, ok := flow.scopes.lookupActiveScope(key); ok {
		block.Generation = current.Generation + 1
		block.NextTrialAt = latestTrialDeadline(&current, block.NextTrialAt)
		block.SlowStartThreshold = slowStartThresholdAfterThrottle(
			flow.engine.transferWorkers, current.ReleaseAllowance, current.SlowStartThreshold)
	}

	return block
}

// latestTrialDeadline is the trial deadline a new 429 leaves a scope with:
// the later of the one it holds and the one the 429 asks for, while the
// scope is blocking. The transfers in flight when a throttle hits each
// bring their own Retry-After, and Microsoft extends the penalty for every
// request made inside it, so the answers that arrive later carry the later
// deadlines; but their completions reach the loop in scheduler order, and a
// shorter Retry-After applied after a longer one would pull the trial inside
// the penalty the drive still holds, where it costs a 429 and another
// extension (R-6.8.24). A scope on probation holds no server deadline: its
// trial is the stall-recovery re-arm one interval out, and the 429 that
// ends the probation sets the deadline outright.
func latestTrialDeadline(current *activeScope, requested time.Time) time.Time {
	if current.ReleaseAllowance == 0 && current.NextTrialAt.After(requested) {
		return current.NextTrialAt
	}

	return requested
}

func (flow *engineFlow) extendScopeTrial(
	ctx context.Context,
	scopeKey ScopeKey,
	retryAfter time.Duration,
) error {
	block, ok := flow.scopes.lookupActiveScope(scopeKey)
	if !ok {
		return nil
	}

	newInterval := computeTrialInterval(retryAfter)
	nextAt := flow.deps.now().Add(newInterval)

	flow.deps.logger.Debug("extending trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("new_interval", newInterval),
		slog.Duration("retry_after", retryAfter),
	)

	// A 429 from a trial or a probation member is a re-block: the ramp
	// starts over, remembering where it was stopped, and the admitted work
	// the scope blocks is taken back.
	block.NextTrialAt = latestTrialDeadline(&block, nextAt)
	block.TrialInterval = newInterval
	block.SlowStartThreshold = slowStartThresholdAfterThrottle(
		flow.engine.transferWorkers, block.ReleaseAllowance, block.SlowStartThreshold)
	block.ReleaseAllowance = 0
	block.BatchOutstanding = 0
	block.Generation++
	if err := flow.activateScope(ctx, &block); err != nil {
		return fmt.Errorf("extend trial interval for %s: %w", scopeKey.String(), err)
	}
	flow.handOverColdStart(scopeKey)
	if err := flow.retireAdmittedWorkBlockedBy(ctx, scopeKey); err != nil {
		return fmt.Errorf("extend trial interval for %s: %w", scopeKey.String(), err)
	}

	flow.signals.armTrialTimer()

	return nil
}

func (flow *engineFlow) rearmScopeTrial(ctx context.Context, scopeKey ScopeKey) error {
	block, ok := flow.scopes.lookupActiveScope(scopeKey)
	if !ok {
		return nil
	}
	if block.TrialInterval <= 0 {
		return nil
	}

	block.NextTrialAt = flow.deps.now().Add(block.TrialInterval)
	if err := flow.activateScope(ctx, &block); err != nil {
		return fmt.Errorf("rearm trial interval for %s: %w", scopeKey.String(), err)
	}

	flow.deps.logger.Debug("rearming trial interval",
		slog.String("scope_key", scopeKey.String()),
		slog.Duration("interval", block.TrialInterval),
	)

	flow.signals.armTrialTimer()

	return nil
}

func (flow *engineFlow) scopeHasBlockedRetryWork(ctx context.Context, scopeKey ScopeKey) (bool, error) {
	rows, err := flow.deps.store.ListBlockedRetryWork(ctx)
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

func (flow *engineFlow) rearmOrDiscardScope(ctx context.Context, scopeKey ScopeKey) error {
	if scopeKey.IsZero() {
		return nil
	}

	hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		return fmt.Errorf("rearm or discard scope %s: %w", scopeKey.String(), err)
	}

	switch decideTimedBlockScopeAction(hasBlockedWork) {
	case timedBlockScopeKeep:
		return flow.rearmScopeTrial(ctx, scopeKey)
	case timedBlockScopeDiscard:
		return flow.discardScope(ctx, scopeKey)
	}

	return nil
}

// detectScope feeds an action completion into scope detection's sliding
// windows and reports whether it calls for a block scope. The caller
// persists the completion's work blocked under that scope before activating
// it (persistFailureWithScopeEffects).
func (flow *engineFlow) detectScope(r *actionCompletion) scopeUpdateResult {
	if flow.scopes.state == nil || r.HTTPStatus == 0 {
		return scopeUpdateResult{}
	}

	return flow.scopes.state.UpdateScope(r)
}

// applyBlockScope persists and activates a new block scope using the same
// timing policy as trial extension and rearm.
func (flow *engineFlow) applyBlockScope(ctx context.Context, sr scopeUpdateResult) error {
	now := flow.deps.now()
	interval := computeTrialInterval(sr.RetryAfter)

	block := flow.newBlockActivation(sr.ScopeKey, interval, now)
	wasActive := flow.scopes.hasActiveScope(sr.ScopeKey)
	if err := flow.activateScope(ctx, block); err != nil {
		// activateScope already names the operation and the scope.
		return err
	}
	flow.handOverColdStart(sr.ScopeKey)
	if err := flow.retireAdmittedWorkBlockedBy(ctx, sr.ScopeKey); err != nil {
		return err
	}

	flow.logBlockScopeActivated(sr.ScopeKey, sr.ConditionType, interval, wasActive)

	flow.signals.armTrialTimer()

	return nil
}

func (flow *engineFlow) transitionTrialScopeToPersistedBlock(
	ctx context.Context,
	from ScopeKey,
	to ScopeKey,
	conditionType string,
	retryAfter time.Duration,
) error {
	if to.IsZero() {
		return nil
	}

	now := flow.deps.now()
	interval := computeTrialInterval(retryAfter)
	block := flow.newBlockActivation(to, interval, now)
	wasActive := flow.scopes.hasActiveScope(to)
	if err := flow.persistActiveScope(ctx, block); err != nil {
		return fmt.Errorf("transition blocked scope %s -> %s: %w", from.String(), to.String(), err)
	}
	flow.deps.emit(engineDebugEvent{
		Type:     engineDebugEventScopeActivated,
		ScopeKey: to,
	})
	flow.handOverColdStart(to)
	if err := flow.retireAdmittedWorkBlockedBy(ctx, to); err != nil {
		return fmt.Errorf("transition blocked scope %s -> %s: %w", from.String(), to.String(), err)
	}

	if from != to && !from.IsZero() {
		hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, from)
		if err != nil {
			return fmt.Errorf("transition blocked scope %s -> %s: check old scope blocked work: %w", from.String(), to.String(), err)
		}
		if hasBlockedWork {
			if err := flow.rearmScopeTrial(ctx, from); err != nil {
				return fmt.Errorf("transition blocked scope %s -> %s: rearm old scope: %w", from.String(), to.String(), err)
			}
		} else {
			if err := flow.deps.store.DiscardScope(ctx, from); err != nil {
				return fmt.Errorf("transition blocked scope %s -> %s: discard old scope: %w", from.String(), to.String(), err)
			}
			flow.scopes.removeActiveScope(from)
			flow.deps.emit(engineDebugEvent{
				Type:     engineDebugEventScopeDiscarded,
				ScopeKey: from,
			})
		}
	}

	flow.logBlockScopeActivated(to, conditionType, interval, wasActive)

	flow.mustAssertInvariants(ctx, "transition blocked scope")
	return nil
}

// logBlockScopeActivated reports a block scope once, when it starts. Every
// in-flight transfer that fails the same way (each 429 of a throttling burst,
// each upload into a full drive) re-applies the same scope, and trial
// transitions re-persist it; those repeats stay at Debug. Throttling and
// service outages are handled automatically, so they are Info; conditions
// the user can act on (storage full, local disk full, no permission) are one
// Warn in plain words.
func (flow *engineFlow) logBlockScopeActivated(
	key ScopeKey,
	conditionType string,
	interval time.Duration,
	wasActive bool,
) {
	fields := []any{
		slog.String("scope_key", key.String()),
		slog.String("condition_type", conditionType),
		slog.Duration("trial_interval", interval),
	}
	if wasActive {
		flow.deps.logger.Debug("block scope active — actions blocked", fields...)
		return
	}

	retryIn := waitText(interval)
	switch conditionType {
	case issueRateLimited:
		flow.deps.logger.Info("OneDrive is limiting requests; pausing affected transfers and retrying in "+retryIn, fields...)
	case issueServiceOutage:
		flow.deps.logger.Info("OneDrive is temporarily unavailable; pausing transfers and retrying in "+retryIn, fields...)
	case IssueQuotaExceeded:
		flow.deps.logger.Warn("OneDrive storage is full; uploads are paused until space is freed (checking again in "+retryIn+")",
			fields...)
	case issueDiskFull:
		// The same scope covers the min_free_space reserve and a real
		// ENOSPC/EDQUOT write (inodes included); the per-file error names
		// which one, so this line names both limits.
		flow.deps.logger.Warn("The local disk is full or below the min_free_space reserve; downloads are paused "+
			"until there is room (checking again in "+retryIn+")",
			fields...)
	case issueLocalReadOnly:
		flow.deps.logger.Warn("The local filesystem holding "+key.Humanize()+" is read-only; local changes there are "+
			"paused until it is writable again (checking again in "+retryIn+")",
			fields...)
	default:
		flow.deps.logger.Warn("Paused syncing "+key.Humanize()+" ("+strings.ReplaceAll(conditionType, "_", " ")+
			"); checking again in "+retryIn, fields...)
	}
}

// discardScope atomically removes the block scope and deletes blocked retry
// work tied to it. Used when the blocked subtree itself disappears.
func (flow *engineFlow) discardScope(ctx context.Context, key ScopeKey) error {
	if err := flow.deps.store.DiscardScope(ctx, key); err != nil {
		return fmt.Errorf("sync: discarding scope %s: %w", key.String(), err)
	}

	flow.scopes.removeActiveScope(key)
	flow.signals.armTrialTimer()
	flow.deps.emit(engineDebugEvent{
		Type:     engineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, key, "discard scope")
	flow.mustAssertInvariants(ctx, "discard scope")

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

	row, err := flow.deps.store.RecordBlockedRetryWork(ctx, work, scopeKey)
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

	flow.retries.rowsByKey[row.WorkKey()] = *row
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

	if err := flow.deps.store.ClearBlockedRetryWork(ctx, work, scopeKey); err != nil {
		return fmt.Errorf("clear blocked retry_work for %s under %s: %w", work.Path, scopeKey.String(), err)
	}
	if row, ok := flow.retries.rowsByKey[work]; ok && row.Blocked && row.ScopeKey == scopeKey {
		delete(flow.retries.rowsByKey, work)
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
	current *trackedAction,
	work RetryWorkKey,
) error {
	if current == nil {
		return nil
	}

	row, ok := flow.retries.rowsByKey[work]
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
	current *trackedAction,
	r *actionCompletion,
	scopeKey ScopeKey,
) error {
	if current == nil {
		return nil
	}

	if err := flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), scopeKey); err != nil {
		return err
	}
	flow.holdAction(current, heldReasonScope, scopeKey, time.Time{})
	flow.signals.armHeldTimers()
	return nil
}

func (flow *engineFlow) rehomeBlockedRetryWork(
	ctx context.Context,
	r *actionCompletion,
	scopeKey ScopeKey,
) error {
	return flow.persistBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), scopeKey)
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
) ([]*trackedAction, error) {
	now := flow.deps.now()
	var ready []*trackedAction

	for _, key := range flow.dueRetryKeys(now) {
		ta := flow.retries.releaseHeldAction(key)
		if ta == nil {
			continue
		}
		ta.IsTrial = false
		ta.TrialScopeKey = ScopeKey{}
		ta.TrialScopeGeneration = 0
		ta.AdmitAt = time.Time{}
		ready = append(ready, ta)
	}

	for _, key := range flow.dueTrialKeys(now) {
		held := flow.retries.heldByKey[key]
		if held == nil {
			continue
		}

		ta := flow.retries.releaseHeldAction(key)
		if ta == nil {
			continue
		}
		// A due trial means the ramp stalled (or never started): whatever
		// batch was outstanding is not coming back, so the trial's success
		// releases the next batch outright.
		if scope, ok := flow.scopes.lookupActiveScope(held.ScopeKey); ok {
			if scope.BatchOutstanding != 0 {
				scope.BatchOutstanding = 0
				flow.scopes.upsertActiveScope(&scope)
			}
			ta.TrialScopeGeneration = scope.Generation
		}
		ta.IsTrial = true
		ta.TrialScopeKey = held.ScopeKey
		// The trial is the one request the server-given Retry-After allows
		// when it is up; it is not jittered.
		ta.AdmitAt = time.Time{}
		ready = append(ready, ta)
	}

	if len(ready) == 0 {
		return nil, nil
	}

	return flow.admitReady(ctx, ready)
}
