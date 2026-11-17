package sync

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/errclass"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// Validates: R-2.10.71, R-6.6.30
//
// OneDrive counts more than the drive-relative path toward its limit, so a
// folder the scanner let through can still be refused with 400 (live,
// 2026-11-02: a 373-character path). The refusal is a condition the user has
// to clear: it is kept with the retry row, waits on the durable curve, is
// said once in the log, and status lists it under "Path too long" instead of
// as anonymous retrying work.
func TestRunOnce_FolderOneDriveRefusesAsTooLongIsAConditionNotAnonymousRetryWork(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	creates := 0
	client.createFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		creates++
		return nil, &graph.GraphError{
			StatusCode: http.StatusBadRequest,
			Message:    "The specified file or folder name is too long. The URL path for all files and folders must be 400 characters or less.",
			Err:        graph.ErrBadRequest,
		}
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	eng, syncRoot := newTestEngineWithLogger(t, client, logger)
	clock := newManualClock(time.Date(2026, 11, 2, 22, 23, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "deep"), 0o750))

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 1, creates)

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "deep", rows[0].Path)
	assert.Equal(t, ActionFolderCreate, rows[0].ActionType)
	assert.Equal(t, issuePathTooLong, rows[0].ConditionType, "the refusal is kept with the row")
	assert.GreaterOrEqual(t, rows[0].NextRetryAt-clock.Now().UnixNano(), int64(durableRecheckDelay),
		"a condition the user must clear waits on the durable curve")

	assertStatusListsRefusalUnderItsCondition(t, eng)

	warned := strings.Count(logBuf.String(), "Not syncing deep:")
	assert.Equal(t, 1, warned, "said once when first refused: %s", logBuf.String())

	// Before the row is due nothing is retried; once due, the refusal is
	// tried again, backs off further and is not announced again.
	clock.Advance(durableRecheckDelay / 2)
	report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Zero(t, report.Failed+report.Succeeded, "held with backoff: %+v", report)
	assert.Equal(t, 1, creates)

	clock.Advance(retryBackoffCeilingForTest)
	report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, 2, creates)
	rows, err = eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].AttemptCount)
	assert.Equal(t, issuePathTooLong, rows[0].ConditionType)
	assert.Equal(t, 1, strings.Count(logBuf.String(), "Not syncing deep:"), "a retry of the same refusal is not re-announced")
}

// Validates: R-2.10.71, R-6.6.18, R-6.6.28
//
// A folder OneDrive refuses as too long is a condition only the user can
// clear, known when the refusal is classified. The run's activity names that
// condition as status lists it, and what the run left counts the folder with
// what is still not synced, not as failed, on the first refusal and on a retry
// of the durable curve alike; the activity marks the first refusal as the
// condition starting and the retry not. A run whose only pass holds the folder
// until its retry is due attempts nothing and counts it as work to retry.
// Sync's output says the condition to the user from the activity when it
// starts, so the engine's own "Not syncing" line stays in the log at Info
// instead of warning it a second time.
func TestRunOnce_ARefusalOnlyTheUserCanClearIsLeftAsItsCondition(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	client.createFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		return nil, &graph.GraphError{
			StatusCode: http.StatusBadRequest,
			Message:    scenarioPathLimitRefusal,
			Err:        graph.ErrBadRequest,
		}
	}
	logger, logs := newCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, client, logger)
	clock := newManualClock(time.Date(2026, 11, 2, 22, 23, 0, 0, time.UTC))
	installManualClockForTestEngine(eng, clock)
	var activities []Activity
	eng.SetActivityHook(func(a Activity) { activities = append(activities, a) })
	require.NoError(t, os.Mkdir(filepath.Join(syncRoot, "deep"), 0o750))

	for _, run := range []string{"the first refusal", "a retry"} {
		activities = nil
		report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
		require.NoError(t, err)

		require.Len(t, activities, 1, run)
		assert.Equal(t, "deep", activities[0].Path, run)
		assert.Equal(t, ConditionPathTooLong, activities[0].ConditionKey, run)
		assert.Equal(t, issuePathTooLong, activities[0].ConditionType, run)
		assert.False(t, activities[0].WillRetry, run)
		assert.Equal(t, run == "the first refusal", activities[0].ConditionStarts, run)
		assert.Equal(t, 1, report.Failed, "%s: every failed action is still counted as done work", run)
		assert.Equal(t, LeftWork{StillOpen: 1}, report.LeftWork(), run)

		activities = nil
		report, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
		require.NoError(t, err)
		assert.Empty(t, activities, "%s: held until its retry is due", run)
		assert.Equal(t, LeftWork{WillRetry: 1}, report.LeftWork(), run)
		clock.Advance(retryBackoffCeilingForTest)
	}

	assert.Equal(t, 1, strings.Count(logs.String(), `level=INFO msg="Not syncing deep:`), logs.String())
	assert.NotContains(t, logs.String(), `level=WARN msg="Not syncing deep:`)
}

