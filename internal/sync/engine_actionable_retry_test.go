package sync

import (
	"bytes"
	"context"
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

	snapshot, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	require.Len(t, snapshot.ActionableRetryWork, 1, "status reads the row as a condition")
	assert.Equal(t, issuePathTooLong, snapshot.ActionableRetryWork[0].ConditionType)
	assert.Zero(t, snapshot.RetryingItems, "not counted as retrying")
	groups := ProjectStoredConditionGroups(&snapshot)
	require.Len(t, groups, 1)
	assert.Equal(t, ConditionPathTooLong, groups[0].ConditionKey)
	assert.Equal(t, []string{"deep"}, groups[0].Paths)

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
