package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildPermissionMaintenancePlan_StartupCombinesResolvedRetryAndRechecks(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)
	now := time.Unix(1_700_000_000, 0)
	ph.nowFn = func() time.Time { return now }
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "restored"), 0o750))

	plan := buildPermissionMaintenancePlan(
		t.Context(),
		ph,
		&Baseline{},
		permissionMaintenanceRequest{Reason: permissionMaintenanceStartup},
		permissionMaintenanceSnapshot{trackLastCheckedAt: true},
		permissionMaintenanceState{
			blockScopes: []*BlockScope{{
				Key:           SKPermLocalWrite("restored"),
				ConditionType: IssueLocalWriteDenied,
			}},
			blockedRetryWork: []permissionBlockedRetryState{{
				row: RetryWorkRow{
					Path:       "Shared/file.txt",
					ActionType: ActionRemoteDelete,
					ScopeKey:   SKPermRemoteWrite("Shared"),
				},
				resolved: true,
			}},
		},
	)

	assert.True(t, plan.UpdateLastCheckedAt)
	assert.Equal(t, now, plan.CheckedAt)
	require.Len(t, plan.RetryWorkReadies, 1)
	assert.Equal(t, "Shared/file.txt", plan.RetryWorkReadies[0].Path)
	require.Len(t, plan.Decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, plan.Decisions[0].Kind)
	assert.Equal(t, SKPermLocalWrite("restored"), plan.Decisions[0].ScopeKey)
}

func TestBuildPermissionMaintenancePlan_PeriodicHonorsCadence(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)
	now := time.Unix(1_700_000_000, 0)
	ph.nowFn = func() time.Time { return now }
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "restored"), 0o750))

	state := permissionMaintenanceState{
		blockScopes: []*BlockScope{{
			Key:           SKPermLocalWrite("restored"),
			ConditionType: IssueLocalWriteDenied,
		}},
	}

	throttled := buildPermissionMaintenancePlan(
		t.Context(),
		ph,
		&Baseline{},
		permissionMaintenanceRequest{Reason: permissionMaintenancePeriodic},
		permissionMaintenanceSnapshot{
			lastCheckedAt:      now.Add(-(permissionMaintenanceInterval - time.Second)),
			trackLastCheckedAt: true,
		},
		state,
	)
	assert.False(t, throttled.UpdateLastCheckedAt)
	assert.Empty(t, throttled.Decisions)

	due := buildPermissionMaintenancePlan(
		t.Context(),
		ph,
		&Baseline{},
		permissionMaintenanceRequest{Reason: permissionMaintenancePeriodic},
		permissionMaintenanceSnapshot{
			lastCheckedAt:      now.Add(-(permissionMaintenanceInterval + time.Second)),
			trackLastCheckedAt: true,
		},
		state,
	)
	assert.True(t, due.UpdateLastCheckedAt)
	assert.Equal(t, now, due.CheckedAt)
	require.Len(t, due.Decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, due.Decisions[0].Kind)
}

func TestBuildPermissionMaintenancePlan_LocalObservationOnlyReadiesRelevantResolvedRetryWork(t *testing.T) {
	t.Parallel()

	plan := buildPermissionMaintenancePlan(
		t.Context(),
		nil,
		nil,
		permissionMaintenanceRequest{
			Reason:       permissionMaintenanceLocalObservation,
			ChangedPaths: map[string]bool{"Shared": true},
		},
		permissionMaintenanceSnapshot{},
		permissionMaintenanceState{
			blockedRetryWork: []permissionBlockedRetryState{
				{
					row: RetryWorkRow{
						Path:       "Shared/file.txt",
						ActionType: ActionRemoteDelete,
						ScopeKey:   SKPermRemoteWrite("Shared"),
					},
					resolved: true,
				},
				{
					row: RetryWorkRow{
						Path:       "Else/file.txt",
						ActionType: ActionRemoteDelete,
						ScopeKey:   SKPermRemoteWrite("Else"),
					},
					resolved: true,
				},
			},
		},
	)

	require.Len(t, plan.RetryWorkReadies, 1)
	assert.Equal(t, "Shared/file.txt", plan.RetryWorkReadies[0].Path)
	assert.Empty(t, plan.Decisions)
}