// Validates: R-2.10.71, R-6.6.30
//
// A condition starts again when the plan dropped the path's retry work and
// the condition then refuses the path again, with no activity of the path in
// between: the refused folder was deleted, so the plan had nothing to do for
// it, and was then made again. The refusal that follows is marked as the
// condition starting, so sync's output says it again, and the engine's own
// "Not syncing" line for it stays at Info as for the first start.
func TestRunOnce_ARefusalAfterItsRetryWorkWasDroppedStartsTheConditionAgain(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	client.createFolderFn = func(context.Context, driveid.ID, string, string) (*graph.Item, error) {
		return nil, &graph.GraphError{
			StatusCode: http.StatusBadRequest,
			Message:    scenarioPathLimitRefusal,
			Err:        graph.ErrBadRequest,
		}
	}
	logger, logs := newCapturedLogger()
	eng, syncRoot := newTestEngineWithLogger(t, client, logger)
	var activities []Activity
	eng.SetActivityHook(func(a Activity) { activities = append(activities, a) })
	deep := filepath.Join(syncRoot, "deep")
	require.NoError(t, os.Mkdir(deep, 0o750))

	_, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Len(t, activities, 1)
	assert.True(t, activities[0].ConditionStarts, "the first refusal starts the condition")

	activities = nil
	require.NoError(t, os.Remove(deep))
	_, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Empty(t, activities, "the plan has nothing to do for the deleted folder")
	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Empty(t, rows, "the plan dropped the folder's retry work")

	require.NoError(t, os.Mkdir(deep, 0o750))
	_, err = eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Len(t, activities, 1)
	assert.Equal(t, ConditionPathTooLong, activities[0].ConditionKey)
	assert.True(t, activities[0].ConditionStarts, "the condition starts again")

	assert.Equal(t, 2, strings.Count(logs.String(), `level=INFO msg="Not syncing deep:`), logs.String())
	assert.NotContains(t, logs.String(), `level=WARN msg="Not syncing deep:`)
}

