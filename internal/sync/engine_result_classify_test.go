package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.10.4, R-2.10.7, R-2.10.8, R-2.11.7
func TestIssueTypeForHTTPResult_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-http")
	testCases := []struct {
		name   string
		result *ActionCompletion
		want   IssueType
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "unauthorized", result: &ActionCompletion{HTTPStatus: http.StatusUnauthorized}, want: IssueUnauthorized, ok: true},
		{
			name: "path too long for OneDrive",
			result: &ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: &graph.GraphError{
				StatusCode: http.StatusBadRequest,
				Message:    "The specified file or folder name is too long. The URL path for all files and folders must be 400 characters or less.",
				Err:        graph.ErrBadRequest,
			}},
			want: IssuePathTooLong,
			ok:   true,
		},
		{name: "other bad request", result: &ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: graph.ErrBadRequest}, want: "", ok: false},
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
		{name: "request timeout", result: &ActionCompletion{HTTPStatus: http.StatusRequestTimeout}, want: IssueRequestTimeout, ok: true},
		{name: "precondition failed", result: &ActionCompletion{HTTPStatus: http.StatusPreconditionFailed}, want: IssueTransientConflict, ok: true},
		{name: "not found", result: &ActionCompletion{HTTPStatus: http.StatusNotFound}, want: IssueTransientNotFound, ok: true},
		{name: "locked", result: &ActionCompletion{HTTPStatus: http.StatusLocked}, want: IssueResourceLocked, ok: true},
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

// Validates: R-2.10.4, R-6.2.5, R-2.11.8
func TestIssueTypeForFilesystemResult_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-fs")
	testCases := []struct {
		name   string
		result *ActionCompletion
		want   IssueType
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "disk full", result: &ActionCompletion{Err: driveops.ErrDiskFull}, want: IssueDiskFull, ok: true},
		{name: "file too large for space", result: &ActionCompletion{Err: driveops.ErrFileTooLargeForSpace}, want: IssueFileTooLargeForSpace, ok: true},
		{name: "onedrive limit", result: &ActionCompletion{Err: driveops.ErrFileExceedsOneDriveLimit}, want: IssueFileTooLarge, ok: true},
		{
			name:   "upload stored differently",
			result: &ActionCompletion{Err: fmt.Errorf("uploading a.txt: %w", driveops.ErrUploadVerificationFailed)},
			want:   IssueUploadVerification,
			ok:     true,
		},
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
		{
			name: "name too long for local filesystem",
			result: &ActionCompletion{
				Err: &os.PathError{Op: "open", Path: "very-long-name.partial", Err: syscall.ENAMETOOLONG},
			},
			want: IssueLocalNameTooLong,
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
				Persistence:   PersistNone,
				RecordSuccess: true,
				TrialHint:     TrialHintRelease,
			},
		},
		{
			name: "shutdown",
			in:   &ActionCompletion{Err: context.Canceled, RunEnded: true},
			want: ResultDecision{
				Class:        resultShutdown,
				ConditionKey: "",
				TrialHint:    TrialHintShutdown,
			},
		},
		{
			// Validates: R-6.8.29
			name: "a cancel while the run goes on is retried",
			in:   &ActionCompletion{Err: context.Canceled},
			want: ResultDecision{
				Class:        resultRequeue,
				ConditionKey: ConditionUnexpectedCondition,
				Persistence:  PersistRetryWork,
				TrialHint:    TrialHintReclassify,
			},
		},
		{
			// Validates: R-6.8.29
			name: "a separate failure after the run ended keeps its class",
			in:   &ActionCompletion{Err: errors.New("disk I/O error"), RunEnded: true},
			want: ResultDecision{
				Class:        resultSkip,
				ConditionKey: ConditionUnexpectedCondition,
				Persistence:  PersistRetryWork,
				TrialHint:    TrialHintReclassify,
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
				Persistence:   PersistNone,
				TrialHint:     TrialHintFatal,
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
				Persistence:   PersistRetryWork,
				TrialHint:     TrialHintReclassify,
				ConditionType: IssueRemoteReadDenied,
			},
		},
		{
			name: "target throttle",
			in: &ActionCompletion{
				HTTPStatus: http.StatusTooManyRequests,
				ActionType: ActionUpload,
				Path:       "retry.txt",
				DriveID:    driveID,
			},
			want: ResultDecision{
				Class:             resultBlockScope,
				ConditionKey:      ConditionRateLimited,
				ScopeKey:          SKThrottleDrive(driveID),
				ScopeEvidence:     SKThrottleDrive(driveID),
				Persistence:       PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         TrialHintExtendOnMatchingScope,
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
				Persistence:       PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         TrialHintExtendOnMatchingScope,
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
				Persistence:       PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         TrialHintExtendOnMatchingScope,
				ConditionType:     IssueServiceOutage,
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
		in   *ActionCompletion
		want ResultDecision
	}{
		{
			name: "precondition changed",
			in:   &ActionCompletion{Err: ErrActionPreconditionChanged},
			want: ResultDecision{
				Class:        resultSuperseded,
				ConditionKey: "",
				Persistence:  PersistNone,
				TrialHint:    TrialHintReclassify,
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
				Persistence:   PersistRetryWork,
				TrialHint:     TrialHintExtendOnMatchingScope,
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
				Class:         resultSkip,
				ConditionKey:  ConditionLocalReadDenied,
				Persistence:   PersistRetryWork,
				TrialHint:     TrialHintReclassify,
				ConditionType: IssueLocalReadDenied,
			},
		},
	})
}

