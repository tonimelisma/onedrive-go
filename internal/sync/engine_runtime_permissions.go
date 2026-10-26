package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// shouldHandleRemote403Permission selects completions the remote permission
// probe owns: a write refused as a plain permission answer. It keys on the
// server-answer table, not the bare 403, so a full drive (403
// quotaLimitReached) goes to the quota scope and blocked content (403
// malwareDetected) stays a per-file issue instead of a probed write boundary.
func shouldHandleRemote403Permission(r *actionCompletion) bool {
	return r != nil && r.serverAnswer() == serverAnswerForbidden && remoteWriteScopeBlocksAction(r.ActionType)
}

// shouldHandleLocalPermission selects completions the local probe owns:
// permission denials, and EROFS, which is not os.ErrPermission but is the
// same "this subtree cannot be written" fact at a filesystem boundary.
func shouldHandleLocalPermission(r *actionCompletion) bool {
	return r != nil && (errors.Is(r.Err, os.ErrPermission) ||
		driveops.ClassifyLocalError(r.Err) == driveops.LocalErrorReadOnly)
}

func (flow *engineFlow) maybeHandlePermissionFailure(
	ctx context.Context,
	decision *resultDecision,
	current *trackedAction,
	r *actionCompletion,
	bl *Baseline,
) (bool, error) {
	switch {
	case shouldHandleRemote403Permission(r):
		return flow.handleRemote403PermissionFailure(ctx, decision, current, r, bl)
	case shouldHandleLocalPermission(r):
		return flow.handleLocalPermissionFailure(ctx, decision, current, r)
	default:
		return false, nil
	}
}

func (flow *engineFlow) handleRemote403PermissionFailure(
	ctx context.Context,
	decision *resultDecision,
	current *trackedAction,
	r *actionCompletion,
	bl *Baseline,
) (bool, error) {
	if bl == nil || flow.engine.permHandler.permChecker == nil {
		return false, nil
	}

	evidence := flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType)
	if evidence.Kind == permissionEvidenceNone {
		return false, nil
	}

	if err := flow.applyPermissionFailureEvidence(ctx, current, r, evidence, true); err != nil {
		return true, err
	}
	flow.signals.armHeldTimers()
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) handleLocalPermissionFailure(
	ctx context.Context,
	decision *resultDecision,
	current *trackedAction,
	r *actionCompletion,
) (bool, error) {
	evidence := flow.engine.permHandler.handleLocalPermission(ctx, r)
	if evidence.Kind == permissionEvidenceNone {
		return false, nil
	}

	if err := flow.applyPermissionFailureEvidence(ctx, current, r, evidence, false); err != nil {
		return true, err
	}
	flow.signals.armHeldTimers()
	flow.recordError(decision, r)

	return true, nil
}

func (flow *engineFlow) applyPermissionFailureEvidence(
	ctx context.Context,
	current *trackedAction,
	r *actionCompletion,
	evidence permissionEvidence,
	remote bool,
) error {
	work := retryWorkKeyForCompletion(r)

	switch evidence.Kind {
	case permissionEvidenceNone:
		return fmt.Errorf("apply permission failure for %s: missing permission evidence", work.Path)
	case permissionEvidenceKnownActiveBoundary:
		if blocking := flow.scopes.findBlockingScope(current); !blocking.IsZero() {
			return flow.holdActionUnderScope(ctx, current, r, blocking)
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
			if err := flow.applyBlockScope(ctx, scopeUpdateResult{
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
	r *actionCompletion,
	bl *Baseline,
) (bool, error) {
	evidence, handled := flow.permissionEvidenceForCompletion(ctx, r, bl)
	if !handled {
		return false, nil
	}
	if evidence.Kind == permissionEvidenceNone {
		return false, nil
	}

	work := retryWorkKeyForCompletion(r)
	if err := flow.clearBlockedRetryWorkForScope(ctx, work, r.TrialScopeKey); err != nil {
		return false, err
	}

	if err := flow.applyTrialPermissionEvidence(ctx, work, r.TrialScopeKey, evidence); err != nil {
		return false, err
	}
	flow.signals.armHeldTimers()

	return true, nil
}

func (flow *engineFlow) permissionEvidenceForCompletion(
	ctx context.Context,
	r *actionCompletion,
	bl *Baseline,
) (permissionEvidence, bool) {
	switch {
	case shouldHandleRemote403Permission(r):
		if bl == nil || flow.engine.permHandler.permChecker == nil {
			return permissionEvidence{}, false
		}
		return flow.engine.permHandler.handle403(ctx, bl, r.Path, r.ActionType), true
	case shouldHandleLocalPermission(r):
		return flow.engine.permHandler.handleLocalPermission(ctx, r), true
	default:
		return permissionEvidence{}, false
	}
}

func (flow *engineFlow) applyTrialPermissionEvidence(
	ctx context.Context,
	work RetryWorkKey,
	trialScopeKey ScopeKey,
	evidence permissionEvidence,
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
			if err := flow.applyBlockScope(ctx, scopeUpdateResult{
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

func scopeKeyForPermissionEvidence(evidence permissionEvidence) ScopeKey {
	switch evidence.IssueType {
	case issueLocalReadDenied:
		return sKPermLocalRead(evidence.BoundaryPath)
	case issueLocalWriteDenied:
		return SKPermLocalWrite(evidence.BoundaryPath)
	case issueLocalReadOnly:
		return SKLocalReadOnly(evidence.BoundaryPath)
	case IssueRemoteWriteDenied:
		return SKPermRemoteWrite(evidence.BoundaryPath)
	default:
		return ScopeKey{}
	}
}

func (flow *engineFlow) logRemotePermissionBoundary(
	scopeKey ScopeKey,
	evidence permissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.deps.logger.Debug("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (flow *engineFlow) logLocalPermissionBoundary(
	scopeKey ScopeKey,
	evidence permissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.deps.logger.Info("local permission denied: directory blocked", fields...)
}

func (flow *engineFlow) recordPermissionRetryWork(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
	conditionType string,
) error {
	row, err := flow.deps.store.RecordRetryWorkFailure(ctx, &RetryWorkFailure{
		Work:     work,
		ScopeKey: scopeKey,
	}, retry.ReconcilePolicy().Delay)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("sync store returned no retry_work row")
	}
	flow.retries.rowsByKey[row.WorkKey()] = *row
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		conditionKeyForStoredCondition(conditionType, scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", conditionType),
	)
	flow.deps.logger.Debug("retry_work permission failure recorded", fields...)
	return nil
}

func (flow *engineFlow) recordPermissionBlockedRetry(
	ctx context.Context,
	work RetryWorkKey,
	scopeKey ScopeKey,
) error {
	row, err := flow.deps.store.RecordBlockedRetryWork(ctx, work, scopeKey)
	if err != nil {
		return err
	}
	if row == nil {
		return errors.New("sync store returned no retry_work row")
	}
	flow.retries.rowsByKey[row.WorkKey()] = *row
	fields := append(flow.summaryLogFields(
		errclass.ClassScopeBlockingTransient,
		conditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", scopeKey.ConditionType()),
	)
	flow.deps.logger.Debug("retry_work permission failure recorded", fields...)
	return nil
}
