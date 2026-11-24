package sync

import (
	"context"
	"errors"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
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
func classifyResult(r *syncdomain.ActionCompletion) syncdomain.ResultDecision {
	if r.Success {
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     syncdomain.TrialHintRelease,
		})
	}

	// Shutdown is a fact about the run, never the shape of an error: only an
	// action whose run had ended is a shutdown, and then only for the
	// cancellation's own error. A separate failure that landed during the stop
	// is classified like any other, and so is a store access that outlasted
	// its own bound: its deadline is the engine's limit on its database, which
	// the stop does not set, and the record it failed to write is lost.
	if r.RunEnded && isCancellationEcho(r.Err) && !errors.Is(r.Err, syncdomain.ErrStoreAccessTimedOut) {
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:     resultShutdown,
			TrialHint: syncdomain.TrialHintShutdown,
		})
	}

	// A request that did not reach OneDrive or got no answer while the run
	// goes on is the service's silence: retry work and the outage evidence an
	// answered 5xx is, by the scope its server answer names
	// (ServerAnswerServiceOutage), never an issue that names the file. A
	// timeout's condition names the timeout; a name that does not resolve or
	// a connection refused, unroutable or reset is the outage itself. One that
	// lands once the run has ended is retry work but no evidence, whatever its
	// shape: a stop is never read as the service's silence.
	if syncdomain.IsServiceUnreached(r.Err) {
		conditionType := syncdomain.IssueServiceOutage
		if syncdomain.IsRequestTimeout(r.Err) {
			conditionType = syncdomain.IssueRequestTimeout
		}
		if r.RunEnded {
			return withRuntimeSummary(&syncdomain.ResultDecision{
				Class:         resultRequeue,
				Persistence:   syncdomain.PersistRetryWork,
				TrialHint:     syncdomain.TrialHintReclassify,
				ConditionType: conditionType,
			})
		}

		return retryWithScopeEvidence(deriveScopeKey(r), conditionType)
	}

	// A create refused because OneDrive holds a differently spelled sibling
	// of the same name: the user has to rename one, so it is a durable
	// name-collision condition with backoff, not a 409 retried every pass.
	var nameTaken *syncexec.RemoteNameTakenError
	if errors.As(r.Err, &nameTaken) {
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultSkip,
			Persistence:   syncdomain.PersistRetryWork,
			TrialHint:     syncdomain.TrialHintReclassify,
			ConditionType: syncdomain.IssueCaseCollision,
		})
	}

	// No cancel the engine makes reaches an action while its run goes on: the
	// per-action cancel lands after the action returned, and the pool and pass
	// cancels end the worker's run. A cancel that arrives anyway came from
	// below the engine without a reason it names, so the action is tried again
	// rather than dropped (and the engine says so, isLiveRunCancel).
	if isLiveRunCancel(r) {
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:       resultRequeue,
			Persistence: syncdomain.PersistRetryWork,
			TrialHint:   syncdomain.TrialHintReclassify,
		})
	}

	// Before any server answer: a stale precondition is the executor's verdict
	// that the plan no longer holds, and the Graph error it may wrap (a
	// create's 409, a conditional overwrite's 412) is the evidence it read,
	// not a failure to retry. Classified by that status first, a stale 409
	// would be held as actionable retry work and a stale 412 as
	// transient_conflict, instead of being replanned at once.
	if errors.Is(r.Err, syncdomain.ErrActionPreconditionChanged) {
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:       resultSuperseded,
			Persistence: syncdomain.PersistNone,
			TrialHint:   syncdomain.TrialHintReclassify,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

// isLiveRunCancel reports an action canceled while its run went on: something
// narrower than the run canceled it.
func isLiveRunCancel(r *syncdomain.ActionCompletion) bool {
	return !r.RunEnded && errors.Is(r.Err, context.Canceled)
}

func classifyHTTPResult(r *syncdomain.ActionCompletion) (syncdomain.ResultDecision, bool) {
	answer := r.ServerAnswer()
	scopeEvidence := deriveScopeKey(r)
	conditionType := issueTypeForResult(r)

	switch answer {
	case syncdomain.ServerAnswerNone:
		return syncdomain.ResultDecision{}, false
	case syncdomain.ServerAnswerUnauthorized:
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultFatal,
			Persistence:   syncdomain.PersistNone,
			TrialHint:     syncdomain.TrialHintFatal,
			ConditionType: conditionType,
		}), true
	case syncdomain.ServerAnswerThrottled, syncdomain.ServerAnswerQuota:
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       syncdomain.PersistRetryWork,
			RunScopeDetection: true,
			TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case syncdomain.ServerAnswerServiceOutage,
		syncdomain.ServerAnswerRequestTimeout,
		syncdomain.ServerAnswerTransientConflict,
		syncdomain.ServerAnswerTransientNotFound,
		syncdomain.ServerAnswerLocked,
		syncdomain.ServerAnswerTransferURLRejected:
		return retryWithScopeEvidence(scopeEvidence, conditionType), true
	case syncdomain.ServerAnswerForbidden,
		syncdomain.ServerAnswerUnsupported,
		syncdomain.ServerAnswerPathTooLong,
		syncdomain.ServerAnswerMalware,
		syncdomain.ServerAnswerOther:
		// Actionable per item: retrying the same request gets the same
		// answer, and none of these say anything about a wider scope.
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultSkip,
			Persistence:   syncdomain.PersistRetryWork,
			TrialHint:     syncdomain.TrialHintReclassify,
			ConditionType: conditionType,
		}), true
	}

	return syncdomain.ResultDecision{}, false
}

