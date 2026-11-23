package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func (flow *engineFlow) failAfterControlStateError(current *syncdomain.TrackedAction, err error) error {
	flow.sched.Finished(current)
	return err
}

// applyRuntimeCompletionStage is the runtime completion boundary: classify the
// finished exact action, apply the resulting mutation/persistence decision, and
// then release any due held work back into the ready frontier.
func (flow *engineFlow) applyRuntimeCompletionStage(
	ctx context.Context,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) ([]*syncdomain.TrackedAction, error) {
	if r == nil {
		return nil, nil
	}

	decision := classifyResult(r)
	if isLiveRunCancel(r) {
		flow.deps.logger.Warn("Syncing "+r.Path+" was canceled while the sync went on; it will be tried again",
			slog.String("path", r.Path),
			slog.String("action_type", r.ActionType.String()),
			slog.Any("error", r.Err),
		)
	}
	current := flow.sched.TrackedForCompletion(r)
	flow.deps.activity(r, &decision, flow.conditionStartsWith(&decision, r))
	if decision.Class != errclass.ClassSuperseded {
		// The action ran: whatever came of it, its supersedes were not
		// consecutive, so a wedge streak (D-059d) starts over.
		flow.clearSupersedeStreak(r)
	}

	var (
		dispatched []*syncdomain.TrackedAction
		err        error
	)
	// A trial whose scope is gone (the ramp's last batch released it while
	// this member ran) is an ordinary action again: a success is just a
	// success, and a 429 starts a new episode through scope detection.
	if r.IsTrial && !r.TrialScopeKey.IsZero() && flow.sched.HasScope(r.TrialScopeKey) {
		dispatched, err = flow.applyTrialCompletionDecision(ctx, r.TrialScopeKey, &decision, current, r, bl)
	} else {
		dispatched, err = flow.applyNormalCompletionDecision(ctx, &decision, current, r, bl)
	}
	if err != nil {
		return dispatched, err
	}

	return flow.reduceReadyFrontierStage(ctx, bl, dispatched)
}

func (flow *engineFlow) applyNormalCompletionDecision(
	ctx context.Context,
	decision *syncdomain.ResultDecision,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) ([]*syncdomain.TrackedAction, error) {
	handled, err := flow.maybeHandlePermissionFailure(ctx, decision, current, r, bl)
	if err != nil {
		return nil, flow.failAfterControlStateError(current, err)
	}
	if handled {
		return nil, nil
	}

	switch decision.Class {
	case errclass.ClassInvalid:
		if _, err := flow.applyResultPersistence(ctx, decision, r, nil); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		flow.sched.Finished(current)
		return nil, fmt.Errorf("classify action completion: invalid failure class")
	case errclass.ClassSuccess:
		dispatched, err := flow.applyCompletionSuccess(ctx, current, r)
		if err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		return dispatched, nil
	case errclass.ClassShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "shutdown action completion")
		return nil, nil
	case errclass.ClassSuperseded:
		if err := flow.applySupersededCompletion(ctx, current, r, "superseded action completion"); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		return nil, nil
	case errclass.ClassFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "fatal action completion")
		flow.applyFatalAuthEffects(ctx, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassScopeBlockingTransient, errclass.ClassActionable:
		if err := flow.applyOrdinaryFailureEffects(ctx, current, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
	}

	return nil, nil
}

