package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.4.8, R-2.4.10, R-4.1.4
func TestMountLifecycleDetails_CoverEveryStateReason(t *testing.T) {
	t.Parallel()

	tests := []struct {
		reason MountStateReason
		state  MountState
	}{
		{reason: MountStateReasonShortcutBindingUnavailable, state: MountStateUnavailable},
		{reason: MountStateReasonDuplicateContentRoot, state: MountStateConflict},
		{reason: MountStateReasonExplicitStandaloneContentRoot, state: MountStateConflict},
		{reason: MountStateReasonShortcutRemoved, state: MountStatePendingRemoval},
		{reason: MountStateReasonRemovedProjectionDirty, state: MountStatePendingRemoval},
		{reason: MountStateReasonRemovedProjectionUnavailable, state: MountStatePendingRemoval},
		{reason: MountStateReasonLocalProjectionConflict, state: MountStateConflict},
		{reason: MountStateReasonLocalProjectionUnavailable, state: MountStateUnavailable},
		{reason: MountStateReasonLocalAliasRenameConflict, state: MountStateConflict},
		{reason: MountStateReasonLocalAliasRenameUnavailable, state: MountStateUnavailable},
		{reason: MountStateReasonLocalAliasDeleteUnavailable, state: MountStateUnavailable},
		{reason: MountStateReasonLocalRootCollision, state: MountStateConflict},
		{reason: MountStateReasonLocalRootUnavailable, state: MountStateUnavailable},
	}

	assert.Len(t, AllMountStateReasons(), len(tests))
	for _, tt := range tests {
		t.Run(string(tt.reason), func(t *testing.T) {
			t.Parallel()

			detail, ok := MountLifecycleDetailFor(tt.state, tt.reason)
			require.True(t, ok)
			assert.Equal(t, tt.state, detail.RequiredState)
			assert.True(t, detail.KeepsReservation)
			assert.False(t, detail.StartsChild)
			assert.True(t, detail.AutoRetry)
			assert.NotEmpty(t, detail.StatusDetail)
			assert.NotEmpty(t, detail.RecoveryAction)
			assert.NotContains(t, strings.ToLower(detail.StatusDetail), "rerun sync")
			assert.NotContains(t, strings.ToLower(detail.RecoveryAction), "rerun sync")
			require.NoError(t, validateMountStateReason(tt.state, tt.reason))

			_, ok = MountLifecycleDetailFor(MountStateActive, tt.reason)
			assert.False(t, ok)
		})
	}

	err := validateMountStateReason(MountStateActive, MountStateReasonShortcutRemoved)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires state")
}
