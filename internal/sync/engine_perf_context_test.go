package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/perf"
)

// The sync store records each transaction on the collector the run context
// carries, so a sync owner that runs a mount's pass under a context holding
// that mount's collector sees the pass's store work in the mount's snapshot.
// internal/multisync does that wrap; this is the engine half of the contract,
// with a real pass against a real store.
//
// Validates: R-6.6.15
func TestRunOnce_StoreTransactionsCountOnTheRunContextCollector(t *testing.T) {
	t.Parallel()

	const content = "alpha!!"

	driveID := driveid.New(engineTestDriveID)
	mock := newDownloadDeltaMock(
		driveID,
		&graph.Item{
			ID: "item-a", Name: "alpha.txt", ParentID: "root",
			DriveID: driveID, Size: int64(len(content)), QuickXorHash: testQuickXor(content),
		},
		"token-perf",
		[]byte(content),
	)

	mountCollector := perf.NewCollector(nil)
	eng, _ := newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.PerfCollector = mountCollector
	})

	report, err := eng.RunOnce(perf.WithCollector(t.Context(), mountCollector), SyncBidirectional, RunOptions{})
	require.NoError(t, err, "RunOnce")
	require.Equal(t, 1, report.Downloads, "the pass downloaded the file and committed it to the store")

	snapshot := mountCollector.Snapshot()
	assert.Positive(t, snapshot.DBTransactionCount, "store transactions of the pass")
}