func (flow *engineFlow) applyTrialCompletionDecision(
	ctx context.Context,
	trialScopeKey syncdomain.ScopeKey,
	decision *syncdomain.ResultDecision,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) ([]*syncdomain.TrackedAction, error) {
	if decision.Class == errclass.ClassSuperseded {
		flow.noteProbationMemberFinished(trialScopeKey, r.TrialScopeGeneration)
		if err := flow.applySupersededCompletion(ctx, current, r, "trial superseded action completion"); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		if err := flow.rearmOrDiscardScope(ctx, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		return nil, nil
	}

	outcome := syncstore.EvaluateScopeTrialOutcome(trialScopeKey, decision)
	if outcome == syncstore.ScopeTrialOutcomeRearmOrDiscard {
		flow.noteProbationMemberFinished(trialScopeKey, r.TrialScopeGeneration)
	}

	switch outcome {
	case syncstore.ScopeTrialOutcomeRelease:
		dispatched, err := flow.applyTrialReleaseDecision(ctx, current, r, trialScopeKey)
		if err != nil {
			return dispatched, flow.failAfterControlStateError(current, err)
		}
		return dispatched, nil
	case syncstore.ScopeTrialOutcomeShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "trial shutdown action completion")
		return nil, nil
	case syncstore.ScopeTrialOutcomeExtend:
		if err := flow.applyTrialExtendDecision(ctx, current, r, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case syncstore.ScopeTrialOutcomeRearmOrDiscard:
		if err := flow.applyTrialRearmOrDiscardDecision(ctx, current, decision, r, bl, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case syncstore.ScopeTrialOutcomeFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "trial fatal action completion")
		flow.applyFatalAuthEffects(ctx, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	}

	return nil, nil
}

func (flow *engineFlow) applySupersededCompletion(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	depGraphReason string,
) error {
	if err := flow.forgetRemoteItemProvenGone(ctx, r); err != nil {
		return err
	}
	if err := flow.forgetLocalCtimeProvenChanged(ctx, r); err != nil {
		return err
	}
	flow.results.superseded++
	if streak := flow.recordSupersede(r); streak >= supersedeParkThreshold {
		if err := flow.parkWedgedAction(ctx, r, current); err != nil {
			return err
		}
	} else if err := flow.clearRetryWorkOnSuperseded(ctx, r, current); err != nil {
		return err
	}

	actionID := int64(0)
	if r != nil {
		actionID = r.ActionID
	}
	flow.applyCompletedSubtree(current, actionID, depGraphReason)
	flow.signals.markReplanNeeded()

	return nil
}

// forgetRemoteItemProvenGone applies an executor's live 404 to remote truth.
func (flow *engineFlow) forgetRemoteItemProvenGone(ctx context.Context, r *syncdomain.ActionCompletion) error {
	var gone *syncexec.RemoteItemGoneError
	if r == nil || !errors.As(r.Err, &gone) || gone.ItemID == "" || flow.deps.store == nil {
		return nil
	}
	if err := flow.deps.store.ForgetRemoteItem(ctx, gone.ItemID); err != nil {
		return fmt.Errorf("sync: forgetting remote item %s proven gone: %w", gone.ItemID, err)
	}

	return nil
}

// forgetLocalCtimeProvenChanged applies a local delete's content check to
// local truth: the file no longer holds what its baseline row records, so the
// row loses its stored change time and the next scan reads the file instead
// of trusting the row, and plans the change as the local edit it is.
func (flow *engineFlow) forgetLocalCtimeProvenChanged(ctx context.Context, r *syncdomain.ActionCompletion) error {
	var changed *syncexec.LocalContentChangedError
	if r == nil || !errors.As(r.Err, &changed) || flow.deps.store == nil {
		return nil
	}
	if err := flow.deps.store.ForgetBaselineLocalCtime(ctx, changed.Path); err != nil {
		return fmt.Errorf("sync: recording the changed content of %s: %w", changed.Path, err)
	}

	return nil
}

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
) ([]*syncdomain.TrackedAction, error) {
	return flow.applyTrackedActionSuccess(ctx, current, r, "successful action completion")
}

func (flow *engineFlow) applyTrackedActionSuccess(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	depGraphReason string,
) ([]*syncdomain.TrackedAction, error) {
	if flow.sched.Finished(current) {
		flow.noteColdStartSuccess(current)
	}
	flow.results.succeeded++
	flow.clearSupersedeStreak(r)
	var clearErr error
	if current != nil {
		clearErr = flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	} else {
		clearErr = flow.clearRetryWorkOnSuccess(ctx, r)
	}
	if clearErr != nil {
		return nil, clearErr
	}
	if r != nil {
		flow.sched.ObserveSuccess()
	}

	actionID := int64(0)
	switch {
	case current != nil:
		actionID = current.ID
	case r != nil:
		actionID = r.ActionID
	default:
		return nil, nil
	}

	return flow.admitReadyAfterSuccessfulAction(ctx, actionID, depGraphReason)
}

// applyOrdinaryFailureEffects handles post-routing side effects for normal
// worker failures. Trial results intentionally use separate scope-relative
// policy so they do not accidentally mutate the original scope via generic
// failure recording or scope detection.
func (flow *engineFlow) applyOrdinaryFailureEffects(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	decision *syncdomain.ResultDecision,
	r *syncdomain.ActionCompletion,
) error {
	persisted, persistedAs, err := flow.persistFailureWithScopeEffects(ctx, current, decision, r)
	if err != nil {
		return err
	}
	flow.armFailureTimers(persistedAs, persisted)

	flow.recordError(decision, r)
	return nil
}

// authRequirementMarkTimeout bounds recording that the engine's account needs
// sign-in: the catalog write waits for the catalog lock up to that lock's own
// 10 s bound, and the rest is one small file.
const authRequirementMarkTimeout = 15 * time.Second

// persistAndHoldFailure persists a failure's retry_work and holds its action.
// activation, when not nil, is the block scope the failure activates: its
// row is written in the blocked row's transaction.
func (flow *engineFlow) persistAndHoldFailure(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	decision *syncdomain.ResultDecision,
	r *syncdomain.ActionCompletion,
	activation *blockActivation,
) (bool, error) {
	if decision.Persistence != syncdomain.PersistRetryWork {
		return false, nil
	}
	row, err := flow.applyResultPersistence(ctx, decision, r, activation)
	if err != nil {
		return false, err
	}
	flow.holdActionFromPersistedRetryState(current, row)

	return true, nil
}

// persistFailureWithScopeEffects persists a failure's retry_work, holds its
// action, and activates the block scope the failure's evidence calls for. It
// returns whether it persisted, and the decision the work was persisted as.
//
// Scope detection runs before the failure is persisted: when this failure is
// the one that activates a scope (a 429, a 503 with Retry-After, or the
// failure that crosses a sliding window), it is that scope's first blocked
// work, recorded blocked under it and held for the scope's trial. Recorded as
// ordinary retry work first, it left the new scope with no blocked work at
// all, which the persisted invariants forbid (found by the chaos Graph layer,
// D-092): a scope with nothing to trial is dead state. The scope row and the
// blocked row are written in one transaction, the scope row first, since the
// blocked row references it.
func (flow *engineFlow) persistFailureWithScopeEffects(
	ctx context.Context,
	current *syncdomain.TrackedAction,
	decision *syncdomain.ResultDecision,
	r *syncdomain.ActionCompletion,
) (bool, *syncdomain.ResultDecision, error) {
	detected := flow.detectFailureScope(decision, r)
	persistAs := decision
	switch {
	case detected.Block:
		persistAs = decisionBlockedUnder(decision, detected.ScopeKey)
	case decision.RunScopeDetection &&
		decision.Class == errclass.ClassScopeBlockingTransient &&
		!flow.sched.HasScope(decision.ScopeKey):
		// The failure names a scope, but detection has not crossed its
		// window yet (a 507 needs three paths in ten seconds) and no scope
		// is active: there is nothing to block under. Recorded blocked, the
		// row referenced a scope that did not exist, which nothing ever
		// trialed or released (R-2.10.33; live on 2026-11-03, where two
		// quota rows sat blocked across a restart with no scope). It is
		// timed retry work carrying the scope as evidence; the activation a
		// later failure brings blocks it then.
		persistAs = decisionAsScopeEvidence(decision)
	}

	activates := syncdomain.ScopeUpdateResult{}
	switch {
	case detected.Block:
		activates = detected
	case !decision.RunScopeDetection &&
		decision.Class == errclass.ClassScopeBlockingTransient && !decision.ScopeKey.IsZero():
		activates = syncdomain.ScopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ScopeKey.ConditionType(),
		}
	}
	var activation *blockActivation
	if activates.Block && persistAs.Persistence == syncdomain.PersistRetryWork {
		prepared, err := flow.prepareBlockActivation(activates)
		if err != nil {
			return false, persistAs, err
		}
		activation = prepared
	}

	persisted, err := flow.persistAndHoldFailure(ctx, current, persistAs, r, activation)
	if err != nil || !persisted {
		return persisted, persistAs, err
	}
	if activation != nil {
		err = flow.applyBlockActivation(ctx, activation)
	}

	return true, persistAs, err
}

