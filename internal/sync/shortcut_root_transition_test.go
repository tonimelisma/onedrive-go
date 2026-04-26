package sync

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.8, R-2.4.10
func TestShortcutRootTransitionTableCoversStates(t *testing.T) {
	t.Parallel()

	states := []ShortcutRootState{
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateSamePathReplacementWaiting,
	}
	transitions := shortcutRootTransitionTable()
	for _, state := range states {
		_, ok := transitions[state]
		assert.Truef(t, ok, "missing transition table entry for %s", state)
	}
}

// Validates: R-2.4.8, R-2.4.10
func TestValidateShortcutRootTransitionAllowsKnownLifecycleEdges(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		from  ShortcutRootState
		event shortcutRootLifecycleEvent
		to    ShortcutRootState
	}{
		{"remote delete drains active", ShortcutRootStateActive, shortcutRootEventRemoteDelete, ShortcutRootStateRemovedFinalDrain},
		{"complete omission drains unavailable", ShortcutRootStateTargetUnavailable, shortcutRootEventCompleteOmission, ShortcutRootStateRemovedFinalDrain},
		{"same path replacement waits behind retiring", ShortcutRootStateRemovedFinalDrain, shortcutRootEventSamePathReplacement, ShortcutRootStateSamePathReplacementWaiting},
		{"final drain promotes waiting replacement", ShortcutRootStateSamePathReplacementWaiting, shortcutRootEventWaitingReplacementPromote, ShortcutRootStateActive},
		{"local ambiguity blocks active", ShortcutRootStateActive, shortcutRootEventAliasRenameAmbiguous, ShortcutRootStateRenameAmbiguous},
		{"cleanup failure stays protected", ShortcutRootStateRemovedFinalDrain, shortcutRootEventProjectionCleanupFailed, ShortcutRootStateRemovedCleanupBlocked},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			require.NoError(t, validateShortcutRootTransition(tt.from, tt.event, tt.to))
		})
	}
}

// Validates: R-2.4.8, R-2.4.10
func TestValidateShortcutRootTransitionRejectsIllegalLifecycleEdges(t *testing.T) {
	t.Parallel()

	err := validateShortcutRootTransition(
		ShortcutRootStateRemovedFinalDrain,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not allowed")
}

// Validates: R-2.4.8, R-2.4.10
func TestPlannedShortcutRootTransitionPreservesStateOnIllegalEdge(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateRemovedFinalDrain,
	}

	next := plannedShortcutRootTransition(
		record,
		shortcutRootEventLocalRootReady,
		ShortcutRootStateActive,
		"",
	)

	assert.Equal(t, ShortcutRootStateRemovedFinalDrain, next.State)
	assert.Contains(t, next.BlockedDetail, "not allowed")
	assert.Equal(t, []string{"Shared/Docs"}, next.ProtectedPaths)
}
