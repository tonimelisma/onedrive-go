package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func assertNoDurablePermissionPipelineState(
	t *testing.T,
	store *SyncStore,
) {
	t.Helper()

	retryRows, err := store.ListRetryWork(t.Context())
	require.NoError(t, err)
	assert.Empty(t, retryRows)

	blockScopes, err := store.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blockScopes)

	observationIssues, err := store.ListObservationIssues(t.Context())
	require.NoError(t, err)
	assert.Empty(t, observationIssues)
}

// Validates: R-2.14.1
func TestPermissionProbe_HandleLocalPermission_DoesNotPersistDurableState(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "accessible"), 0o750))

	evidence := ph.handleLocalPermission(t.Context(), &ActionCompletion{
		Path:              "accessible/file.txt",
		ActionType:        ActionDownload,
		Err:               os.ErrPermission,
		ErrMsg:            "permission denied",
		FailureCapability: PermissionCapabilityLocalRead,
	})

	require.True(t, evidence.Matched())
	assert.Equal(t, permissionEvidenceFileDenied, evidence.Kind)
	assertNoDurablePermissionPipelineState(t, ph.store)
}

// Validates: R-2.14.1
func TestPermissionProbe_HandlePermissionCheckError_DoesNotPersistDurableState(t *testing.T) {
	t.Parallel()

	ph, _ := newTestPermHandler(t, nil)

	evidence := ph.handlePermissionCheckError(
		assert.AnError,
		"Shared/Docs/file.txt",
		"Shared/Docs",
		ActionUpload,
	)

	assert.False(t, evidence.Matched())
	assertNoDurablePermissionPipelineState(t, ph.store)
}

// Validates: R-2.14.1
func TestPermissionApply_ActivateTimedRemoteWriteScope_PersistsRetryWorkAndScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	matched := controller.applyPermissionOutcome(t.Context(), nil, permissionFlowRemote403, &PermissionOutcome{
		Matched:      true,
		Kind:         permissionOutcomeActivateDerivedScope,
		ScopeKey:     scopeKey,
		BoundaryPath: "Shared/Docs",
		TriggerPath:  "Shared/Docs/file.txt",
		RetryWorkFailure: &RetryWorkFailure{
			Path:          "Shared/Docs/file.txt",
			ActionType:    ActionUpload,
			ConditionType: IssueRemoteWriteDenied,
			ScopeKey:      scopeKey,
			LastError:     "folder is read-only",
			HTTPStatus:    403,
			Blocked:       true,
		},
	})

	require.True(t, matched)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Shared/Docs/file.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, scopeKey, blockScopes[0].Key)
}

// Validates: R-2.14.1
func TestPermissionApply_ReadBoundaryScope_DoesNotPersistBlockScopeRow(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKPermLocalRead("Private")

	matched := controller.applyPermissionOutcome(t.Context(), nil, permissionFlowLocalPermission, &PermissionOutcome{
		Matched:      true,
		Kind:         permissionOutcomeActivateBoundaryScope,
		ScopeKey:     scopeKey,
		BoundaryPath: "Private",
		TriggerPath:  "Private/file.txt",
		RetryWorkFailure: &RetryWorkFailure{
			Path:          "Private/file.txt",
			ActionType:    ActionDownload,
			ConditionType: IssueLocalReadDenied,
			ScopeKey:      scopeKey,
			LastError:     "directory not accessible",
			Blocked:       true,
		},
	})

	require.True(t, matched)

	retryRows := listRetryWorkForTest(t, eng.baseline, t.Context())
	require.Len(t, retryRows, 1)
	assert.Equal(t, "Private/file.txt", retryRows[0].Path)
	assert.Equal(t, scopeKey, retryRows[0].ScopeKey)
	assert.True(t, retryRows[0].Blocked)

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	assert.Empty(t, blockScopes, "read-boundary outcomes must not materialize block_scopes rows")
}

// Validates: R-2.10.33, R-2.14.1
func TestPermissionApply_KnownActiveBoundary_DoesNotPersistOrArmRetryTimer(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	setupWatchEngine(t, eng)
	rt := testWatchRuntime(t, eng)
	controller := testEngineFlow(t, eng).scopeController()
	scopeKey := SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &BlockScope{
		Key:           scopeKey,
		BlockedAt:     time.Unix(1, 0),
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(61, 0),
	}))

	matched := controller.applyPermissionOutcome(t.Context(), rt, permissionFlowRemote403, &PermissionOutcome{
		Matched:      true,
		Kind:         permissionOutcomeNone,
		BoundaryPath: "Shared/Docs",
		TriggerPath:  "Shared/Docs/file.txt",
	})

	require.True(t, matched)
	assert.False(t, rt.hasRetryTimer())
	assert.Empty(t, listRetryWorkForTest(t, eng.baseline, t.Context()))

	blockScopes, err := eng.baseline.ListBlockScopes(t.Context())
	require.NoError(t, err)
	require.Len(t, blockScopes, 1)
	assert.Equal(t, scopeKey, blockScopes[0].Key)

	observationIssues, err := eng.baseline.ListObservationIssues(t.Context())
	require.NoError(t, err)
	assert.Empty(t, observationIssues)
}
