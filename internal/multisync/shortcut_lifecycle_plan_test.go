package multisync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
)

// Validates: R-2.4.8, R-2.4.10
func TestShortcutLifecyclePlanner_EveryReasonHasRecoveryMetadata(t *testing.T) {
	t.Parallel()

	for _, reason := range config.AllMountStateReasons() {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()

			var expectedState config.MountState
			for _, state := range []config.MountState{
				config.MountStateConflict,
				config.MountStateUnavailable,
				config.MountStatePendingRemoval,
			} {
				if _, ok := config.MountLifecycleDetailFor(state, reason); ok {
					expectedState = state
					break
				}
			}
			require.NotEmpty(t, expectedState)
			detail, ok := config.MountLifecycleDetailFor(expectedState, reason)
			require.True(t, ok)
			assert.True(t, detail.KeepsReservation)
			assert.False(t, detail.StartsChild)
			assert.True(t, detail.AutoRetry)
			assert.NotEmpty(t, detail.RecoveryAction)
		})
	}
}

// Validates: R-2.4.8, R-2.4.10
func TestShortcutLifecyclePlanner_PendingRemovalOrdersProjectionCleanupBeforeDBPurge(t *testing.T) {
	t.Parallel()

	record := testChildRecord(mountID("personal:owner@example.com"), "binding-1", "Shortcuts/Docs")
	plan, err := planShortcutPendingRemoval(&record)
	require.NoError(t, err)

	assert.Equal(t, config.MountStatePendingRemoval, plan.Record.State)
	assert.Equal(t, config.MountStateReasonShortcutRemoved, plan.Record.StateReason)
	assert.Equal(t, []shortcutLifecycleEffect{
		shortcutEffectPersistInventoryBeforeProjectionCleanup,
		shortcutEffectKeepReservationsActive,
		shortcutEffectPurgeChildDBAfterProjectionCleanup,
	}, plan.Effects)
}

// Validates: R-2.4.8, R-2.4.10
func TestShortcutLifecyclePlanner_RejectsIllegalTransitions(t *testing.T) {
	t.Parallel()

	record := testChildRecord(mountID("personal:owner@example.com"), "binding-1", "Shortcuts/Docs")

	_, err := planPendingRemovalCleanup(&record, true, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pending_removal")

	_, err = planChildRootLifecycleActionSuccess(&record, &childRootLifecycleAction{}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown child root lifecycle action")
}

// Validates: R-2.4.8, R-2.4.10
func TestShortcutLifecyclePlanner_ProjectionFailuresRemainRetryableWithoutChildRunner(t *testing.T) {
	t.Parallel()

	record := testChildRecord(mountID("personal:owner@example.com"), "binding-1", "Shortcuts/New Docs")
	move := &childProjectionMove{
		mountID:               mountID(record.MountID),
		fromRelativeLocalPath: "Shortcuts/Old Docs",
		toRelativeLocalPath:   "Shortcuts/New Docs",
	}

	plan, err := planProjectionMoveFailure(
		&record,
		move,
		config.MountStateConflict,
		config.MountStateReasonLocalProjectionConflict,
	)
	require.NoError(t, err)

	assert.Equal(t, config.MountStateConflict, plan.Record.State)
	assert.Equal(t, config.MountStateReasonLocalProjectionConflict, plan.Record.StateReason)
	assert.Contains(t, plan.Record.ReservedLocalPaths, "Shortcuts/Old Docs")
	assert.Contains(t, plan.Effects, shortcutEffectRetryConflictUnavailableWithoutRunner)
	assert.Contains(t, plan.Effects, shortcutEffectKeepReservationsActive)
}

// Validates: R-2.4.8, R-2.4.10
func TestShortcutLifecyclePlanner_ProjectionSuccessClearsRetryableProjectionReason(t *testing.T) {
	t.Parallel()

	record := testChildRecord(mountID("personal:owner@example.com"), "binding-1", "Shortcuts/New Docs")
	record.State = config.MountStateConflict
	record.StateReason = config.MountStateReasonLocalProjectionConflict
	record.ReservedLocalPaths = []string{"Shortcuts/Old Docs"}
	move := &childProjectionMove{
		mountID:               mountID(record.MountID),
		fromRelativeLocalPath: "Shortcuts/Old Docs",
		toRelativeLocalPath:   "Shortcuts/New Docs",
	}

	plan, err := planProjectionMoveSuccess(&record, move, &config.RootIdentity{Device: 7, Inode: 9})
	require.NoError(t, err)

	assert.Equal(t, config.MountStateActive, plan.Record.State)
	assert.Empty(t, plan.Record.StateReason)
	assert.Empty(t, plan.Record.ReservedLocalPaths)
	assert.Equal(t, &config.RootIdentity{Device: 7, Inode: 9}, plan.Record.LocalRootIdentity)
	assert.Contains(t, plan.Effects, shortcutEffectStopChildBeforeProjectionMove)
	assert.Contains(t, plan.Effects, shortcutEffectRecompileAfterLifecycleMutation)
}
