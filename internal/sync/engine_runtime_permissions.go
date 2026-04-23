package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

func shouldHandleRemote403Permission(r *ActionCompletion) bool {
	return r != nil && r.HTTPStatus == http.StatusForbidden && remoteWriteScopeBlocksAction(r.ActionType)
}

func shouldHandleLocalPermission(r *ActionCompletion) bool {
	return r != nil && errors.Is(r.Err, os.ErrPermission)
}

func (flow *engineFlow) maybeHandlePermissionFailure(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	switch {
	case shouldHandleRemote403Permission(r):
		return flow.handleRemote403PermissionFailure(ctx, watch, decision, current, r, bl)
	case shouldHandleLocalPermission(r):
		return flow.handleLocalPermissionFailure(ctx, watch, decision, current, r)
	default:
		return false, nil
	}
}

func (flow *engineFlow) handleRemote403PermissionFailure(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	if bl == nil || !flow.engine.permHandler.HasPermChecker() {
		return false, nil
	}

	evidence := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType)
	if !evidence.Matched() {
		return false, nil
	}

	if err := flow.applyPermissionFailureEvidence(ctx, watch, current, r, evidence, true); err != nil {
		return true, err
	}
	if watch != nil {
		watch.armHeldTimers()
	}
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) handleLocalPermissionFailure(
	ctx context.Context,
	watch *watchRuntime,
	decision *ResultDecision,
	current *TrackedAction,
	r *ActionCompletion,
) (bool, error) {
	evidence := flow.engine.permHandler.handleLocalPermission(ctx, r)
	if !evidence.Matched() {
		return false, nil
	}

	if err := flow.applyPermissionFailureEvidence(ctx, watch, current, r, evidence, false); err != nil {
		return true, err
	}
	if watch != nil {
		watch.armHeldTimers()
	}
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) applyPermissionFailureEvidence(
	ctx context.Context,
	watch *watchRuntime,
	current *TrackedAction,
	r *ActionCompletion,
	evidence PermissionEvidence,
	remote bool,
) error {
	work := retryWorkKeyForCompletion(r)

	switch evidence.Kind {
	case permissionEvidenceNone:
		return fmt.Errorf("apply permission failure for %s: missing permission evidence", work.Path)
	case permissionEvidenceKnownActiveBoundary:
		if blocking := flow.findBlockingScope(current); !blocking.IsZero() {
			return flow.holdActionUnderScope(ctx, watch, current, r, blocking)
		}
		return nil
	case permissionEvidenceFileDenied:
		if err := flow.recordPermissionRetryWork(ctx, work, ScopeKey{}, evidence.IssueType); err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
	case permissionEvidenceBoundaryDenied:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		if err := flow.recordPermissionBlockedRetry(ctx, work, scopeKey); err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
		if scopeKey.PersistsInBlockScopes() {
			if err := flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      scopeKey,
				ConditionType: scopeKey.ConditionType(),
			}); err != nil {
				return err
			}
		}
		if remote {
			flow.logRemotePermissionBoundary(scopeKey, evidence)
		} else {
			flow.logLocalPermissionBoundary(scopeKey, evidence)
		}
	default:
		panic(fmt.Sprintf("unknown permission evidence kind %d", evidence.Kind))
	}

	if evidence.Kind != permissionEvidenceKnownActiveBoundary {
		if err := flow.holdActionFromPersistedRetryState(current, work); err != nil {
			return err
		}
	}

	return nil
}

func (flow *engineFlow) applyTrialPermissionReclassification(
	ctx context.Context,
	watch *watchRuntime,
	r *ActionCompletion,
	bl *Baseline,
) (bool, error) {
	evidence, handled := flow.permissionEvidenceForCompletion(ctx, r, bl)
	if !handled {
		return false, nil
	}
	if !evidence.Matched() {
		return false, nil
	}

	work := retryWorkKeyForCompletion(r)
	if err := flow.clearBlockedRetryWorkForScope(ctx, work, r.TrialScopeKey); err != nil {
		return false, err
	}

	if err := flow.applyTrialPermissionEvidence(ctx, watch, work, r.TrialScopeKey, evidence); err != nil {
		return false, err
	}
	if watch != nil {
		watch.armHeldTimers()
	}

	return true, nil
}