// Validates: R-2.10.71, R-6.6.18, R-6.6.28
//
// One predicate decides "waits for the user": the activity feed names a
// failure's condition exactly when the pass counts the failure with what waits
// for the user, so a file line and the summary agree. A failure the permission
// probes own is not one: the condition they record is known only after the
// probe ran, and can differ from the classified one. Nor is a transfer
// OneDrive answered with other bytes than its own facts describe: nothing the
// user does clears it, and its retry may succeed.
func TestFailureAwaitsUser_TheFeedAndThePassCountAgree(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	var activities []Activity
	eng.SetActivityHook(func(a Activity) { activities = append(activities, a) })

	refused := errors.New("refused")
	for _, tc := range []struct {
		name       string
		decision   resultDecision
		action     actionType
		httpStatus int
		err        error
		awaits     bool
	}{
		{
			name:     "a condition only the user can clear",
			decision: actionableDecision(issuePathTooLong, ConditionPathTooLong),
			err:      refused,
			awaits:   true,
		},
		{
			name:     "an actionable failure that names no condition",
			decision: actionableDecision("", ConditionUnexpectedCondition),
			err:      refused,
		},
		{
			name: "a transient failure",
			decision: resultDecision{
				Class: errclass.ClassRetryableTransient, Persistence: persistRetryWork,
				ConditionType: issueServiceOutage, ConditionKey: ConditionServiceOutage,
			},
			err: refused,
		},
		{
			name:     "a denial the local permission probe owns",
			decision: actionableDecision(issueLocalWriteDenied, ConditionLocalWriteDenied),
			err:      fmt.Errorf("write file: %w", os.ErrPermission),
		},
		{
			name:       "a write denial the remote permission probe owns",
			decision:   actionableDecision(IssueRemoteWriteDenied, ConditionRemoteWriteDenied),
			action:     ActionUpload,
			httpStatus: http.StatusForbidden,
			err:        &graph.GraphError{StatusCode: http.StatusForbidden, Message: "Access denied", Err: graph.ErrForbidden},
		},
		{
			name:     "an upload OneDrive stored differently than it was sent",
			decision: actionableDecision(issueUploadVerification, ConditionUploadVerification),
			action:   ActionUpload,
			err:      fmt.Errorf("upload: %w", driveops.ErrUploadVerificationFailed),
		},
		{
			name:     "a download OneDrive served differently than it describes",
			decision: actionableDecision(issueDownloadIntegrity, ConditionDownloadIntegrity),
			err:      fmt.Errorf("download: %w", driveops.ErrDownloadIntegrity),
		},
	} {
		flow.resetResultStats()
		activities = nil
		// The zero action is ActionDownload.
		completion := &actionCompletion{ActionType: tc.action, Path: "a.txt", HTTPStatus: tc.httpStatus, Err: tc.err}
		require.Equal(t, tc.httpStatus == http.StatusForbidden, shouldHandleRemote403Permission(completion), tc.name)

		eng.emitActivity(completion, &tc.decision, true)
		flow.recordError(&tc.decision, completion)

		require.Len(t, activities, 1, tc.name)
		assert.Equal(t, tc.awaits, activities[0].ConditionKey != "", tc.name)
		assert.Equal(t, tc.awaits, flow.results.awaitingUser == 1, tc.name)
		assert.Equal(t, 1, flow.results.failed, tc.name)
		if tc.awaits {
			assert.Equal(t, tc.decision.ConditionKey, activities[0].ConditionKey, tc.name)
			assert.Equal(t, tc.decision.ConditionType, activities[0].ConditionType, tc.name)
		}
	}
}

// actionableDecision is the decision for an action refused per item, recorded
// as retry work with conditionType.
func actionableDecision(conditionType IssueType, key ConditionKey) resultDecision {
	return resultDecision{
		Class:         errclass.ClassActionable,
		Persistence:   persistRetryWork,
		ConditionType: conditionType,
		ConditionKey:  key,
	}
}

// assertStatusListsRefusalUnderItsCondition reads the status snapshot the
// way `status` does and checks the refused folder is a "Path too long"
// condition, not retrying work.
func assertStatusListsRefusalUnderItsCondition(t *testing.T, eng *testEngine) {
	t.Helper()

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.ActionableRetryWork, 1, "status reads the row as a condition")
	assert.Equal(t, issuePathTooLong, snapshot.ActionableRetryWork[0].ConditionType)
	assert.Zero(t, snapshot.RetryingItems, "not counted as retrying")
	groups := ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1)
	assert.Equal(t, ConditionPathTooLong, groups[0].ConditionKey)
	assert.Equal(t, []string{"deep"}, groups[0].Paths)
}
