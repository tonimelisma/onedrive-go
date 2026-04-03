package sync

import (
	"context"
	"errors"
	"net/http"
	"os"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// resultClass categorizes a synctypes.WorkerResult for routing by processWorkerResult.
type resultClass int

const (
	resultSuccess resultClass = iota
	resultRequeue
	resultScopeBlock
	resultSkip
	resultShutdown
	resultFatal
)

type failureRecordMode int

const (
	recordFailureNone failureRecordMode = iota
	recordFailureActionable
	recordFailureReconcile
)

type permissionFlow int

const (
	permissionFlowNone permissionFlow = iota
	permissionFlowRemote403
	permissionFlowLocalPermission
)

// ResultDecision is the single classification output consumed by result
// routing. The decision is behavior-complete so downstream code does not
// re-derive policy from raw HTTP/local error facts.
type ResultDecision struct {
	Class             resultClass
	ScopeKey          synctypes.ScopeKey
	RecordMode        failureRecordMode
	PermissionFlow    permissionFlow
	RunScopeDetection bool
	RecordSuccess     bool
}

// classifyResult is a pure function that maps a synctypes.WorkerResult to a
// single ResultDecision. No side effects — classification is separate from
// routing.
func classifyResult(r *synctypes.WorkerResult) ResultDecision {
	if r.Success {
		return ResultDecision{
			Class:         resultSuccess,
			RecordSuccess: true,
		}
	}

	if errors.Is(r.Err, context.Canceled) || errors.Is(r.Err, context.DeadlineExceeded) {
		return ResultDecision{Class: resultShutdown}
	}

	if decision, handled := classifyHTTPResult(r); handled {
		return decision
	}

	return classifyLocalResult(r)
}

func classifyHTTPResult(r *synctypes.WorkerResult) (ResultDecision, bool) {
	switch {
	case r.HTTPStatus == 0:
		return ResultDecision{}, false
	case r.HTTPStatus == http.StatusUnauthorized:
		return ResultDecision{
			Class:      resultFatal,
			RecordMode: recordFailureActionable,
		}, true
	case r.HTTPStatus == http.StatusForbidden:
		return ResultDecision{
			Class:          resultSkip,
			RecordMode:     recordFailureActionable,
			PermissionFlow: permissionFlowRemote403,
		}, true
	case r.HTTPStatus == http.StatusTooManyRequests:
		return ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          synctypes.SKThrottleAccount(),
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case r.HTTPStatus == http.StatusInsufficientStorage:
		return ResultDecision{
			Class:             resultScopeBlock,
			ScopeKey:          synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey),
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case r.HTTPStatus >= http.StatusInternalServerError:
		return ResultDecision{
			Class:             resultRequeue,
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	case isRetryableHTTPStatus(r.HTTPStatus):
		return ResultDecision{
			Class:             resultRequeue,
			RecordMode:        recordFailureReconcile,
			RunScopeDetection: true,
		}, true
	default:
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}, true
	}
}

func isRetryableHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusPreconditionFailed ||
		status == http.StatusNotFound ||
		status == http.StatusLocked
}

func classifyLocalResult(r *synctypes.WorkerResult) ResultDecision {
	switch {
	case errors.Is(r.Err, driveops.ErrDiskFull):
		return ResultDecision{
			Class:      resultScopeBlock,
			ScopeKey:   synctypes.SKDiskLocal(),
			RecordMode: recordFailureReconcile,
		}
	case errors.Is(r.Err, driveops.ErrFileTooLargeForSpace):
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}
	case errors.Is(r.Err, os.ErrPermission):
		return ResultDecision{
			Class:          resultSkip,
			RecordMode:     recordFailureActionable,
			PermissionFlow: permissionFlowLocalPermission,
		}
	default:
		return ResultDecision{
			Class:      resultSkip,
			RecordMode: recordFailureActionable,
		}
	}
}

func isDeleteLikeSyncStatus(status synctypes.SyncStatus) bool {
	return status == synctypes.SyncStatusDeleted ||
		status == synctypes.SyncStatusDeleting ||
		status == synctypes.SyncStatusDeleteFailed ||
		status == synctypes.SyncStatusPendingDelete
}

func isResolvedRemoteSyncStatus(status synctypes.SyncStatus) bool {
	return status == synctypes.SyncStatusSynced ||
		status == synctypes.SyncStatusDeleted ||
		status == synctypes.SyncStatusFiltered
}

// deriveScopeKey maps a worker result to its typed scope key. Delegates to
// synctypes.ScopeKeyForStatus — single source of truth for HTTP status → scope key
// mapping. Returns the zero-value synctypes.ScopeKey for non-scope statuses.
func deriveScopeKey(r *synctypes.WorkerResult) synctypes.ScopeKey {
	return synctypes.ScopeKeyForStatus(r.HTTPStatus, r.ShortcutKey)
}

// issueTypeForHTTPStatus maps an HTTP status code and error to a sync
// failure issue type. Used by recordFailure to populate the issue_type
// column. Returns empty string for generic/unknown failures.
func issueTypeForHTTPStatus(httpStatus int, err error) string {
	switch {
	case httpStatus == http.StatusUnauthorized:
		return synctypes.IssueUnauthorized
	case httpStatus == http.StatusTooManyRequests:
		return synctypes.IssueRateLimited
	case httpStatus == http.StatusInsufficientStorage:
		return synctypes.IssueQuotaExceeded
	case httpStatus == http.StatusForbidden:
		return synctypes.IssuePermissionDenied
	case httpStatus >= http.StatusInternalServerError:
		return synctypes.IssueServiceOutage
	case httpStatus == http.StatusRequestTimeout:
		return "request_timeout"
	case httpStatus == http.StatusPreconditionFailed:
		return "transient_conflict"
	case httpStatus == http.StatusNotFound:
		return "transient_not_found"
	case httpStatus == http.StatusLocked:
		return "resource_locked"
	case errors.Is(err, driveops.ErrDiskFull):
		return synctypes.IssueDiskFull
	case errors.Is(err, driveops.ErrFileTooLargeForSpace):
		return synctypes.IssueFileTooLargeForSpace
	case errors.Is(err, os.ErrPermission):
		return synctypes.IssueLocalPermissionDenied
	default:
		return ""
	}
}

// directionFromAction maps a synctypes.ActionType to a typed Direction enum.
func directionFromAction(at synctypes.ActionType) synctypes.Direction {
	switch at {
	case synctypes.ActionUpload:
		return synctypes.DirectionUpload
	case synctypes.ActionDownload, synctypes.ActionFolderCreate, synctypes.ActionConflict:
		return synctypes.DirectionDownload
	case synctypes.ActionLocalDelete, synctypes.ActionRemoteDelete:
		return synctypes.DirectionDelete
	case synctypes.ActionLocalMove, synctypes.ActionRemoteMove,
		synctypes.ActionUpdateSynced, synctypes.ActionCleanup:
		return synctypes.DirectionDownload
	}
	return synctypes.DirectionDownload
}