// retryWithScopeEvidence is the decision for a transient failure that is
// timed retry work and the evidence of scopeEvidence (zero for none): scope
// detection counts it, and a trial that fails with it keeps that scope.
func retryWithScopeEvidence(scopeEvidence syncdomain.ScopeKey, conditionType syncdomain.IssueType) syncdomain.ResultDecision {
	return withRuntimeSummary(&syncdomain.ResultDecision{
		Class:             resultRequeue,
		ScopeEvidence:     scopeEvidence,
		Persistence:       syncdomain.PersistRetryWork,
		RunScopeDetection: true,
		TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
		ConditionType:     conditionType,
	})
}

func classifyLocalResult(r *syncdomain.ActionCompletion) syncdomain.ResultDecision {
	conditionType := issueTypeForResult(r)

	switch driveops.ClassifyLocalError(r.Err) {
	case driveops.LocalErrorNoSpace:
		// Every write on a full filesystem fails the same way, so one
		// disk-wide scope replaces N per-file "unexpected" failures whether the
		// min_free_space pre-check or a real ENOSPC/EDQUOT write found it.
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultBlockScope,
			ScopeKey:      syncdomain.SKDiskLocal(),
			ScopeEvidence: syncdomain.SKDiskLocal(),
			Persistence:   syncdomain.PersistRetryWork,
			TrialHint:     syncdomain.TrialHintExtendOnMatchingScope,
			ConditionType: conditionType,
		})
	case driveops.LocalErrorBusy:
		return withRuntimeSummary(&syncdomain.ResultDecision{
			Class:         resultRequeue,
			Persistence:   syncdomain.PersistRetryWork,
			TrialHint:     syncdomain.TrialHintReclassify,
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

	return withRuntimeSummary(&syncdomain.ResultDecision{
		Class:         resultSkip,
		Persistence:   syncdomain.PersistRetryWork,
		TrialHint:     syncdomain.TrialHintReclassify,
		ConditionType: conditionType,
	})
}

func withRuntimeSummary(decision *syncdomain.ResultDecision) syncdomain.ResultDecision {
	decision.ConditionKey = conditionKeyForRuntimeResult(decision.Class, decision.ConditionType)
	return *decision
}

// deriveScopeKey maps an action completion to its typed scope key. Delegates to
// ScopeKeyForResult, the single source of truth for server answer -> scope
// key. Returns the zero-value ScopeKey for answers that are not scope
// evidence.
func deriveScopeKey(r *syncdomain.ActionCompletion) syncdomain.ScopeKey {
	return syncdomain.ScopeKeyForResult(r.ServerAnswer(), r.DriveID)
}

func issueTypeForResult(r *syncdomain.ActionCompletion) syncdomain.IssueType {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *syncdomain.ActionCompletion) (syncdomain.IssueType, bool) {
	if r == nil {
		return "", false
	}

	answer := r.ServerAnswer()
	if answer == syncdomain.ServerAnswerForbidden {
		return issueTypeForForbiddenResult(r), true
	}

	issueType, ok := serverAnswerIssueTypes()[answer]
	return issueType, ok
}

// serverAnswerIssueTypes names the issue each server answer records. A
// function, not a package-level map, so the table cannot be mutated.
// ServerAnswerNone, ServerAnswerForbidden (probe-dependent) and
// ServerAnswerOther have no fixed issue.
func serverAnswerIssueTypes() map[syncdomain.ServerAnswer]syncdomain.IssueType {
	return map[syncdomain.ServerAnswer]syncdomain.IssueType{
		syncdomain.ServerAnswerPathTooLong:         syncdomain.IssuePathTooLong,
		syncdomain.ServerAnswerUnauthorized:        syncdomain.IssueUnauthorized,
		syncdomain.ServerAnswerThrottled:           syncdomain.IssueRateLimited,
		syncdomain.ServerAnswerQuota:               syncdomain.IssueQuotaExceeded,
		syncdomain.ServerAnswerUnsupported:         syncdomain.IssueUnsupportedOperation,
		syncdomain.ServerAnswerServiceOutage:       syncdomain.IssueServiceOutage,
		syncdomain.ServerAnswerRequestTimeout:      syncdomain.IssueRequestTimeout,
		syncdomain.ServerAnswerTransientConflict:   syncdomain.IssueTransientConflict,
		syncdomain.ServerAnswerTransientNotFound:   syncdomain.IssueTransientNotFound,
		syncdomain.ServerAnswerLocked:              syncdomain.IssueResourceLocked,
		syncdomain.ServerAnswerTransferURLRejected: syncdomain.IssueTransferURLRejected,
		syncdomain.ServerAnswerMalware:             syncdomain.IssueMalwareDetected,
	}
}

func issueTypeForForbiddenResult(r *syncdomain.ActionCompletion) syncdomain.IssueType {
	switch effectiveRemotePermissionCapability(r) {
	case syncdomain.PermissionCapabilityRemoteRead:
		return syncdomain.IssueRemoteReadDenied
	case syncdomain.PermissionCapabilityUnknown,
		syncdomain.PermissionCapabilityLocalRead,
		syncdomain.PermissionCapabilityLocalWrite,
		syncdomain.PermissionCapabilityRemoteWrite:
		return syncdomain.IssueRemoteWriteDenied
	default:
		return syncdomain.IssueRemoteWriteDenied
	}
}

func issueTypeForFilesystemResult(r *syncdomain.ActionCompletion) (syncdomain.IssueType, bool) {
	if r == nil {
		return "", false
	}

	err := r.Err
	if issue, ok := issueTypeForSentinelResult(err); ok {
		return issue, true
	}
	switch {
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return syncdomain.IssueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return syncdomain.IssueFileTooLarge, true
	case errors.Is(err, driveops.ErrUploadVerificationFailed):
		return syncdomain.IssueUploadVerification, true
	case errors.Is(err, driveops.ErrDownloadIntegrity):
		return syncdomain.IssueDownloadIntegrity, true
	}

	switch driveops.ClassifyLocalError(err) {
	case driveops.LocalErrorNoSpace:
		return syncdomain.IssueDiskFull, true
	case driveops.LocalErrorReadOnly:
		return syncdomain.IssueLocalReadOnly, true
	case driveops.LocalErrorFileTooLarge:
		return syncdomain.IssueLocalFileTooLarge, true
	case driveops.LocalErrorBusy:
		return syncdomain.IssueLocalBusy, true
	case driveops.LocalErrorCrossDevice:
		return syncdomain.IssueLocalCrossDevice, true
	case driveops.LocalErrorNameTooLong:
		// OneDrive accepts names longer than the 255 bytes a Linux or
		// macOS filesystem can store; without a named issue each download
		// would retry forever as an "unexpected" failure.
		return syncdomain.IssueLocalNameTooLong, true
	case driveops.LocalErrorOther:
	}

	if errors.Is(err, os.ErrPermission) {
		return issueTypeForLocalPermissionResult(r), true
	}

	return "", false
}

// issueTypeForSentinelResult maps the sync package's own durable-condition
// errors.
func issueTypeForSentinelResult(err error) (syncdomain.IssueType, bool) {
	switch {
	case errors.Is(err, syncexec.ErrDatabaseInUse):
		return syncdomain.IssueLocalDatabaseOpen, true
	case errors.Is(err, syncexec.ErrRemoteFolderTooLargeToVerify):
		return syncdomain.IssueRemoteFolderUnverifiable, true
	default:
		return "", false
	}
}

func issueTypeForLocalPermissionResult(r *syncdomain.ActionCompletion) syncdomain.IssueType {
	switch effectiveLocalPermissionCapability(r) {
	case syncdomain.PermissionCapabilityLocalRead:
		return syncdomain.IssueLocalReadDenied
	case syncdomain.PermissionCapabilityUnknown,
		syncdomain.PermissionCapabilityLocalWrite,
		syncdomain.PermissionCapabilityRemoteRead,
		syncdomain.PermissionCapabilityRemoteWrite:
		return syncdomain.IssueLocalWriteDenied
	default:
		return syncdomain.IssueLocalWriteDenied
	}
}

func effectiveRemotePermissionCapability(r *syncdomain.ActionCompletion) syncdomain.PermissionCapability {
	if r == nil {
		return syncdomain.PermissionCapabilityUnknown
	}
	if r.FailureCapability == syncdomain.PermissionCapabilityRemoteRead || r.FailureCapability == syncdomain.PermissionCapabilityRemoteWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return syncdomain.PermissionCapabilityUnknown
	}

	switch r.ActionType {
	case syncdomain.ActionDownload:
		return syncdomain.PermissionCapabilityRemoteRead
	case syncdomain.ActionUpload, syncdomain.ActionRemoteDelete, syncdomain.ActionRemoteMove, syncdomain.ActionFolderCreate:
		return syncdomain.PermissionCapabilityRemoteWrite
	case syncdomain.ActionConflictCopy, syncdomain.ActionLocalDelete, syncdomain.ActionLocalMove, syncdomain.ActionBaselineUpdate,
		syncdomain.ActionCleanup:
		return syncdomain.PermissionCapabilityUnknown
	default:
		panic(syncdomain.OutsideTheEnum(r.ActionType))
	}
}

