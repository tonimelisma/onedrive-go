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
		result *actionCompletion
		want   string
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "unauthorized", result: &actionCompletion{HTTPStatus: http.StatusUnauthorized}, want: IssueUnauthorized, ok: true},
		{name: "rate limited", result: &actionCompletion{HTTPStatus: http.StatusTooManyRequests}, want: issueRateLimited, ok: true},
		{name: "quota exceeded", result: &actionCompletion{HTTPStatus: http.StatusInsufficientStorage}, want: IssueQuotaExceeded, ok: true},
		{
			name: "forbidden remote read",
			result: &actionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: issueRemoteReadDenied,
			ok:   true,
		},
		{
			name: "forbidden remote write",
			result: &actionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: IssueRemoteWriteDenied,
			ok:   true,
		},
		{name: "service outage", result: &actionCompletion{HTTPStatus: http.StatusBadGateway}, want: issueServiceOutage, ok: true},
		{name: "request timeout", result: &actionCompletion{HTTPStatus: http.StatusRequestTimeout}, want: "request_timeout", ok: true},
		{name: "precondition failed", result: &actionCompletion{HTTPStatus: http.StatusPreconditionFailed}, want: "transient_conflict", ok: true},
		{name: "not found", result: &actionCompletion{HTTPStatus: http.StatusNotFound}, want: "transient_not_found", ok: true},
		{name: "locked", result: &actionCompletion{HTTPStatus: http.StatusLocked}, want: "resource_locked", ok: true},
		{name: "unmapped", result: &actionCompletion{HTTPStatus: http.StatusTeapot}, want: "", ok: false},
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
		result *actionCompletion
		want   string
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "disk full", result: &actionCompletion{Err: driveops.ErrDiskFull}, want: issueDiskFull, ok: true},
		{name: "file too large for space", result: &actionCompletion{Err: driveops.ErrFileTooLargeForSpace}, want: issueFileTooLargeForSpace, ok: true},
		{name: "onedrive limit", result: &actionCompletion{Err: driveops.ErrFileExceedsOneDriveLimit}, want: issueFileTooLarge, ok: true},
		{
			name: "local read denied",
			result: &actionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: issueLocalReadDenied,
			ok:   true,
		},
		{
			name: "local write denied",
			result: &actionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: issueLocalWriteDenied,
			ok:   true,
		},
		{name: "unmapped", result: &actionCompletion{Err: errors.New("no mapping")}, want: "", ok: false},
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
		in   *actionCompletion
		want resultDecision
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
		in   *actionCompletion
		want resultDecision
	}{
		{
			name: "success",
			in:   &actionCompletion{Success: true},
			want: resultDecision{
				Class:         resultSuccess,
				ConditionKey:  "",
				Persistence:   persistNone,
				RecordSuccess: true,
				TrialHint:     trialHintRelease,
			},
		},
		{
			name: "shutdown",
			in:   &actionCompletion{Err: context.Canceled},
			want: resultDecision{
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
		in   *actionCompletion
		want resultDecision
	}{
		{
			name: "unauthorized",
			in:   &actionCompletion{HTTPStatus: http.StatusUnauthorized},
			want: resultDecision{
				Class:         resultFatal,
				ConditionKey:  ConditionAuthenticationRequired,
				Persistence:   persistNone,
				TrialHint:     trialHintFatal,
				ConditionType: IssueUnauthorized,
			},
		},
		{
			name: "forbidden download",
			in: &actionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			},
			want: resultDecision{
				Class:         resultSkip,
				ConditionKey:  ConditionRemoteReadDenied,
				Persistence:   persistRetryWork,
				TrialHint:     trialHintReclassify,
				ConditionType: issueRemoteReadDenied,
			},
		},
		{
			name: "target throttle",
			in: &actionCompletion{
				HTTPStatus: http.StatusTooManyRequests,
				ActionType: ActionUpload,
				Path:       "retry.txt",
				DriveID:    driveID,
			},
			want: resultDecision{
				Class:             resultBlockScope,
				ConditionKey:      ConditionRateLimited,
				ScopeKey:          SKThrottleDrive(driveID),
				ScopeEvidence:     SKThrottleDrive(driveID),
				Persistence:       persistRetryWork,
				RunScopeDetection: true,
				TrialHint:         trialHintExtendOnMatchingScope,
				ConditionType:     issueRateLimited,
			},
		},
		{
			name: "quota exceeded",
			in: &actionCompletion{
				HTTPStatus: http.StatusInsufficientStorage,
				ActionType: ActionUpload,
				Path:       "quota.txt",
				DriveID:    driveID,
			},
			want: resultDecision{
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
			in: &actionCompletion{
				HTTPStatus: http.StatusBadGateway,
				ActionType: ActionUpload,
				Path:       "service.txt",
				DriveID:    driveID,
			},
			want: resultDecision{
				Class:             resultRequeue,
				ConditionKey:      ConditionServiceOutage,
				ScopeEvidence:     SKService(),
				Persistence:       persistRetryWork,
				RunScopeDetection: true,
				TrialHint:         trialHintExtendOnMatchingScope,
				ConditionType:     issueServiceOutage,
			},
		},
	})
}

