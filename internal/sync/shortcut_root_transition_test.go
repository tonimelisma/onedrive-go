package sync

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// Validates: R-2.4.8, R-2.4.10
func TestShortcutRootTransitionTableCoversStates(t *testing.T) {
	t.Parallel()

	states := []ShortcutRootState{
		ShortcutRootStateActive,
		ShortcutRootStateTargetUnavailable,
		ShortcutRootStateLocalRootUnavailable,
		ShortcutRootStateBlockedPath,
		ShortcutRootStateRenameAmbiguous,
		ShortcutRootStateAliasMutationBlocked,
		ShortcutRootStateRemovedFinalDrain,
		ShortcutRootStateRemovedReleasePending,
		ShortcutRootStateRemovedCleanupBlocked,
		ShortcutRootStateRemovedChildCleanupPending,
		ShortcutRootStateSamePathReplacementWaiting,
		ShortcutRootStateDuplicateTarget,
	}
	transitions := shortcutRootTransitionTable()
	for _, state := range states {
		_, ok := transitions[state]
		assert.Truef(t, ok, "missing transition table entry for %s", state)
	}
}

// Validates: R-2.4.8
func TestShortcutRootStatusMetadataCoversNonActiveStates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state        ShortcutRootState
		issue        ShortcutRootIssueClass
		recovery     ShortcutRootRecoveryClass
		protectsPath bool
	}{
		{ShortcutRootStateTargetUnavailable, ShortcutRootIssueTargetUnavailable, ShortcutRootRecoveryRestoreTargetOrRemoveAlias, true},
		{ShortcutRootStateLocalRootUnavailable, ShortcutRootIssueLocalRootUnavailable, ShortcutRootRecoveryRestoreLocalRootOrDiscard, true},
		{ShortcutRootStateBlockedPath, ShortcutRootIssueBlockedPath, ShortcutRootRecoveryClearBlockedPath, true},
		{ShortcutRootStateRenameAmbiguous, ShortcutRootIssueRenameAmbiguous, ShortcutRootRecoveryDisambiguateAliasRename, true},
		{ShortcutRootStateAliasMutationBlocked, ShortcutRootIssueAliasMutationBlocked, ShortcutRootRecoveryFixAliasMutation, true},
		{ShortcutRootStateRemovedFinalDrain, ShortcutRootIssueRemovedFinalDrain, ShortcutRootRecoveryRestoreTargetOrDiscard, true},
		{ShortcutRootStateRemovedReleasePending, ShortcutRootIssueRemovedReleasePending, ShortcutRootRecoveryWaitForRetry, true},
		{ShortcutRootStateRemovedCleanupBlocked, ShortcutRootIssueRemovedCleanupBlocked, ShortcutRootRecoveryClearBlockedPath, true},
		{ShortcutRootStateRemovedChildCleanupPending, ShortcutRootIssueRemovedChildCleanupPending, ShortcutRootRecoveryWaitForRetry, false},
		{ShortcutRootStateSamePathReplacementWaiting, ShortcutRootIssueSamePathReplacementWaiting, ShortcutRootRecoveryWaitForRetry, true},
		{ShortcutRootStateDuplicateTarget, ShortcutRootIssueDuplicateTarget, ShortcutRootRecoveryRemoveDuplicateAlias, true},
	}
	for _, tt := range cases {
		metadata := ShortcutRootStatus(tt.state)
		assert.Equal(t, string(tt.state), metadata.DisplayState)
		assert.Equal(t, string(tt.state), metadata.StateReason)
		assert.Equal(t, tt.issue, metadata.IssueClass)
		assert.NotEmpty(t, metadata.Issue)
		assert.Equal(t, tt.recovery, metadata.RecoveryClass)
		assert.True(t, metadata.AutoRetry)
		assert.Equal(t, tt.protectsPath, metadata.ProtectsPath)
		lifecycle, ok := shortcutRootLifecycleMetadataFor(tt.state)
		require.Truef(t, ok, "missing lifecycle metadata for %s", tt.state)
		assert.Equal(t, metadata, lifecycle.status)
		assert.Equal(t, metadata.ProtectsPath, lifecycle.protectsPath)
		assert.Equal(t, lifecycle.protectsPath, shortcutRootStateKeepsProtectedPaths(tt.state))
		if lifecycle.publishesCleanup {
			assert.Empty(t, lifecycle.runMode)
		}
	}
}

