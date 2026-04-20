package sync

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newTestPermHandler(t *testing.T, checker PermissionChecker) (*PermissionHandler, *SyncStore, string) {
	t.Helper()

	syncRoot := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	store := newTestStore(t)

	return &PermissionHandler{
		store:       store,
		permChecker: checker,
		syncTree:    mustOpenSyncTree(t, syncRoot),
		driveID:     driveid.New("test-drive"),
		logger:      newTestLogger(t),
		nowFn:       time.Now,
	}, store, syncRoot
}

func seedLocalPermissionDeniedIssue(t *testing.T, store *SyncStore, scopeKey ScopeKey) {
	t.Helper()

	if scopeKey.IsPermDir() {
		issueType := IssueLocalWriteDenied
		if scopeKey.IsPermLocalRead() {
			issueType = IssueLocalReadDenied
		}
		require.NoError(t, store.UpsertBlockScope(t.Context(), &BlockScope{
			Key:          scopeKey,
			IssueType:    issueType,
			TimingSource: ScopeTimingNone,
			BlockedAt:    time.Now(),
		}))
		return
	}
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NilChecker(t *testing.T) {
	t.Parallel()

	ph, _, _ := newTestPermHandler(t, nil)

	result := ph.handle403(t.Context(), &Baseline{}, "some/path.txt", ActionUpload)
	assert.False(t, result.Matched)
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoPermissionRoot(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	ph, _, _ := newTestPermHandler(t, checker)

	result := ph.handle403(t.Context(), &Baseline{}, "unmatched/path.txt", ActionUpload)
	assert.False(t, result.Matched)
}

func TestPermHandler_HandlePermissionCheckError_NotFound(t *testing.T) {
	t.Parallel()

	ph, _, _ := newTestPermHandler(t, nil)

	result := ph.handlePermissionCheckError(
		t.Context(),
		graph.ErrNotFound,
		"failed/file.txt",
		"failed",
		ActionUpload,
		driveid.New("remote-drive-1"),
	)
	assert.True(t, result.Matched)
	assert.Equal(t, permissionCheckActivateDerivedScope, result.Kind)
	require.NotNil(t, result.RetryWorkFailure)
	assert.Equal(t, "failed/file.txt", result.RetryWorkFailure.Path)
	assert.Equal(t, IssueRemoteWriteDenied, result.RetryWorkFailure.IssueType)
	require.NotNil(t, result.BlockScope)
	assert.Equal(t, IssueRemoteWriteDenied, result.BlockScope.IssueType)
	assert.Equal(t, SKPermRemoteWrite("failed"), result.ScopeKey)
}

func TestPermHandler_HandlePermissionCheckError_OtherError(t *testing.T) {
	t.Parallel()

	ph, _, _ := newTestPermHandler(t, nil)

	result := ph.handlePermissionCheckError(
		t.Context(),
		errors.New("timeout"),
		"failed/file.txt",
		"failed",
		ActionUpload,
		driveid.New("remote-drive-1"),
	)
	assert.False(t, result.Matched)
}

func TestPermHandler_HandleLocalPermission_SyncRootInaccessible(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)

	require.NoError(t, os.Chmod(syncRoot, 0o000))
	r := &ActionCompletion{
		Path:       "file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	require.NotNil(t, decision.RetryWorkFailure)
	assert.Equal(t, IssueLocalReadDenied, decision.RetryWorkFailure.IssueType)
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	r := &ActionCompletion{
		Path:       "blocked/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckActivateBoundaryScope, decision.Kind)
	require.NotNil(t, decision.RetryWorkFailure)
	assert.Equal(t, IssueLocalReadDenied, decision.RetryWorkFailure.IssueType)
	require.NotNil(t, decision.BlockScope)
	assert.Equal(t, SKPermLocalRead("blocked"), decision.BlockScope.Key)
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	r := &ActionCompletion{
		Path:       "accessible/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	require.NotNil(t, decision.RetryWorkFailure)
	assert.Equal(t, "accessible/file.txt", decision.RetryWorkFailure.Path)
}

func TestPermHandler_RecheckLocalPermissions_Restored(t *testing.T) {
	t.Parallel()

	ph, store, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "restored")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	scopeKey := SKPermLocalWrite("restored")
	seedLocalPermissionDeniedIssue(t, store, scopeKey)

	decisions := ph.recheckLocalPermissions(t.Context())

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey)
}

func TestPermHandler_RecheckLocalPermissions_StillDenied(t *testing.T) {
	t.Parallel()

	ph, store, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))

	scopeKey := SKPermLocalWrite("blocked")
	seedLocalPermissionDeniedIssue(t, store, scopeKey)

	decisions := ph.recheckLocalPermissions(t.Context())

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckKeepScope, decisions[0].Kind)
}

func TestPermHandler_ClearScannerResolved_FileLevelIgnored(t *testing.T) {
	t.Parallel()

	ph, store, _ := newTestPermHandler(t, nil)
	seedLocalPermissionDeniedIssue(t, store, ScopeKey{})

	observed := map[string]bool{"docs/file.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	assert.Empty(t, decisions)
}

func TestPermHandler_ClearScannerResolved_DirLevel(t *testing.T) {
	t.Parallel()

	ph, store, _ := newTestPermHandler(t, nil)

	scopeKey := SKPermLocalRead("blocked")
	seedLocalPermissionDeniedIssue(t, store, scopeKey)

	observed := map[string]bool{"blocked/child.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey)
}

func TestPermHandler_ClearScannerResolved_ReleasesScopedIssueInOneShotMode(t *testing.T) {
	t.Parallel()

	ph, store, _ := newTestPermHandler(t, nil)

	scopeKey := SKPermLocalRead("blocked")
	seedLocalPermissionDeniedIssue(t, store, scopeKey)

	observed := map[string]bool{"blocked/child.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey, "scoped permission recovery should release the scope in one-shot mode too")
}