// Validates: R-2.8.7, R-2.10.4, R-6.2.5
func TestClassifyResult_LocalPersistenceAndScopeRouting(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-classify-local")

	assertClassifyResultCases(t, []struct {
		name string
		in   *actionCompletion
		want resultDecision
	}{
		{
			name: "precondition changed",
			in:   &actionCompletion{Err: errActionPreconditionChanged},
			want: resultDecision{
				Class:        resultSuperseded,
				ConditionKey: "",
				Persistence:  persistNone,
				TrialHint:    trialHintReclassify,
			},
		},
		{
			name: "disk full",
			in:   &actionCompletion{Err: driveops.ErrDiskFull},
			want: resultDecision{
				Class:         resultBlockScope,
				ConditionKey:  ConditionDiskFull,
				ScopeKey:      SKDiskLocal(),
				ScopeEvidence: SKDiskLocal(),
				Persistence:   persistRetryWork,
				TrialHint:     trialHintExtendOnMatchingScope,
				ConditionType: issueDiskFull,
			},
		},
		{
			name: "permission denied upload",
			in: &actionCompletion{
				Err:        os.ErrPermission,
				ActionType: ActionUpload,
				Path:       "local.txt",
				DriveID:    driveID,
			},
			want: resultDecision{
				Class:         resultSkip,
				ConditionKey:  ConditionLocalReadDenied,
				Persistence:   persistRetryWork,
				TrialHint:     trialHintReclassify,
				ConditionType: issueLocalReadDenied,
			},
		},
	})
}

// Validates: R-2.10.4
func TestRuntimeConditionKey_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, ConditionInvalidFilename, conditionKeyForRuntimeResult(resultSkip, IssueInvalidFilename))
	assert.Equal(t, ConditionRateLimited, conditionKeyForRuntimeResult(resultBlockScope, issueRateLimited))
	assert.Equal(t, ConditionRemoteWriteDenied, conditionKeyForRuntimeResult(resultSkip, IssueRemoteWriteDenied))
	assert.Equal(t, ConditionUnexpectedCondition, conditionKeyForRuntimeResult(errclass.ClassFatal, "mystery"))
	assert.Equal(t, ConditionUnexpectedCondition, conditionKeyForRuntimeResult(errclass.ClassActionable, ""))
	assert.Equal(t, ConditionKey(""), conditionKeyForRuntimeResult(errclass.ClassSuccess, ""))
	assert.Equal(t, ConditionKey(""), conditionKeyForRuntimeResult(errclass.ClassSuperseded, ""))
}

// Validates: R-2.10.4
func TestPermissionCapabilityFallbacks(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-cap")
	assert.Equal(t, permissionCapabilityUnknown, effectiveRemotePermissionCapability(nil))
	assert.Equal(t, permissionCapabilityUnknown, effectiveLocalPermissionCapability(nil))

	require.Equal(t,
		permissionCapabilityRemoteRead,
		effectiveRemotePermissionCapability(&actionCompletion{
			ActionType: ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		permissionCapabilityRemoteWrite,
		effectiveRemotePermissionCapability(&actionCompletion{
			ActionType: ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		permissionCapabilityLocalRead,
		effectiveLocalPermissionCapability(&actionCompletion{
			ActionType: ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		permissionCapabilityLocalWrite,
		effectiveLocalPermissionCapability(&actionCompletion{
			ActionType: ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)

	assert.Equal(t, IssueRemoteWriteDenied, issueTypeForForbiddenResult(&actionCompletion{
		FailureCapability: permissionCapabilityRemoteWrite,
	}))
	assert.Equal(t, issueLocalWriteDenied, issueTypeForLocalPermissionResult(&actionCompletion{
		FailureCapability: permissionCapabilityUnknown,
		ActionType:        ActionDownload,
		Path:              "download.txt",
		DriveID:           driveID,
	}))
	assert.Equal(t, SKThrottleDrive(driveID), deriveScopeKey(&actionCompletion{
		HTTPStatus: http.StatusTooManyRequests,
		DriveID:    driveID,
	}))
	assert.Equal(t, SKService(), deriveScopeKey(&actionCompletion{
		HTTPStatus: http.StatusServiceUnavailable,
	}))
	assert.Equal(t, ScopeKey{}, deriveScopeKey(&actionCompletion{HTTPStatus: http.StatusForbidden}))
	assert.Equal(t, "drive:"+driveID.String(), (&actionCompletion{DriveID: driveID}).ThrottleTargetKey())
	assert.Empty(t, (&actionCompletion{}).ThrottleTargetKey())
}
