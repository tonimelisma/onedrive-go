package sync

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
)

// Validates: R-2.10.4, R-2.10.7, R-2.10.8, R-2.11.7
func TestIssueTypeForHTTPResult_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-http")
	testCases := []struct {
		name   string
		result *syncdomain.ActionCompletion
		want   syncdomain.IssueType
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "unauthorized", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusUnauthorized}), want: syncdomain.IssueUnauthorized, ok: true},
		{
			name: "path too long for OneDrive",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: &graph.GraphError{
				StatusCode: http.StatusBadRequest,
				Message:    "The specified file or folder name is too long. The URL path for all files and folders must be 400 characters or less.",
				Err:        graph.ErrBadRequest,
			}}),
			want: syncdomain.IssuePathTooLong,
			ok:   true,
		},
		{name: "other bad request", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusBadRequest, Err: graph.ErrBadRequest}), want: "", ok: false},
		{name: "rate limited", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusTooManyRequests}), want: syncdomain.IssueRateLimited, ok: true},
		{name: "quota exceeded", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusInsufficientStorage}), want: syncdomain.IssueQuotaExceeded, ok: true},
		{
			name: "forbidden remote read",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: syncdomain.ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.IssueRemoteReadDenied,
			ok:   true,
		},
		{
			name: "forbidden remote write",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: syncdomain.ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.IssueRemoteWriteDenied,
			ok:   true,
		},
		{name: "service outage", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusBadGateway}), want: syncdomain.IssueServiceOutage, ok: true},
		{name: "request timeout", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusRequestTimeout}), want: syncdomain.IssueRequestTimeout, ok: true},
		{name: "precondition failed", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusPreconditionFailed}), want: syncdomain.IssueTransientConflict, ok: true},
		{name: "not found", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusNotFound}), want: syncdomain.IssueTransientNotFound, ok: true},
		{name: "locked", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusLocked}), want: syncdomain.IssueResourceLocked, ok: true},
		{name: "unmapped", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusTeapot}), want: "", ok: false},
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
		result *syncdomain.ActionCompletion
		want   syncdomain.IssueType
		ok     bool
	}{
		{name: "nil", result: nil, want: "", ok: false},
		{name: "disk full", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: driveops.ErrDiskFull}), want: syncdomain.IssueDiskFull, ok: true},
		{name: "file too large for space", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: driveops.ErrFileTooLargeForSpace}), want: syncdomain.IssueFileTooLargeForSpace, ok: true},
		{name: "onedrive limit", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: driveops.ErrFileExceedsOneDriveLimit}), want: syncdomain.IssueFileTooLarge, ok: true},
		{
			name:   "upload stored differently",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: fmt.Errorf("uploading a.txt: %w", driveops.ErrUploadVerificationFailed)}),
			want:   syncdomain.IssueUploadVerification,
			ok:     true,
		},
		{
			name: "local read denied",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: syncdomain.ActionUpload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.IssueLocalReadDenied,
			ok:   true,
		},
		{
			name: "local write denied",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: syncdomain.ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.IssueLocalWriteDenied,
			ok:   true,
		},
		{
			name: "name too long for local filesystem",
			result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Err: &os.PathError{Op: "open", Path: "very-long-name.partial", Err: syscall.ENAMETOOLONG},
			}),
			want: syncdomain.IssueLocalNameTooLong,
			ok:   true,
		},
		{name: "unmapped", result: syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: errors.New("no mapping")}), want: "", ok: false},
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
		in   *syncdomain.ActionCompletion
		want syncdomain.ResultDecision
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
		in   *syncdomain.ActionCompletion
		want syncdomain.ResultDecision
	}{
		{
			name: "success",
			in:   &syncdomain.ActionCompletion{Success: true},
			want: syncdomain.ResultDecision{
				Class:         resultSuccess,
				ConditionKey:  "",
				Persistence:   syncdomain.PersistNone,
				RecordSuccess: true,
				TrialHint:     syncdomain.TrialHintRelease,
			},
		},
		{
			name: "shutdown",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: context.Canceled, RunEnded: true}),
			want: syncdomain.ResultDecision{
				Class:        resultShutdown,
				ConditionKey: "",
				TrialHint:    syncdomain.TrialHintShutdown,
			},
		},
		{
			// Validates: R-6.8.29
			name: "a cancel while the run goes on is retried",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: context.Canceled}),
			want: syncdomain.ResultDecision{
				Class:        resultRequeue,
				ConditionKey: syncdomain.ConditionUnexpectedCondition,
				Persistence:  syncdomain.PersistRetryWork,
				TrialHint:    syncdomain.TrialHintReclassify,
			},
		},
		{
			// Validates: R-6.8.29
			name: "a separate failure after the run ended keeps its class",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: errors.New("disk I/O error"), RunEnded: true}),
			want: syncdomain.ResultDecision{
				Class:        resultSkip,
				ConditionKey: syncdomain.ConditionUnexpectedCondition,
				Persistence:  syncdomain.PersistRetryWork,
				TrialHint:    syncdomain.TrialHintReclassify,
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
		in   *syncdomain.ActionCompletion
		want syncdomain.ResultDecision
	}{
		{
			name: "unauthorized",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusUnauthorized}),
			want: syncdomain.ResultDecision{
				Class:         resultFatal,
				ConditionKey:  syncdomain.ConditionAuthenticationRequired,
				Persistence:   syncdomain.PersistNone,
				TrialHint:     syncdomain.TrialHintFatal,
				ConditionType: syncdomain.IssueUnauthorized,
			},
		},
		{
			name: "forbidden download",
			in: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusForbidden,
				ActionType: syncdomain.ActionDownload,
				Path:       "blocked.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.ResultDecision{
				Class:         resultSkip,
				ConditionKey:  syncdomain.ConditionRemoteReadDenied,
				Persistence:   syncdomain.PersistRetryWork,
				TrialHint:     syncdomain.TrialHintReclassify,
				ConditionType: syncdomain.IssueRemoteReadDenied,
			},
		},
		{
			name: "target throttle",
			in: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusTooManyRequests,
				ActionType: syncdomain.ActionUpload,
				Path:       "retry.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.ResultDecision{
				Class:             resultBlockScope,
				ConditionKey:      syncdomain.ConditionRateLimited,
				ScopeKey:          syncdomain.SKThrottleDrive(driveID),
				ScopeEvidence:     syncdomain.SKThrottleDrive(driveID),
				Persistence:       syncdomain.PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
				ConditionType:     syncdomain.IssueRateLimited,
			},
		},
		{
			name: "quota exceeded",
			in: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusInsufficientStorage,
				ActionType: syncdomain.ActionUpload,
				Path:       "quota.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.ResultDecision{
				Class:             resultBlockScope,
				ConditionKey:      syncdomain.ConditionQuotaExceeded,
				ScopeKey:          syncdomain.SKQuotaOwn(),
				ScopeEvidence:     syncdomain.SKQuotaOwn(),
				Persistence:       syncdomain.PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
				ConditionType:     syncdomain.IssueQuotaExceeded,
			},
		},
		{
			name: "service outage",
			in: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				HTTPStatus: http.StatusBadGateway,
				ActionType: syncdomain.ActionUpload,
				Path:       "service.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.ResultDecision{
				Class:             resultRequeue,
				ConditionKey:      syncdomain.ConditionServiceOutage,
				ScopeEvidence:     syncdomain.SKService(),
				Persistence:       syncdomain.PersistRetryWork,
				RunScopeDetection: true,
				TrialHint:         syncdomain.TrialHintExtendOnMatchingScope,
				ConditionType:     syncdomain.IssueServiceOutage,
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
		in   *syncdomain.ActionCompletion
		want syncdomain.ResultDecision
	}{
		{
			name: "precondition changed",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: syncdomain.ErrActionPreconditionChanged}),
			want: syncdomain.ResultDecision{
				Class:        resultSuperseded,
				ConditionKey: "",
				Persistence:  syncdomain.PersistNone,
				TrialHint:    syncdomain.TrialHintReclassify,
			},
		},
		{
			name: "disk full",
			in:   syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: driveops.ErrDiskFull}),
			want: syncdomain.ResultDecision{
				Class:         resultBlockScope,
				ConditionKey:  syncdomain.ConditionDiskFull,
				ScopeKey:      syncdomain.SKDiskLocal(),
				ScopeEvidence: syncdomain.SKDiskLocal(),
				Persistence:   syncdomain.PersistRetryWork,
				TrialHint:     syncdomain.TrialHintExtendOnMatchingScope,
				ConditionType: syncdomain.IssueDiskFull,
			},
		},
		{
			name: "permission denied upload",
			in: syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Err:        os.ErrPermission,
				ActionType: syncdomain.ActionUpload,
				Path:       "local.txt",
				DriveID:    driveID,
			}),
			want: syncdomain.ResultDecision{
				Class:         resultSkip,
				ConditionKey:  syncdomain.ConditionLocalReadDenied,
				Persistence:   syncdomain.PersistRetryWork,
				TrialHint:     syncdomain.TrialHintReclassify,
				ConditionType: syncdomain.IssueLocalReadDenied,
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
		return fmt.Errorf("%w: %w", syncdomain.ErrActionPreconditionChanged, cause)
	}
	testCases := []struct {
		name string
		err  error
	}{
		{
			name: "409 on a create resolved as stale",
			err: syncexec.UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusConflict, Code: "nameAlreadyExists", Err: graph.ErrConflict,
			}),
		},
		{
			name: "409 resourceModified on a create",
			err: syncexec.UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusConflict, Code: syncexec.GraphCodeResourceModified, Err: graph.ErrConflict,
			}),
		},
		{
			name: "412 on a conditional overwrite",
			err: syncexec.UploadTransferError(&graph.GraphError{
				StatusCode: http.StatusPreconditionFailed, Code: syncexec.GraphCodeResourceModified, Err: graph.ErrPreconditionFailed,
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

			require.ErrorIs(t, tc.err, syncdomain.ErrActionPreconditionChanged)
			r := syncexec.CompletionForTest(&syncdomain.ActionCompletion{
				Path:       "stale.txt",
				ActionType: syncdomain.ActionUpload,
				DriveID:    driveid.New("drive-classify-stale"),
				Err:        tc.err,
				HTTPStatus: syncexec.ExtractHTTPStatus(tc.err),
			})
			require.NotZero(t, r.HTTPStatus, "the Graph status travels with the stale error")

			assert.Equal(t, syncdomain.ResultDecision{
				Class:       resultSuperseded,
				Persistence: syncdomain.PersistNone,
				TrialHint:   syncdomain.TrialHintReclassify,
			}, classifyResult(r))
		})
	}
}

