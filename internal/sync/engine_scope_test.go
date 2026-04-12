package sync

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncscope"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

func seedInterruptedScopeTransitionState(t *testing.T, syncRoot string) string {
	t.Helper()

	snapshot, err := syncscope.NewSnapshot(syncscope.Config{}, nil)
	require.NoError(t, err)
	snapshotJSON, err := syncscope.MarshalSnapshot(snapshot)
	require.NoError(t, err)

	dbPath := filepath.Join(filepath.Dir(syncRoot), "test.db")
	store, err := syncstore.NewSyncStore(t.Context(), dbPath, testLogger(t))
	require.NoError(t, err)
	_, err = store.DB().ExecContext(t.Context(), `
		UPDATE remote_state
		SET is_filtered = 1, filter_generation = 1, filter_reason = ?
		WHERE item_id = ?`,
		synctypes.RemoteFilterPathScope,
		"drop-item",
	)
	require.NoError(t, err)
	require.NoError(t, store.ApplyScopeState(t.Context(), ScopeStateApplyRequest{
		State: ScopeStateRecord{
			Generation:            2,
			EffectiveSnapshotJSON: snapshotJSON,
			ObservationPlanHash:   "interrupted",
			ObservationMode:       synctypes.ScopeObservationRootDelta,
			WebsocketEnabled:      true,
			PendingReentry:        true,
			LastReconcileKind:     synctypes.ScopeReconcileEnteredPath,
			UpdatedAt:             time.Now().UnixNano(),
		},
	}))
	require.NoError(t, store.Close(context.Background()))

	return dbPath
}

func reopenEngineForInterruptedScopeRepair(
	t *testing.T,
	dbPath string,
	syncRoot string,
	driveID driveid.ID,
	mock *engineMockClient,
) *Engine {
	t.Helper()

	reopened, err := newEngine(t.Context(), &engineInputs{
		DBPath:                 dbPath,
		SyncRoot:               syncRoot,
		DriveID:                driveID,
		Fetcher:                mock,
		SocketIOFetcher:        mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		PathConvergenceFactory: &enginePathConvergenceStub{},
		FolderDelta:            mock,
		RecursiveLister:        mock,
		PermChecker:            mock,
		Logger:                 testLogger(t),
	})
	require.NoError(t, err)
	reopened.assertInvariants = true
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	return reopened
}

