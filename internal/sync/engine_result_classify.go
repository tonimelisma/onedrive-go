package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/failures"
)

const (
	resultSuccess    = failures.ClassSuccess
	resultRequeue    = failures.ClassRetryableTransient
	resultScopeBlock = failures.ClassScopeBlockingTransient
	resultSkip       = failures.ClassActionable
	resultShutdown   = failures.ClassShutdown
	resultFatal      = failures.ClassFatal
)

type resultPersistenceMode int

const (
	persistNone resultPersistenceMode = iota
	persistActionableFailure
	persistTransientFailure
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
	trialHintPreserve
	trialHintShutdown
	trialHintFatal
)

// ResultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type ResultDecision struct {
	Class             failures.Class
	SummaryKey        SummaryKey
	ScopeKey          ScopeKey
	ScopeEvidence     ScopeKey
	Persistence       resultPersistenceMode
	PermissionFlow    permissionFlow
	RunScopeDetection bool
	RecordSuccess     bool
	TrialHint         trialHint
	IssueType         string
	LogOwner          failures.LogOwner
	LogLevel          slog.Level
}

// classifyResult is a pure function that maps a WorkerResult to a
// single ResultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *WorkerResult) ResultDecision {
	if r.Success {
		return withRuntimeSummary(&ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
			TrialHint:     trialHintRelease,
			LogOwner:      failures.LogOwnerSync,
			LogLevel:      slog.LevelInfo,
		})
	}

	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return withRuntimeSummary(&ResultDecision{
			Class:     resultShutdown,
			TrialHint: trialHintShutdown,
			LogOwner:  failures.LogOwnerSync,
			LogLevel:  slog.LevelInfo,
		})
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

func classifyHTTPResult(r *WorkerResult) (ResultDecision, bool) {
	scopeEvidence := deriveScopeKey(r)
	issueType := issueTypeForResult(r)
	permissionDecisionFlow := remote403PermissionFlow(r)

	switch {
	case r.HTTPStatus == 0:
		return ResultDecision{}, false
	case r.HTTPStatus == http.StatusUnauthorized:
		return withRuntimeSummary(&ResultDecision{
			Class:       resultFatal,
			Persistence: persistActionableFailure,
			TrialHint:   trialHintFatal,
			IssueType:   issueType,
			LogOwner:    failures.LogOwnerSync,
			LogLevel:    slog.LevelError,
		}), true
	case r.HTTPStatus == http.StatusForbidden:
		return withRuntimeSummary(&ResultDecision{
			Class:          resultSkip,
			Persistence:    persistActionableFailure,
			PermissionFlow: permissionDecisionFlow,
			TrialHint:      trialHintPreserve,
			IssueType:      issueType,
			LogOwner:       failures.LogOwnerSync,
			LogLevel:       slog.LevelWarn,
		}), true
	case r.HTTPStatus == http.StatusTooManyRequests:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistTransientFailure,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			IssueType:         issueType,
			LogOwner:          failures.LogOwnerSync,
			LogLevel:          slog.LevelWarn,
		}), true
	case r.HTTPStatus == http.StatusInsufficientStorage:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          scopeEvidence,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistTransientFailure,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			IssueType:         issueType,
			LogOwner:          failures.LogOwnerSync,
			LogLevel:          slog.LevelWarn,
		}), true
	case r.HTTPStatus >= http.StatusInternalServerError:
		return withRuntimeSummary(&ResultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistTransientFailure,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			IssueType:         issueType,
			LogOwner:          failures.LogOwnerSync,
			LogLevel:          slog.LevelWarn,
		}), true
	case isRetryableHTTPStatus(r.HTTPStatus):
		return withRuntimeSummary(&ResultDecision{
			Class:             resultRequeue,
			ScopeEvidence:     scopeEvidence,
			Persistence:       persistTransientFailure,
			RunScopeDetection: true,
			TrialHint:         trialHintExtendOnMatchingScope,
			IssueType:         issueType,
			LogOwner:          failures.LogOwnerSync,
			LogLevel:          slog.LevelWarn,
		}), true
	default:
		return withRuntimeSummary(&ResultDecision{
			Class:       resultSkip,
			Persistence: persistActionableFailure,
			TrialHint:   trialHintPreserve,
			IssueType:   issueType,
			LogOwner:    failures.LogOwnerSync,
			LogLevel:    slog.LevelWarn,
		}), true
	}
}

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusPreconditionFailed ||
		status == http.StatusNotFound ||
		status == http.StatusLocked
}