// Validates: R-2.8.7, R-5.6.2, R-5.6.14
//
// A stale precondition is superseded whatever Graph answer travels in its
// error chain: the executor has already read that answer as "the plan is
// stale", and the replan decides from fresh truth. Classified by its status
// first, a create's 409 resolved as stale became anonymous actionable retry
// work on the durable curve, and a 412 on a conditional overwrite became
// transient_conflict retry work.
func TestClassifyResult_StalePreconditionIsSupersededWhateverStatusItCarries(t *testing.T) {
	t.Parallel()

	staleAround := func(cause error) error {
		return fmt.Errorf("%w: %w", ErrActionPreconditionChanged, cause)
	}
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "409 on a create resolved as stale",
			err: UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict,
			}),
		},
		{
			name: "409 resourceModified on a create",
			err: UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusConflict, Code: GraphCodeResourceModified, Err: graph.ErrConflict,
			}),
		},
		{
			name: "412 on a conditional overwrite",
			err: UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusPreconditionFailed, Code: GraphCodeResourceModified, Err: graph.ErrPreconditionFailed,
			}),
		},
		{name: "401", err: staleAround(&graph.GraphError{StatusCode: http.StatusUnauthorized, Err: graph.ErrUnauthorized})},
		{name: "403", err: staleAround(&graph.GraphError{StatusCode: http.StatusForbidden, Err: graph.ErrForbidden})},
		{name: "404", err: staleAround(&graph.GraphError{StatusCode: http.StatusNotFound, Err: graph.ErrNotFound})},
		{name: "423", err: staleAround(&graph.GraphError{StatusCode: http.StatusLocked, Err: graph.ErrLocked})},
		{name: "429", err: staleAround(&graph.GraphError{StatusCode: http.StatusTooManyRequests, Err: graph.ErrThrottled})},
		{name: "503", err: staleAround(&graph.GraphError{StatusCode: http.StatusServiceUnavailable, Err: graph.ErrServerError})},
		{name: "507", err: staleAround(&graph.GraphError{StatusCode: http.StatusInsufficientStorage, Err: graph.ErrServerError})},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			require.ErrorIs(t, tc.err, ErrActionPreconditionChanged)
			r := &ActionCompletion{
				Path:       "stale.txt",
				ActionType: ActionUpload,
				DriveID:    driveid.New("drive-classify-stale"),
				Err:        tc.err,
				HTTPStatus: ExtractHTTPStatus(tc.err),
			}
			require.NotZero(t, r.HTTPStatus, "the Graph status travels with the stale error")

			assert.Equal(t, ResultDecision{
				Class:       resultSuperseded,
				Persistence: PersistNone,
				TrialHint:   TrialHintReclassify,
			}, classifyResult(r))
		})
	}
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
	assert.Equal(t, ConditionKey(""), ConditionKeyForRuntimeResult(errclass.ClassSuperseded, ""))
}

// A transient runtime condition is an expected failure that is retried: its
// result has no condition family. It used to be logged with condition_key
// unexpected_condition, the key of a failure nothing explains.
// Validates: R-6.6.13, R-6.8.16
func TestRuntimeConditionKey_TransientConditionsHaveNoFamily(t *testing.T) {
	t.Parallel()

	for _, condition := range []IssueType{
		IssueRequestTimeout, IssueTransientConflict, IssueTransientNotFound, IssueResourceLocked, IssueTransferURLRejected,
	} {
		assert.Empty(t, ConditionKeyForRuntimeResult(resultRequeue, condition), "%q", condition)
	}

	// The classifier gives a 408 such a condition, so its decision carries no key.
	timedOut := classifyResult(&ActionCompletion{
		HTTPStatus: http.StatusRequestTimeout,
		Err:        errors.New("the request timed out"),
	})
	assert.Equal(t, resultRequeue, timedOut.Class)
	assert.Equal(t, IssueRequestTimeout, timedOut.ConditionType)
	assert.Empty(t, timedOut.ConditionKey)
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
		HTTPStatus: http.StatusTooManyRequests,
		DriveID:    driveID,
	}))
	assert.Equal(t, SKService(), deriveScopeKey(&ActionCompletion{
		HTTPStatus: http.StatusServiceUnavailable,
	}))
	assert.Equal(t, ScopeKey{}, deriveScopeKey(&ActionCompletion{HTTPStatus: http.StatusForbidden}))
	assert.Equal(t, "drive:"+driveID.String(), (&ActionCompletion{DriveID: driveID}).ThrottleTargetKey())
	assert.Empty(t, (&ActionCompletion{}).ThrottleTargetKey())
}