// decisionBlockedUnder is decision as work blocked under scopeKey: the
// failure that activates a scope is that scope's first blocked work.
func decisionBlockedUnder(decision *syncdomain.ResultDecision, scopeKey syncdomain.ScopeKey) *syncdomain.ResultDecision {
	blocked := *decision
	blocked.Class = errclass.ClassScopeBlockingTransient
	blocked.ScopeKey = scopeKey
	blocked.ScopeEvidence = scopeKey

	return &blocked
}

// decisionAsScopeEvidence is decision as timed retry work that records the
// scope it would have blocked under as evidence only: the scope is not
// active, so the row waits on the ordinary retry curve and is blocked when,
// and if, the scope activates.
func decisionAsScopeEvidence(decision *syncdomain.ResultDecision) *syncdomain.ResultDecision {
	evidence := *decision
	evidence.Class = errclass.ClassRetryableTransient
	evidence.ScopeEvidence = decision.ScopeKey
	evidence.ScopeKey = syncdomain.ScopeKey{}

	return &evidence
}

// detectFailureScope feeds a failure that will be persisted as retry work
// into scope detection and returns the verdict.
func (flow *engineFlow) detectFailureScope(
	decision *syncdomain.ResultDecision, r *syncdomain.ActionCompletion,
) syncdomain.ScopeUpdateResult {
	if decision.Persistence != syncdomain.PersistRetryWork || !decision.RunScopeDetection {
		return syncdomain.ScopeUpdateResult{}
	}

	return flow.detectScope(r)
}