// Validates: R-2.10.4
func TestRuntimeConditionKey_RepresentativeMappings(t *testing.T) {
	t.Parallel()

	assert.Equal(t, syncdomain.ConditionInvalidFilename, conditionKeyForRuntimeResult(resultSkip, syncdomain.IssueInvalidFilename))
	assert.Equal(t, syncdomain.ConditionRateLimited, conditionKeyForRuntimeResult(resultBlockScope, syncdomain.IssueRateLimited))
	assert.Equal(t, syncdomain.ConditionRemoteWriteDenied, conditionKeyForRuntimeResult(resultSkip, syncdomain.IssueRemoteWriteDenied))
	assert.Equal(t, syncdomain.ConditionUnexpectedCondition, conditionKeyForRuntimeResult(errclass.ClassFatal, "mystery"))
	assert.Equal(t, syncdomain.ConditionUnexpectedCondition, conditionKeyForRuntimeResult(errclass.ClassActionable, ""))
	assert.Equal(t, syncdomain.ConditionKey(""), conditionKeyForRuntimeResult(errclass.ClassSuccess, ""))
	assert.Equal(t, syncdomain.ConditionKey(""), conditionKeyForRuntimeResult(errclass.ClassSuperseded, ""))
}

// A transient runtime condition is an expected failure that is retried: its
// result has no condition family. It used to be logged with condition_key
// unexpected_condition, the key of a failure nothing explains.
// Validates: R-6.6.13, R-6.8.16
func TestRuntimeConditionKey_TransientConditionsHaveNoFamily(t *testing.T) {
	t.Parallel()

	for _, condition := range []syncdomain.IssueType{
		syncdomain.IssueRequestTimeout, syncdomain.IssueTransientConflict, syncdomain.IssueTransientNotFound, syncdomain.IssueResourceLocked, syncdomain.IssueTransferURLRejected,
	} {
		assert.Empty(t, conditionKeyForRuntimeResult(resultRequeue, condition), "%q", condition)
	}

	// The classifier gives a 408 such a condition, so its decision carries no key.
	timedOut := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusRequestTimeout,
		Err:        errors.New("the request timed out"),
	}))
	assert.Equal(t, resultRequeue, timedOut.Class)
	assert.Equal(t, syncdomain.IssueRequestTimeout, timedOut.ConditionType)
	assert.Empty(t, timedOut.ConditionKey)
}

