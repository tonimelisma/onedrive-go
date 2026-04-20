package sync

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
)

// Validates: R-2.10.4, R-2.10.7, R-2.10.8
func TestIssueTypeForHTTPResult_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-http")
	testCases := []struct {
		name   string
		result *ActionCompletion
		want   string
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "unauthorized", result: &ActionCompletion{HTTPStatus: http.StatusUnauthorized}, want: IssueUnauthorized, ok: true},
		{name: "rate limited", result: &ActionCompletion{HTTPStatus: http.StatusTooManyRequests}, want: IssueRateLimited, ok: true},
		{name: "quota exceeded", result: &ActionCompletion{HTTPStatus: http.StatusInsufficientStorage}, want: IssueQuotaExceeded, ok: true},
		{
			name: "forbidden remote read",
			result: &ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: IssueRemoteReadDenied,
			ok:   true,
		},
		{
			name: "forbidden remote write",
			result: &ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: IssueRemoteWriteDenied,
			ok:   true,
		},
		{name: "service outage", result: &ActionCompletion{HTTPStatus: http.StatusBadGateway}, want: IssueServiceOutage, ok: true},
		{name: "request timeout", result: &ActionCompletion{HTTPStatus: http.StatusRequestTimeout}, want: "request_timeout", ok: true},
		{name: "precondition failed", result: &ActionCompletion{HTTPStatus: http.StatusPreconditionFailed}, want: "transient_conflict", ok: true},
		{name: "not found", result: &ActionCompletion{HTTPStatus: http.StatusNotFound}, want: "transient_not_found", ok: true},
		{name: "locked", result: &ActionCompletion{HTTPStatus: http.StatusLocked}, want: "resource_locked", ok: true},
		{name: "unmapped", result: &ActionCompletion{HTTPStatus: http.StatusTeapot}, want: "", ok: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := issueTypeForHTTPResult(tc.result)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Validates: R-2.10.4, R-6.2.5
func TestIssueTypeForFilesystemResult_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-fs")
	testCases := []struct {
		name   string
		result *ActionCompletion
		want   string
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "disk full", result: &ActionCompletion{Err: driveops.ErrDiskFull}, want: IssueDiskFull, ok: true},
		{name: "file too large for space", result: &ActionCompletion{Err: driveops.ErrFileTooLargeForSpace}, want: IssueFileTooLargeForSpace, ok: true},
		{name: "onedrive limit", result: &ActionCompletion{Err: driveops.ErrFileExceedsOneDriveLimit}, want: IssueFileTooLarge, ok: true},
		{
			name: "local read denied",
			result: &ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: IssueLocalReadDenied,
			ok:   true,
		},
		{
			name: "local write denied",
			result: &ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: IssueLocalWriteDenied,
			ok:   true,
		},
		{name: "unmapped", result: &ActionCompletion{Err: errors.New("no mapping")}, want: "", ok: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := issueTypeForFilesystemResult(tc.result)
			assert.Equal(t, tc.ok, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func assertClassifyResultCases(
	t *testing.T,
	testCases []struct {
		name string
		in   *ActionCompletion
		want ResultDecision
	},
) {
	t.Helper()

	for i := range testCases {
		tc := &testCases[i]
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(tc.in)
			assert.Equal(t, tc.want, got)
		})
	}
}

// Validates: R-2.10.4, R-2.10.7, R-2.10.8, R-6.2.5
func TestClassifyResult_SuccessAndShutdown(t *testing.T) {
	t.Parallel()

	assertClassifyResultCases(t, []struct {
		name string
		in   *ActionCompletion
		want ResultDecision
	}{
		{
			name: "success",
			in:   &ActionCompletion{Success: true},
			want: ResultDecision{
				Class:         resultSuccess,
				ConditionKey:  "",
				Persistence:   persistNone,
				RecordSuccess: true,
				TrialHint:     trialHintRelease,
			},
		},
		{
			name: "shutdown",
			in:   &ActionCompletion{Err: context.Canceled},
			want: ResultDecision{
				Class:        resultShutdown,
				ConditionKey: "",
				TrialHint:    trialHintShutdown,
			},
		},
	})
}

// Validates: R-2.10.4, R-2.10.7, R-2.10.8
func TestClassifyResult_HTTPPersistenceAndScopeRouting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-classify-http")

	assertClassifyResultCases(t, []struct {
		name string
		in   *ActionCompletion
		want ResultDecision
	}{
		{
			name: "unauthorized",
			in:   &ActionCompletion{HTTPStatus: http.StatusUnauthorized},
			want: ResultDecision{
				Class:         resultFatal,
				ConditionKey:  ConditionAuthenticationRequired,
				Persistence:   persistNone,
				TrialHint:     trialHintFatal,
				ConditionType: IssueUnauthorized,
			},
		},
		{
			name: "forbidden download",
			in: &ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: ResultDecision{
				Class:         resultSkip,
				ConditionKey:  ConditionRemoteReadDenied,
				Persistence:   persistRetryWork,
				TrialHint:     trialHintPreserve,
				ConditionType: IssueRemoteReadDenied,
			},
		},
		{
			name: "target throttle",
			in: &ActionCompletion{
				HTTPStatus:    http.StatusTooManyRequests,
				ActionType:    ActionUpload,
				Path:          "retry.txt",
				TargetDriveID: driveID,
			},
			want: ResultDecision{
				Class:             resultBlockScope,
				ConditionKey:      ConditionRateLimited,
				ScopeKey:          SKThrottleDrive(driveID),
				ScopeEvidence:     SKThrottleDrive(driveID),
				Persistence:       persistRetryWork,
				RunScopeDetection: true,
				TrialHint:         trialHintExtendOnMatchingScope,
				ConditionType:     IssueRateLimited,
			},
		},
		{
			name: "quota exceeded",
			in: &ActionCompletion{
				HTTPStatus: http.StatusInsufficientStorage,
				ActionType: ActionUpload,
				Path:       "quota.txt",
				DriveID:    driveID,
			},
			want: ResultDecision{
				Class:             resultBlockScope,
				ConditionKey:      ConditionQuotaExceeded,
				ScopeKey:          SKQuotaOwn(),
				ScopeEvidence:     SKQuotaOwn(),
				Persistence:       persistRetryWork,
				RunScopeDetection: true,
				TrialHint:         trialHintExtendOnMatchingScope,
				ConditionType:     IssueQuotaExceeded,
			},
		},
		{
			name: "service outage",
			in: &ActionCompletion{
				HTTPStatus: http.StatusBadGateway,
				ActionType: ActionUpload,
				Path:       "service.txt",
				DriveID:    driveID,
			},
			want: ResultDecision{
				Class:             resultRequeue,
				ConditionKey:      ConditionServiceOutage,
				ScopeEvidence:     SKService(),
				Persistence:       persistRetryWork,
				RunScopeDetection: true,
				TrialHint:         trialHintExtendOnMatchingScope,
				ConditionType:     IssueServiceOutage,
			},
		},
	})
}

