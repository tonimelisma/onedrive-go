package sync

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/humanunit"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncsched"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
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

	activeScopes := make([]syncdomain.ActiveScope, 0, len(blocks))
	for i := range blocks {
		activeScopes = append(activeScopes, syncdomain.ActiveScopeFromBlockScopeRow(blocks[i]))
	}
	flow.sched.ReplaceScopes(activeScopes)

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
	for _, step := range syncstore.PlanPersistedScopeNormalization(blocks, blockedRetries) {
		if dropErr := flow.dropStartupScopeRow(ctx, step.Key, step.Note); dropErr != nil {
			return dropErr
		}
	}

	// The converse of the discard above: a blocked row whose scope is not
	// persisted has nothing that will ever trial or release it (R-2.10.33).
	// Earlier builds wrote such rows for a 507 below the quota window; a
	// store that still holds them gets the work back as due retry work.
	released, err := flow.retryWork.releaseOrphans(ctx, flow.deps.store, flow.deps.now())
	if err != nil {
		return fmt.Errorf("sync: releasing blocked retry_work without a scope: %w", err)
	}
	if released > 0 {
		flow.deps.logger.Info("Released work that was blocked under a pause that no longer exists; it is retried now",
			slog.Int("items", released),
		)
	}

	flow.mustAssertInvariants(ctx, "normalize persisted scopes")

	return nil
}

func (flow *engineFlow) dropStartupScopeRow(ctx context.Context, key syncdomain.ScopeKey, note string) error {
	if err := flow.deps.store.DeleteBlockScope(ctx, key); err != nil {
		return fmt.Errorf("sync: deleting startup scope %s: %w", key.String(), err)
	}
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventStartupScopeNormalized,
		ScopeKey: key,
		Note:     note,
	})
	return nil
}

func (flow *engineFlow) activateScope(ctx context.Context, block *syncdomain.ActiveScope) error {
	if block == nil {
		return fmt.Errorf("sync: activating scope: missing block")
	}
	if err := flow.persistActiveScope(ctx, block); err != nil {
		return fmt.Errorf("sync: activating scope %s: %w", block.Key.String(), err)
	}

	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventScopeActivated,
		ScopeKey: block.Key,
	})

	flow.mustAssertInvariants(ctx, "activate scope")

	return nil
}

// persistActiveScope writes a scope's timing to both authorities, the store
// row and the ledger, without announcing an activation.
func (flow *engineFlow) persistActiveScope(ctx context.Context, block *syncdomain.ActiveScope) error {
	persisted, err := syncdomain.BlockScopeRowFromActiveScope(*block)
	if err != nil {
		return err
	}
	if err := flow.deps.store.UpsertBlockScope(ctx, persisted); err != nil {
		return err
	}
	flow.sched.PutScope(block)

	return nil
}