// Validates: R-2.4.5
func TestApplyRemoteScope_ClassifiesBoundaryMoves(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	logger := testLogger(t)

	tests := []struct {
		name             string
		config           syncscope.Config
		event            ChangeEvent
		wantObserved     int
		wantFiltered     bool
		wantEmittedTypes []synctypes.ChangeType
		wantEmittedPaths []string
	}{
		{
			name: "move from in-scope to out-of-scope becomes delete on exit",
			config: syncscope.Config{
				SyncPaths: []string{"docs"},
			},
			event: ChangeEvent{
				Source:   synctypes.SourceRemote,
				Type:     synctypes.ChangeMove,
				DriveID:  driveID,
				ItemID:   "move-1",
				OldPath:  "docs/keep.txt",
				Path:     "archive/keep.txt",
				ItemType: synctypes.ItemTypeFile,
			},
			wantObserved:     1,
			wantFiltered:     true,
			wantEmittedTypes: []synctypes.ChangeType{synctypes.ChangeDelete},
			wantEmittedPaths: []string{"docs/keep.txt"},
		},
		{
			name: "move from out-of-scope to in-scope becomes create on entry",
			config: syncscope.Config{
				SyncPaths: []string{"docs"},
			},
			event: ChangeEvent{
				Source:   synctypes.SourceRemote,
				Type:     synctypes.ChangeMove,
				DriveID:  driveID,
				ItemID:   "move-2",
				OldPath:  "archive/keep.txt",
				Path:     "docs/keep.txt",
				ItemType: synctypes.ItemTypeFile,
			},
			wantObserved:     1,
			wantFiltered:     false,
			wantEmittedTypes: []synctypes.ChangeType{synctypes.ChangeCreate},
			wantEmittedPaths: []string{"docs/keep.txt"},
		},
		{
			name: "move entirely outside scope is dropped from planning",
			config: syncscope.Config{
				SyncPaths: []string{"docs"},
			},
			event: ChangeEvent{
				Source:   synctypes.SourceRemote,
				Type:     synctypes.ChangeMove,
				DriveID:  driveID,
				ItemID:   "move-3",
				OldPath:  "archive/old.txt",
				Path:     "archive/new.txt",
				ItemType: synctypes.ItemTypeFile,
			},
			wantObserved:     1,
			wantFiltered:     true,
			wantEmittedTypes: []synctypes.ChangeType{},
			wantEmittedPaths: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			snapshot, err := syncscope.NewSnapshot(tt.config, nil)
			require.NoError(t, err)

			result := applyRemoteScope(logger, snapshot, 7, []ChangeEvent{tt.event})
			require.Len(t, result.observed, tt.wantObserved)
			assert.Equal(t, tt.wantFiltered, result.observed[0].Filtered)

			gotTypes := make([]synctypes.ChangeType, 0, len(result.emitted))
			gotPaths := make([]string, 0, len(result.emitted))
			for i := range result.emitted {
				gotTypes = append(gotTypes, result.emitted[i].Type)
				gotPaths = append(gotPaths, result.emitted[i].Path)
			}

			assert.Equal(t, tt.wantEmittedTypes, gotTypes)
			assert.Equal(t, tt.wantEmittedPaths, gotPaths)
		})
	}
}

// Validates: R-2.4.4
func TestRunOnce_PersistsEffectiveScopeSnapshot(t *testing.T) {
	t.Parallel()

	eng, syncRoot := newTestEngine(t, &engineMockClient{})
	eng.syncScopeConfig = syncscope.Config{
		IgnoreMarker: ".odignore",
	}

	writeLocalFile(t, syncRoot, "visible.txt", "visible")
	writeLocalFile(t, syncRoot, "blocked/.odignore", "")
	writeLocalFile(t, syncRoot, "blocked/secret.txt", "blocked")

	_, err := eng.RunOnce(t.Context(), SyncUploadOnly, RunOptions{})
	require.NoError(t, err)

	scopeState, found, err := eng.baseline.ReadScopeState(t.Context())
	require.NoError(t, err)
	require.True(t, found, "scope state should be persisted")

	persisted, err := syncscope.UnmarshalSnapshot(scopeState.EffectiveSnapshotJSON)
	require.NoError(t, err)
	assert.True(t, persisted.AllowsPath("visible.txt"))
	assert.False(t, persisted.AllowsPath("blocked/secret.txt"))
	assert.True(t, persisted.HasMarkerDir("blocked"))
	assert.Equal(t, synctypes.ScopeObservationRootDelta, scopeState.ObservationMode)
}

