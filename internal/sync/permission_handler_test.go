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

func newTestPermHandler(t *testing.T, checker PermissionChecker) (*PermissionHandler, string) {
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
	}, syncRoot
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NilChecker(t *testing.T) {
	t.Parallel()

	ph, _ := newTestPermHandler(t, nil)

	result := ph.handle403(t.Context(), &Baseline{}, "some/path.txt", ActionUpload)
	assert.False(t, result.Matched())
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoPermissionRoot(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	ph, _ := newTestPermHandler(t, checker)

	result := ph.handle403(t.Context(), &Baseline{}, "unmatched/path.txt", ActionUpload)
	assert.False(t, result.Matched())
}

func TestPermHandler_HandlePermissionCheckError_NotFound(t *testing.T) {
	t.Parallel()

	ph, _ := newTestPermHandler(t, nil)

	result := ph.handlePermissionCheckError(
		graph.ErrNotFound,
		"failed/file.txt",
		"failed",
		ActionUpload,
	)
	assert.True(t, result.Matched())
	assert.Equal(t, permissionEvidenceBoundaryDenied, result.Kind)
	assert.Equal(t, "failed/file.txt", result.TriggerPath)
	assert.Equal(t, "failed", result.BoundaryPath)
	assert.Equal(t, IssueRemoteWriteDenied, result.IssueType)
}

func TestPermHandler_HandlePermissionCheckError_OtherError(t *testing.T) {
	t.Parallel()

	ph, _ := newTestPermHandler(t, nil)

	result := ph.handlePermissionCheckError(
		errors.New("timeout"),
		"failed/file.txt",
		"failed",
		ActionUpload,
	)
	assert.False(t, result.Matched())
}

func TestPermHandler_HandleLocalPermission_SyncRootInaccessible(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	require.NoError(t, os.Chmod(syncRoot, 0o000))
	r := &ActionCompletion{
		Path:       "file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched())
	assert.Equal(t, permissionEvidenceFileDenied, decision.Kind)
	assert.Equal(t, IssueLocalReadDenied, decision.IssueType)
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	r := &ActionCompletion{
		Path:       "blocked/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched())
	assert.Equal(t, permissionEvidenceBoundaryDenied, decision.Kind)
	assert.Equal(t, IssueLocalReadDenied, decision.IssueType)
	assert.Equal(t, "blocked", decision.BoundaryPath)
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	r := &ActionCompletion{
		Path:       "accessible/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched())
	assert.Equal(t, permissionEvidenceFileDenied, decision.Kind)
	assert.Equal(t, "accessible/file.txt", decision.TriggerPath)
}