// newBlockActivation is the ledger shape of a (re)activation of key: one
// generation past the active scope's, with the ramp's memory of where the
// activating 429 stopped it, or the first generation of a new scope. The
// trial never moves earlier than a deadline the scope already holds
// (latestTrialDeadline).
func (flow *engineFlow) newBlockActivation(key syncdomain.ScopeKey, interval time.Duration, now time.Time) *syncdomain.ActiveScope {
	block := &syncdomain.ActiveScope{
		Key:           key,
		TrialInterval: interval,
		NextTrialAt:   now.Add(interval),
		Generation:    1,
	}
	if current, ok := flow.sched.Scope(key); ok {
		block.Generation = current.Generation + 1
		block.NextTrialAt = latestTrialDeadline(&current, block.NextTrialAt)
		block.SlowStartThreshold = syncsched.SlowStartThresholdAfterThrottle(
			flow.cfg.transferWorkers, current.ReleaseAllowance, current.SlowStartThreshold)
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
func latestTrialDeadline(current *syncdomain.ActiveScope, requested time.Time) time.Time {
	if current.ReleaseAllowance == 0 && current.NextTrialAt.After(requested) {
		return current.NextTrialAt
	}

	return requested
}

func (flow *engineFlow) extendScopeTrial(
	ctx context.Context,
	scopeKey syncdomain.ScopeKey,
	retryAfter time.Duration,
) error {
	block, ok := flow.sched.Scope(scopeKey)
	if !ok {
		return nil
	}

	newInterval := syncstore.ComputeTrialInterval(retryAfter)
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
	block.SlowStartThreshold = syncsched.SlowStartThresholdAfterThrottle(
		flow.cfg.transferWorkers, block.ReleaseAllowance, block.SlowStartThreshold)
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

func (flow *engineFlow) rearmScopeTrial(ctx context.Context, scopeKey syncdomain.ScopeKey) error {
	block, ok := flow.sched.Scope(scopeKey)
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

// scopeHasBlockedRetryWork asks the store whether any retry_work row is
// blocked under scopeKey: a count of that scope's rows, not a listing of
// every blocked row.
func (flow *engineFlow) scopeHasBlockedRetryWork(ctx context.Context, scopeKey syncdomain.ScopeKey) (bool, error) {
	hasBlockedWork, err := flow.deps.store.ScopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		return false, fmt.Errorf("sync: checking blocked retry work for scope %s: %w", scopeKey.String(), err)
	}

	return hasBlockedWork, nil
}

func (flow *engineFlow) rearmOrDiscardScope(ctx context.Context, scopeKey syncdomain.ScopeKey) error {
	if scopeKey.IsZero() {
		return nil
	}

	hasBlockedWork, err := flow.scopeHasBlockedRetryWork(ctx, scopeKey)
	if err != nil {
		return fmt.Errorf("rearm or discard scope %s: %w", scopeKey.String(), err)
	}

	switch syncstore.DecideTimedBlockScopeAction(hasBlockedWork) {
	case syncstore.TimedBlockScopeKeep:
		return flow.rearmScopeTrial(ctx, scopeKey)
	case syncstore.TimedBlockScopeDiscard:
		return flow.discardScope(ctx, scopeKey)
	}

	return nil
}

// detectScope feeds an action completion into scope detection's sliding
// windows and reports whether it calls for a block scope. The caller
// persists the completion's work blocked under that scope, together with the
// scope row, before activating it (persistFailureWithScopeEffects). A completion with no status is fed
// too: a request OneDrive never answered is outage evidence.
func (flow *engineFlow) detectScope(r *syncdomain.ActionCompletion) syncdomain.ScopeUpdateResult {
	return flow.sched.ObserveFailure(r)
}

// blockActivation is a block scope a failure activates, prepared before the
// failure's work is recorded blocked under it: the store writes the scope row
// and the blocked row in one transaction (RecordBlockedRetryWork), so a crash
// between them cannot leave a scope with nothing to trial or a blocked row
// that references no scope. It uses the same timing policy as trial extension
// and rearm.
type blockActivation struct {
	update    syncdomain.ScopeUpdateResult
	block     *syncdomain.ActiveScope
	row       *syncdomain.BlockScope
	interval  time.Duration
	wasActive bool
}

// prepareBlockActivation is the activation sr calls for, before anything is
// written. It changes no state.
func (flow *engineFlow) prepareBlockActivation(sr syncdomain.ScopeUpdateResult) (*blockActivation, error) {
	interval := syncstore.ComputeTrialInterval(sr.RetryAfter)
	block := flow.newBlockActivation(sr.ScopeKey, interval, flow.deps.now())
	row, err := syncdomain.BlockScopeRowFromActiveScope(*block)
	if err != nil {
		return nil, fmt.Errorf("sync: activating scope %s: %w", sr.ScopeKey.String(), err)
	}

	return &blockActivation{
		update:    sr,
		block:     block,
		row:       row,
		interval:  interval,
		wasActive: flow.sched.HasScope(sr.ScopeKey),
	}, nil
}

// storeRow is the scope row the blocked row's transaction writes: none for
// no activation.
func (a *blockActivation) storeRow() *syncdomain.BlockScope {
	if a == nil {
		return nil
	}

	return a.row
}

// recordActivatingBlockedRetryWork records work blocked under activation's
// scope together with the scope row, and returns the row the store holds.
func (flow *engineFlow) recordActivatingBlockedRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	activation *blockActivation,
) (*syncdomain.RetryWorkRow, error) {
	return flow.retryWork.recordBlocked(ctx, flow.deps.store, work, activation.update.ScopeKey, activation.row)
}

// applyBlockActivation makes a prepared activation, whose scope row the store
// wrote with its first blocked row, active: the ledger takes the scope, the
// admitted work it blocks is taken back, and its trial is armed.
func (flow *engineFlow) applyBlockActivation(ctx context.Context, activation *blockActivation) error {
	key := activation.update.ScopeKey
	flow.sched.PutScope(activation.block)
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventScopeActivated,
		ScopeKey: key,
	})
	flow.mustAssertInvariants(ctx, "activate scope")

	flow.handOverColdStart(key)
	if err := flow.retireAdmittedWorkBlockedBy(ctx, key); err != nil {
		return err
	}

	flow.logBlockScopeActivated(key, activation.update.ConditionType, activation.interval, activation.wasActive)

	flow.signals.armTrialTimer()

	return nil
}

