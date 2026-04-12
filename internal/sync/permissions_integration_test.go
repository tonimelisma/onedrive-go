package sync

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func startDrainLoopForEngine(
	t *testing.T,
	eng *testEngine,
) (chan synctypes.WorkerResult, <-chan struct{}, context.CancelFunc) {
	t.Helper()

	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		setupWatchEngine(t, eng)
		rt = testWatchRuntime(t, eng)
	}
	if rt.buf == nil {
		rt.buf = NewBuffer(eng.logger)
	}

	results := make(chan synctypes.WorkerResult, 16)

	ctx, cancel := context.WithCancel(t.Context())
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	safety := synctypes.DefaultSafetyConfig()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer rt.stopTrialTimer()
		runResultDrainLoopForTest(ctx, rt, bl, safety, results)
	}()

	t.Cleanup(func() {
		cancel()
		<-done
	})

	return results, done, cancel
}

// Validates: R-2.10.9, R-2.10.11, R-2.14.4
func TestRemotePermissionRecovery_RedispatchesHeldUploadWithoutNewObservation(t *testing.T) {
	t.Parallel()

	const (
		blockedPath  = "Shared/TeamDocs/sub/file.txt"
		boundaryPath = "Shared/TeamDocs/sub"
	)

	remoteDriveID := permissionsRemoteDriveID
	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":folder-id": {{
				ID:    "perm-1",
				Roles: []string{"write"},
			}},
		},
	}

	shortcuts := []synctypes.Shortcut{{
		ItemID:       "shortcut-1",
		RemoteDrive:  remoteDriveID,
		RemoteItem:   "root-id",
		LocalPath:    "Shared/TeamDocs",
		Observation:  synctypes.ObservationDelta,
		DiscoveredAt: 1000,
	}}
	baselineEntries := []synctypes.Outcome{
		{
			Action:   synctypes.ActionDownload,
			Success:  true,
			Path:     "Shared/TeamDocs",
			DriveID:  driveid.New(remoteDriveID),
			ItemID:   "root-id",
			ParentID: "root",
			ItemType: synctypes.ItemTypeFolder,
		},
		{
			Action:   synctypes.ActionDownload,
			Success:  true,
			Path:     boundaryPath,
			DriveID:  driveid.New(remoteDriveID),
			ItemID:   "folder-id",
			ParentID: "root-id",
			ItemType: synctypes.ItemTypeFolder,
		},
	}

	eng, bl, syncRoot := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	writeLocalFile(t, syncRoot, blockedPath, "blocked upload payload")

	ctx := t.Context()
	scopeKey := synctypes.SKPermRemote(boundaryPath)
	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, blockedPath)
	setTestScopeBlock(t, eng, &synctypes.ScopeBlock{
		Key:       scopeKey,
		IssueType: synctypes.IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	results, done, cancel := startDrainLoopForEngine(t, eng)
	defer cancel()
	_ = done
	_ = results

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	require.Eventually(t, func() bool {
		return !isTestScopeBlocked(eng, scopeKey)
	}, 5*time.Second, 10*time.Millisecond, "permission recovery should release the active remote scope")

	require.Eventually(t, func() bool {
		return len(listRemoteBlockedFailures(t, eng, ctx)) == 0
	}, 5*time.Second, 10*time.Millisecond, "remote-blocked issue rows should be cleared once permissions return")

	var retried *synctypes.TrackedAction
	require.Eventually(t, func() bool {
		select {
		case retried = <-testWatchRuntime(t, eng).dispatchCh:
			return retried != nil && retried.Action.Path == blockedPath
		default:
			return false
		}
	}, 5*time.Second, 10*time.Millisecond, "released shared-folder writes should be redispatched without any new observation event")

	require.NotNil(t, retried)
	assert.Equal(t, synctypes.ActionUpload, retried.Action.Type)
}
