package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
)

func assertNoDurablePermissionPipelineState(
	t *testing.T,
	store *syncstore.SyncStore,
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

	evidence := ph.handleLocalPermission(t.Context(), syncexec.CompletionForTest(&syncdomain.ActionCompletion{
		Path:              "accessible/file.txt",
		ActionType:        syncdomain.ActionDownload,
		Err:               os.ErrPermission,
		ErrMsg:            "permission denied",
		FailureCapability: syncdomain.PermissionCapabilityLocalRead,
	}))

	require.NotEqual(t, permissionEvidenceNone, evidence.Kind)
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
		syncdomain.ActionUpload,
	)

	assert.Equal(t, permissionEvidenceNone, evidence.Kind)
	assertNoDurablePermissionPipelineState(t, ph.store)
}

// Validates: R-2.14.1
func TestPermissionApply_ActivateTimedRemoteWriteScope_PersistsRetryWorkAndScope(t *testing.T) {
	t.Parallel()

	eng := newSingleOwnerEngine(t)
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, flow.applyPermissionFailureEvidence(t.Context(), nil, &syncdomain.ActionCompletion{
		Path:       "Shared/Docs/file.txt",
		ActionType: syncdomain.ActionUpload,
	}, permissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: "Shared/Docs",
		TriggerPath:  "Shared/Docs/file.txt",
		IssueType:    syncdomain.IssueRemoteWriteDenied,
	}, true))

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
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKPermLocalRead("Private")

	require.NoError(t, flow.applyPermissionFailureEvidence(t.Context(), nil, &syncdomain.ActionCompletion{
		Path:       "Private/file.txt",
		ActionType: syncdomain.ActionDownload,
	}, permissionEvidence{
		Kind:         permissionEvidenceBoundaryDenied,
		BoundaryPath: "Private",
		TriggerPath:  "Private/file.txt",
		IssueType:    syncdomain.IssueLocalReadDenied,
	}, false))

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
	flow := testEngineFlow(t, eng)
	scopeKey := syncdomain.SKPermRemoteWrite("Shared/Docs")

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           scopeKey,
		TrialInterval: time.Minute,
		NextTrialAt:   time.Unix(61, 0),
	}))

	require.NoError(t, flow.applyPermissionFailureEvidence(t.Context(), nil, &syncdomain.ActionCompletion{
		Path:       "Shared/Docs/file.txt",
		ActionType: syncdomain.ActionUpload,
	}, permissionEvidence{
		Kind:         permissionEvidenceKnownActiveBoundary,
		BoundaryPath: "Shared/Docs",
		TriggerPath:  "Shared/Docs/file.txt",
		IssueType:    syncdomain.IssueRemoteWriteDenied,
	}, true))
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