// Validates: R-2.4.5
func TestRunOnce_ScopeExpansionReconcilesPreviouslyFilteredRemoteItems(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	var deltaTokens []string
	var downloaded []string
	contents := map[string]string{
		"keep-item": "keep-data",
		"drop-item": "drop-data",
	}

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, token string) (*graph.DeltaPage, error) {
			deltaTokens = append(deltaTokens, token)
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "keep-item",
					Name:         "keep.txt",
					ParentID:     "root",
					DriveID:      driveID,
					QuickXorHash: hashContentQuickXor(t, contents["keep-item"]),
					Size:         int64(len(contents["keep-item"])),
				},
				{
					ID:           "drop-item",
					Name:         "drop.txt",
					ParentID:     "root",
					DriveID:      driveID,
					QuickXorHash: hashContentQuickXor(t, contents["drop-item"]),
					Size:         int64(len(contents["drop-item"])),
				},
			}, "delta-token-1"), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			downloaded = append(downloaded, itemID)
			n, err := w.Write([]byte(contents[itemID]))
			return int64(n), err
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/keep.txt"},
	}

	firstReport, err := eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, firstReport.Downloads)
	assert.Equal(t, []string{"keep-item"}, downloaded)

	filteredRow, found, err := eng.baseline.GetRemoteStateByPath(t.Context(), "drop.txt", driveID)
	require.NoError(t, err)
	require.True(t, found)
	assert.True(t, filteredRow.IsFiltered)

	downloaded = nil
	eng.syncScopeConfig = syncscope.Config{}

	secondReport, err := eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, secondReport.Downloads)
	assert.Equal(t, []string{"drop-item"}, downloaded)
	assert.Equal(t, []string{"", ""}, deltaTokens, "scope expansion should force a full reconciliation instead of replaying the saved delta token")

	reenteredRow, reenteredFound, reenteredErr := eng.baseline.GetRemoteStateByPath(t.Context(), "drop.txt", driveID)
	require.NoError(t, reenteredErr)
	require.True(t, reenteredFound)
	assert.False(t, reenteredRow.IsFiltered)

	scopeState, found, err := eng.baseline.ReadScopeState(t.Context())
	require.NoError(t, err)
	require.True(t, found)

	persisted, err := syncscope.UnmarshalSnapshot(scopeState.EffectiveSnapshotJSON)
	require.NoError(t, err)
	assert.True(t, persisted.AllowsPath("drop.txt"))
	assert.False(t, scopeState.PendingReentry)
}

// Validates: R-2.4.4, R-2.4.5
func TestRunOnce_StartupRepair_InterruptedScopeTransitionClearsPendingReentryOnSuccess(t *testing.T) {
	t.Parallel()

	driveID := driveid.New(engineTestDriveID)
	var downloaded []string
	contents := map[string]string{
		"keep-item": "keep-data",
		"drop-item": "drop-data",
	}
	mock := newTwoFileDownloadDeltaMock(t, driveID, contents, &downloaded, "delta-token")

	eng, syncRoot := newTestEngine(t, mock)
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/keep.txt"},
	}

	_, err := eng.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	require.NoError(t, eng.Close(context.Background()))

	dbPath := seedInterruptedScopeTransitionState(t, syncRoot)
	reopened := reopenEngineForInterruptedScopeRepair(t, dbPath, syncRoot, driveID, mock)
	reopened.syncScopeConfig = syncscope.Config{}

	downloaded = nil
	report, err := reopened.RunOnce(t.Context(), SyncDownloadOnly, RunOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, report.Downloads)
	assert.Equal(t, []string{"drop-item"}, downloaded)

	scopeState, found, err := reopened.baseline.ReadScopeState(t.Context())
	require.NoError(t, err)
	require.True(t, found)
	assert.False(t, scopeState.PendingReentry)
	assert.Equal(t, synctypes.ScopeReconcileNone, scopeState.LastReconcileKind)

	reenteredRow, rowFound, rowErr := reopened.baseline.GetRemoteStateByPath(t.Context(), "drop.txt", driveID)
	require.NoError(t, rowErr)
	require.True(t, rowFound)
	assert.False(t, reenteredRow.IsFiltered)
}

