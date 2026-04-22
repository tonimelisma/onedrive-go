package sync

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/tonimelisma/onedrive-go/internal/authstate"
	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

func (flow *engineFlow) completeDepGraphAction(actionID int64, reason string) []*TrackedAction {
	if flow.depGraph == nil {
		panic(fmt.Sprintf("dep_graph: complete action %d during %s with nil graph", actionID, reason))
	}

	ready, ok := flow.depGraph.Complete(actionID)
	if !ok {
		panic(fmt.Sprintf("dep_graph: complete unknown action ID %d during %s", actionID, reason))
	}

	return ready
}

func (flow *engineFlow) failAfterControlStateError(current *TrackedAction, err error) error {
	flow.markFinished(current)
	return err
}

// processActionCompletion is the runtime completion boundary: classify the
// finished exact action, apply the resulting mutation/persistence decision, and
// then release any due held work back into the ready frontier.
func (flow *engineFlow) processActionCompletion(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	if r == nil {
		return nil, nil
	}

	decision := classifyResult(r)
	current := flow.trackedActionForCompletion(r)

	var (
		dispatched []*TrackedAction
		err        error
	)
	if r.IsTrial && !r.TrialScopeKey.IsZero() {
		dispatched, err = flow.processTrialDecision(ctx, watch, r.TrialScopeKey, &decision, current, r, bl)
	} else {
		dispatched, err = flow.processNormalDecision(ctx, watch, &decision, current, r, bl)
	}
	if err != nil {
		return dispatched, err
	}

	dueHeld, err := flow.drainDueHeldWorkNow(ctx, watch)
	if err != nil {
		return dispatched, err
	}
	return append(dispatched, dueHeld...), nil
}

func (flow *engineFlow) applySuccessEffects(ctx context.Context, r *ActionCompletion) {
	flow.succeeded++
	flow.clearRetryWorkOnSuccess(ctx, r)
	if flow.scopeState != nil {
		flow.scopeState.RecordSuccess(r)
	}
}

func (flow *engineFlow) applyCompletionSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
) ([]*TrackedAction, error) {
	flow.markFinished(current)
	flow.applySuccessEffects(ctx, r)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, r.ActionID, "successful action completion")
}

func (flow *engineFlow) applyPublicationSuccess(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
) ([]*TrackedAction, error) {
	if current == nil {
		return nil, nil
	}

	flow.markFinished(current)
	flow.succeeded++
	flow.clearRetryWorkOnActionSuccess(ctx, &current.Action)
	return flow.admitReadyAfterSuccessfulAction(ctx, watch, current.ID, "publication action completion")
}

func (flow *engineFlow) applyCompletedSubtree(
	current *TrackedAction,
	actionID int64,
	reason string,
) {
	flow.markFinished(current)
	if current == nil {
		return
	}

	ready := flow.completeDepGraphAction(actionID, reason)
	flow.completeSubtree(ready)
}

// completeSubtree silently completes all transitive dependents without
// recording failures. Used for shutdown.
func (flow *engineFlow) completeSubtree(ready []*TrackedAction) {
	seen := make(map[int64]bool)
	queue := ready

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]

		if seen[current.ID] {
			continue
		}
		seen[current.ID] = true

		next := flow.completeDepGraphAction(current.ID, "completeSubtree")
		queue = append(queue, next...)
	}
}

func (flow *engineFlow) processNormalDecision(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	if handled, err := flow.maybeHandlePermissionOutcome(ctx, watch, decision, current, r, bl); handled {
		return nil, err
	}

	switch decision.Class {
	case errclass.ClassInvalid:
		if err := flow.applyResultPersistence(ctx, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		flow.markFinished(current)
		return nil, fmt.Errorf("classify action completion: invalid failure class")
	case errclass.ClassSuccess:
		dispatched, err := flow.applyCompletionSuccess(ctx, watch, current, r)
		if err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		return dispatched, nil
	case errclass.ClassShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "shutdown action completion")
		return nil, nil
	case errclass.ClassFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "fatal action completion")
		flow.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	case errclass.ClassRetryableTransient, errclass.ClassBlockScopeingTransient, errclass.ClassActionable:
		if err := flow.applyOrdinaryFailureEffects(ctx, watch, current, decision, r); err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
	}

	return nil, nil
}

func (flow *engineFlow) processTrialDecision(
	ctx context.Context,
	watch *watchRuntime,
	trialScopeKey ScopeKey,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) ([]*TrackedAction, error) {
	switch evaluateScopeTrialOutcome(trialScopeKey, decision) {
	case scopeTrialOutcomeRelease:
		dispatched, err := flow.applyTrialReleaseDecision(ctx, watch, current, r, trialScopeKey)
		if err != nil {
			return nil, flow.failAfterControlStateError(current, err)
		}
		return dispatched, nil
	case scopeTrialOutcomeShutdown:
		flow.applyCompletedSubtree(current, r.ActionID, "trial shutdown action completion")
		return nil, nil
	case scopeTrialOutcomeExtend:
		if err := flow.applyTrialExtendDecision(ctx, watch, current, r, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeRearmOrDiscard:
		if err := flow.applyTrialRearmOrDiscardDecision(ctx, watch, current, decision, r, bl, trialScopeKey); err != nil {
			return nil, flow.failAfterControlStateError(nil, err)
		}
		flow.recordError(decision, r)
		return nil, nil
	case scopeTrialOutcomeFatal:
		flow.applyCompletedSubtree(current, r.ActionID, "trial fatal action completion")
		flow.applyFatalAuthEffects(ctx, watch, r, decision.ConditionKey)
		flow.recordError(decision, r)
		return nil, fatalResultError(r)
	}

	return nil, nil
}

func (flow *engineFlow) trackedActionForCompletion(r *ActionCompletion) *TrackedAction {
	if r == nil || flow.depGraph == nil {
		return nil
	}

	ta, ok := flow.depGraph.Get(r.ActionID)
	if !ok {
		return nil
	}

	return ta
}

func (flow *engineFlow) admitReadyAfterSuccessfulAction(
	ctx context.Context,
	watch *watchRuntime,
	actionID int64,
	reason string,
) ([]*TrackedAction, error) {
	ready := flow.completeDepGraphAction(actionID, reason)
	return flow.admitReady(ctx, watch, ready)
}

func fatalResultError(r *ActionCompletion) error {
	if r.Err != nil {
		return fmt.Errorf("sync: unauthorized action completion for %s: %w", r.Path, r.Err)
	}

	return fmt.Errorf("sync: unauthorized action completion for %s", r.Path)
}

func (flow *engineFlow) applyFatalAuthEffects(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	conditionKey ConditionKey,
) {
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
			flow.engine.logger.Warn("fatal unauthorized: failed to persist catalog auth requirement", fields...)
		}
	}

	flow.engine.logger.Error("authentication required: sync stopping",
		logFields...,
	)

	_ = ctx
	_ = watch
}