func effectiveLocalPermissionCapability(r *syncdomain.ActionCompletion) syncdomain.PermissionCapability {
	if r == nil {
		return syncdomain.PermissionCapabilityUnknown
	}
	if r.FailureCapability == syncdomain.PermissionCapabilityLocalRead || r.FailureCapability == syncdomain.PermissionCapabilityLocalWrite {
		return r.FailureCapability
	}
	if !hasPermissionActionContext(r) {
		return syncdomain.PermissionCapabilityUnknown
	}

	switch r.ActionType {
	case syncdomain.ActionUpload:
		return syncdomain.PermissionCapabilityLocalRead
	case syncdomain.ActionDownload, syncdomain.ActionLocalDelete, syncdomain.ActionLocalMove, syncdomain.ActionFolderCreate,
		syncdomain.ActionConflictCopy, syncdomain.ActionCleanup:
		return syncdomain.PermissionCapabilityLocalWrite
	case syncdomain.ActionRemoteDelete, syncdomain.ActionRemoteMove, syncdomain.ActionBaselineUpdate:
		return syncdomain.PermissionCapabilityUnknown
	default:
		panic(syncdomain.OutsideTheEnum(r.ActionType))
	}
}

func hasPermissionActionContext(r *syncdomain.ActionCompletion) bool {
	if r == nil {
		return false
	}

	return r.ActionID != 0 || r.Path != "" || !r.DriveID.IsZero()
}