func (flow *engineFlow) permissionEvidenceForCompletion(
	ctx context.Context,
	r *ActionCompletion,
	bl *Baseline,
) (PermissionEvidence, bool) {
	switch {
	case shouldHandleRemote403Permission(r):
		if bl == nil || !flow.engine.permHandler.HasPermChecker() {
			return PermissionEvidence{}, false
		}
		return flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType), true
	case shouldHandleLocalPermission(r):
		return flow.engine.permHandler.handleLocalPermission(ctx, r), true
	default:
		return PermissionEvidence{}, false
	}
}

func (flow *engineFlow) applyTrialPermissionEvidence(
	ctx context.Context,
	watch *watchRuntime,
	work RetryWorkKey,
	trialScopeKey ScopeKey,
	evidence PermissionEvidence,
) error {
	switch evidence.Kind {
	case permissionEvidenceNone:
		return fmt.Errorf("apply trial permission failure for %s: missing permission evidence", work.Path)
	case permissionEvidenceKnownActiveBoundary:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		if scopeKey.IsZero() {
			scopeKey = trialScopeKey
		}
		if err := flow.recordPermissionBlockedRetry(ctx, work, scopeKey); err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
	case permissionEvidenceFileDenied:
		if err := flow.recordPermissionRetryWork(ctx, work, ScopeKey{}, evidence.IssueType); err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
	case permissionEvidenceBoundaryDenied:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		if err := flow.recordPermissionBlockedRetry(ctx, work, scopeKey); err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
		if scopeKey.PersistsInBlockScopes() {
			if err := flow.applyBlockScope(ctx, watch, ScopeUpdateResult{
				Block:         true,
				ScopeKey:      scopeKey,
				ConditionType: scopeKey.ConditionType(),
			}); err != nil {
				return err
			}
		}
	default:
		panic(fmt.Sprintf("unknown permission evidence kind %d", evidence.Kind))
	}

	return nil
}

func scopeKeyForPermissionEvidence(evidence PermissionEvidence) ScopeKey {
	switch evidence.IssueType {
	case IssueLocalReadDenied:
		return SKPermLocalRead(evidence.BoundaryPath)
	case IssueLocalWriteDenied:
		return SKPermLocalWrite(evidence.BoundaryPath)
	case IssueRemoteWriteDenied:
		return SKPermRemoteWrite(evidence.BoundaryPath)
	default:
		return ScopeKey{}
	}
}

func (flow *engineFlow) logRemotePermissionBoundary(
	scopeKey ScopeKey,
	evidence PermissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.engine.logger.Info("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (flow *engineFlow) logLocalPermissionBoundary(
	scopeKey ScopeKey,
	evidence PermissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.engine.logger.Info("local permission denied: directory blocked", fields...)
}

func (flow *engineFlow) recordPermissionRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
	conditionType string,
) error {
	row, err := flow.engine.baseline.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Work:     work,
		ScopeKey: scopeKey,
	}, retry.ReconcilePolicy().Delay)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("missing persisted row")
	}
	flow.retryRowsByKey[row.WorkKey()] = *row
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		ConditionKeyForStoredCondition(conditionType, scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", conditionType),
	)
	flow.engine.logger.Debug("retry_work permission failure recorded", fields...)
	return nil
}

func (flow *engineFlow) recordPermissionBlockedRetry(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	row, err := flow.engine.baseline.RecordBlockedRetryWork(ctx, work, scopeKey)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("missing persisted row")
	}
	flow.retryRowsByKey[row.WorkKey()] = *row
	fields := append(flow.summaryLogFields(
		errclass.ClassBlockScopeingTransient,
		ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", scopeKey.ConditionType()),
	)
	flow.engine.logger.Debug("retry_work permission failure recorded", fields...)
	return nil
}