// Validates: R-2.10.60, R-2.10.61, R-2.10.62
func TestClassifyResult_LocalErrnoTable(t *testing.T) {
	t.Parallel()

	pathErr := func(errno syscall.Errno) error {
		return &os.PathError{Op: "write", Path: "/sync/docs/report.pdf", Err: errno}
	}
	testCases := []struct {
		name      string
		err       error
		class     errclass.Class
		scopeKey  ScopeKey
		issueType IssueType
	}{
		{name: "enospc mid-write is the disk scope", err: pathErr(syscall.ENOSPC), class: resultBlockScope, scopeKey: SKDiskLocal(), issueType: IssueDiskFull},
		{name: "edquot is the disk scope", err: pathErr(syscall.EDQUOT), class: resultBlockScope, scopeKey: SKDiskLocal(), issueType: IssueDiskFull},
		{name: "erofs is named read-only", err: pathErr(syscall.EROFS), class: resultSkip, issueType: IssueLocalReadOnly},
		{name: "efbig is a per-item issue", err: pathErr(syscall.EFBIG), class: resultSkip, issueType: IssueLocalFileTooLarge},
		{name: "exdev is a per-item issue", err: pathErr(syscall.EXDEV), class: resultSkip, issueType: IssueLocalCrossDevice},
		{name: "ebusy is retried", err: pathErr(syscall.EBUSY), class: resultRequeue, issueType: IssueLocalBusy},
		{name: "etxtbsy is retried", err: pathErr(syscall.ETXTBSY), class: resultRequeue, issueType: IssueLocalBusy},
		{name: "estale is retried", err: pathErr(syscall.ESTALE), class: resultRequeue, issueType: IssueLocalBusy},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(&ActionCompletion{Err: tc.err, ActionType: ActionDownload, Path: "docs/report.pdf"})
			assert.Equal(t, tc.class, got.Class)
			assert.Equal(t, tc.scopeKey, got.ScopeKey)
			assert.Equal(t, tc.issueType, got.ConditionType)
			assert.NotEqual(t, ConditionUnexpectedCondition, got.ConditionKey,
				"a recognized errno must never be reported as an unexpected failure")
		})
	}
}

// Validates: R-2.10.61
func TestShouldHandleLocalPermission_IncludesReadOnlyFilesystem(t *testing.T) {
	t.Parallel()

	assert.True(t, shouldHandleLocalPermission(&ActionCompletion{
		Err: &os.PathError{Op: "open", Path: "/sync/a", Err: syscall.EROFS},
	}))
	assert.False(t, shouldHandleLocalPermission(&ActionCompletion{
		Err: &os.PathError{Op: "open", Path: "/sync/a", Err: syscall.EBUSY},
	}))
}