// Validates: R-2.10.4
func TestPermissionCapabilityFallbacks(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-cap")
	assert.Equal(t, syncdomain.PermissionCapabilityUnknown, effectiveRemotePermissionCapability(nil))
	assert.Equal(t, syncdomain.PermissionCapabilityUnknown, effectiveLocalPermissionCapability(nil))

	require.Equal(t,
		syncdomain.PermissionCapabilityRemoteRead,
		effectiveRemotePermissionCapability(&syncdomain.ActionCompletion{
			ActionType: syncdomain.ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		syncdomain.PermissionCapabilityRemoteWrite,
		effectiveRemotePermissionCapability(&syncdomain.ActionCompletion{
			ActionType: syncdomain.ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		syncdomain.PermissionCapabilityLocalRead,
		effectiveLocalPermissionCapability(&syncdomain.ActionCompletion{
			ActionType: syncdomain.ActionUpload,
			Path:       "upload.txt",
			DriveID:    driveID,
		}),
	)
	require.Equal(t,
		syncdomain.PermissionCapabilityLocalWrite,
		effectiveLocalPermissionCapability(&syncdomain.ActionCompletion{
			ActionType: syncdomain.ActionDownload,
			Path:       "download.txt",
			DriveID:    driveID,
		}),
	)

	assert.Equal(t, syncdomain.IssueRemoteWriteDenied, issueTypeForForbiddenResult(&syncdomain.ActionCompletion{
		FailureCapability: syncdomain.PermissionCapabilityRemoteWrite,
	}))
	assert.Equal(t, syncdomain.IssueLocalWriteDenied, issueTypeForLocalPermissionResult(&syncdomain.ActionCompletion{
		FailureCapability: syncdomain.PermissionCapabilityUnknown,
		ActionType:        syncdomain.ActionDownload,
		Path:              "download.txt",
		DriveID:           driveID,
	}))
	assert.Equal(t, syncdomain.SKThrottleDrive(driveID), deriveScopeKey(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusTooManyRequests,
		DriveID:    driveID,
	})))
	assert.Equal(t, syncdomain.SKService(), deriveScopeKey(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusServiceUnavailable,
	})))
	assert.Equal(t, syncdomain.ScopeKey{}, deriveScopeKey(syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: http.StatusForbidden})))
	assert.Equal(t, "drive:"+driveID.String(), (&syncdomain.ActionCompletion{DriveID: driveID}).ThrottleTargetKey())
	assert.Empty(t, (&syncdomain.ActionCompletion{}).ThrottleTargetKey())
}

