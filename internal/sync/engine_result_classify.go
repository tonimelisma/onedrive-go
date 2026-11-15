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

// classifyResult is a pure function that maps an actionCompletion to a
// single resultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *actionCompletion) resultDecision {
	if r.Success {
		return withRuntimeSummary(&resultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     trialHintRelease,
		})
	}

	// Shutdown is a fact about the run, never the shape of an error: only an
	// action whose run had ended is a shutdown, and then only for the
	// cancellation's own error. A separate failure that landed during the stop
	// is classified like any other, and so is a store access that outlasted
	// its own bound: its deadline is the engine's limit on its database, which
	// the stop does not set, and the record it failed to write is lost.
	if r.RunEnded && isCancellationEcho(r.Err) && !errors.Is(r.Err, errStoreAccessTimedOut) {
		return withRuntimeSummary(&resultDecision{
			Class:     resultShutdown,
			TrialHint: trialHintShutdown,
		})
	}

	// A request that timed out while the run goes on is the service's silence:
	// retry work and the outage evidence an answered 5xx is, by the scope its
	// server answer names (serverAnswerServiceOutage). Its issue names the
	// timeout. One that lands once the run has ended is retry work but no
	// evidence, whatever its shape: a stop is never read as the service's
	// silence.
	if isRequestTimeout(r.Err) {
		if r.RunEnded {
			return withRuntimeSummary(&resultDecision{
				Class:         resultRequeue,
				Persistence:   persistRetryWork,
				TrialHint:     trialHintReclassify,
				ConditionType: issueRequestTimeout,
			})
		}

		return retryWithScopeEvidence(deriveScopeKey(r), issueRequestTimeout)
	}

	// A create refused because OneDrive holds a differently spelled sibling
	// of the same name: the user has to rename one, so it is a durable
	// name-collision condition with backoff, not a 409 retried every pass.
	var nameTaken *remoteNameTakenError
	if errors.As(r.Err, &nameTaken) {
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: issueCaseCollision,
		})
	}

	// No cancel the engine makes reaches an action while its run goes on: the
	// per-action cancel lands after the action returned, and the pool and pass
	// cancels end the worker's run. A cancel that arrives anyway came from
	// below the engine without a reason it names, so the action is tried again
	// rather than dropped (and the engine says so, isLiveRunCancel).
	if isLiveRunCancel(r) {
		return withRuntimeSummary(&resultDecision{
			Class:       resultRequeue,
			Persistence: persistRetryWork,
			TrialHint:   trialHintReclassify,
		})
	}

	// Before any server answer: a stale precondition is the executor's verdict
	// that the plan no longer holds, and the Graph error it may wrap (a
	// create's 409, a conditional overwrite's 412) is the evidence it read,
	// not a failure to retry. Classified by that status first, a stale 409
	// would be held as actionable retry work and a stale 412 as
	// transient_conflict, instead of being replanned at once.
	if errors.Is(r.Err, errActionPreconditionChanged) {
		return withRuntimeSummary(&resultDecision{
			Class:       resultSuperseded,
			Persistence: persistNone,
			TrialHint:   trialHintReclassify,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

// isLiveRunCancel reports an action canceled while its run went on: something
// narrower than the run canceled it.
func isLiveRunCancel(r *actionCompletion) bool {
	return !r.RunEnded && errors.Is(r.Err, context.Canceled)
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
		return retryWithScopeEvidence(scopeEvidence, conditionType), true
	case serverAnswerForbidden,
		serverAnswerUnsupported,
		serverAnswerPathTooLong,
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

// retryWithScopeEvidence is the decision for a transient failure that is
// timed retry work and the evidence of scopeEvidence (zero for none): scope
// detection counts it, and a trial that fails with it keeps that scope.
func retryWithScopeEvidence(scopeEvidence ScopeKey, conditionType IssueType) resultDecision {
	return withRuntimeSummary(&resultDecision{
		Class:             resultRequeue,
		ScopeEvidence:     scopeEvidence,
		Persistence:       persistRetryWork,
		RunScopeDetection: true,
		TrialHint:         trialHintExtendOnMatchingScope,
		ConditionType:     conditionType,
	})
}

func classifyLocalResult(r *actionCompletion) resultDecision {
	conditionType := issueTypeForResult(r)

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

func issueTypeForResult(r *actionCompletion) IssueType {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *actionCompletion) (IssueType, bool) {
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
func serverAnswerIssueTypes() map[serverAnswer]IssueType {
	return map[serverAnswer]IssueType{
		serverAnswerPathTooLong:         issuePathTooLong,
		serverAnswerUnauthorized:        IssueUnauthorized,
		serverAnswerThrottled:           issueRateLimited,
		serverAnswerQuota:               IssueQuotaExceeded,
		serverAnswerUnsupported:         issueUnsupportedOperation,
		serverAnswerServiceOutage:       issueServiceOutage,
		serverAnswerRequestTimeout:      issueRequestTimeout,
		serverAnswerTransientConflict:   issueTransientConflict,
		serverAnswerTransientNotFound:   issueTransientNotFound,
		serverAnswerLocked:              issueResourceLocked,
		serverAnswerTransferURLRejected: issueTransferURLRejected,
		serverAnswerMalware:             issueMalwareDetected,
	}
}

func issueTypeForForbiddenResult(r *actionCompletion) IssueType {
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

func issueTypeForFilesystemResult(r *actionCompletion) (IssueType, bool) {
	if r == nil {
		return "", false
	}

	err := r.Err
	if issue, ok := issueTypeForSentinelResult(err); ok {
		return issue, true
	}
	switch {
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return issueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return issueFileTooLarge, true
	case errors.Is(err, driveops.ErrUploadVerificationFailed):
		return issueUploadVerification, true
	case errors.Is(err, driveops.ErrDownloadIntegrity):
		return issueDownloadIntegrity, true
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
		// would retry forever as an "unexpected" failure.
		return issueLocalNameTooLong, true
	case driveops.LocalErrorOther:
	}

	if errors.Is(err, os.ErrPermission) {
		return issueTypeForLocalPermissionResult(r), true
	}

	return "", false
}

// issueTypeForSentinelResult maps the sync package's own durable-condition
// errors.
func issueTypeForSentinelResult(err error) (IssueType, bool) {
	switch {
	case errors.Is(err, errDatabaseInUse):
		return issueLocalDatabaseOpen, true
	case errors.Is(err, errRemoteFolderTooLargeToVerify):
		return issueRemoteFolderUnverifiable, true
	default:
		return "", false
	}
}

func issueTypeForLocalPermissionResult(r *actionCompletion) IssueType {
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
		panic(outsideTheEnum(r.ActionType))
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
		panic(outsideTheEnum(r.ActionType))
	}
}

func hasPermissionActionContext(r *actionCompletion) bool {
	if r == nil {
		return false
	}

	return r.ActionID != 0 || r.Path != "" || !r.DriveID.IsZero()
}