// Validates: R-2.10.4, R-6.2.5
func TestClassifyResult_LocalPersistenceAndScopeRouting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-classify-local")

	assertClassifyResultCases(t, []struct {
		name string
		in   *ActionCompletion
		want ResultDecision
	}{
		{
			name: "precondition changed",
			in:   &ActionCompletion{Err: ErrActionPreconditionChanged},
			want: ResultDecision{
				Class:         resultRequeue,
				ConditionKey:  ConditionUnexpectedCondition,
				Persistence:   persistRetryWork,
				TrialHint:     trialHintPreserve,
				ConditionType: "transient_conflict",
			},
		},
		{
			name: "disk full",
			in:   &ActionCompletion{Err: driveops.ErrDiskFull},
			want: ResultDecision{
				Class:         resultBlockScope,
				ConditionKey:  ConditionDiskFull,
				ScopeKey:      SKDiskLocal(),
				ScopeEvidence: SKDiskLocal(),
				Persistence:   persistRetryWork,
				TrialHint:     trialHintExtendOnMatchingScope,
				ConditionType: IssueDiskFull,
			},
		},
		{
			name: "permission denied upload",
			in: &ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionUpload,
				Path:       "local.txt",
				DriveID:    driveID,
			},
			want: ResultDecision{
				Class:          resultSkip,
				ConditionKey:   ConditionLocalReadDenied,
				Persistence:    persistRetryWork,
				PermissionFlow: permissionFlowLocalPermission,
				TrialHint:      trialHintPreserve,
				ConditionType:  IssueLocalReadDenied,
			},
		},
	})
}