// Validates: R-2.10.60, R-2.10.61, R-2.10.62
func TestClassifyResult_LocalErrnoTable(t *testing.T) {
	t.Parallel()

	pathErr := func(errno syscall.Errno) error {
		return &os.PathError{Op: "write", Path: "/sync/docs/report.pdf", Err: errno}
	}
	// An upload's body read fails inside the request, so the read's
	// *fs.PathError arrives wrapped in the request's *url.Error.
	readErr := func(errno syscall.Errno) error {
		return &url.Error{Op: "Put", URL: "https://upload.example/session", Err: &os.PathError{Op: "read", Path: "/sync/docs/report.pdf", Err: errno}}
	}
	testCases := []struct {
		name      string
		err       error
		class     errclass.Class
		scopeKey  syncdomain.ScopeKey
		issueType syncdomain.IssueType
	}{
		{name: "enospc mid-write is the disk scope", err: pathErr(syscall.ENOSPC), class: resultBlockScope, scopeKey: syncdomain.SKDiskLocal(), issueType: syncdomain.IssueDiskFull},
		{name: "edquot is the disk scope", err: pathErr(syscall.EDQUOT), class: resultBlockScope, scopeKey: syncdomain.SKDiskLocal(), issueType: syncdomain.IssueDiskFull},
		{name: "erofs is named read-only", err: pathErr(syscall.EROFS), class: resultSkip, issueType: syncdomain.IssueLocalReadOnly},
		{name: "efbig is a per-item issue", err: pathErr(syscall.EFBIG), class: resultSkip, issueType: syncdomain.IssueLocalFileTooLarge},
		{name: "exdev is a per-item issue", err: pathErr(syscall.EXDEV), class: resultSkip, issueType: syncdomain.IssueLocalCrossDevice},
		{name: "ebusy is retried", err: pathErr(syscall.EBUSY), class: resultRequeue, issueType: syncdomain.IssueLocalBusy},
		{name: "etxtbsy is retried", err: pathErr(syscall.ETXTBSY), class: resultRequeue, issueType: syncdomain.IssueLocalBusy},
		{name: "estale is retried", err: pathErr(syscall.ESTALE), class: resultRequeue, issueType: syncdomain.IssueLocalBusy},
		{name: "a timed-out local read is retried, not outage evidence", err: readErr(syscall.ETIMEDOUT), class: resultRequeue, issueType: syncdomain.IssueLocalBusy},
		{name: "eagain of a local read is retried", err: readErr(syscall.EAGAIN), class: resultRequeue, issueType: syncdomain.IssueLocalBusy},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: tc.err, ActionType: syncdomain.ActionDownload, Path: "docs/report.pdf"}))
			assert.Equal(t, tc.class, got.Class)
			assert.Equal(t, tc.scopeKey, got.ScopeKey)
			assert.Equal(t, tc.issueType, got.ConditionType)
			assert.NotEqual(t, syncdomain.ConditionUnexpectedCondition, got.ConditionKey,
				"a recognized errno must never be reported as an unexpected failure")
		})
	}
}

