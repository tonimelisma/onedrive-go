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
)

func (flow *engineFlow) failAfterControlStateError(current *trackedAction, err error) error {
	flow.sched.markFinished(current)
	return err
}

// applyRuntimeCompletionStage is the runtime completion boundary: classify the
// finished exact action, apply the resulting mutation/persistence decision, and
// then release any due held work back into the ready frontier.
func (flow *engineFlow) applyRuntimeCompletionStage(
	ctx context.Context,
	r *actionCompletion,
	bl *Baseline,
) ([]*trackedAction, error) {
	if r == nil {
		return nil, nil
	}

	decision := classifyResult(r)
	current := flow.sched.trackedActionForCompletion(r)
	flow.engine.emitActivity(r, decision.Class)
	if decision.Class != errclass.ClassSuperseded {
		// The action ran: whatever came of it, its supersedes were not
		// consecutive, so a wedge streak (D-059d) starts over.
		flow.clearSupersedeStreak(r)
	}

	var (
		dispatched []*trackedAction
		err        error
	)
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
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
	decision *resultDecision,
	current *trackedAction,
	r *actionCompletion,
	bl *Baseline,
) ([]*trackedAction, error) {
	if handled, err := flow.maybeHandlePermissionFailure(ctx, decision, current, r, bl); handled {
		return nil, err
	}

	switch decision.Class {
	case errclass.ClassInvalid:
		if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		flow.sched.markFinished(current)
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
	trialScopeKey ScopeKey,
	decision *resultDecision,
	current *trackedAction,
	r *actionCompletion,
	bl *Baseline,
) ([]*trackedAction, error) {
	if decision.Class == errclass.ClassSuperseded {
		if err := flow.applySupersededCompletion(ctx, current, r, "trial superseded action completion"); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		if err := flow.rearmOrDiscardScope(ctx, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		return nil, nil
	}

	switch evaluateScopeTrialOutcome(trialScopeKey, decision) {
	case scopeTrialOutcomeRelease:
		dispatched, err := flow.applyTrialReleaseDecision(ctx, current, r, trialScopeKey)
		if err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		return dispatched, nil
	case scopeTrialOutcomeShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "trial shutdown action completion")
		return nil, nil
	case scopeTrialOutcomeExtend:
		if err := flow.applyTrialExtendDecision(ctx, current, r, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeRearmOrDiscard:
		if err := flow.applyTrialRearmOrDiscardDecision(ctx, current, decision, r, bl, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "trial fatal action completion")
		flow.applyFatalAuthEffects(ctx, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	}

	return nil, nil
}

func (flow *engineFlow) applySupersededCompletion(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	depGraphReason string,
) error {
	if err := flow.forgetRemoteItemProvenGone(ctx, r); err != nil {
		return err
	}
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
func (flow *engineFlow) forgetRemoteItemProvenGone(ctx context.Context, r *actionCompletion) error {
	var gone *remoteItemGoneError
	if r == nil || !errors.As(r.Err, &gone) || gone.itemID == "" || flow.deps.store == nil {
		return nil
	}
	if err := flow.deps.store.ForgetRemoteItem(ctx, gone.itemID); err != nil {
		return fmt.Errorf("sync: forgetting remote item %s proven gone: %w", gone.itemID, err)
	}

	return nil
}

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
) ([]*trackedAction, error) {
	return flow.applyTrackedActionSuccess(ctx, current, r, "successful action completion")
}

func (flow *engineFlow) applyTrackedActionSuccess(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	depGraphReason string,
) ([]*trackedAction, error) {
	flow.sched.markFinished(current)
	flow.results.succeeded++
	flow.clearSupersedeStreak(r)
	if current != nil {
		flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	} else {
		flow.clearRetryWorkOnSuccess(ctx, r)
	}
	if flow.scopes.state != nil && r != nil {
		flow.scopes.state.RecordSuccess()
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
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
) error {
	persisted, persistedAs, err := flow.persistFailureWithScopeEffects(ctx, current, decision, r)
	if err != nil {
		return err
	}
	flow.armFailureTimers(persistedAs, persisted)

	flow.recordError(decision, r)
	return nil
}

func (flow *engineFlow) persistAndHoldFailure(
	ctx context.Context,
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
) (bool, error) {
	if decision.Persistence != persistRetryWork {
		return false, nil
	}
	if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
		return false, err
	}
	if err := flow.holdActionFromPersistedRetryState(current, retryWorkKeyForCompletion(r)); err != nil {
		return false, err
	}

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
// D-092): a scope with nothing to trial is dead state.
func (flow *engineFlow) persistFailureWithScopeEffects(
	ctx context.Context,
	current *trackedAction,
	decision *resultDecision,
	r *actionCompletion,
) (bool, *resultDecision, error) {
	detected := flow.detectFailureScope(decision, r)
	persistAs := decision
	if detected.Block {
		persistAs = decisionBlockedUnder(decision, detected.ScopeKey)
	}

	persisted, err := flow.persistAndHoldFailure(ctx, current, persistAs, r)
	if err != nil || !persisted {
		return persisted, persistAs, err
	}

	switch {
	case detected.Block:
		err = flow.applyBlockScope(ctx, detected)
	case !decision.RunScopeDetection &&
		decision.Class == errclass.ClassScopeBlockingTransient && !decision.ScopeKey.IsZero():
		err = flow.applyBlockScope(ctx, scopeUpdateResult{
			Block:         true,
			ScopeKey:      decision.ScopeKey,
			ConditionType: decision.ScopeKey.ConditionType(),
		})
	}

	return true, persistAs, err
}

// decisionBlockedUnder is decision as work blocked under scopeKey: the
// failure that activates a scope is that scope's first blocked work.
func decisionBlockedUnder(decision *resultDecision, scopeKey ScopeKey) *resultDecision {
	blocked := *decision
	blocked.Class = errclass.ClassScopeBlockingTransient
	blocked.ScopeKey = scopeKey
	blocked.ScopeEvidence = scopeKey

	return &blocked
}

// detectFailureScope feeds a failure that will be persisted as retry work
// into scope detection and returns the verdict.
func (flow *engineFlow) detectFailureScope(decision *resultDecision, r *actionCompletion) scopeUpdateResult {
	if decision.Persistence != persistRetryWork || !decision.RunScopeDetection {
		return scopeUpdateResult{}
	}

	return flow.detectScope(r)
}

func (flow *engineFlow) armFailureTimers(
	decision *resultDecision,
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

func fatalResultError(r *actionCompletion) error {
	return &fatalCompletionError{path: r.Path, err: r.Err}
}

func (flow *engineFlow) applyFatalAuthEffects(
	ctx context.Context,
	r *actionCompletion,
	conditionKey ConditionKey,
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
		ScopeKey{},
	)

	if flow.engine.permHandler != nil && flow.engine.permHandler.accountEmail != "" {
		if err := config.MarkAccountAuthRequired(
			flow.engine.dataDir,
			flow.engine.permHandler.accountEmail,
			authstate.ReasonSyncAuthRejected,
		); err != nil {
			fields := append([]any{}, logFields...)
			fields = append(fields,
				slog.String("account", flow.engine.permHandler.accountEmail),
				slog.String("error", err.Error()),
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

	_ = ctx
}

func isPublicationOnlyActionType(actionType actionType) bool {
	switch actionType {
	case ActionBaselineUpdate, ActionCleanup:
		return true
	case ActionDownload,
		ActionUpload,
		ActionLocalDelete,
		ActionRemoteDelete,
		ActionLocalMove,
		ActionRemoteMove,
		ActionFolderCreate,
		ActionConflictCopy:
		return false
	}

	panic(fmt.Sprintf("unknown action type %d", actionType))
}

func (flow *engineFlow) applyPublicationMutation(ctx context.Context, ta *trackedAction) error {
	mutation, err := publicationMutationFromAction(&ta.Action, flow.engine.driveID)
	if err == nil {
		err = flow.deps.store.CommitMutation(ctx, mutation)
	}
	return err
}

func partitionPublicationFrontier(ready []*trackedAction) ([]*trackedAction, []*trackedAction) {
	concrete := make([]*trackedAction, 0, len(ready))
	publication := make([]*trackedAction, 0, len(ready))

	for _, ta := range ready {
		if ta == nil {
			continue
		}
		if isPublicationOnlyActionType(ta.Action.Type) {
			publication = append(publication, ta)
			continue
		}
		concrete = append(concrete, ta)
	}

	return concrete, publication
}

func (flow *engineFlow) failPublicationDrainAction(
	ctx context.Context,
	bl *Baseline,
	current *trackedAction,
	cause error,
) ([]*trackedAction, error) {
	completion := actionCompletionFromTrackedAction(current, nil, cause)
	return flow.applyRuntimeCompletionStage(ctx, &completion, bl)
}

func (flow *engineFlow) applyPublicationDrainAction(
	ctx context.Context,
	bl *Baseline,
	current *trackedAction,
) ([]*trackedAction, error) {
	if err := flow.applyPublicationMutation(ctx, current); err != nil {
		return flow.failPublicationDrainAction(ctx, bl, current, err)
	}

	return flow.completePublicationDrainAction(ctx, current)
}

func (flow *engineFlow) completePublicationDrainAction(
	ctx context.Context,
	current *trackedAction,
) ([]*trackedAction, error) {
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
// complete as shutdown instead of dispatching.
func (flow *engineFlow) runPublicationDrainStage(
	ctx context.Context,
	bl *Baseline,
	ready []*trackedAction,
) ([]*trackedAction, error) {
	concrete, publication := partitionPublicationFrontier(ready)
	queue := append([]*trackedAction(nil), publication...)

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

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

func (flow *engineFlow) recordRetryWork(
	ctx context.Context,
	decision *resultDecision,
	r *actionCompletion,
	delayFn func(int) time.Duration,
) error {
	scopeKey := decision.ScopeEvidence
	blocked := flow.retryWorkShouldBeBlocked(decision.Class, scopeKey)

	if decision.Class == errclass.ClassActionable {
		fields := append(flow.resultLogFields(decision, r),
			slog.String("condition_type", decision.ConditionType),
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
	var row *RetryWorkRow
	var recErr error
	if blocked {
		row, recErr = flow.deps.store.RecordBlockedRetryWork(ctx, retryWorkKeyForCompletion(r), scopeKey)
	} else {
		row, recErr = flow.deps.store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
			Work:     retryWorkKeyForCompletion(r),
			ScopeKey: scopeKey,
		}, delayFn)
	}
	if recErr != nil {
		return fmt.Errorf("record retry_work for %s: %w", r.Path, recErr)
	}

	fields := append(flow.resultLogFields(decision, r),
		slog.String("condition_type", decision.ConditionType),
		slog.String("error", r.ErrMsg),
		slog.String("scope_evidence", scopeKey.String()),
		slog.Bool("blocked", blocked),
	)
	if row == nil {
		return fmt.Errorf("record retry_work for %s: missing persisted row", r.Path)
	}
	flow.retries.rowsByKey[row.WorkKey()] = *row
	fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	flow.deps.logger.Debug("retry_work recorded", fields...)

	return nil
}

func (flow *engineFlow) retryWorkShouldBeBlocked(
	class errclass.Class,
	scopeKey ScopeKey,
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

	return flow.scopes.hasActiveScope(scopeKey)
}

func (flow *engineFlow) applyResultPersistence(
	ctx context.Context,
	decision *resultDecision,
	r *actionCompletion,
) error {
	switch decision.Persistence {
	case persistNone:
		return nil
	case persistRetryWork:
		return flow.recordRetryWork(ctx, decision, r, retryDelayForCondition(decision.ConditionType))
	default:
		panic(fmt.Sprintf("unknown failure persistence mode %d", decision.Persistence))
	}
}

// durableCollisionRecheckDelay is the first wait before a create refused by
// a name collision is tried again (retryDelayForCondition).
const durableCollisionRecheckDelay = 30 * time.Second

// retryDelayForCondition is the backoff curve for retry work of a condition.
// A transient failure follows the reconcile curve, which starts at one
// second. A name collision does not clear by itself: the user renames one of
// the two names, or the other one goes away online. On the one-second curve
// it was "held" only while the next pass began within a second of the
// failure, so a slower pass (over a second under -race) or a watch replan
// retried the refused create again and again. It waits from
// durableCollisionRecheckDelay, doubling to an hour; a rename plans new work
// that this row does not hold.
func retryDelayForCondition(condition string) func(int) time.Duration {
	if condition == issueCaseCollision {
		return durableCollisionRetryDelay
	}

	return retry.ReconcilePolicy().Delay
}

func durableCollisionRetryDelay(attempt int) time.Duration {
	delay := durableCollisionRecheckDelay
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
func (flow *engineFlow) parkWedgedAction(ctx context.Context, r *actionCompletion, current *trackedAction) error {
	work := retryWorkKeyForCompletion(r)
	if current != nil {
		work = retryWorkKeyForAction(&current.Action)
	}
	row, err := flow.deps.store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{Work: work}, supersedeParkDelay)
	if err != nil {
		return fmt.Errorf("park wedged action %s: %w", r.Path, err)
	}
	if row == nil {
		return fmt.Errorf("park wedged action %s: missing persisted row", r.Path)
	}
	if flow.retries.rowsByKey != nil {
		flow.retries.rowsByKey[row.WorkKey()] = *row
	}
	flow.signals.armRetryTimer()

	return nil
}

// supersedeStreakWarnThreshold is how many consecutive supersedes of the same
// action make the stall visible at Warn. One or two are ordinary: truth moved
// on between planning and execution and the replan absorbs it.
const supersedeStreakWarnThreshold = 3

func supersedeStreakKey(r *actionCompletion) string {
	return r.ActionType.String() + "\x00" + r.OldPath + "\x00" + r.Path
}

// recordSupersede logs every superseded completion at Debug and warns once
// when the same action keeps being superseded, so a precondition that can
// never hold is visible instead of a silent stall.
func (flow *engineFlow) recordSupersede(r *actionCompletion) int {
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
func (flow *engineFlow) pruneSupersedeStreaks(plan *actionPlan) {
	if len(flow.supersedeStreaks) == 0 {
		return
	}
	planned := map[string]bool{}
	if plan != nil {
		for i := range plan.Actions {
			action := &plan.Actions[i]
			planned[supersedeStreakKey(&actionCompletion{ActionType: action.Type, OldPath: action.OldPath, Path: action.Path})] = true
		}
	}
	for key := range flow.supersedeStreaks {
		if !planned[key] {
			delete(flow.supersedeStreaks, key)
		}
	}
}

func (flow *engineFlow) clearSupersedeStreak(r *actionCompletion) {
	if r == nil || flow.supersedeStreaks == nil {
		return
	}
	delete(flow.supersedeStreaks, supersedeStreakKey(r))
}