func classifyLocalResult(r *WorkerResult) ResultDecision {
	issueType := issueTypeForResult(r)
	permissionDecisionFlow := localPermissionDecisionFlow(r)

	switch {
	case errors.Is(r.Err, driveops.ErrDiskFull):
		return withRuntimeSummary(&ResultDecision{
			Class:         resultScopeBlock,
			ScopeKey:      SKDiskLocal(),
			ScopeEvidence: SKDiskLocal(),
			Persistence:   persistTransientFailure,
			TrialHint:     trialHintExtendOnMatchingScope,
			IssueType:     issueType,
			LogOwner:      failures.LogOwnerSync,
			LogLevel:      slog.LevelWarn,
		})
	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		return withRuntimeSummary(&ResultDecision{
			Class:       resultSkip,
			Persistence: persistActionableFailure,
			TrialHint:   trialHintPreserve,
			IssueType:   issueType,
			LogOwner:    failures.LogOwnerSync,
			LogLevel:    slog.LevelWarn,
		})
	case errors.Is(r.Err, driveops.ErrFileExceedsOneDriveLimit):
		return withRuntimeSummary(&ResultDecision{
			Class:       resultSkip,
			Persistence: persistActionableFailure,
			TrialHint:   trialHintPreserve,
			IssueType:   issueType,
			LogOwner:    failures.LogOwnerSync,
			LogLevel:    slog.LevelWarn,
		})
	case errors.Is(r.Err, os.ErrPermission):
		return withRuntimeSummary(&ResultDecision{
			Class:          resultSkip,
			Persistence:    persistActionableFailure,
			PermissionFlow: permissionDecisionFlow,
			TrialHint:      trialHintPreserve,
			IssueType:      issueType,
			LogOwner:       failures.LogOwnerSync,
			LogLevel:       slog.LevelWarn,
		})
	default:
		return withRuntimeSummary(&ResultDecision{
			Class:       resultSkip,
			Persistence: persistActionableFailure,
			TrialHint:   trialHintPreserve,
			IssueType:   issueType,
			LogOwner:    failures.LogOwnerSync,
			LogLevel:    slog.LevelWarn,
		})
	}
}

func remote403PermissionFlow(r *WorkerResult) permissionFlow {
	if r == nil || r.HTTPStatus != http.StatusForbidden {
		return permissionFlowNone
	}
	if effectiveRemotePermissionCapability(r) == PermissionCapabilityRemoteWrite {
		return permissionFlowRemote403
	}

	return permissionFlowNone
}

func localPermissionDecisionFlow(r *WorkerResult) permissionFlow {
	if r == nil || !errors.Is(r.Err, os.ErrPermission) {
		return permissionFlowNone
	}
	if effectiveLocalPermissionCapability(r).IsLocal() {
		return permissionFlowLocalPermission
	}

	return permissionFlowNone
}

func withRuntimeSummary(decision *ResultDecision) ResultDecision {
	decision.SummaryKey = runtimeSummaryKey(decision.Class, decision.IssueType)
	return *decision
}

func runtimeSummaryKey(class failures.Class, issueType string) SummaryKey {
	if key, ok := runtimeSummaryKeyForIssueType(issueType); ok {
		return key
	}

	if class == failures.ClassRetryableTransient ||
		class == failures.ClassScopeBlockingTransient ||
		class == failures.ClassActionable ||
		class == failures.ClassFatal {
		return SummarySyncFailure
	}

	return ""
}

func runtimeSummaryKeyForIssueType(issueType string) (SummaryKey, bool) {
	if key, ok := runtimeSummaryKeyForPathIssue(issueType); ok {
		return key, true
	}
	if key, ok := runtimeSummaryKeyForServiceIssue(issueType); ok {
		return key, true
	}
	if key, ok := runtimeSummaryKeyForPermissionIssue(issueType); ok {
		return key, true
	}

	switch issueType {
	case IssueCaseCollision:
		return SummaryCaseCollision, true
	default:
		return "", false
	}
}