// Validates: R-2.4.5
func TestFetchRemoteChanges_SyncPathsPersonalUsesFolderScopedDelta(t *testing.T) {
	t.Parallel()

	const (
		docsPath      = "Docs"
		docsToken     = "docs-token"
		reportRelPath = "Docs/report.txt"
	)

	var rootDeltaCalls int
	var scopedDeltaCalls []string

	mock := &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			rootDeltaCalls++
			return deltaPageWithItems(nil, "root-token"), nil
		},
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			switch remotePath {
			case reportRelPath:
				return &graph.Item{ID: "report-id", Name: "report.txt", ParentID: "docs-id"}, nil
			case docsPath:
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			default:
				return nil, graph.ErrNotFound
			}
		},
		folderDeltaFn: func(_ context.Context, _ driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			scopedDeltaCalls = append(scopedDeltaCalls, folderID+":"+token)
			return []graph.Item{{
				ID:       "report-id",
				Name:     "report.txt",
				ParentID: "docs-id",
			}}, docsToken, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/Docs/report.txt"},
	}

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncDownloadOnly,
		Purpose:  observationPlanPurposeOneShot,
	})
	require.NoError(t, err)

	result, err := flow.fetchRemoteChanges(t.Context(), bl, &plan, false)
	require.NoError(t, err)
	require.Len(t, result.events, 1)
	assert.Equal(t, reportRelPath, result.events[0].Path)
	assert.Zero(t, rootDeltaCalls)
	assert.Equal(t, []string{"docs-id:"}, scopedDeltaCalls)
	require.Len(t, result.deferred, 1)
	assert.Equal(t, "docs-id", result.deferred[0].scopeID)
	assert.Equal(t, docsToken, result.deferred[0].token)
}

// Validates: R-2.4.5
func TestFetchRemoteChanges_SyncPathsPersonalFallsBackToRecursiveEnumerationWhenScopedDeltaUnavailable(t *testing.T) {
	t.Parallel()

	const docsPath = "Docs"

	var scopedDeltaCalls []string
	var recursiveCalls []string

	mock := &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			if remotePath == docsPath {
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			}

			return nil, graph.ErrNotFound
		},
		folderDeltaFn: func(_ context.Context, _ driveid.ID, folderID, token string) ([]graph.Item, string, error) {
			scopedDeltaCalls = append(scopedDeltaCalls, folderID+":"+token)
			return nil, "", graph.ErrNotFound
		},
		listChildrenRecursiveFn: func(_ context.Context, _ driveid.ID, folderID string) ([]graph.Item, error) {
			recursiveCalls = append(recursiveCalls, folderID)
			return []graph.Item{{
				ID:       "report-id",
				Name:     "report.txt",
				ParentID: "docs-id",
			}}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/Docs"},
	}

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncDownloadOnly,
		Purpose:  observationPlanPurposeOneShot,
	})
	require.NoError(t, err)

	result, err := flow.fetchRemoteChanges(t.Context(), bl, &plan, false)
	require.NoError(t, err)
	require.Len(t, result.events, 1)
	assert.Equal(t, "Docs/report.txt", result.events[0].Path)
	assert.Equal(t, []string{"docs-id:"}, scopedDeltaCalls)
	assert.Equal(t, []string{"docs-id"}, recursiveCalls)
	assert.Empty(t, result.deferred)
	assert.Equal(t, []string{"Docs"}, result.fullPrefixes)
}

// Validates: R-2.4.5
func TestFetchRemoteChanges_SyncPathsBusinessUsesRecursiveEnumeration(t *testing.T) {
	t.Parallel()

	const docsPath = "Docs"

	var scopedDeltaCalls int
	var recursiveCalls []string

	mock := &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			if remotePath == docsPath {
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			}

			return nil, graph.ErrNotFound
		},
		folderDeltaFn: func(_ context.Context, _ driveid.ID, _, _ string) ([]graph.Item, string, error) {
			scopedDeltaCalls++
			return nil, "", nil
		},
		listChildrenRecursiveFn: func(_ context.Context, _ driveid.ID, folderID string) ([]graph.Item, error) {
			recursiveCalls = append(recursiveCalls, folderID)
			return []graph.Item{{
				ID:       "report-id",
				Name:     "report.txt",
				ParentID: "docs-id",
			}}, nil
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypeBusiness
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/Docs"},
	}

	bl, err := eng.baseline.Load(t.Context())
	require.NoError(t, err)

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)
	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncDownloadOnly,
		Purpose:  observationPlanPurposeOneShot,
	})
	require.NoError(t, err)

	result, err := flow.fetchRemoteChanges(t.Context(), bl, &plan, false)
	require.NoError(t, err)
	require.Len(t, result.events, 1)
	assert.Equal(t, "Docs/report.txt", result.events[0].Path)
	assert.Zero(t, scopedDeltaCalls)
	assert.Equal(t, []string{"docs-id"}, recursiveCalls)
	assert.Empty(t, result.deferred)
}

