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

// classifyResult is a pure function that maps an ActionCompletion to a
// single ResultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *ActionCompletion) ResultDecision {
	if r.Success {
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     TrialHintRelease,
		})
	}

	// Shutdown is a fact about the run, never the shape of an error: only an
	// action whose run had ended is a shutdown, and then only for the
	// cancellation's own error. A separate failure that landed during the stop
	// is classified like any other, and so is a store access that outlasted
	// its own bound: its deadline is the engine's limit on its database, which
	// the stop does not set, and the record it failed to write is lost.
	if r.RunEnded && IsCancellationEcho(r.Err) && !errors.Is(r.Err, ErrStoreAccessTimedOut) {
		return withRuntimeSummary(&ResultDecision{
			Class:     resultShutdown,
			TrialHint: TrialHintShutdown,
		})
	}

	// A request that timed out while the run goes on is the service's silence:
	// retry work and the outage evidence an answered 5xx is, by the scope its
	// server answer names (ServerAnswerServiceOutage). Its issue names the
	// timeout. One that lands once the run has ended is retry work but no
	// evidence, whatever its shape: a stop is never read as the service's
	// silence.
	if IsRequestTimeout(r.Err) {
		if r.RunEnded {
			return withRuntimeSummary(&ResultDecision{
				Class:         resultRequeue,
				Persistence:   PersistRetryWork,
				TrialHint:     TrialHintReclassify,
				ConditionType: IssueRequestTimeout,
			})
		}

		return retryWithScopeEvidence(deriveScopeKey(r), IssueRequestTimeout)
	}

	// A create refused because OneDrive holds a differently spelled sibling
	// of the same name: the user has to rename one, so it is a durable
	// name-collision condition with backoff, not a 409 retried every pass.
	var nameTaken *RemoteNameTakenError
	if errors.As(r.Err, &nameTaken) {
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSkip,
			Persistence:   PersistRetryWork,
			TrialHint:     TrialHintReclassify,
			ConditionType: IssueCaseCollision,
		})
	}

	// No cancel the engine makes reaches an action while its run goes on: the
	// per-action cancel lands after the action returned, and the pool and pass
	// cancels end the worker's run. A cancel that arrives anyway came from
	// below the engine without a reason it names, so the action is tried again
	// rather than dropped (and the engine says so, isLiveRunCancel).
	if isLiveRunCancel(r) {
		return withRuntimeSummary(&ResultDecision{
			Class:       resultRequeue,
			Persistence: PersistRetryWork,
			TrialHint:   TrialHintReclassify,
		})
	}

	// Before any server answer: a stale precondition is the executor's verdict
	// that the plan no longer holds, and the Graph error it may wrap (a
	// create's 409, a conditional overwrite's 412) is the evidence it read,
	// not a failure to retry. Classified by that status first, a stale 409
	// would be held as actionable retry work and a stale 412 as
	// transient_conflict, instead of being replanned at once.
	if errors.Is(r.Err, ErrActionPreconditionChanged) {
		return withRuntimeSummary(&ResultDecision{
			Class:       resultSuperseded,
			Persistence: PersistNone,
			TrialHint:   TrialHintReclassify,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

// isLiveRunCancel reports an action canceled while its run went on: something
// narrower than the run canceled it.
func isLiveRunCancel(r *ActionCompletion) bool {
	return !r.RunEnded && errors.Is(r.Err, context.Canceled)
}

func classifyHTTPResult(r *ActionCompletion) (ResultDecision, bool) {
	answer := r.ServerAnswer()
	scopeEvidence := deriveScopeKey(r)
	conditionType := issueTypeForResult(r)

	switch answer {
	case ServerAnswerNone:
		return ResultDecision{}, false
	case ServerAnswerUnauthorized:
		return withRuntimeSummary(&ResultDecision{
			Class:         resultFatal,
			Persistence:   PersistNone,
			TrialHint:     TrialHintFatal,
			ConditionType: conditionType,
		}), true
	case ServerAnswerThrottled, ServerAnswerQuota:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       PersistRetryWork,
			RunScopeDetection: true,
			TrialHint:         TrialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case ServerAnswerServiceOutage,
		ServerAnswerRequestTimeout,
		ServerAnswerTransientConflict,
		ServerAnswerTransientNotFound,
		ServerAnswerLocked,
		ServerAnswerTransferURLRejected:
		return retryWithScopeEvidence(scopeEvidence, conditionType), true
	case ServerAnswerForbidden,
		ServerAnswerUnsupported,
		ServerAnswerPathTooLong,
		ServerAnswerMalware,
		ServerAnswerOther:
		// Actionable per item: retrying the same request gets the same
		// answer, and none of these say anything about a wider scope.
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSkip,
			Persistence:   PersistRetryWork,
			TrialHint:     TrialHintReclassify,
			ConditionType: conditionType,
		}), true
	}

	return ResultDecision{}, false
}

// retryWithScopeEvidence is the decision for a transient failure that is
// timed retry work and the evidence of scopeEvidence (zero for none): scope
// detection counts it, and a trial that fails with it keeps that scope.
func retryWithScopeEvidence(scopeEvidence ScopeKey, conditionType IssueType) ResultDecision {
	return withRuntimeSummary(&ResultDecision{
		Class:             resultRequeue,
		ScopeEvidence:     scopeEvidence,
		Persistence:       PersistRetryWork,
		RunScopeDetection: true,
		TrialHint:         TrialHintExtendOnMatchingScope,
		ConditionType:     conditionType,
	})
}

func classifyLocalResult(r *ActionCompletion) ResultDecision {
	conditionType := issueTypeForResult(r)

	switch driveops.ClassifyLocalError(r.Err) {
	case driveops.LocalErrorNoSpace:
		// Every write on a full filesystem fails the same way, so one
		// disk-wide scope replaces N per-file "unexpected" failures whether the
		// min_free_space pre-check or a real ENOSPC/EDQUOT write found it.
		return withRuntimeSummary(&ResultDecision{
			Class:         resultBlockScope,
			ScopeKey:      SKDiskLocal(),
			ScopeEvidence: SKDiskLocal(),
			Persistence:   PersistRetryWork,
			TrialHint:     TrialHintExtendOnMatchingScope,
			ConditionType: conditionType,
		})
	case driveops.LocalErrorBusy:
		return withRuntimeSummary(&ResultDecision{
			Class:         resultRequeue,
			Persistence:   PersistRetryWork,
			TrialHint:     TrialHintReclassify,
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

	return withRuntimeSummary(&ResultDecision{
		Class:         resultSkip,
		Persistence:   PersistRetryWork,
		TrialHint:     TrialHintReclassify,
		ConditionType: conditionType,
	})
}

func withRuntimeSummary(decision *ResultDecision) ResultDecision {
	decision.ConditionKey = ConditionKeyForRuntimeResult(decision.Class, decision.ConditionType)
	return *decision
}

// deriveScopeKey maps an action completion to its typed scope key. Delegates to
// ScopeKeyForResult, the single source of truth for server answer -> scope
// key. Returns the zero-value ScopeKey for answers that are not scope
// evidence.
func deriveScopeKey(r *ActionCompletion) ScopeKey {
	return ScopeKeyForResult(r.ServerAnswer(), r.DriveID)
}

func issueTypeForResult(r *ActionCompletion) IssueType {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *ActionCompletion) (IssueType, bool) {
	if r == nil {
		return "", false
	}

	answer := r.ServerAnswer()
	if answer == ServerAnswerForbidden {
		return issueTypeForForbiddenResult(r), true
	}

	issueType, ok := serverAnswerIssueTypes()[answer]
	return issueType, ok
}

// serverAnswerIssueTypes names the issue each server answer records. A
// function, not a package-level map, so the table cannot be mutated.
// ServerAnswerNone, ServerAnswerForbidden (probe-dependent) and
// ServerAnswerOther have no fixed issue.
func serverAnswerIssueTypes() map[ServerAnswer]IssueType {
	return map[ServerAnswer]IssueType{
		ServerAnswerPathTooLong:         IssuePathTooLong,
		ServerAnswerUnauthorized:        IssueUnauthorized,
		ServerAnswerThrottled:           IssueRateLimited,
		ServerAnswerQuota:               IssueQuotaExceeded,
		ServerAnswerUnsupported:         IssueUnsupportedOperation,
		ServerAnswerServiceOutage:       IssueServiceOutage,
		ServerAnswerRequestTimeout:      IssueRequestTimeout,
		ServerAnswerTransientConflict:   IssueTransientConflict,
		ServerAnswerTransientNotFound:   IssueTransientNotFound,
		ServerAnswerLocked:              IssueResourceLocked,
		ServerAnswerTransferURLRejected: IssueTransferURLRejected,
		ServerAnswerMalware:             IssueMalwareDetected,
	}
}

func issueTypeForForbiddenResult(r *ActionCompletion) IssueType {
	switch effectiveRemotePermissionCapability(r) {
	case PermissionCapabilityRemoteRead:
		return IssueRemoteReadDenied
	case PermissionCapabilityUnknown,
		PermissionCapabilityLocalRead,
		PermissionCapabilityLocalWrite,
		PermissionCapabilityRemoteWrite:
		return IssueRemoteWriteDenied
	default:
		return IssueRemoteWriteDenied
	}
}

func issueTypeForFilesystemResult(r *ActionCompletion) (IssueType, bool) {
	if r == nil {
		return "", false
	}

	err := r.Err
	if issue, ok := issueTypeForSentinelResult(err); ok {
		return issue, true
	}
	switch {
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return IssueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return IssueFileTooLarge, true
	case errors.Is(err, driveops.ErrUploadVerificationFailed):
		return IssueUploadVerification, true
	case errors.Is(err, driveops.ErrDownloadIntegrity):
		return IssueDownloadIntegrity, true
	}

	switch driveops.ClassifyLocalError(err) {
	case driveops.LocalErrorNoSpace:
		return IssueDiskFull, true
	case driveops.LocalErrorReadOnly:
		return IssueLocalReadOnly, true
	case driveops.LocalErrorFileTooLarge:
		return IssueLocalFileTooLarge, true
	case driveops.LocalErrorBusy:
		return IssueLocalBusy, true
	case driveops.LocalErrorCrossDevice:
		return IssueLocalCrossDevice, true
	case driveops.LocalErrorNameTooLong:
		// OneDrive accepts names longer than the 255 bytes a Linux or
		// macOS filesystem can store; without a named issue each download
		// would retry forever as an "unexpected" failure.
		return IssueLocalNameTooLong, true
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
	case errors.Is(err, ErrDatabaseInUse):
		return IssueLocalDatabaseOpen, true
	case errors.Is(err, ErrRemoteFolderTooLargeToVerify):
		return IssueRemoteFolderUnverifiable, true
	default:
		return "", false
	}
}

func issueTypeForLocalPermissionResult(r *ActionCompletion) IssueType {
	switch effectiveLocalPermissionCapability(r) {
	case PermissionCapabilityLocalRead:
		return IssueLocalReadDenied
	case PermissionCapabilityUnknown,
		PermissionCapabilityLocalWrite,
		PermissionCapabilityRemoteRead,
		PermissionCapabilityRemoteWrite:
		return IssueLocalWriteDenied
	default:
		return IssueLocalWriteDenied
	}
}

func effectiveRemotePermissionCapability(r *ActionCompletion) PermissionCapability {
	if r == nil {
		return PermissionCapabilityUnknown
	}
	if r.FailureCapability == PermissionCapabilityRemoteRead || r.FailureCapability == PermissionCapabilityRemoteWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return PermissionCapabilityUnknown
	}

	switch r.ActionType {
	case ActionDownload:
		return PermissionCapabilityRemoteRead
	case ActionUpload, ActionRemoteDelete, ActionRemoteMove, ActionFolderCreate:
		return PermissionCapabilityRemoteWrite
	case ActionConflictCopy, ActionLocalDelete, ActionLocalMove, ActionBaselineUpdate, ActionCleanup:
		return PermissionCapabilityUnknown
	default:
		panic(OutsideTheEnum(r.ActionType))
	}
}

func effectiveLocalPermissionCapability(r *ActionCompletion) PermissionCapability {
	if r == nil {
		return PermissionCapabilityUnknown
	}
	if r.FailureCapability == PermissionCapabilityLocalRead || r.FailureCapability == PermissionCapabilityLocalWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return PermissionCapabilityUnknown
	}

	switch r.ActionType {
	case ActionUpload:
		return PermissionCapabilityLocalRead
	case ActionDownload, ActionLocalDelete, ActionLocalMove, ActionFolderCreate, ActionConflictCopy, ActionCleanup:
		return PermissionCapabilityLocalWrite
	case ActionRemoteDelete, ActionRemoteMove, ActionBaselineUpdate:
		return PermissionCapabilityUnknown
	default:
		panic(OutsideTheEnum(r.ActionType))
	}
}

func hasPermissionActionContext(r *ActionCompletion) bool {
	if r == nil {
		return false
	}

	return r.ActionID != 0 || r.Path != "" || !r.DriveID.IsZero()
}