// Validates: R-6.8.20, R-2.10.62
//
// A transfer the stall watchdog abandoned while its local side hung (an
// upload's read or a download's write of the local file) is the local disk's
// trouble, not OneDrive's silence: it is retried as a busy file and is no
// evidence for the service scope. A stall on the network side stays a
// request timeout that is.
func TestClassifyResult_AStallOfTheLocalSideIsRetriedAsBusyAndIsNoServiceEvidence(t *testing.T) {
	t.Parallel()

	localStall := fmt.Errorf("upload report.pdf: %w: %w: no data moved for 2m0s (context canceled)",
		graph.ErrTransferStalled, graph.ErrLocalSideStalled)
	local := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: localStall, ActionType: syncdomain.ActionUpload, Path: "docs/report.pdf"}))
	assert.Equal(t, resultRequeue, local.Class)
	assert.Equal(t, syncdomain.IssueLocalBusy, local.ConditionType)
	assert.True(t, local.ScopeEvidence.IsZero(), "a hung local disk is no evidence of a service outage")
	assert.True(t, local.ScopeKey.IsZero())

	networkStall := fmt.Errorf("upload report.pdf: %w: no data moved for 2m0s (context canceled)", graph.ErrTransferStalled)
	network := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: networkStall, ActionType: syncdomain.ActionUpload, Path: "docs/report.pdf"}))
	assert.Equal(t, syncdomain.IssueRequestTimeout, network.ConditionType)
	assert.False(t, network.ScopeEvidence.IsZero(), "a silent service is evidence for the service scope")
}