func (flow *engineFlow) armFailureTimers(
	decision *syncdomain.ResultDecision,
	persisted bool,
) {
	if decision.Class == errclass.ClassScopeBlockingTransient {
		flow.signals.armTrialTimer()
	}
	if persisted {
		flow.signals.armRetryTimer()
	}
}

// fatalCompletionError is the run-ending error for an action that failed
// because sign-in is required. It is returned, never logged here: the CLI
// renders it once as a sign-in message. Shutdown bookkeeping recognizes it so
// that the same 401 on other in-flight transfers is not reported again.
type fatalCompletionError struct {
	path string
	err  error
}

func (e *fatalCompletionError) Error() string {
	if e.err != nil {
		return fmt.Sprintf("sync: unauthorized action completion for %s: %v", e.path, e.err)
	}

	return fmt.Sprintf("sync: unauthorized action completion for %s", e.path)
}

func (e *fatalCompletionError) Unwrap() error {
	return e.err
}

func fatalResultError(r *syncdomain.ActionCompletion) error {
	return &fatalCompletionError{path: r.Path, err: r.Err}
}

// applyFatalAuthEffects marks the account as needing a sign-in for the first
// fatal 401 of a run. The mark is a catalog update under the config lock,
// whose wait the config package bounds. It records what already happened, so
// it runs on a context detached from the run's cancel: a shutdown that
// arrives with the 401 must not lose the mark.
func (flow *engineFlow) applyFatalAuthEffects(
	ctx context.Context,
	r *syncdomain.ActionCompletion,
	conditionKey syncdomain.ConditionKey,
) {
	// The first 401 of a run marks the account and ends the run; later
	// in-flight transfers that also got a 401 add nothing new.
	if flow.results.fatalAuthRecorded {
		return
	}
	flow.results.fatalAuthRecorded = true

	logFields := flow.summaryLogFields(
		errclass.ClassFatal,
		conditionKey,
		r.Path,
		syncdomain.ScopeKey{},
	)

	// The mark goes on the engine's own account, by canonical ID: another
	// account at the same address keeps its own sign-in state.
	if account := flow.cfg.accountCanonicalID; !account.IsZero() {
		// Recorded even when the run is stopping, so a shutdown does not
		// lose it, under the one detached-wait helper and a bound past the
		// catalog lock's own wait.
		if err := syncexec.RunDetachedStoreAccess(ctx, authRequirementMarkTimeout, func(markCtx context.Context) error {
			return config.MarkAccountAuthRequired(markCtx, flow.cfg.dataDir, account, authstate.ReasonSyncAuthRejected)
		}); err != nil {
			fields := append([]any{}, logFields...)
			fields = append(fields,
				slog.String("account", account.String()),
				slog.Any("error", err),
			)
			flow.deps.logger.Warn("Could not record that sign-in is required; status may not show it until the next sync",
				fields...)
		}
	}

	// Debug only: the returned fatalCompletionError is the user-facing
	// report, printed once by the CLI as a sign-in message.
	flow.deps.logger.Debug("authentication required: sync stopping",
		logFields...,
	)
}

