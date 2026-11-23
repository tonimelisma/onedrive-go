package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// shouldHandleRemote403Permission selects completions the remote permission
// probe owns: a write refused as a plain permission answer. It keys on the
// server-answer table, not the bare 403, so a full drive (403
// quotaLimitReached) goes to the quota scope and blocked content (403
// malwareDetected) stays a per-file issue instead of a probed write boundary.
func shouldHandleRemote403Permission(r *syncdomain.ActionCompletion) bool {
	return r != nil && r.ServerAnswer() == syncdomain.ServerAnswerForbidden && syncdomain.RemoteWriteScopeBlocksAction(r.ActionType)
}

// shouldHandleLocalPermission selects completions the local probe owns:
// permission denials, and EROFS, which is not os.ErrPermission but is the
// same "this subtree cannot be written" fact at a filesystem boundary.
func shouldHandleLocalPermission(r *syncdomain.ActionCompletion) bool {
	return r != nil && (errors.Is(r.Err, os.ErrPermission) ||
		driveops.ClassifyLocalError(r.Err) == driveops.LocalErrorReadOnly)
}

// maybeHandlePermissionFailure hands a failure to the permission probes when
// its error is a permission answer. A superseded completion never is, whatever
// its error chain also holds: the stale verdict decides (classifyResult), and
// probing it recorded permission retry work or a write-denied scope for work
// the replan replaces. A stopping run probes nothing (isStoppingRun): it asks
// OneDrive and the disk nothing new, records the failure as the retry work it
// is, and leaves the probe to the next sync.
func (flow *engineFlow) maybeHandlePermissionFailure(
	ctx context.Context,
	decision *syncdomain.ResultDecision,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) (bool, error) {
	switch {
	case decision.Class == errclass.ClassSuperseded, isStoppingRun(ctx):
		return false, nil
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
	decision *syncdomain.ResultDecision,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) (bool, error) {
	if bl == nil || flow.perms.permChecker == nil {
		return false, nil
	}

	evidence, err := flow.perms.handle403(ctx, bl, r.Path, r.ActionType)
	if err != nil {
		return false, err
	}
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
	decision *syncdomain.ResultDecision,
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
) (bool, error) {
	evidence := flow.perms.handleLocalPermission(ctx, r)
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
	current *syncdomain.TrackedAction,
	r *syncdomain.ActionCompletion,
	evidence permissionEvidence,
	remote bool,
) error {
	work := syncdomain.RetryWorkKeyForCompletion(r)

	var row *syncdomain.RetryWorkRow
	switch evidence.Kind {
	case permissionEvidenceNone:
		return fmt.Errorf("apply permission failure for %s: missing permission evidence", work.Path)
	case permissionEvidenceKnownActiveBoundary:
		if blocking := flow.sched.BlockingScope(current); !blocking.IsZero() {
			return flow.holdActionUnderScope(ctx, current, r, blocking)
		}
		return nil
	case permissionEvidenceFileDenied:
		recorded, err := flow.recordPermissionRetryWork(ctx, work, syncdomain.ScopeKey{}, evidence.IssueType, r.ErrMsg)
		if err != nil {
			return fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
		row = recorded
	case permissionEvidenceBoundaryDenied:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		recorded, err := flow.recordPermissionBoundaryDenied(ctx, work, scopeKey)
		if err != nil {
			return err
		}
		row = recorded
		if remote {
			flow.logRemotePermissionBoundary(scopeKey, evidence)
		} else {
			flow.logLocalPermissionBoundary(scopeKey, evidence)
		}
	default:
		panic(fmt.Sprintf("unknown permission evidence kind %d", evidence.Kind))
	}

	flow.holdActionFromPersistedRetryState(current, row)

	return nil
}

// applyTrialPermissionReclassification hands a scope trial's failure to the
// permission probes when its error is a permission answer, and returns the
// retry_work row the store wrote when the probes' evidence decided
// it, or nil when it did not. A stopping run probes nothing for a trial either
// (isStoppingRun): the trial falls back to the retry work it is.
func (flow *engineFlow) applyTrialPermissionReclassification(
	ctx context.Context,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) (*syncdomain.RetryWorkRow, error) {
	if isStoppingRun(ctx) {
		return nil, nil
	}

	evidence, handled, err := flow.permissionEvidenceForCompletion(ctx, r, bl)
	if err != nil {
		return nil, err
	}
	if !handled {
		return nil, nil
	}
	if evidence.Kind == permissionEvidenceNone {
		return nil, nil
	}

	work := syncdomain.RetryWorkKeyForCompletion(r)
	if err := flow.clearBlockedRetryWorkForScope(ctx, work, r.TrialScopeKey); err != nil {
		return nil, err
	}

	row, err := flow.applyTrialPermissionEvidence(ctx, work, r.TrialScopeKey, evidence)
	if err != nil {
		return nil, err
	}
	flow.signals.armHeldTimers()

	return row, nil
}

func (flow *engineFlow) permissionEvidenceForCompletion(
	ctx context.Context,
	r *syncdomain.ActionCompletion,
	bl syncdomain.BaselineView,
) (permissionEvidence, bool, error) {
	switch {
	case shouldHandleRemote403Permission(r):
		if bl == nil || flow.perms.permChecker == nil {
			return permissionEvidence{}, false, nil
		}
		evidence, err := flow.perms.handle403(ctx, bl, r.Path, r.ActionType)
		if err != nil {
			return permissionEvidence{}, true, err
		}
		return evidence, true, nil
	case shouldHandleLocalPermission(r):
		return flow.perms.handleLocalPermission(ctx, r), true, nil
	default:
		return permissionEvidence{}, false, nil
	}
}

func (flow *engineFlow) applyTrialPermissionEvidence(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	trialScopeKey syncdomain.ScopeKey,
	evidence permissionEvidence,
) (*syncdomain.RetryWorkRow, error) {
	switch evidence.Kind {
	case permissionEvidenceNone:
		return nil, fmt.Errorf("apply trial permission failure for %s: missing permission evidence", work.Path)
	case permissionEvidenceKnownActiveBoundary:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		if scopeKey.IsZero() {
			scopeKey = trialScopeKey
		}
		row, err := flow.recordPermissionBlockedRetry(ctx, work, scopeKey, nil)
		if err != nil {
			return nil, fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
		return row, nil
	case permissionEvidenceFileDenied:
		row, err := flow.recordPermissionRetryWork(ctx, work, syncdomain.ScopeKey{}, evidence.IssueType, evidence.LastError)
		if err != nil {
			return nil, fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
		}
		return row, nil
	case permissionEvidenceBoundaryDenied:
		scopeKey := scopeKeyForPermissionEvidence(evidence)
		return flow.recordPermissionBoundaryDenied(ctx, work, scopeKey)
	default:
		panic(fmt.Sprintf("unknown permission evidence kind %d", evidence.Kind))
	}
}

// scopeKeyForPermissionEvidence is the boundary scope a permission probe's
// evidence names, decided for every IssueType by name (the exhaustive linter
// holds the switch to that), so a new permission condition gets its scope on
// purpose. The zero scope means "no boundary scope": a remote read denial is a
// boundary observation finds rather than probe evidence, and every other
// member, like a value outside the enum, is not a permission condition. The
// store refuses blocked work without a scope, so boundary evidence of such a
// type fails loudly instead of being parked nowhere, and a scope trial falls
// back to its own scope (applyTrialPermissionEvidence).
func scopeKeyForPermissionEvidence(evidence permissionEvidence) syncdomain.ScopeKey {
	switch evidence.IssueType {
	case syncdomain.IssueLocalReadDenied:
		return syncdomain.SKPermLocalRead(evidence.BoundaryPath)
	case syncdomain.IssueLocalWriteDenied:
		return syncdomain.SKPermLocalWrite(evidence.BoundaryPath)
	case syncdomain.IssueLocalReadOnly:
		return syncdomain.SKLocalReadOnly(evidence.BoundaryPath)
	case syncdomain.IssueRemoteWriteDenied:
		return syncdomain.SKPermRemoteWrite(evidence.BoundaryPath)
	case syncdomain.IssueRemoteReadDenied,
		syncdomain.IssueRemoteRootNotFound,
		syncdomain.IssueInvalidFilename,
		syncdomain.IssuePathTooLong,
		syncdomain.IssueFileTooLarge,
		syncdomain.IssueHashError,
		syncdomain.IssueUnauthorized,
		syncdomain.IssueQuotaExceeded,
		syncdomain.IssueRateLimited,
		syncdomain.IssueCaseCollision,
		syncdomain.IssueRemoteNameTwin,
		syncdomain.IssueDiskFull,
		syncdomain.IssueServiceOutage,
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
		return syncdomain.ScopeKey{}
	default:
		return syncdomain.ScopeKey{}
	}
}

func (flow *engineFlow) logRemotePermissionBoundary(
	scopeKey syncdomain.ScopeKey,
	evidence permissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		syncdomain.ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.deps.logger.Debug("handle403: read-only remote boundary detected, writes suppressed recursively", fields...)
}

func (flow *engineFlow) logLocalPermissionBoundary(
	scopeKey syncdomain.ScopeKey,
	evidence permissionEvidence,
) {
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		syncdomain.ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		evidence.TriggerPath,
		scopeKey,
	),
		slog.String("boundary", evidence.BoundaryPath),
		slog.String("trigger_path", evidence.TriggerPath),
	)
	flow.deps.logger.Info("local permission denied: directory blocked", fields...)
}

// recordPermissionRetryWork records a per-file permission denial the way the
// executor's other user-clearable refusals are recorded (R-2.10.71): with
// its condition, on the durable curve, said once when it starts.
func (flow *engineFlow) recordPermissionRetryWork(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
	conditionType syncdomain.IssueType,
	errMsg string,
) (*syncdomain.RetryWorkRow, error) {
	row, err := flow.retryWork.recordFailure(ctx, flow.deps.store, &syncdomain.RetryWorkFailure{
		Work:          work,
		ScopeKey:      scopeKey,
		ConditionType: conditionType,
	}, durableRecheckRetryDelay)
	if err != nil {
		return nil, err
	}
	if row.AttemptCount == 1 {
		flow.deps.logger.Warn("Not syncing "+work.Path+": "+errMsg,
			slog.String("condition_type", string(conditionType)),
		)
	}
	fields := append(flow.summaryLogFields(
		errclass.ClassActionable,
		syncdomain.ConditionKeyForStoredCondition(conditionType, scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", string(conditionType)),
	)
	flow.deps.logger.Debug("retry_work permission failure recorded", fields...)
	return row, nil
}

// recordPermissionBoundaryDenied records work blocked under the boundary a
// permission probe found. A write boundary is kept in block_scopes: its scope
// row is written with the blocked row, in one transaction, and the scope is
// then activated. A read boundary is observation's (it has no block_scopes
// row), so the work is only recorded blocked under it.
func (flow *engineFlow) recordPermissionBoundaryDenied(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
) (*syncdomain.RetryWorkRow, error) {
	var activation *blockActivation
	if scopeKey.PersistsInBlockScopes() {
		prepared, err := flow.prepareBlockActivation(syncdomain.ScopeUpdateResult{
			Block:         true,
			ScopeKey:      scopeKey,
			ConditionType: scopeKey.ConditionType(),
		})
		if err != nil {
			return nil, err
		}
		activation = prepared
	}
	row, err := flow.recordPermissionBlockedRetry(ctx, work, scopeKey, activation)
	if err != nil {
		return nil, fmt.Errorf("record permission retry_work for %s: %w", work.Path, err)
	}
	if activation == nil {
		return row, nil
	}
	if err := flow.applyBlockActivation(ctx, activation); err != nil {
		return nil, err
	}

	return row, nil
}

func (flow *engineFlow) recordPermissionBlockedRetry(
	ctx context.Context,
	work syncdomain.RetryWorkKey,
	scopeKey syncdomain.ScopeKey,
	activation *blockActivation,
) (*syncdomain.RetryWorkRow, error) {
	row, err := flow.retryWork.recordBlocked(ctx, flow.deps.store, work, scopeKey, activation.storeRow())
	if err != nil {
		return nil, err
	}
	fields := append(flow.summaryLogFields(
		errclass.ClassScopeBlockingTransient,
		syncdomain.ConditionKeyForStoredCondition(scopeKey.ConditionType(), scopeKey),
		work.Path,
		scopeKey,
	),
		slog.String("condition_type", string(scopeKey.ConditionType())),
	)
	flow.deps.logger.Debug("retry_work permission failure recorded", fields...)
	return row, nil
}