// Validates: R-2.4.8, R-2.4.10
func TestShortcutRootLifecycleMetadataDrivesRunnerProtectionAndCleanup(t *testing.T) {
	t.Parallel()

	cases := []struct {
		state            ShortcutRootState
		protectsPath     bool
		runMode          ShortcutChildRunMode
		publishesCleanup bool
	}{
		{ShortcutRootStateActive, true, ShortcutChildRunModeNormal, false},
		{ShortcutRootStateRemovedFinalDrain, true, ShortcutChildRunModeFinalDrain, false},
		{ShortcutRootStateSamePathReplacementWaiting, true, ShortcutChildRunModeFinalDrain, false},
		{ShortcutRootStateRemovedChildCleanupPending, false, "", true},
		{ShortcutRootStateDuplicateTarget, true, "", false},
	}
	for _, tt := range cases {
		t.Run(string(tt.state), func(t *testing.T) {
			t.Parallel()

			metadata, ok := shortcutRootLifecycleMetadataFor(tt.state)
			require.True(t, ok)
			assert.Equal(t, tt.protectsPath, shortcutRootStateKeepsProtectedPaths(tt.state))
			assert.Equal(t, tt.runMode, shortcutChildRunModeForRoot(tt.state))
			assert.Equal(t, tt.publishesCleanup, metadata.publishesCleanup)
		})
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
		{"clean drain enters release pending", ShortcutRootStateRemovedFinalDrain, shortcutRootEventChildFinalDrainClean, ShortcutRootStateRemovedReleasePending},
		{"release pending cleanup can promote waiting replacement", ShortcutRootStateRemovedReleasePending, shortcutRootEventWaitingReplacementPromote, ShortcutRootStateActive},
		{"duplicate target blocks active", ShortcutRootStateActive, shortcutRootEventDuplicateTargetDetected, ShortcutRootStateDuplicateTarget},
		{"duplicate target resolves to active", ShortcutRootStateDuplicateTarget, shortcutRootEventDuplicateTargetResolved, ShortcutRootStateActive},
		{"duplicate target local root ready stays duplicate", ShortcutRootStateDuplicateTarget, shortcutRootEventLocalRootReady, ShortcutRootStateDuplicateTarget},
		{"alias rename success restores blocked root", ShortcutRootStateAliasMutationBlocked, shortcutRootEventAliasMutationSucceeded, ShortcutRootStateActive},
		{"alias delete success drains active root", ShortcutRootStateActive, shortcutRootEventAliasMutationSucceeded, ShortcutRootStateRemovedFinalDrain},
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
func TestPlanMissingMaterializedShortcutRootChoosesAliasEffect(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		NamespaceID:       "personal:owner@example.com",
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		RemoteDriveID:     driveid.New("remote-drive"),
		RemoteItemID:      "remote-root",
		RemoteIsFolder:    true,
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}

	deletePlan := planMissingMaterializedShortcutRoot(record, "Shared/Docs", nil)
	assert.Equal(t, shortcutRootMissingAliasDelete, deletePlan.Action)
	assert.Equal(t, shortcutAliasMutationDelete, deletePlan.Mutation.Kind)
	assert.Equal(t, "binding-1", deletePlan.Mutation.BindingItemID)

	renamePlan := planMissingMaterializedShortcutRoot(record, "Shared/Docs", []string{"Shared/Renamed"})
	assert.Equal(t, shortcutRootMissingAliasRename, renamePlan.Action)
	assert.Equal(t, shortcutAliasMutationRename, renamePlan.Mutation.Kind)
	assert.Equal(t, "Shared/Renamed", renamePlan.Mutation.RelativeLocalPath)
	assert.Equal(t, "Renamed", renamePlan.Mutation.LocalAlias)

	ambiguousPlan := planMissingMaterializedShortcutRoot(record, "Shared/Docs", []string{"Shared/A", "Shared/B"})
	assert.Equal(t, shortcutRootMissingAliasRenameAmbiguous, ambiguousPlan.Action)
	assert.Equal(t, ShortcutRootStateRenameAmbiguous, ambiguousPlan.Next.State)
	assert.ElementsMatch(t, []string{"Shared/Docs", "Shared/A", "Shared/B"}, ambiguousPlan.Next.ProtectedPaths)
}

// Validates: R-2.4.8, R-2.4.10
func TestPlanMissingMaterializedShortcutRootPrefersHistoricalProjectionMove(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		NamespaceID:       "personal:owner@example.com",
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs", "Shared/Old"},
	}

	plan := planMissingMaterializedShortcutRoot(record, "Shared/Docs", []string{"Shared/Old"})

	assert.Equal(t, shortcutRootMissingAliasMoveProjection, plan.Action)
	assert.Equal(t, "Shared/Old", plan.FromRelativePath)
	assert.Equal(t, "Shared/Docs", plan.ToRelativePath)
}