// Validates: R-2.10.4
func TestRuntimeConditionKey_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionInvalidFilename, ConditionKeyForRuntimeResult(resultSkip, IssueInvalidFilename))
	assert.Equal(t, ConditionRateLimited, ConditionKeyForRuntimeResult(resultBlockScope, IssueRateLimited))
	assert.Equal(t, ConditionRemoteWriteDenied, ConditionKeyForRuntimeResult(resultSkip, IssueRemoteWriteDenied))
	assert.Equal(t, ConditionUnexpectedCondition, ConditionKeyForRuntimeResult(errclass.ClassFatal, "mystery"))
	assert.Equal(t, ConditionUnexpectedCondition, ConditionKeyForRuntimeResult(errclass.ClassActionable, ""))
	assert.Equal(t, ConditionKey(""), ConditionKeyForRuntimeResult(errclass.ClassSuccess, ""))
}

// Validates: R-2.10.4
func TestPermissionCapabilityFallbacks(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-cap")
	assert.Equal(t, PermissionCapabilityUnknown, effectiveRemotePermissionCapability(nil))
	assert.Equal(t, PermissionCapabilityUnknown, effectiveLocalPermissionCapability(nil))

	require.Equal(t,
		PermissionCapabilityRemoteRead,
		effectiveRemotePermissionCapability(&ActionCompletion{
			ActionType: ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		PermissionCapabilityRemoteWrite,
		effectiveRemotePermissionCapability(&ActionCompletion{
			ActionType: ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		PermissionCapabilityLocalRead,
		effectiveLocalPermissionCapability(&ActionCompletion{
			ActionType: ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		PermissionCapabilityLocalWrite,
		effectiveLocalPermissionCapability(&ActionCompletion{
			ActionType: ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)

	assert.Equal(t, IssueRemoteWriteDenied, issueTypeForForbiddenResult(&ActionCompletion{
		FailureCapability: PermissionCapabilityRemoteWrite,
	}))
	assert.Equal(t, IssueLocalWriteDenied, issueTypeForLocalPermissionResult(&ActionCompletion{
		FailureCapability: PermissionCapabilityUnknown,
		ActionType:        ActionDownload,
		Path:              "download.txt",
		DriveID:           driveID,
	}))
	assert.Equal(t, SKThrottleDrive(driveID), deriveScopeKey(&ActionCompletion{
		HTTPStatus:    http.StatusTooManyRequests,
		TargetDriveID: driveID,
	}))
	assert.Equal(t, SKService(), deriveScopeKey(&ActionCompletion{
		HTTPStatus: http.StatusServiceUnavailable,
	}))
	assert.Equal(t, ScopeKey{}, deriveScopeKey(&ActionCompletion{HTTPStatus: http.StatusForbidden}))
	assert.Equal(t, "drive:"+driveID.String(), (&ActionCompletion{TargetDriveID: driveID}).ThrottleTargetKey())
	assert.Empty(t, (&ActionCompletion{}).ThrottleTargetKey())
}