// transitionTrialScopeToPersistedBlock moves a failed trial from its scope to
// the scope its failure activated, whose row the store wrote with the trial's
// blocked row: the new scope becomes active, and the trial's scope is rearmed
// while it still has blocked work, or discarded.
func (flow *engineFlow) transitionTrialScopeToPersistedBlock(
	ctx context.Context,
	from syncdomain.ScopeKey,
	activation *blockActivation,
) error {
	to := activation.update.ScopeKey
	flow.sched.PutScope(activation.block)
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventScopeActivated,
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
		} else if err := flow.discardScope(ctx, from); err != nil {
			return fmt.Errorf("transition blocked scope %s -> %s: discard old scope: %w", from.String(), to.String(), err)
		}
	}

	flow.logBlockScopeActivated(to, activation.update.ConditionType, activation.interval, activation.wasActive)

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
	key syncdomain.ScopeKey,
	conditionType syncdomain.IssueType,
	interval time.Duration,
	wasActive bool,
) {
	fields := []any{
		slog.String("scope_key", key.String()),
		slog.String("condition_type", string(conditionType)),
		slog.Duration("trial_interval", interval),
	}
	if wasActive {
		flow.deps.logger.Debug("block scope active — actions blocked", fields...)
		return
	}

	line, clearsAutomatically := blockScopeActivationLine(key, conditionType, humanunit.Duration(interval, humanunit.Wait))
	if clearsAutomatically {
		flow.deps.logger.Info(line, fields...)
		return
	}

	flow.deps.logger.Warn(line, fields...)
}

// blockScopeActivationLine is the line said when a block scope of a condition
// starts, and whether the condition clears without the user, which makes the
// line Info rather than Warn. That verdict is the condition family's
// (ConditionClearsAutomatically), the same one that keeps the watch condition
// summary below a warning, so the two cannot disagree; a condition with no
// family is not known to clear by itself.
func blockScopeActivationLine(
	key syncdomain.ScopeKey, conditionType syncdomain.IssueType, retryIn string,
) (line string, clearsAutomatically bool) {
	family, hasFamily := syncdomain.ConditionKeyForIssueType(conditionType)

	return blockScopeActivationWording(key, conditionType, retryIn), hasFamily && syncdomain.ConditionClearsAutomatically(family)
}