// Validates: R-6.8.20, R-6.8.32
//
// A local-side stall whose transfer left the stuck call behind is its own
// condition, local_call_abandoned: retry work recorded under it from the
// first attempt and retried on the durable curve, so `status` names the
// file whose disk stopped answering. A stall the transfer waited out (the
// record of abandoned calls was full) stays local_busy.
func TestClassifyResult_ALocalCallLeftBehindIsItsOwnRetriedCondition(t *testing.T) {
	t.Parallel()

	abandoned := fmt.Errorf("upload report.pdf: %w: %w: %w: no data moved for 2m0s (context canceled)",
		graph.ErrTransferStalled, graph.ErrLocalSideStalled, graph.ErrLocalCallAbandoned)
	for _, actionType := range []syncdomain.ActionType{syncdomain.ActionUpload, syncdomain.ActionDownload} {
		decision := classifyResult(&syncdomain.ActionCompletion{Err: abandoned, ActionType: actionType, Path: "docs/report.pdf"})
		assert.Equal(t, resultSkip, decision.Class, actionType.String())
		assert.Equal(t, syncdomain.PersistRetryWork, decision.Persistence, actionType.String())
		assert.Equal(t, syncdomain.IssueLocalCallAbandoned, decision.ConditionType, actionType.String())
		assert.Equal(t, syncdomain.ConditionLocalCallAbandoned, decision.ConditionKey, actionType.String())
		assert.True(t, decision.ScopeEvidence.IsZero(), "a hung local disk is no evidence of a service outage")
	}
}

// Validates: R-2.10.61
func TestShouldHandleLocalPermission_IncludesReadOnlyFilesystem(t *testing.T) {
	t.Parallel()

	assert.True(t, shouldHandleLocalPermission(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Err: &os.PathError{Op: "open", Path: "/sync/a", Err: syscall.EROFS},
	})))
	assert.False(t, shouldHandleLocalPermission(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Err: &os.PathError{Op: "open", Path: "/sync/a", Err: syscall.EBUSY},
	})))
}