func (flow *engineFlow) applyPublicationMutation(ctx context.Context, ta *syncdomain.TrackedAction) error {
	mutation, err := syncstore.PublicationMutationFromAction(&ta.Action, flow.cfg.driveID)
	if err == nil {
		err = flow.deps.store.CommitMutation(ctx, mutation)
	}
	return err
}

func partitionPublicationFrontier(ready []*syncdomain.TrackedAction) ([]*syncdomain.TrackedAction, []*syncdomain.TrackedAction) {
	concrete := make([]*syncdomain.TrackedAction, 0, len(ready))
	publication := make([]*syncdomain.TrackedAction, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}
		if syncdomain.IsPublicationOnlyActionType(ta.Action.Type) {
			publication = append(publication, ta)
			continue
		}
		concrete = append(concrete, ta)
	}

	return concrete, publication
}

func (flow *engineFlow) failPublicationDrainAction(
	ctx context.Context,
	bl syncdomain.BaselineView,
	current *syncdomain.TrackedAction,
	cause error,
) ([]*syncdomain.TrackedAction, error) {
	completion := syncdomain.ActionCompletionFromTrackedAction(current, nil, cause)
	// The engine runs publication-only actions itself, under the run's
	// context, so it is the one to say whether that run had ended.
	completion.RunEnded = syncdomain.RunEnded(ctx, cause)
	return flow.applyRuntimeCompletionStage(ctx, &completion, bl)
}

func (flow *engineFlow) applyPublicationDrainAction(
	ctx context.Context,
	bl syncdomain.BaselineView,
	current *syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	if err := flow.applyPublicationMutation(ctx, current); err != nil {
		return flow.failPublicationDrainAction(ctx, bl, current, err)
	}

	return flow.completePublicationDrainAction(ctx, current)
}

func (flow *engineFlow) completePublicationDrainAction(
	ctx context.Context,
	current *syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	if current == nil {
		return nil, nil
	}

	return flow.applyTrackedActionSuccess(ctx, current, nil, "successful action completion")
}

// runPublicationDrainStage keeps publication-only actions on the engine/store
// side of the runtime boundary. It durably applies publication work, routes
// publication failures through the same runtime completion stage, and returns
// only the remaining concrete worker frontier. If it returns an error, the
// returned slice contains exact actions the caller still owns and should
// complete as shutdown instead of dispatching. A stopping run never gets here
// (reduceReadyFrontier).
func (flow *engineFlow) runPublicationDrainStage(
	ctx context.Context,
	bl syncdomain.BaselineView,
	ready []*syncdomain.TrackedAction,
) ([]*syncdomain.TrackedAction, error) {
	concrete, publication := partitionPublicationFrontier(ready)
	queue := append([]*syncdomain.TrackedAction(nil), publication...)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		flow.hooks.recordFrontierStep(frontierPublished, current)

		released, err := flow.applyPublicationDrainAction(ctx, bl, current)
		if err != nil {
			return append(concrete, queue...), err
		}

		nextConcrete, nextPublication := partitionPublicationFrontier(released)
		concrete = append(concrete, nextConcrete...)
		queue = append(queue, nextPublication...)
	}

	return concrete, nil
}

