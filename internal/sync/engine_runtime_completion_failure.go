package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// applyOrdinaryFailureEffects handles post-routing side effects for normal
// worker failures. Trial results intentionally use separate scope-relative
// policy so they do not accidentally mutate the original scope via generic
// failure recording or scope detection.
func (flow *engineFlow) applyOrdinaryFailureEffects(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
) error {
	persisted, err := flow.persistAndHoldFailure(ctx, current, decision, r)
	if err != nil {
		return err
	}
	if err := flow.applyPersistedFailureScopeEffects(ctx, watch, decision, r, persisted); err != nil {
		return err
	}
	flow.armFailureTimers(watch, decision, persisted)

	flow.recordError(decision, r)
	return nil
}

func (flow *engineFlow) persistAndHoldFailure(
	ctx context.Context,
	current *TrackedAction,
	decision *ResultDecision,
	r *ActionCompletion,
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

func (flow *engineFlow) applyPersistedFailureScopeEffects(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	r *ActionCompletion,
	persisted bool,
) error {
	if !persisted {
		return nil
	}
	if decision.RunScopeDetection {
		return flow.feedScopeDetection(ctx, watch, r)
	}
	if decision.Class != errclass.ClassBlockScopeingTransient || decision.ScopeKey.IsZero() {
		return nil
	}

	return flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
		Block:         true,
		ScopeKey:      decision.ScopeKey,
		ConditionType: decision.ScopeKey.ConditionType(),
	})
}

func (flow *engineFlow) armFailureTimers(
	watch *watchRuntime,
	decision *ResultDecision,
	persisted bool,
) {
	if watch == nil {
		return
	}
	if decision.Class == errclass.ClassBlockScopeingTransient {
		watch.armTrialTimer()
	}
	if persisted {
		watch.armRetryTimer()
	}
}

func (flow *engineFlow) recordRetryWork(
	ctx context.Context,
	decision *ResultDecision,
	r *ActionCompletion,
	delayFn func(int) time.Duration,
) error {
	scopeKey := decision.ScopeEvidence
	blocked := flow.retryWorkShouldBeBlocked(decision.Class, scopeKey)

	if decision.Class == errclass.ClassActionable {
		fields := append(flow.resultLogFields(decision, r),
			slog.String("condition_type", decision.ConditionType),
			slog.String("scope_evidence", decision.ScopeEvidence.String()),
		)
		flow.engine.logger.Debug(
			"execution recorded retry_work for a current-truth condition; observation may suppress the next plan and prune it",
			fields...,
		)
	}

	row, recErr := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Path:          r.Path,
		OldPath:       r.OldPath,
		ActionType:    r.ActionType,
		ConditionType: decision.ConditionType,
		ScopeKey:      scopeKey,
		LastError:     r.ErrMsg,
		HTTPStatus:    r.HTTPStatus,
		Blocked:       blocked,
	}, delayFn)
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
	flow.retryRowsByKey[retryWorkKeyForCompletion(r)] = *row
	fields = append(fields, slog.Int("attempt_count", row.AttemptCount))
	flow.engine.logger.Debug("retry_work recorded", fields...)

	return nil
}

func (flow *engineFlow) retryWorkShouldBeBlocked(
	class errclass.Class,
	scopeKey ScopeKey,
) bool {
	if scopeKey.IsZero() {
		return false
	}
	if class == errclass.ClassBlockScopeingTransient {
		return true
	}
	if class != errclass.ClassRetryableTransient {
		return false
	}

	return flow.hasActiveScope(scopeKey)
}