func runtimeSummaryKeyForPathIssue(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueInvalidFilename:
		return SummaryInvalidFilename, true
	case IssuePathTooLong:
		return SummaryPathTooLong, true
	case IssueFileTooLarge:
		return SummaryFileTooLarge, true
	case IssueFileTooLargeForSpace:
		return SummaryFileTooLargeForSpace, true
	case IssueDiskFull:
		return SummaryDiskFull, true
	case IssueHashPanic:
		return SummaryHashError, true
	default:
		return "", false
	}
}

func runtimeSummaryKeyForServiceIssue(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueUnauthorized:
		return SummaryAuthenticationRequired, true
	case IssueQuotaExceeded:
		return SummaryQuotaExceeded, true
	case IssueServiceOutage:
		return SummaryServiceOutage, true
	case IssueRateLimited:
		return SummaryRateLimited, true
	default:
		return "", false
	}
}

func runtimeSummaryKeyForPermissionIssue(issueType string) (SummaryKey, bool) {
	switch issueType {
	case IssueRemoteWriteDenied:
		return SummaryRemoteWriteDenied, true
	case IssueRemoteReadDenied:
		return SummaryRemoteReadDenied, true
	case IssueLocalReadDenied:
		return SummaryLocalReadDenied, true
	case IssueLocalWriteDenied:
		return SummaryLocalWriteDenied, true
	default:
		return "", false
	}
}

// deriveScopeKey maps a worker result to its typed scope key. Delegates to
// ScopeKeyForResult — single source of truth for HTTP status → scope
// key mapping. Returns the zero-value ScopeKey for non-scope statuses.
func deriveScopeKey(r *WorkerResult) ScopeKey {
	targetDriveID := r.TargetDriveID
	if targetDriveID.IsZero() {
		targetDriveID = r.DriveID
	}
	return ScopeKeyForResult(r.HTTPStatus, targetDriveID)
}

func issueTypeForResult(r *WorkerResult) string {
	if issueType, ok := issueTypeForHTTPResult(r); ok {
		return issueType
	}
	if issueType, ok := issueTypeForFilesystemResult(r); ok {
		return issueType
	}

	return ""
}

func issueTypeForHTTPResult(r *WorkerResult) (string, bool) {
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

func issueTypeForForbiddenResult(r *WorkerResult) string {
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

func issueTypeForFilesystemResult(r *WorkerResult) (string, bool) {
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

func issueTypeForLocalPermissionResult(r *WorkerResult) string {
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

func effectiveRemotePermissionCapability(r *WorkerResult) PermissionCapability {
	if r == nil {
		return PermissionCapabilityUnknown
	}
	if r.FailureCapability == PermissionCapabilityRemoteRead || r.FailureCapability == PermissionCapabilityRemoteWrite {
		return r.FailureCapability
	}

	switch r.ActionType {
	case ActionDownload:
		return PermissionCapabilityRemoteRead
	case ActionUpload, ActionRemoteDelete, ActionRemoteMove, ActionFolderCreate, ActionConflict:
		return PermissionCapabilityRemoteWrite
	case ActionLocalDelete, ActionLocalMove, ActionUpdateSynced, ActionCleanup:
		return PermissionCapabilityUnknown
	default:
		return PermissionCapabilityUnknown
	}
}

func effectiveLocalPermissionCapability(r *WorkerResult) PermissionCapability {
	if r == nil {
		return PermissionCapabilityUnknown
	}
	if r.FailureCapability == PermissionCapabilityLocalRead || r.FailureCapability == PermissionCapabilityLocalWrite {
		return r.FailureCapability
	}

	switch r.ActionType {
	case ActionUpload:
		return PermissionCapabilityLocalRead
	case ActionDownload, ActionLocalDelete, ActionLocalMove, ActionFolderCreate, ActionCleanup:
		return PermissionCapabilityLocalWrite
	case ActionConflict:
		return PermissionCapabilityLocalWrite
	case ActionRemoteDelete, ActionRemoteMove, ActionUpdateSynced:
		return PermissionCapabilityUnknown
	default:
		return PermissionCapabilityUnknown
	}
}

func (m resultPersistenceMode) failureCategory() FailureCategory {
	switch m {
	case persistNone:
		return ""
	case persistActionableFailure:
		return CategoryActionable
	case persistTransientFailure:
		return CategoryTransient
	}

	return ""
}

// directionFromAction maps a ActionType to a typed Direction enum.
func directionFromAction(at ActionType) Direction {
	return at.Direction()
}