// Validates: R-2.10.64, R-2.10.65, R-2.10.66, R-2.10.67
func TestClassifyResult_ServerAnswerTable(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		status    int
		err       error
		class     errclass.Class
		scopeKey  syncdomain.ScopeKey
		evidence  syncdomain.ScopeKey
		issueType syncdomain.IssueType
	}{
		{
			name:   "501 is a per-item refusal",
			status: http.StatusNotImplemented,
			err:    &graph.GraphError{StatusCode: http.StatusNotImplemented, Err: graph.ErrServerError},
			class:  resultSkip, issueType: syncdomain.IssueUnsupportedOperation,
		},
		{
			name:   "505 is a per-item refusal",
			status: http.StatusHTTPVersionNotSupported,
			err:    &graph.GraphError{StatusCode: http.StatusHTTPVersionNotSupported, Err: graph.ErrServerError},
			class:  resultSkip, issueType: syncdomain.IssueUnsupportedOperation,
		},
		{
			name:   "500 insufficient space is quota",
			status: http.StatusInternalServerError,
			err: &graph.GraphError{
				StatusCode: http.StatusInternalServerError, Message: "Insufficient Space Available", Err: graph.ErrServerError,
			},
			class: resultBlockScope, scopeKey: syncdomain.SKQuotaOwn(), evidence: syncdomain.SKQuotaOwn(), issueType: syncdomain.IssueQuotaExceeded,
		},
		{
			name:   "409 resourceModified is retried",
			status: http.StatusConflict,
			err:    &graph.GraphError{StatusCode: http.StatusConflict, Code: "resourceModified", Err: graph.ErrConflict},
			class:  resultRequeue, issueType: syncdomain.IssueTransientConflict,
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
			class:  resultRequeue, evidence: syncdomain.SKService(), issueType: syncdomain.IssueServiceOutage,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{HTTPStatus: tc.status, Err: tc.err, ActionType: syncdomain.ActionUpload, Path: "a.txt"}))
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

	rps := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err: &graph.GraphError{
			StatusCode: http.StatusUnauthorized, Message: "Unable to initialize RPS", Err: graph.ErrAuthTransient,
		},
		ActionType: syncdomain.ActionUpload, Path: "a.txt",
	}))
	assert.Equal(t, resultRequeue, rps.Class, "a server-side auth fault must not end the watch session")
	assert.Equal(t, syncdomain.SKService(), rps.ScopeEvidence)
	assert.Equal(t, syncdomain.IssueServiceOutage, rps.ConditionType)

	preAuth := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err:        &graph.GraphError{StatusCode: http.StatusUnauthorized, Err: graph.ErrPreAuthURLRejected},
		ActionType: syncdomain.ActionDownload, Path: "a.txt",
	}))
	assert.Equal(t, resultRequeue, preAuth.Class, "an expired transfer URL is one file's problem")
	assert.True(t, preAuth.ScopeEvidence.IsZero())
	assert.Equal(t, syncdomain.IssueTransferURLRejected, preAuth.ConditionType)

	signIn := classifyResult(syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		HTTPStatus: http.StatusUnauthorized,
		Err:        &graph.GraphError{StatusCode: http.StatusUnauthorized, Err: graph.ErrUnauthorized},
	}))
	assert.Equal(t, resultFatal, signIn.Class)
	assert.Equal(t, syncdomain.IssueUnauthorized, signIn.ConditionType)
}

// Validates: R-2.10.70
func TestClassifyResult_MalwareIsPerFileNotReadDenied(t *testing.T) {
	t.Parallel()

	for name, completion := range map[string]*syncdomain.ActionCompletion{
		"facet refusal before any request": syncexec.CompletionForTest(&syncdomain.ActionCompletion{Err: graph.ErrMalwareDetected}),
		"content host 403": syncexec.CompletionForTest(&syncdomain.ActionCompletion{
			HTTPStatus: http.StatusForbidden,
			Err:        &graph.GraphError{StatusCode: http.StatusForbidden, Code: "malwareDetected", Err: graph.ErrMalwareDetected},
		}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			completion.ActionType = syncdomain.ActionDownload
			completion.Path = "eicar.com"
			completion.DriveID = driveid.New("drive-malware")
			got := classifyResult(completion)
			assert.Equal(t, resultSkip, got.Class)
			assert.Equal(t, syncdomain.IssueMalwareDetected, got.ConditionType, "not remote_read_denied")
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

	const unlisted = syncdomain.ActionCleanup + 1
	completion := &syncdomain.ActionCompletion{ActionType: unlisted, Path: "docs/a.txt"}

	decisions := map[string]func(){
		"activityKindFor":                     func() { activityKindFor(unlisted) },
		"effectiveRemotePermissionCapability": func() { effectiveRemotePermissionCapability(completion) },
		"effectiveLocalPermissionCapability":  func() { effectiveLocalPermissionCapability(completion) },
	}
	for name, decide := range decisions {
		assert.PanicsWithValue(t, syncdomain.OutsideTheEnum(unlisted), decide, name)
	}
}