// recordRetryWork records a failure's retry_work and returns the row the
// store wrote. activation, when not nil, is the block scope the failure
// activates; the failure must then be blocked under that scope, and its row is
// written with the scope row.
func (flow *engineFlow) recordRetryWork(
	ctx context.Context,
	decision *syncdomain.ResultDecision,
	r *syncdomain.ActionCompletion,
	delayFn func(int) time.Duration,
	activation *blockActivation,
) (*syncdomain.RetryWorkRow, error) {
	scopeKey := decision.RetryWorkScope()
	blocked := flow.retryWorkShouldBeBlocked(decision.Class, scopeKey)
	if activation != nil && (!blocked || activation.update.ScopeKey != scopeKey) {
		return nil, fmt.Errorf("record retry_work for %s: activating scope %s, but the work is not blocked under it",
			r.Path, activation.update.ScopeKey.String())
	}

	if decision.Class == errclass.ClassActionable {
		fields := append(flow.resultLogFields(decision, r),
			slog.String("condition_type", string(decision.ConditionType)),
			slog.String("scope_evidence", decision.ScopeEvidence.String()),
		)
		flow.deps.logger.Debug(
			"execution recorded retry_work for a current-truth condition; observation may suppress the next plan and prune it",
			fields...,
		)
	}

	// One failure is one attempt: blocked work is recorded blocked, never
	// first as timed retry work and then again as blocked, which counted
	// every throttled failure twice.
	actionable := decision.Class == errclass.ClassActionable && decision.ConditionType != ""
	// Read before the failure is recorded below, as the activity feed read it.
	conditionStarts := flow.conditionStartsWith(decision, r)
	var row *syncdomain.RetryWorkRow
	var recErr error
	if blocked {
		row, recErr = flow.retryWork.recordBlocked(ctx, flow.deps.store, syncdomain.RetryWorkKeyForCompletion(r), scopeKey,
			activation.storeRow())
	} else {
		failure := &syncdomain.RetryWorkFailure{
			Work:     syncdomain.RetryWorkKeyForCompletion(r),
			ScopeKey: scopeKey,
		}
		if actionable {
			failure.ConditionType = decision.ConditionType
		}
		row, recErr = flow.retryWork.recordFailure(ctx, flow.deps.store, failure, delayFn)
	}
	if recErr != nil {
		return nil, fmt.Errorf("record retry_work for %s: %w", r.Path, recErr)
	}
	if actionable && !blocked && row.AttemptCount == 1 {
		// Said once, when the condition starts (R-6.6.30): the retries that
		// follow are on the durable curve and change nothing the user can
		// see. Status keeps listing the path under the condition meanwhile.
		// When the activity feed says that the condition starts, sync's
		// output says it to the user, so the log keeps the line at Info
		// rather than warn it a second time; otherwise it warns.
		level := slog.LevelWarn
		if flow.deps.activitySaysConditionStarts(decision, r, conditionStarts) {
			level = slog.LevelInfo
		}
		flow.deps.logger.Log(ctx, level, "Not syncing "+r.Path+": "+r.ErrMsg,
			slog.String("condition_type", string(decision.ConditionType)),
		)
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("condition_type", string(decision.ConditionType)),
		slog.String("error", r.ErrMsg),
		slog.String("scope_evidence", scopeKey.String()),
		slog.Bool("blocked", blocked),
	)
	fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	flow.deps.logger.Debug("retry_work recorded", fields...)

	return row, nil
}

func (flow *engineFlow) retryWorkShouldBeBlocked(
	class errclass.Class,
	scopeKey syncdomain.ScopeKey,
) bool {
	if scopeKey.IsZero() {
		return false
	}
	if class == errclass.ClassScopeBlockingTransient {
		return true
	}
	if class != errclass.ClassRetryableTransient {
		return false
	}

	return flow.sched.HasScope(scopeKey)
}

