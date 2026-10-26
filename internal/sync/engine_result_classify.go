package sync

import (
	"context"
	"errors"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

const (
	resultSuccess    = errclass.ClassSuccess
	resultSuperseded = errclass.ClassSuperseded
	resultRequeue    = errclass.ClassRetryableTransient
	resultBlockScope = errclass.ClassScopeBlockingTransient
	resultSkip       = errclass.ClassActionable
	resultShutdown   = errclass.ClassShutdown
	resultFatal      = errclass.ClassFatal
)

// classifyResult is a pure function that maps a ActionCompletion to a
// single ResultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *actionCompletion) resultDecision {
	if r.Success {
		return withRuntimeSummary(&resultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     trialHintRelease,
		})
	}

	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return withRuntimeSummary(&resultDecision{
			Class:     resultShutdown,
			TrialHint: trialHintShutdown,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

func classifyHTTPResult(r *actionCompletion) (resultDecision, bool) {
	answer := r.serverAnswer()
	scopeEvidence := deriveScopeKey(r)
	conditionType := issueTypeForResult(r)

	switch answer {
	case serverAnswerNone:
		return resultDecision{}, false
	case serverAnswerUnauthorized:
		return withRuntimeSummary(&resultDecision{
			Class:         resultFatal,
			Persistence:   persistNone,
			TrialHint:     trialHintFatal,
			ConditionType: conditionType,
		}), true
	case serverAnswerThrottled, serverAnswerQuota:
		return withRuntimeSummary(&resultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case serverAnswerServiceOutage,
		serverAnswerRequestTimeout,
		serverAnswerTransientConflict,
		serverAnswerTransientNotFound,
		serverAnswerLocked,
		serverAnswerTransferURLRejected:
		return withRuntimeSummary(&resultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case serverAnswerForbidden,
		serverAnswerUnsupported,
		serverAnswerPathTooLong,
		serverAnswerTimestampOutOfRange,
		serverAnswerMalware,
		serverAnswerOther:
		// Actionable per item: retrying the same request gets the same
		// answer, and none of these say anything about a wider scope.
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		}), true
	}

	return resultDecision{}, false
}

func classifyLocalResult(r *actionCompletion) resultDecision {
	conditionType := issueTypeForResult(r)

	if errors.Is(r.Err, errActionPreconditionChanged) {
		return withRuntimeSummary(&resultDecision{
			Class:       resultSuperseded,
			Persistence: persistNone,
			TrialHint:   trialHintReclassify,
		})
	}

	switch driveops.ClassifyLocalError(r.Err) {
	case driveops.LocalErrorNoSpace:
		// Every write on a full filesystem fails the same way, so one
		// disk-wide scope replaces N per-file "unexpected" failures whether the
		// min_free_space pre-check or a real ENOSPC/EDQUOT write found it.
		return withRuntimeSummary(&resultDecision{
			Class:         resultBlockScope,
			ScopeKey:      SKDiskLocal(),
			ScopeEvidence: SKDiskLocal(),
			Persistence:   persistRetryWork,
			TrialHint:     trialHintExtendOnMatchingScope,
			ConditionType: conditionType,
		})
	case driveops.LocalErrorBusy:
		return withRuntimeSummary(&resultDecision{
			Class:         resultRequeue,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	case driveops.LocalErrorOther,
		driveops.LocalErrorReadOnly,
		driveops.LocalErrorFileTooLarge,
		driveops.LocalErrorCrossDevice,
		driveops.LocalErrorNameTooLong:
		// Read-only filesystems become a boundary scope through the local
		// permission probe (engine_runtime_permissions.go); the per-item kinds
		// are actionable rows named by issueTypeForFilesystemResult.
	}

	return withRuntimeSummary(&resultDecision{
		Class:         resultSkip,
		Persistence:   persistRetryWork,
		TrialHint:     trialHintReclassify,
		ConditionType: conditionType,
	})
}

func withRuntimeSummary(decision *resultDecision) resultDecision {
	decision.ConditionKey = conditionKeyForRuntimeResult(decision.Class, decision.ConditionType)
	return *decision
}

// deriveScopeKey maps an action completion to its typed scope key. Delegates to
// scopeKeyForResult, the single source of truth for server answer -> scope
// key. Returns the zero-value ScopeKey for answers that are not scope
// evidence.
func deriveScopeKey(r *actionCompletion) ScopeKey {
	return scopeKeyForResult(r.serverAnswer(), r.DriveID)
}

func issueTypeForResult(r *actionCompletion) string {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *actionCompletion) (string, bool) {
	if r == nil {
		return "", false
	}

	answer := r.serverAnswer()
	if answer == serverAnswerForbidden {
		return issueTypeForForbiddenResult(r), true
	}

	issueType, ok := serverAnswerIssueTypes()[answer]
	return issueType, ok
}

// serverAnswerIssueTypes names the issue each server answer records. A
// function, not a package-level map, so the table cannot be mutated.
// serverAnswerNone, serverAnswerForbidden (probe-dependent) and
// serverAnswerOther have no fixed issue.
func serverAnswerIssueTypes() map[serverAnswer]string {
	return map[serverAnswer]string{
		serverAnswerPathTooLong:         issuePathTooLong,
		serverAnswerUnauthorized:        IssueUnauthorized,
		serverAnswerThrottled:           issueRateLimited,
		serverAnswerQuota:               IssueQuotaExceeded,
		serverAnswerUnsupported:         issueUnsupportedOperation,
		serverAnswerTimestampOutOfRange: issueTimestampOutOfRange,
		serverAnswerServiceOutage:       issueServiceOutage,
		serverAnswerRequestTimeout:      "request_timeout",
		serverAnswerTransientConflict:   "transient_conflict",
		serverAnswerTransientNotFound:   "transient_not_found",
		serverAnswerLocked:              "resource_locked",
		serverAnswerTransferURLRejected: "transfer_url_rejected",
		serverAnswerMalware:             issueMalwareDetected,
	}
}

func issueTypeForForbiddenResult(r *actionCompletion) string {
	switch effectiveRemotePermissionCapability(r) {
	case permissionCapabilityRemoteRead:
		return issueRemoteReadDenied
	case permissionCapabilityUnknown,
		permissionCapabilityLocalRead,
		permissionCapabilityLocalWrite,
		permissionCapabilityRemoteWrite:
		return IssueRemoteWriteDenied
	default:
		return IssueRemoteWriteDenied
	}
}

func issueTypeForFilesystemResult(r *actionCompletion) (string, bool) {
	if r == nil {
		return "", false
	}

	err := r.Err
	switch {
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return issueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return issueFileTooLarge, true
	}

	switch driveops.ClassifyLocalError(err) {
	case driveops.LocalErrorNoSpace:
		return issueDiskFull, true
	case driveops.LocalErrorReadOnly:
		return issueLocalReadOnly, true
	case driveops.LocalErrorFileTooLarge:
		return issueLocalFileTooLarge, true
	case driveops.LocalErrorBusy:
		return issueLocalBusy, true
	case driveops.LocalErrorCrossDevice:
		return issueLocalCrossDevice, true
	case driveops.LocalErrorNameTooLong:
		// OneDrive accepts names longer than the 255 bytes a Linux or
		// macOS filesystem can store; without a named issue each download
		// retried forever as an "unexpected" failure.
		return issueLocalNameTooLong, true
	case driveops.LocalErrorOther:
	}

	if errors.Is(err, os.ErrPermission) {
		return issueTypeForLocalPermissionResult(r), true
	}

	return "", false
}

func issueTypeForLocalPermissionResult(r *actionCompletion) string {
	switch effectiveLocalPermissionCapability(r) {
	case permissionCapabilityLocalRead:
		return issueLocalReadDenied
	case permissionCapabilityUnknown,
		permissionCapabilityLocalWrite,
		permissionCapabilityRemoteRead,
		permissionCapabilityRemoteWrite:
		return issueLocalWriteDenied
	default:
		return issueLocalWriteDenied
	}
}

func effectiveRemotePermissionCapability(r *actionCompletion) permissionCapability {
	if r == nil {
		return permissionCapabilityUnknown
	}
	if r.FailureCapability == permissionCapabilityRemoteRead || r.FailureCapability == permissionCapabilityRemoteWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return permissionCapabilityUnknown
	}

	switch r.ActionType {
	case ActionDownload:
		return permissionCapabilityRemoteRead
	case ActionUpload, ActionRemoteDelete, ActionRemoteMove, ActionFolderCreate:
		return permissionCapabilityRemoteWrite
	case ActionConflictCopy, ActionLocalDelete, ActionLocalMove, ActionBaselineUpdate, ActionCleanup:
		return permissionCapabilityUnknown
	default:
		return permissionCapabilityUnknown
	}
}

func effectiveLocalPermissionCapability(r *actionCompletion) permissionCapability {
	if r == nil {
		return permissionCapabilityUnknown
	}
	if r.FailureCapability == permissionCapabilityLocalRead || r.FailureCapability == permissionCapabilityLocalWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return permissionCapabilityUnknown
	}

	switch r.ActionType {
	case ActionUpload:
		return permissionCapabilityLocalRead
	case ActionDownload, ActionLocalDelete, ActionLocalMove, ActionFolderCreate, ActionConflictCopy, ActionCleanup:
		return permissionCapabilityLocalWrite
	case ActionRemoteDelete, ActionRemoteMove, ActionBaselineUpdate:
		return permissionCapabilityUnknown
	default:
		return permissionCapabilityUnknown
	}
}

func hasPermissionActionContext(r *actionCompletion) bool {
	if r == nil {
		return false
	}

	return r.ActionID != 0 || r.Path != "" || !r.DriveID.IsZero()
}
