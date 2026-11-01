package cli

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// Live incident, 2026-11-01: 300 edited files waited an hour for a plan while
// `status` said "Up to date", because only remote drift counted.
//
// Validates: R-2.10.50
func TestFinalizeStatusDriveState_LocalDriftIsNotUpToDate(t *testing.T) {
	t.Parallel()

	pending := &statusDrive{State: driveStateReady, SyncState: &syncStateInfo{LocalDrift: 300}}
	finalizeStatusDriveState(pending)
	assert.Equal(t, driveStatePending, pending.State)

	syncing := &statusDrive{
		State:        driveStateReady,
		RuntimeState: statusRuntimeStateActive,
		SyncState:    &syncStateInfo{LocalDrift: 300},
	}
	finalizeStatusDriveState(syncing)
	assert.Equal(t, driveStateSyncing, syncing.State)

	converged := &statusDrive{State: driveStateReady, SyncState: &syncStateInfo{FileCount: 300}}
	finalizeStatusDriveState(converged)
	assert.Equal(t, driveStateReady, converged.State)
}

// Validates: R-2.10.50
func TestBuildSyncStateInfo_CarriesLocalDriftAndRendersIt(t *testing.T) {
	t.Parallel()

	info := buildSyncStateInfo(&syncengine.DriveStatusSnapshot{
		BaselineFileCount: 7,
		LocalDriftItems:   3,
	}, false, 0)
	assert.Equal(t, 3, info.LocalDrift)

	var buf bytes.Buffer
	require.NoError(t, printSyncStateSummaryLines(&buf, "    ", &info))
	assert.Contains(t, buf.String(), "    Local changes: 3 items\n")
	assert.NotContains(t, buf.String(), "Remote changes")
}
