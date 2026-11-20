package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// Validates: R-6.6.25
func TestDriveStatusSnapshot_NeverSyncedClearsAfterTheFirstPass(t *testing.T) {
	t.Parallel()

	remote := newMoveScenarioRemote(driveid.New(engineTestDriveID))
	eng, syncRoot := newTestEngine(t, remote.client(t))
	writeLocalFile(t, syncRoot, "first.txt", "content")

	before, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.True(t, before.NeverSynced)

	report, err := eng.RunOnce(t.Context(), SyncBidirectional, RunOptions{})
	require.NoError(t, err)
	require.Zero(t, report.Failed, "errors: %v", report.Errors)

	after, err := eng.baseline.ReadDriveStatusSnapshot(t.Context())
	require.NoError(t, err)
	assert.False(t, after.NeverSynced, "a completed pass commits a cursor and a baseline")
}
