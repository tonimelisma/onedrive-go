package cli

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// Validates: R-6.6.25
//
// A state database that exists but has never completed a sync (right after
// `drive reset-sync-state`) is "not synced yet", or "syncing" while a watch
// is bootstrapping it; its zero counts are not "up to date".
func TestFinalizeStatusDriveState_NeverSyncedStoreIsNotSyncedYetOrSyncingUnderAWatch(t *testing.T) {
	t.Parallel()

	idle := &statusDrive{State: driveStateReady, SyncState: &syncStateInfo{NeverSynced: true}}
	finalizeStatusDriveState(idle)
	assert.Equal(t, driveStateNotSynced, idle.State)

	watched := &statusDrive{
		State:        driveStateReady,
		RuntimeState: statusRuntimeStateActive,
		SyncState:    &syncStateInfo{NeverSynced: true},
	}
	finalizeStatusDriveState(watched)
	assert.Equal(t, driveStateSyncing, watched.State)

	synced := &statusDrive{State: driveStateReady, SyncState: &syncStateInfo{FileCount: 3}}
	finalizeStatusDriveState(synced)
	assert.Equal(t, driveStateReady, synced.State)
}

// Validates: R-2.8.1
//
// A reload hands the config's log_level to the command, which moves the log
// file's level in place; an unknown value keeps the current level and is
// warned about once per value, not on every reload.
func TestApplyFileLogLevel_MovesTheFileLevelAndWarnsOnceForAnUnknownValue(t *testing.T) {
	t.Parallel()

	var logBuf bytes.Buffer
	cc := &CLIContext{
		Logger:       slog.New(slog.NewTextHandler(&logBuf, nil)),
		fileLogLevel: new(slog.LevelVar),
	}
	cc.fileLogLevel.Set(slog.LevelInfo)

	cc.applyFileLogLevel("debug")
	assert.Equal(t, slog.LevelDebug, cc.fileLogLevel.Level())

	cc.applyFileLogLevel("bogus")
	cc.applyFileLogLevel("bogus")
	assert.Equal(t, slog.LevelDebug, cc.fileLogLevel.Level(), "an unknown value keeps the level")
	assert.Equal(t, 1, strings.Count(logBuf.String(), "unknown log_level"), "warned once per value")

	cc.applyFileLogLevel("")
	assert.Equal(t, slog.LevelInfo, cc.fileLogLevel.Level(), "no log_level means the default")

	var none CLIContext
	none.applyFileLogLevel("debug")
}
