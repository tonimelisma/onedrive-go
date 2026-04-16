package sync

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// B-319: Clock skew resilience tests
// ---------------------------------------------------------------------------

// TestClockSkew_BackwardJump_SyncFailureTimestamp verifies that recording
// sync failures works correctly with backward clock jumps.
func TestClockSkew_BackwardJump_SyncFailureTimestamp(t *testing.T) {
	t.Parallel()

	mgr := newTestManager(t)
	ctx := t.Context()

	// Record at t=5000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(5000, 0) })

	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "file1.txt",
		Direction:  DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	// Jump backward to t=1000.
	mgr.SetNowFunc(func() time.Time { return time.Unix(1000, 0) })

	// Should still succeed.
	require.NoError(t, mgr.RecordFailure(ctx, &SyncFailureParams{
		Path:       "file2.txt",
		Direction:  DirectionUpload,
		IssueType:  "upload_failed",
		ErrMsg:     "err",
		HTTPStatus: 500,
	}, nil))

	issues, err := mgr.ListSyncFailuresByIssueType(ctx, "upload_failed")
	require.NoError(t, err)
	assert.Len(t, issues, 2, "both issues recorded despite clock jump")
}
