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

func seedLocalPermissionDeniedIssue(t *testing.T, store *SyncStore, path string, scopeKey ScopeKey) {
	t.Helper()

	role := FailureRoleItem
	if scopeKey.IsPermLocalWrite() {
		role = FailureRoleBoundary
	}

	err := store.RecordFailure(t.Context(), &SyncFailureParams{
		Path:       path,
		DriveID:    driveid.New("test-drive"),
		Direction:  DirectionDownload,
		ActionType: ActionDownload,
		Role:       role,
		IssueType:  IssueLocalWriteDenied,
		Category:   CategoryActionable,
		ErrMsg:     "permission denied",
		ScopeKey:   scopeKey,
	}, nil)
	require.NoError(t, err)
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NilChecker(t *testing.T) {
	t.Parallel()

	ph, _, _ := newTestPermHandler(t, nil)

	result := ph.handleRemoteWrite403(t.Context(), &Baseline{}, &WorkerResult{
		Path:              "some/path.txt",
		FailurePath:       "some/path.txt",
		ActionType:        ActionUpload,
		FailureCapability: PermissionCapabilityRemoteWrite,
	}, nil)
	assert.False(t, result.Matched)
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoShortcutMatch(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	ph, _, _ := newTestPermHandler(t, checker)

	result := ph.handleRemoteWrite403(t.Context(), &Baseline{}, &WorkerResult{
		Path:              "unmatched/path.txt",
		FailurePath:       "unmatched/path.txt",
		ActionType:        ActionUpload,
		FailureCapability: PermissionCapabilityRemoteWrite,
	}, nil)
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
	assert.Equal(t, "failed/file.txt", result.Failure.Path)
	assert.Equal(t, driveid.New("remote-drive-1"), result.Failure.DriveID)
	assert.Equal(t, IssueRemoteWriteDenied, result.Failure.IssueType)
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
	r := &WorkerResult{
		Path:       "file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	assert.Equal(t, IssueLocalWriteDenied, decision.Failure.IssueType)
	assert.Equal(t, CategoryActionable, decision.Failure.Category)
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	r := &WorkerResult{
		Path:       "blocked/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckActivateBoundaryScope, decision.Kind)
	assert.Equal(t, "blocked", decision.Failure.Path)
	assert.Equal(t, IssueLocalWriteDenied, decision.Failure.IssueType)
	assert.Equal(t, SKPermLocalWrite("blocked"), decision.ScopeBlock.Key)
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	ph, _, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	r := &WorkerResult{
		Path:       "accessible/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	assert.Equal(t, "accessible/file.txt", decision.Failure.Path)
}

func TestPermHandler_RecheckLocalPermissions_Restored(t *testing.T) {
	t.Parallel()

	ph, store, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "restored")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	scopeKey := SKPermLocalWrite("restored")
	seedLocalPermissionDeniedIssue(t, store, "restored", scopeKey)

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
	seedLocalPermissionDeniedIssue(t, store, "blocked", scopeKey)

	decisions := ph.recheckLocalPermissions(t.Context())

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckKeepScope, decisions[0].Kind)
}