// Validates: R-2.10.64, R-2.10.65, R-2.10.66, R-2.10.67
func TestClassifyResult_ServerAnswerTable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		status    int
		err       error
		class     errclass.Class
		scopeKey  ScopeKey
		evidence  ScopeKey
		issueType IssueType
	}{
		{
			name:   "501 is a per-item refusal",
			status: http.StatusNotImplemented,
			err:    &graph.GraphError{StatusCode: http.StatusNotImplemented, Err: graph.ErrServerError},
			class:  resultSkip, issueType: IssueUnsupportedOperation,
		},
		{
			name:   "505 is a per-item refusal",
			status: http.StatusHTTPVersionNotSupported,
			err:    &graph.GraphError{StatusCode: http.StatusHTTPVersionNotSupported, Err: graph.ErrServerError},
			class:  resultSkip, issueType: IssueUnsupportedOperation,
		},
		{
			name:   "500 insufficient space is quota",
			status: http.StatusInternalServerError,
			err: &graph.GraphError{
				StatusCode: http.StatusInternalServerError, Message: "Insufficient Space Available", Err: graph.ErrServerError,
			},
			class: resultBlockScope, scopeKey: SKQuotaOwn(), evidence: SKQuotaOwn(), issueType: IssueQuotaExceeded,
		},
		{
			name:   "409 resourceModified is retried",
			status: http.StatusConflict,
			err:    &graph.GraphError{StatusCode: http.StatusConflict, Code: "resourceModified", Err: graph.ErrConflict},
			class:  resultRequeue, issueType: IssueTransientConflict,
		},
		{
			name:   "pre-1900 mtime 404 is a generic per-file failure, not gone",
			status: http.StatusNotFound,
			err:    &graph.GraphError{StatusCode: http.StatusNotFound, Code: "itemNotFound", Err: graph.ErrTimestampOutOfRange},
			class:  resultSkip, issueType: "",
		},
		{
			name:   "plain 500 is still outage evidence",
			status: http.StatusInternalServerError,
			err:    &graph.GraphError{StatusCode: http.StatusInternalServerError, Err: graph.ErrServerError},
			class:  resultRequeue, evidence: SKService(), issueType: IssueServiceOutage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(&ActionCompletion{HTTPStatus: tc.status, Err: tc.err, ActionType: ActionUpload, Path: "a.txt"})
			assert.Equal(t, tc.class, got.Class)
			assert.Equal(t, tc.scopeKey, got.ScopeKey)
			assert.Equal(t, tc.evidence, got.ScopeEvidence)
			assert.Equal(t, tc.issueType, got.ConditionType)
		})
	}
}

// Validates: R-2.10.45, R-2.10.69
func TestClassifyResult_Only401ErrUnauthorizedIsFatal(t *testing.T) {
	t.Parallel()

	rps := classifyResult(&ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err: &graph.GraphError{
			StatusCode: http.StatusUnauthorized, Message: "Unable to initialize RPS", Err: graph.ErrAuthTransient,
		},
		ActionType: ActionUpload, Path: "a.txt",
	})
	assert.Equal(t, resultRequeue, rps.Class, "a server-side auth fault must not end the watch session")
	assert.Equal(t, SKService(), rps.ScopeEvidence)
	assert.Equal(t, IssueServiceOutage, rps.ConditionType)

	preAuth := classifyResult(&ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err:        &graph.GraphError{StatusCode: http.StatusUnauthorized, Err: graph.ErrPreAuthURLRejected},
		ActionType: ActionDownload, Path: "a.txt",
	})
	assert.Equal(t, resultRequeue, preAuth.Class, "an expired transfer URL is one file's problem")
	assert.True(t, preAuth.ScopeEvidence.IsZero())
	assert.Equal(t, IssueTransferURLRejected, preAuth.ConditionType)

	signIn := classifyResult(&ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err:        &graph.GraphError{StatusCode: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
	})
	assert.Equal(t, resultFatal, signIn.Class)
	assert.Equal(t, IssueUnauthorized, signIn.ConditionType)
}

// Validates: R-2.10.70
func TestClassifyResult_MalwareIsPerFileNotReadDenied(t *testing.T) {
	t.Parallel()

	for name, completion := range map[string]*ActionCompletion{
		"facet refusal before any request": {Err: graph.ErrMalwareDetected},
		"content host 403": {
			HTTPStatus: http.StatusForbidden,
			Err:        &graph.GraphError{StatusCode: http.StatusForbidden, Code: "malwareDetected", Err: graph.ErrMalwareDetected},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			completion.ActionType = ActionDownload
			completion.Path = "eicar.com"
			completion.DriveID = driveid.New("drive-malware")
			got := classifyResult(completion)
			assert.Equal(t, resultSkip, got.Class)
			assert.Equal(t, IssueMalwareDetected, got.ConditionType, "not remote_read_denied")
			assert.True(t, got.ScopeEvidence.IsZero())
			assert.False(t, shouldHandleRemote403Permission(completion), "no permission probe for blocked content")
		})
	}
}

// Validates: R-6.2.16
//
// The engine's decisions on the action type panic with OutsideTheEnum on a
// value outside the enum, like the vocabulary's
// (TestActionTypeDecisions_PanicOnATypeOutsideTheEnum): no activity, no
// capability.
func TestEngineActionTypeDecisions_PanicOnATypeOutsideTheEnum(t *testing.T) {
	t.Parallel()

	const unlisted = ActionCleanup + 1
	completion := &ActionCompletion{ActionType: unlisted, Path: "docs/a.txt"}

	decisions := map[string]func(){
		"activityKindFor":                     func() { activityKindFor(unlisted) },
		"effectiveRemotePermissionCapability": func() { effectiveRemotePermissionCapability(completion) },
		"effectiveLocalPermissionCapability":  func() { effectiveLocalPermissionCapability(completion) },
	}
	for name, decide := range decisions {
		assert.PanicsWithValue(t, OutsideTheEnum(unlisted), decide, name)
	}
}