// Validates: R-2.4.8, R-2.4.10
func TestPlanMissingAliasMutationResultsKeepSideEffectsOutOfDecision(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		LocalAlias:        "Docs",
		State:             ShortcutRootStateActive,
		ProtectedPaths:    []string{"Shared/Docs"},
	}

	deleteFailed := planMissingAliasDeleteResult(record, shortcutRootAliasMutationResult{
		MutationErr: errors.New("delete denied"),
	})
	assert.Equal(t, shortcutRootLocalKeepRecord, deleteFailed.Action)
	assert.Equal(t, ShortcutRootStateAliasMutationBlocked, deleteFailed.Next.State)
	assert.Contains(t, deleteFailed.Next.BlockedDetail, "delete denied")

	deleteSucceeded := planMissingAliasDeleteResult(record, shortcutRootAliasMutationResult{})
	assert.Equal(t, shortcutRootLocalDropRecord, deleteSucceeded.Action)
	assert.False(t, deleteSucceeded.Keep)
	assert.True(t, deleteSucceeded.Changed)

	renameFailed := planMissingAliasRenameResult(record, "Shared/Renamed", shortcutRootAliasMutationResult{
		MutationErr: errors.New("rename denied"),
	})
	assert.Equal(t, ShortcutRootStateAliasMutationBlocked, renameFailed.Next.State)
	assert.Contains(t, renameFailed.Next.ProtectedPaths, "Shared/Renamed")

	renameIdentityFailed := planMissingAliasRenameResult(record, "Shared/Renamed", shortcutRootAliasMutationResult{
		IdentityErr: errors.New("identity denied"),
	})
	assert.Equal(t, ShortcutRootStateLocalRootUnavailable, renameIdentityFailed.Next.State)

	renameSucceeded := planMissingAliasRenameResult(record, "Shared/Renamed", shortcutRootAliasMutationResult{
		Identity: &synctree.FileIdentity{Device: 11, Inode: 12},
	})
	assert.Equal(t, ShortcutRootStateActive, renameSucceeded.Next.State)
	assert.Equal(t, "Shared/Renamed", renameSucceeded.Next.RelativeLocalPath)
	require.NotNil(t, renameSucceeded.Next.LocalRootIdentity)
	assert.Equal(t, uint64(11), renameSucceeded.Next.LocalRootIdentity.Device)
}

// Validates: R-2.4.8, R-2.4.10
func TestPlanShortcutRootMaterializeResultKeepsSideEffectsOutOfDecision(t *testing.T) {
	t.Parallel()

	activeRecord := ShortcutRootRecord{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateActive,
	}
	unavailableRecord := activeRecord
	unavailableRecord.State = ShortcutRootStateLocalRootUnavailable

	createFailed := planShortcutRootMaterializeResult(activeRecord, shortcutRootMaterializeResult{
		CreateErr: synctree.ErrUnsafePath,
	})
	assert.Equal(t, ShortcutRootStateBlockedPath, createFailed.Next.State)
	assert.True(t, createFailed.Keep)

	identityFailed := planShortcutRootMaterializeResult(unavailableRecord, shortcutRootMaterializeResult{
		IdentityErr: errors.New("permission denied"),
	})
	assert.Equal(t, ShortcutRootStateLocalRootUnavailable, identityFailed.Next.State)
	assert.Contains(t, identityFailed.Next.BlockedDetail, "permission denied")

	created := planShortcutRootMaterializeResult(unavailableRecord, shortcutRootMaterializeResult{
		Identity: &synctree.FileIdentity{Device: 7, Inode: 8},
	})
	assert.Equal(t, ShortcutRootStateActive, created.Next.State)
	require.NotNil(t, created.Next.LocalRootIdentity)
	assert.Equal(t, uint64(7), created.Next.LocalRootIdentity.Device)
}