func (flow *engineFlow) applyResultPersistence(
	ctx context.Context,
	decision *syncdomain.ResultDecision,
	r *syncdomain.ActionCompletion,
	activation *blockActivation,
) (*syncdomain.RetryWorkRow, error) {
	switch decision.Persistence {
	case syncdomain.PersistNone:
		return nil, nil
	case syncdomain.PersistRetryWork:
		return flow.recordRetryWork(ctx, decision, r, retryDelayForDecision(decision), activation)
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}

// durableRecheckDelay is the first wait before an action refused by a
// condition that does not clear by itself is tried again
// (retryDelayForCondition).
const durableRecheckDelay = 30 * time.Second

// retryDelayForDecision is the backoff curve for the retry work a decision
// records. Work refused by a condition only the user can clear
// (ClassActionable: OneDrive refused the name or path, a local folder is not
// writable, the file is too large) does not succeed by being tried sooner,
// so it waits on the durable curve whatever the condition; before, only the
// two conditions named below did, and a folder OneDrive refused as too long
// was retried every few seconds (soak of 2026-11-02).
func retryDelayForDecision(decision *syncdomain.ResultDecision) func(int) time.Duration {
	if decision == nil {
		return retry.ReconcilePolicy().Delay
	}
	if decision.Class == errclass.ClassActionable {
		return durableRecheckRetryDelay
	}

	return retryDelayForCondition(decision.ConditionType)
}

// retryDelayForCondition is the backoff curve for retry work of a condition,
// decided for every IssueType by name (the exhaustive linter holds the switch
// to that), so a new condition is put on a curve on purpose. A transient
// failure follows the reconcile curve, which starts at one second. Two
// conditions do not clear by themselves and wait from durableRecheckDelay,
// doubling to an hour, whatever class records them:
//   - a name collision clears when the user renames one of the two names or
//     the other one goes away online. On the one-second curve it was "held"
//     only while the next pass began within a second of the failure, so a
//     slower pass (over a second under -race) or a watch replan retried the
//     refused create again and again; a rename plans new work that this row
//     does not hold;
//   - a folder too large to verify before its recursive delete on OneDrive
//     clears when the user deletes it online or it shrinks. Every retry
//     relists up to RemoteFolderDeleteListLimit items before it fails again,
//     so on the one-second curve a watch session relisted 10,000 items on
//     every pass.
//
// Every other member clears by itself (a timeout, a conflict, a busy file),
// is held under its block scope until the scope's trial passes (throttling,
// an outage, a full drive or disk), is recorded as ClassActionable, which
// retryDelayForDecision already puts on the durable curve, or never becomes
// retry work (a sign-in failure, which stops the run, and the members only
// observation reports). A failure with no condition is transient. A value
// outside the enum is not known to clear by itself, so it waits on the
// durable curve: a hot retry loop is the failure this curve exists to
// prevent.
func retryDelayForCondition(condition syncdomain.IssueType) func(int) time.Duration {
	switch condition {
	case syncdomain.IssueCaseCollision, syncdomain.IssueRemoteFolderUnverifiable:
		return durableRecheckRetryDelay
	case syncdomain.IssueInvalidFilename,
		syncdomain.IssuePathTooLong,
		syncdomain.IssueFileTooLarge,
		syncdomain.IssueHashError,
		syncdomain.IssueUnauthorized,
		syncdomain.IssueRemoteWriteDenied,
		syncdomain.IssueRemoteReadDenied,
		syncdomain.IssueRemoteRootNotFound,
		syncdomain.IssueQuotaExceeded,
		syncdomain.IssueRateLimited,
		syncdomain.IssueLocalReadDenied,
		syncdomain.IssueLocalWriteDenied,
		syncdomain.IssueRemoteNameTwin,
		syncdomain.IssueDiskFull,
		syncdomain.IssueServiceOutage,
		syncdomain.IssueFileTooLargeForSpace,
		syncdomain.IssueLocalNameTooLong,
		syncdomain.IssueLocalReadOnly,
		syncdomain.IssueDeletesHeld,
		syncdomain.IssueLocalUnsyncableReplacement,
		syncdomain.IssueLocalMountBoundary,
		syncdomain.IssueLocalMountMissing,
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
		syncdomain.IssueTransferURLRejected,
		"":
		return retry.ReconcilePolicy().Delay
	default:
		return durableRecheckRetryDelay
	}
}

func durableRecheckRetryDelay(attempt int) time.Duration {
	delay := durableRecheckDelay
	for range attempt {
		delay *= 2
		if delay >= time.Hour {
			return time.Hour
		}
	}

	return delay
}

// supersedeParkThreshold is how many consecutive identical supersedes of one
// action make it wedged (D-059d). Below it a supersede replans at once and
// keeps no retry work (R-2.8.9), as truth moving on between planning and
// execution should. From it on the action is parked in retry_work with
// exponential backoff: a precondition that can never hold costs one attempt
// per backoff step instead of a hot replan loop in watch mode, the parked
// action shows in status as a retrying item, and the park outlives the run.
const supersedeParkThreshold = supersedeStreakWarnThreshold

const (
	supersedeParkBaseDelay = 30 * time.Second
	supersedeParkMaxDelay  = time.Hour
)

// supersedeParkDelay is the backoff after the attempt-th park (0-based) of
// one wedged action: doubling from the base to a cap.
func supersedeParkDelay(attempt int) time.Duration {
	delay := supersedeParkBaseDelay
	for range attempt {
		delay *= 2
		if delay >= supersedeParkMaxDelay {
			return supersedeParkMaxDelay
		}
	}

	return delay
}

// parkWedgedAction persists a wedged action in retry_work, the durable owner
// of per-action retry timing. The row is keyed by the action, so a replan that
// plans something else prunes it and a success resolves it.
func (flow *engineFlow) parkWedgedAction(ctx context.Context, r *syncdomain.ActionCompletion, current *syncdomain.TrackedAction) error {
	work := syncdomain.RetryWorkKeyForCompletion(r)
	if current != nil {
		work = syncdomain.RetryWorkKeyForAction(&current.Action)
	}
	if _, err := flow.retryWork.recordFailure(ctx, flow.deps.store, &syncdomain.RetryWorkFailure{Work: work}, supersedeParkDelay); err != nil {
		return fmt.Errorf("park wedged action %s: %w", r.Path, err)
	}
	flow.signals.armRetryTimer()

	return nil
}

// supersedeStreakWarnThreshold is how many consecutive supersedes of the same
// action make the stall visible at Warn. One or two are ordinary: truth moved
// on between planning and execution and the replan absorbs it.
const supersedeStreakWarnThreshold = 3

func supersedeStreakKey(r *syncdomain.ActionCompletion) string {
	return r.ActionType.String() + "\x00" + r.OldPath + "\x00" + r.Path
}

// recordSupersede logs every superseded completion at Debug and warns once
// when the same action keeps being superseded, so a precondition that can
// never hold is visible instead of a silent stall.
func (flow *engineFlow) recordSupersede(r *syncdomain.ActionCompletion) int {
	if r == nil || flow.supersedeStreaks == nil {
		return 0
	}
	key := supersedeStreakKey(r)
	flow.supersedeStreaks[key]++
	streak := flow.supersedeStreaks[key]

	reason := r.ErrMsg
	if r.Err != nil {
		reason = r.Err.Error()
	}
	logger := flow.deps.logger
	if logger == nil {
		return streak
	}
	logger.Debug("action superseded; replanning",
		slog.String("action", r.ActionType.String()),
		slog.String("path", r.Path),
		slog.String("old_path", r.OldPath),
		slog.Int("consecutive", streak),
		slog.String("reason", reason),
	)
	if streak == supersedeStreakWarnThreshold {
		// Once per streak: a file that is rewritten continuously also lands
		// here, and it must not warn on every pass.
		// The console shows the path and a plain reason; the raw chain (with
		// item IDs) and the classification travel as internal fields that
		// only the log file and --verbose show.
		attrs := []any{slog.String("path", r.Path)}
		if r.OldPath != "" && r.OldPath != r.Path {
			attrs = append(attrs, slog.String("from", r.OldPath))
		}
		attrs = append(attrs,
			slog.String("reason", "it no longer matches what sync planned"),
			slog.String("action_type", r.ActionType.String()),
			slog.Int("supersede_streak", streak),
			slog.String("error_detail", reason),
		)
		logger.Warn("A planned change keeps going stale before it can run; sync parks it and retries with backoff", attrs...)
	}

	return streak
}

// pruneSupersedeStreaks forgets the streaks of actions a new plan no longer
// contains: a plan that dropped the action means its supersedes were not
// consecutive, so it cannot be wedged.
func (flow *engineFlow) pruneSupersedeStreaks(plan *syncdomain.ActionPlan) {
	if len(flow.supersedeStreaks) == 0 {
		return
	}
	planned := map[string]bool{}
	if plan != nil {
		for i := range plan.Actions {
			action := &plan.Actions[i]
			planned[supersedeStreakKey(&syncdomain.ActionCompletion{ActionType: action.Type, OldPath: action.OldPath, Path: action.Path})] = true
		}
	}
	for key := range flow.supersedeStreaks {
		if !planned[key] {
			delete(flow.supersedeStreaks, key)
		}
	}
}

func (flow *engineFlow) clearSupersedeStreak(r *syncdomain.ActionCompletion) {
	if r == nil || flow.supersedeStreaks == nil {
		return
	}
	delete(flow.supersedeStreaks, supersedeStreakKey(r))
}