// conditionKeyForRuntimeResult maps one execution-time classified result into
// the shared condition family used by logs, watch summaries, and status. A
// transient runtime condition (a timeout, a conflict, a locked item) is an
// expected failure that is retried, so it has no family; it used to be filed
// as the unexpected condition. A failure with no condition at all is the
// unexpected condition, and a result that is not a failure has none.
func conditionKeyForRuntimeResult(class errclass.Class, conditionType syncdomain.IssueType) syncdomain.ConditionKey {
	if key, ok := syncdomain.ConditionKeyForIssueType(conditionType); ok {
		return key
	}
	if conditionType != "" {
		// ConditionKeyForIssueType names no family only for the transient
		// members and the empty value.
		return ""
	}

	switch class {
	case errclass.ClassRetryableTransient,
		errclass.ClassScopeBlockingTransient,
		errclass.ClassActionable,
		errclass.ClassFatal:
		return syncdomain.ConditionUnexpectedCondition
	case errclass.ClassInvalid,
		errclass.ClassSuccess,
		errclass.ClassShutdown,
		errclass.ClassSuperseded:
		return ""
	default:
		return ""
	}
}

// isCancellationEcho reports whether err is what a canceled or expired
// context hands back to the work it interrupted. A caller that already knows
// the run has ended (RunEnded) uses it to tell that echo of the stop from a
// separate failure that landed during it, which is still reported.
func isCancellationEcho(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
