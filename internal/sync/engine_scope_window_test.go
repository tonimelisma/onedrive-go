package sync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// refuseUploadsWithQuota makes every new-file upload answer OneDrive's 507
// at the create, the way a full drive answered on 2026-11-03.
func refuseUploadsWithQuota(client *engineMockClient, refused *atomic.Int32) {
	client.uploadCreatingFn = func(
		_ context.Context, _, _ string, _ io.ReaderAt, _ int64, _ graph.CreatedFunc,
	) (*graph.Item, error) {
		refused.Add(1)

		return nil, &graph.GraphError{
			StatusCode: http.StatusInsufficientStorage,
			Code:       "insufficientStorage",
			Message:    "Quota limit reached",
		}
	}
}

// Validates: R-2.10.33
//
// One 507 is below the quota window (three paths in ten seconds), so no scope
// activates. The row used to be recorded blocked under quota:own anyway, a
// scope that did not exist: nothing ever trialed or released it, and on the
// test account two such rows sat blocked across a restart (2026-11-03). It is
// timed retry work that keeps the scope as evidence.
func TestRunOnce_AQuotaRefusalBelowTheWindowIsTimedRetryWorkNotABlock(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	var refused atomic.Int32
	refuseUploadsWithQuota(client, &refused)
	eng, syncRoot := newTestEngine(t, client)
	writeLocalFile(t, syncRoot, "one.txt", "content")

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	require.Equal(t, 1, report.Failed, "errors: %v", report.Errors)
	assert.Equal(t, int32(1), refused.Load())

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.False(t, rows[0].Blocked, "below the window there is no scope to block under")
	assert.Equal(t, SKQuotaOwn().String(), rows[0].ScopeKey.String(), "the scope stays as evidence")
	assert.Positive(t, rows[0].NextRetryAt, "timed retry work has a due time")

	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, scopes, "no scope row without an activation")
}

// Validates: R-2.10.33, R-2.10.66
//
// Three paths refused within the window cross it: the scope activates, is
// persisted, and the work it blocks is blocked under it, so a trial exists
// for every blocked row.
func TestRunOnce_ThreeQuotaRefusalsActivateTheScopeAndBlockTheRows(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	client := remote.client(t)
	var refused atomic.Int32
	refuseUploadsWithQuota(client, &refused)
	eng, syncRoot := newTestEngine(t, client)
	for i := range 3 {
		writeLocalFile(t, syncRoot, fmt.Sprintf("file-%d.txt", i), fmt.Sprintf("content %d", i))
	}

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, report.Failed+report.LeftForNextSync, "every upload was refused or taken back: %+v", report)

	scopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, scopes, 1, "the third refusal crossed the window")
	assert.Equal(t, SKQuotaOwn().String(), scopes[0].Key.String())

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	blocked := 0
	for _, row := range rows {
		if row.Blocked {
			blocked++
			assert.Equal(t, SKQuotaOwn().String(), row.ScopeKey.String())
		}
	}
	assert.Positive(t, blocked, "an active scope has blocked work to trial: %+v", rows)
}

// Validates: R-2.10.33
//
// A store an earlier build left with a blocked row and no scope for it (the
// case above, before the fix) gets the work back at startup: the row is made
// due and the pass does it.
func TestRunOnce_ABlockedRowWhoseScopeIsGoneIsReleasedAtStartup(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	writeLocalFile(t, syncRoot, "stuck.txt", "content")
	_, err := eng.baseline.RecordBlockedRetryWork(t.Context(),
		RetryWorkKey{Path: "stuck.txt", ActionType: ActionUpload}, SKQuotaOwn())
	require.NoError(t, err)

	report, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Succeeded, "the released upload ran: %+v", report)
	assert.Contains(t, remote.tree(), "stuck.txt")

	rows, err := eng.baseline.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, rows, "the orphan row was released and then cleared by the success")
}