// blockScopeActivationWording words the activation line of a condition's
// block scope. It decides every IssueType by name (the exhaustive linter
// holds the switch to that), so a new condition is worded on purpose. The
// conditions with no wording of their own, like a value outside the enum,
// are named in words after the scope.
func blockScopeActivationWording(key syncdomain.ScopeKey, conditionType syncdomain.IssueType, retryIn string) string {
	switch conditionType {
	case syncdomain.IssueRateLimited:
		return "OneDrive is limiting requests; pausing affected transfers and retrying in " + retryIn
	case syncdomain.IssueServiceOutage:
		return "OneDrive is temporarily unavailable; pausing transfers and retrying in " + retryIn
	case syncdomain.IssueQuotaExceeded:
		return "OneDrive storage is full; uploads are paused until space is freed (checking again in " + retryIn + ")"
	case syncdomain.IssueDiskFull:
		// The same scope covers the min_free_space reserve and a real
		// ENOSPC/EDQUOT write (inodes included); the per-file error names
		// which one, so this line names both limits.
		return "The local disk is full or below the min_free_space reserve; downloads are paused " +
			"until there is room (checking again in " + retryIn + ")"
	case syncdomain.IssueLocalReadOnly:
		return "The local filesystem holding " + key.Humanize() + " is read-only; local changes there are " +
			"paused until it is writable again (checking again in " + retryIn + ")"
	case syncdomain.IssueInvalidFilename,
		syncdomain.IssuePathTooLong,
		syncdomain.IssueFileTooLarge,
		syncdomain.IssueHashError,
		syncdomain.IssueUnauthorized,
		syncdomain.IssueRemoteWriteDenied,
		syncdomain.IssueRemoteReadDenied,
		syncdomain.IssueRemoteRootNotFound,
		syncdomain.IssueLocalReadDenied,
		syncdomain.IssueLocalWriteDenied,
		syncdomain.IssueCaseCollision,
		syncdomain.IssueRemoteNameTwin,
		syncdomain.IssueFileTooLargeForSpace,
		syncdomain.IssueLocalNameTooLong,
		syncdomain.IssueDeletesHeld,
		syncdomain.IssueLocalUnsyncableReplacement,
		syncdomain.IssueLocalMountBoundary,
		syncdomain.IssueLocalMountMissing,
		syncdomain.IssueRemoteFolderUnverifiable,
		syncdomain.IssueLocalInterruptedRemoval,
		syncdomain.IssueRemovedFolderKept,
		syncdomain.IssueLocalFileTooLarge,
		syncdomain.IssueLocalBusy,
		syncdomain.IssueLocalCrossDevice,
		syncdomain.IssueUnsupportedOperation,
		syncdomain.IssueMalwareDetected,
		syncdomain.IssueLocalDatabaseOpen,
		syncdomain.IssueUploadVerification,
		syncdomain.IssueDownloadIntegrity,
		syncdomain.IssueRequestTimeout,
		syncdomain.IssueTransientConflict,
		syncdomain.IssueTransientNotFound,
		syncdomain.IssueResourceLocked,
		syncdomain.IssueTransferURLRejected:
		return pausedScopeLine(key, conditionType, retryIn)
	default:
		return pausedScopeLine(key, conditionType, retryIn)
	}
}

// pausedScopeLine is the activation line of a block scope whose condition
// has no wording of its own: the scope, and the condition in words.
func pausedScopeLine(key syncdomain.ScopeKey, conditionType syncdomain.IssueType, retryIn string) string {
	return "Paused syncing " + key.Humanize() + " (" + strings.ReplaceAll(string(conditionType), "_", " ") +
		"); checking again in " + retryIn
}

// discardScope atomically removes the block scope and deletes the retry work
// blocked under it; work that only names the scope keeps its row. A held
// action whose row the store deleted goes back to admission. Used when the
// blocked subtree itself disappears, and when a scope's last blocked work
// moved under another scope.
func (flow *engineFlow) discardScope(ctx context.Context, key syncdomain.ScopeKey) error {
	deleted, err := flow.retryWork.discardScope(ctx, flow.deps.store, key)
	if err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	now := flow.deps.now()
	for _, work := range deleted {
		if ta := flow.sched.Release(work); ta != nil {
			flow.sched.Hold(ta, syncsched.HeldRetry, syncdomain.ScopeKey{}, now)
		}
	}

	flow.sched.DropScope(key)
	flow.signals.armTrialTimer()
	if len(deleted) > 0 {
		flow.signals.armRetryTimer()
	}
	flow.deps.emit(syncdomain.EngineDebugEvent{
		Type:     syncdomain.EngineDebugEventScopeDiscarded,
		ScopeKey: key,
	})

	flow.mustAssertDiscardedScope(ctx, key, "discard scope")
	flow.mustAssertInvariants(ctx, "discard scope")

	return nil
}