// Validates: R-2.4.8, R-2.4.10
func TestPlanShortcutProjectionMoveResultKeepsSideEffectsOutOfDecision(t *testing.T) {
	t.Parallel()

	activeRecord := ShortcutRootRecord{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateActive,
	}
	unavailableRecord := activeRecord
	unavailableRecord.State = ShortcutRootStateLocalRootUnavailable

	moveFailed := planShortcutProjectionMoveResult(activeRecord, shortcutRootProjectionMoveResult{
		MoveErr: errors.New("target exists"),
	})
	assert.Equal(t, ShortcutRootStateBlockedPath, moveFailed.Next.State)
	assert.Contains(t, moveFailed.Next.BlockedDetail, "target exists")

	identityFailed := planShortcutProjectionMoveResult(unavailableRecord, shortcutRootProjectionMoveResult{
		IdentityErr: errors.New("identity unavailable"),
	})
	assert.Equal(t, ShortcutRootStateLocalRootUnavailable, identityFailed.Next.State)

	moved := planShortcutProjectionMoveResult(unavailableRecord, shortcutRootProjectionMoveResult{
		Identity: &synctree.FileIdentity{Device: 9, Inode: 10},
	})
	assert.Equal(t, ShortcutRootStateActive, moved.Next.State)
	require.NotNil(t, moved.Next.LocalRootIdentity)
	assert.Equal(t, uint64(9), moved.Next.LocalRootIdentity.Device)
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

// Validates: R-2.4.8, R-2.4.10
func TestPlanShortcutRootReleaseCleanupBlocksOnCleanupError(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		BindingItemID:     "binding-1",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
	}

	plan := planShortcutRootReleaseCleanup(&record, errors.New("permission denied"))

	require.Error(t, plan.Err)
	assert.True(t, plan.Changed)
	require.Len(t, plan.Records, 1)
	assert.Equal(t, ShortcutRootStateRemovedCleanupBlocked, plan.Records[0].State)
	assert.Equal(t, []string{"Shared/Docs"}, plan.Records[0].ProtectedPaths)
	assert.Contains(t, plan.Records[0].BlockedDetail, "permission denied")
}

// Validates: R-2.4.8, R-2.4.10
func TestPlanShortcutRootReleaseCleanupPromotesWaitingReplacement(t *testing.T) {
	t.Parallel()

	record := ShortcutRootRecord{
		NamespaceID:       "personal:owner@example.com",
		BindingItemID:     "old-binding",
		RelativeLocalPath: "Shared/Docs",
		State:             ShortcutRootStateRemovedReleasePending,
		ProtectedPaths:    []string{"Shared/Docs"},
		Waiting: &ShortcutRootReplacement{
			BindingItemID:     "new-binding",
			RelativeLocalPath: "Shared/Docs",
			LocalAlias:        "Docs",
			RemoteDriveID:     driveid.New("new-drive"),
			RemoteItemID:      "new-item",
			RemoteIsFolder:    true,
		},
	}

	plan := planShortcutRootReleaseCleanup(&record, nil)

	require.NoError(t, plan.Err)
	assert.True(t, plan.Changed)
	require.Len(t, plan.Records, 2)
	assert.Equal(t, ShortcutRootStateRemovedChildCleanupPending, plan.Records[0].State)
	assert.Equal(t, ShortcutRootStateActive, plan.Records[1].State)
	assert.Equal(t, "new-binding", plan.Records[1].BindingItemID)
	assert.Empty(t, plan.Records[0].ProtectedPaths)
}