// Validates: R-2.4.5
func TestBuildObservationSessionPlan_PrimaryScopeUsesScopedTargetPhase(t *testing.T) {
	t.Parallel()

	const docsPath = "Docs"

	mock := &engineMockClient{
		getItemByPathFn: func(_ context.Context, _ driveid.ID, remotePath string) (*graph.Item, error) {
			if remotePath == docsPath {
				return &graph.Item{ID: "docs-id", Name: docsPath, IsFolder: true}, nil
			}

			return nil, graph.ErrNotFound
		},
	}

	eng, _ := newTestEngine(t, mock)
	eng.driveType = driveid.DriveTypePersonal
	eng.syncScopeConfig = syncscope.Config{
		SyncPaths: []string{"/" + docsPath},
	}

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)

	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)

	assert.Equal(t, observationPhaseDriverScopedTarget, plan.PrimaryPhase.Driver)
	assert.Equal(t, observationPhaseDispatchPolicySequentialTargets, plan.PrimaryPhase.DispatchPolicy)
	assert.False(t, flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase))
	require.Len(t, plan.PrimaryPhase.Targets, 1)
	assert.Equal(t, observationPhaseErrorPolicyFailBatch, plan.PrimaryPhase.ErrorPolicy)
	assert.Equal(t, observationPhaseFallbackPolicyDeltaToEnumerate, plan.PrimaryPhase.FallbackPolicy)
	assert.Equal(t, observationPhaseTokenCommitPolicyAfterPlannerAccepts, plan.PrimaryPhase.TokenCommitPolicy)
	assert.Empty(t, plan.ShortcutPhase.Targets)
}

// Validates: R-2.4.5
func TestBuildObservationSessionPlan_ScopedRootUsesScopedRootPhase(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.rootItemID = "root-scope"

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)

	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)

	assert.Equal(t, observationPhaseDriverScopedRoot, plan.PrimaryPhase.Driver)
	assert.Equal(t, observationPhaseDispatchPolicySingleBatch, plan.PrimaryPhase.DispatchPolicy)
	assert.False(t, flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase))
	assert.Equal(t, synctypes.ScopeObservationScopedDelta, flow.scopeObservationMode(&plan))
}

// Validates: R-2.4.5
func TestBuildObservationSessionPlan_FullDriveUsesExplicitRootDeltaPhase(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngine(t, &engineMockClient{})
	eng.enableWebsocket = true
	eng.socketIOFetcher = &engineMockClient{}

	flow := testEngineFlow(t, eng)
	session, err := flow.BuildScopeSession(t.Context(), nil)
	require.NoError(t, err)

	plan, err := flow.BuildObservationSessionPlan(t.Context(), ObservationPlanRequest{
		Session:  &session,
		SyncMode: SyncBidirectional,
		Purpose:  observationPlanPurposeWatch,
	})
	require.NoError(t, err)

	assert.Equal(t, observationPhaseDriverRootDelta, plan.PrimaryPhase.Driver)
	assert.Equal(t, observationPhaseDispatchPolicySingleBatch, plan.PrimaryPhase.DispatchPolicy)
	assert.Equal(t, observationPhaseErrorPolicyFailBatch, plan.PrimaryPhase.ErrorPolicy)
	assert.Equal(t, observationPhaseFallbackPolicyNone, plan.PrimaryPhase.FallbackPolicy)
	assert.Equal(t, observationPhaseTokenCommitPolicyAfterPlannerAccepts, plan.PrimaryPhase.TokenCommitPolicy)
	assert.True(t, flow.websocketEnabledForPrimaryPhase(plan.PrimaryPhase))
}