func (flow *engineFlow) persistBlockedRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
) error {
	if scopeKey.IsZero() {
		return fmt.Errorf("persist blocked retry_work for %s: missing scope key", work.Path)
	}

	_, err := flow.retryWork.recordBlocked(ctx, flow.deps.store, work, scopeKey, nil)
	return err
}

func (flow *engineFlow) clearBlockedRetryWorkForScope(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
) error {
	if scopeKey.IsZero() {
		return nil
	}

	return flow.retryWork.clearBlocked(ctx, flow.deps.store, work, scopeKey)
}

// recordBlockedRetryWork records retry_work for an action that is currently
// blocked by an active scope. Blocked rows have no retry timing until the
// scope is released or trialed.
func (flow *engineFlow) recordBlockedRetryWork(ctx context.Context, action *syncdomain.Action, scopeKey syncdomain.ScopeKey) error {
	return flow.persistBlockedRetryWork(ctx, syncdomain.RetryWorkKeyForAction(action), scopeKey)
}

func (flow *engineFlow) holdActionFromPersistedRetryState(
	current *syncdomain.TrackedAction,
	work syncdomain.RetryWorkKey,
) error {
	if current == nil {
		return nil
	}

	row, ok := flow.retryWork.row(work)
	if !ok {
		return fmt.Errorf("hold action %s from persisted retry state: missing retry_work row", work.Path)
	}
	if row.Blocked {
		flow.sched.Hold(current, syncsched.HeldScope, row.ScopeKey, time.Time{})
		return nil
	}

	nextRetry := time.Time{}
	if syncdomain.StoredTimeIsSet(row.NextRetryAt) {
		nextRetry = time.Unix(0, row.NextRetryAt)
	}
	flow.sched.Hold(current, syncsched.HeldRetry, syncdomain.ScopeKey{}, nextRetry)
	return nil
}

func (flow *engineFlow) holdActionUnderScope(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	scopeKey syncdomain.ScopeKey,
) error {
	if current == nil {
		return nil
	}

	if err := flow.persistBlockedRetryWork(ctx, syncdomain.RetryWorkKeyForCompletion(r), scopeKey); err != nil {
		return err
	}
	flow.sched.Hold(current, syncsched.HeldScope, scopeKey, time.Time{})
	flow.signals.armHeldTimers()
	return nil
}

func (flow *engineFlow) rehomeBlockedRetryWork(
	ctx context.Context,
	r *syncdomain.ActionCompletion,
	scopeKey syncdomain.ScopeKey,
) error {
	return flow.persistBlockedRetryWork(ctx, syncdomain.RetryWorkKeyForCompletion(r), scopeKey)
}

func (flow *engineFlow) drainDueHeldWorkNow(
	ctx context.Context,
) ([]*syncdomain.TrackedAction, error) {
	now := flow.deps.now()
	var ready []*syncdomain.TrackedAction

	for _, key := range flow.sched.DueRetries(now) {
		ta := flow.sched.Release(key)
		if ta == nil {
			continue
		}
		ta.IsTrial = false
		ta.TrialScopeKey = syncdomain.ScopeKey{}
		ta.TrialScopeGeneration = 0
		ta.AdmitAt = time.Time{}
		ready = append(ready, ta)
	}

	for _, key := range flow.sched.DueTrials(now) {
		held, ok := flow.sched.HeldWork(key)
		if !ok {
			continue
		}

		ta := flow.sched.Release(key)
		if ta == nil {
			continue
		}
		// A due trial means the ramp stalled (or never started): whatever
		// batch was outstanding is not coming back, so the trial's success
		// releases the next batch outright.
		if scope, ok := flow.sched.Scope(held.ScopeKey); ok {
			if scope.BatchOutstanding != 0 {
				scope.BatchOutstanding = 0
				flow.sched.PutScope(&scope)
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
