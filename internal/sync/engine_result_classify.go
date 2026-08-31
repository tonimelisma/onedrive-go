package sync

import (
	"context"
	"errors"
	"net/http"
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

type resultPersistenceMode int

const (
	persistNone resultPersistenceMode = iota
	persistRetryWork
)

type trialHint int

const (
	trialHintRelease trialHint = iota
	trialHintExtendOnMatchingScope
	trialHintReclassify
	trialHintShutdown
	trialHintFatal
)

// resultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type resultDecision struct {
	Class             errclass.Class
	ConditionKey      ConditionKey
	ScopeKey          ScopeKey
	ScopeEvidence     ScopeKey
	Persistence       resultPersistenceMode
	RunScopeDetection bool
	RecordSuccess     bool
	TrialHint         trialHint
	ConditionType     string
}

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
	scopeEvidence := deriveScopeKey(r)
	conditionType := issueTypeForResult(r)

	switch {
	case r.HTTPStatus == 0:
		return resultDecision{}, false
	case r.HTTPStatus == http.StatusUnauthorized:
		return withRuntimeSummary(&resultDecision{
			Class:         resultFatal,
			Persistence:   persistNone,
			TrialHint:     trialHintFatal,
			ConditionType: conditionType,
		}), true
	case r.HTTPStatus == http.StatusForbidden:
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		}), true
	case r.HTTPStatus == http.StatusTooManyRequests:
		return withRuntimeSummary(&resultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case r.HTTPStatus == http.StatusInsufficientStorage:
		return withRuntimeSummary(&resultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case r.HTTPStatus >= http.StatusInternalServerError:
		return withRuntimeSummary(&resultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case isRetryableHTTPStatus(r.HTTPStatus):
		return withRuntimeSummary(&resultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	default:
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		}), true
	}
}

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusPreconditionFailed ||
		status == http.StatusNotFound ||
		status == http.StatusLocked
}

func classifyLocalResult(r *actionCompletion) resultDecision {
	conditionType := issueTypeForResult(r)

	switch {
	case errors.Is(r.Err, errActionPreconditionChanged):
		return withRuntimeSummary(&resultDecision{
			Class:       resultSuperseded,
			Persistence: persistNone,
			TrialHint:   trialHintReclassify,
		})
	case errors.Is(r.Err, driveops.ErrDiskFull):
		return withRuntimeSummary(&resultDecision{
			Class:         resultBlockScope,
			ScopeKey:      SKDiskLocal(),
			ScopeEvidence: SKDiskLocal(),
			Persistence:   persistRetryWork,
			TrialHint:     trialHintExtendOnMatchingScope,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, driveops.ErrFileExceedsOneDriveLimit):
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, os.ErrPermission):
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	default:
		return withRuntimeSummary(&resultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	}
}

func withRuntimeSummary(decision *resultDecision) resultDecision {
	decision.ConditionKey = conditionKeyForRuntimeResult(decision.Class, decision.ConditionType)
	return *decision
}

// deriveScopeKey maps an action completion to its typed scope key. Delegates to
// ScopeKeyForResult — single source of truth for HTTP status → scope
// key mapping. Returns the zero-value ScopeKey for non-scope statuses.
func deriveScopeKey(r *actionCompletion) ScopeKey {
	return scopeKeyForResult(r.HTTPStatus, r.DriveID)
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

	switch httpStatus := r.HTTPStatus; {
	case httpStatus == http.StatusUnauthorized:
		return IssueUnauthorized, true
	case httpStatus == http.StatusTooManyRequests:
		return issueRateLimited, true
	case httpStatus == http.StatusInsufficientStorage:
		return IssueQuotaExceeded, true
	case httpStatus == http.StatusForbidden:
		return issueTypeForForbiddenResult(r), true
	case httpStatus >= http.StatusInternalServerError:
		return issueServiceOutage, true
	case httpStatus == http.StatusRequestTimeout:
		return "request_timeout", true
	case httpStatus == http.StatusPreconditionFailed:
		return "transient_conflict", true
	case httpStatus == http.StatusNotFound:
		return "transient_not_found", true
	case httpStatus == http.StatusLocked:
		return "resource_locked", true
	default:
		return "", false
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

	switch err := r.Err; {
	case errors.Is(err, driveops.ErrDiskFull):
		return issueDiskFull, true
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return issueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return issueFileTooLarge, true
	case errors.Is(err, os.ErrPermission):
		return issueTypeForLocalPermissionResult(r), true
	default:
		return "", false
	}
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
