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
	resultRequeue    = errclass.ClassRetryableTransient
	resultBlockScope = errclass.ClassBlockScopeingTransient
	resultSkip       = errclass.ClassActionable
	resultShutdown   = errclass.ClassShutdown
	resultFatal      = errclass.ClassFatal
)

type resultPersistenceMode int

const (
	persistNone resultPersistenceMode = iota
	persistRetryWork
)

type permissionFlow int

const (
	permissionFlowNone permissionFlow = iota
	permissionFlowRemote403
	permissionFlowLocalPermission
)

type trialHint int

const (
	trialHintRelease trialHint = iota
	trialHintExtendOnMatchingScope
	trialHintReclassify
	trialHintShutdown
	trialHintFatal
)

// ResultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type ResultDecision struct {
	Class             errclass.Class
	ConditionKey      ConditionKey
	ScopeKey          ScopeKey
	ScopeEvidence     ScopeKey
	Persistence       resultPersistenceMode
	PermissionFlow    permissionFlow
	RunScopeDetection bool
	RecordSuccess     bool
	TrialHint         trialHint
	ConditionType     string
}

// classifyResult is a pure function that maps a ActionCompletion to a
// single ResultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *ActionCompletion) ResultDecision {
	if r.Success {
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     trialHintRelease,
		})
	}

	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return withRuntimeSummary(&ResultDecision{
			Class:     resultShutdown,
			TrialHint: trialHintShutdown,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

func classifyHTTPResult(r *ActionCompletion) (ResultDecision, bool) {
	scopeEvidence := deriveScopeKey(r)
	conditionType := issueTypeForResult(r)
	permissionFlowKind := remote403PermissionFlow(r)

	switch {
	case r.HTTPStatus == 0:
		return ResultDecision{}, false
	case r.HTTPStatus == http.StatusUnauthorized:
		return withRuntimeSummary(&ResultDecision{
			Class:         resultFatal,
			Persistence:   persistNone,
			TrialHint:     trialHintFatal,
			ConditionType: conditionType,
		}), true
	case r.HTTPStatus == http.StatusForbidden:
		return withRuntimeSummary(&ResultDecision{
			Class:          resultSkip,
			Persistence:    persistRetryWork,
			PermissionFlow: permissionFlowKind,
			TrialHint:      trialHintReclassify,
			ConditionType:  conditionType,
		}), true
	case r.HTTPStatus == http.StatusTooManyRequests:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case r.HTTPStatus == http.StatusInsufficientStorage:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultBlockScope,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case r.HTTPStatus >= http.StatusInternalServerError:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	case isRetryableHTTPStatus(r.HTTPStatus):
		return withRuntimeSummary(&ResultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistRetryWork,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			ConditionType:     conditionType,
		}), true
	default:
		return withRuntimeSummary(&ResultDecision{
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

func classifyLocalResult(r *ActionCompletion) ResultDecision {
	conditionType := issueTypeForResult(r)
	permissionFlowKind := localPermissionFlow(r)

	switch {
	case errors.Is(r.Err, ErrActionPreconditionChanged):
		return withRuntimeSummary(&ResultDecision{
			Class:         resultRequeue,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: "transient_conflict",
		})
	case errors.Is(r.Err, driveops.ErrDiskFull):
		return withRuntimeSummary(&ResultDecision{
			Class:         resultBlockScope,
			ScopeKey:      SKDiskLocal(),
			ScopeEvidence: SKDiskLocal(),
			Persistence:   persistRetryWork,
			TrialHint:     trialHintExtendOnMatchingScope,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, driveops.ErrFileExceedsOneDriveLimit):
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	case errors.Is(r.Err, os.ErrPermission):
		return withRuntimeSummary(&ResultDecision{
			Class:          resultSkip,
			Persistence:    persistRetryWork,
			PermissionFlow: permissionFlowKind,
			TrialHint:      trialHintReclassify,
			ConditionType:  conditionType,
		})
	default:
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSkip,
			Persistence:   persistRetryWork,
			TrialHint:     trialHintReclassify,
			ConditionType: conditionType,
		})
	}
}

func remote403PermissionFlow(r *ActionCompletion) permissionFlow {
	if r == nil || r.HTTPStatus != http.StatusForbidden {
		return permissionFlowNone
	}
	if !remoteWriteScopeBlocksAction(r.ActionType) {
		return permissionFlowNone
	}

	return permissionFlowRemote403
}

func localPermissionFlow(r *ActionCompletion) permissionFlow {
	if r == nil || !errors.Is(r.Err, os.ErrPermission) {
		return permissionFlowNone
	}

	return permissionFlowLocalPermission
}

func withRuntimeSummary(decision *ResultDecision) ResultDecision {
	decision.ConditionKey = ConditionKeyForRuntimeResult(decision.Class, decision.ConditionType)
	return *decision
}

// deriveScopeKey maps an action completion to its typed scope key. Delegates to
// ScopeKeyForResult — single source of truth for HTTP status → scope
// key mapping. Returns the zero-value ScopeKey for non-scope statuses.
func deriveScopeKey(r *ActionCompletion) ScopeKey {
	targetDriveID := r.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = r.DriveID
	}
	return ScopeKeyForResult(r.HTTPStatus, targetDriveID)
}

func issueTypeForResult(r *ActionCompletion) string {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *ActionCompletion) (string, bool) {
	if r == nil {
		return "", false
	}

	switch httpStatus := r.HTTPStatus; {
	case httpStatus == http.StatusUnauthorized:
		return IssueUnauthorized, true
	case httpStatus == http.StatusTooManyRequests:
		return IssueRateLimited, true
	case httpStatus == http.StatusInsufficientStorage:
		return IssueQuotaExceeded, true
	case httpStatus == http.StatusForbidden:
		return issueTypeForForbiddenResult(r), true
	case httpStatus >= http.StatusInternalServerError:
		return IssueServiceOutage, true
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

func issueTypeForForbiddenResult(r *ActionCompletion) string {
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

func issueTypeForFilesystemResult(r *ActionCompletion) (string, bool) {
	if r == nil {
		return "", false
	}

	switch err := r.Err; {
	case errors.Is(err, driveops.ErrDiskFull):
		return IssueDiskFull, true
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return IssueFileTooLargeForSpace, true
	case errors.Is(err, driveops.ErrFileExceedsOneDriveLimit):
		return IssueFileTooLarge, true
	case errors.Is(err, os.ErrPermission):
		return issueTypeForLocalPermissionResult(r), true
	default:
		return "", false
	}
}

func issueTypeForLocalPermissionResult(r *ActionCompletion) string {
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
	case ActionConflictCopy, ActionLocalDelete, ActionLocalMove, ActionUpdateSynced, ActionCleanup:
		return PermissionCapabilityUnknown
	default:
		return PermissionCapabilityUnknown
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
	case ActionRemoteDelete, ActionRemoteMove, ActionUpdateSynced:
		return PermissionCapabilityUnknown
	default:
		return PermissionCapabilityUnknown
	}
}

func hasPermissionActionContext(r *ActionCompletion) bool {
	if r == nil {
		return false
	}

	return r.ActionID != 0 || r.Path != "" || !r.DriveID.IsZero() || !r.TargetDriveID.IsZero()
}
