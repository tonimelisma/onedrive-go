package sync

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func newTestPermHandler(t *testing.T, checker permissionChecker) (*permissionHandler, string) {
	t.Helper()

	syncRoot := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))

	store := newTestStore(t)

	return &permissionHandler{
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

	result, err := ph.handle403(t.Context(), &Baseline{}, "some/path.txt", ActionUpload)
	require.NoError(t, err)
	assert.Equal(t, permissionEvidenceNone, result.Kind)
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoPermissionRoot(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	ph, _ := newTestPermHandler(t, checker)

	result, err := ph.handle403(t.Context(), &Baseline{}, "unmatched/path.txt", ActionUpload)
	require.NoError(t, err)
	assert.Equal(t, permissionEvidenceNone, result.Kind)
}

// Validates: R-2.14.1, R-6.6.28
//
// A failure to read the persisted block scopes used to be Debug-logged and
// answered as "no blocked boundaries", which re-admitted writes under a
// read-only remote subtree. It is a store error and is returned.
func TestPermHandler_Handle403_BlockScopeReadFailureIsReturned(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	ph, _ := newTestPermHandler(t, checker)
	ph.store = closedTestStore(t)

	boundaries, err := ph.ActiveRemoteBlockedBoundaries(t.Context())
	require.Error(t, err)
	assert.Nil(t, boundaries)
	assert.Contains(t, err.Error(), "list remote block scopes")

	result, err := ph.handle403(t.Context(), &Baseline{}, "some/path.txt", ActionUpload)
	require.Error(t, err, "handle403 surfaces the store failure instead of probing as if nothing were blocked")
	assert.Equal(t, permissionEvidenceNone, result.Kind)
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
	assert.NotEqual(t, permissionEvidenceNone, result.Kind)
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
	assert.Equal(t, permissionEvidenceNone, result.Kind)
}

func TestPermHandler_HandleLocalPermission_SyncRootInaccessible(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	require.NoError(t, os.Chmod(syncRoot, 0o000))
	r := &actionCompletion{
		Path:       "file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.NotEqual(t, permissionEvidenceNone, decision.Kind)
	assert.Equal(t, permissionEvidenceFileDenied, decision.Kind)
	assert.Equal(t, issueLocalReadDenied, decision.IssueType)
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	r := &actionCompletion{
		Path:       "blocked/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.NotEqual(t, permissionEvidenceNone, decision.Kind)
	assert.Equal(t, permissionEvidenceBoundaryDenied, decision.Kind)
	assert.Equal(t, issueLocalReadDenied, decision.IssueType)
	assert.Equal(t, "blocked", decision.BoundaryPath)
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)

	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	r := &actionCompletion{
		Path:       "accessible/file.txt",
		ActionType: ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.NotEqual(t, permissionEvidenceNone, decision.Kind)
	assert.Equal(t, permissionEvidenceFileDenied, decision.Kind)
	assert.Equal(t, "accessible/file.txt", decision.TriggerPath)
}

// Validates: R-2.10.61
func TestPermHandler_HandleLocalPermission_ReadOnlyMountIsOneBoundary(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)
	mount := filepath.Join(syncRoot, "media")
	require.NoError(t, os.MkdirAll(filepath.Join(mount, "photos", "2026"), 0o750))
	// Everything at or below media/ is on the read-only filesystem.
	ph.readOnlyDir = func(absDir string) bool {
		return absDir == mount || strings.HasPrefix(absDir, mount+string(filepath.Separator))
	}

	evidence := ph.handleLocalPermission(t.Context(), &actionCompletion{
		Path:       "media/photos/2026/a.jpg",
		ActionType: ActionDownload,
		Err:        &os.PathError{Op: "open", Path: "a.jpg", Err: syscall.EROFS},
	})

	assert.Equal(t, permissionEvidenceBoundaryDenied, evidence.Kind)
	assert.Equal(t, "media", evidence.BoundaryPath)
	assert.Equal(t, issueLocalReadOnly, evidence.IssueType)
	assert.Equal(t, SKLocalReadOnly("media"), scopeKeyForPermissionEvidence(evidence))
}

// Validates: R-2.10.61
func TestPermHandler_HandleLocalPermission_ReadOnlySyncRootBlocksWholeTree(t *testing.T) {
	t.Parallel()

	ph, syncRoot := newTestPermHandler(t, nil)
	require.NoError(t, os.MkdirAll(filepath.Join(syncRoot, "docs"), 0o750))
	ph.readOnlyDir = func(string) bool { return true }

	evidence := ph.handleLocalPermission(t.Context(), &actionCompletion{
		Path:       "docs/a.txt",
		ActionType: ActionDownload,
		Err:        &os.PathError{Op: "open", Path: "a.txt", Err: syscall.EROFS},
	})

	assert.Equal(t, permissionEvidenceBoundaryDenied, evidence.Kind)
	assert.Empty(t, evidence.BoundaryPath, "the walk stops at the sync root")
	scope := scopeKeyForPermissionEvidence(evidence)
	assert.True(t, scope.BlocksAction("docs/a.txt", "", ActionDownload))
	assert.True(t, scope.BlocksAction("other/b.txt", "", ActionLocalDelete))
	assert.False(t, scope.BlocksAction("docs/a.txt", "", ActionUpload), "uploads only read and keep running")
	assert.True(t, scope.PersistsInBlockScopes(), "a timed trial revalidates the mount")
	assert.Equal(t, issueLocalReadOnly, scope.ConditionType())
}

// Validates: R-2.10.61
func TestPermHandler_HandleLocalPermission_ReadOnlyWithWritableParentStaysPerFile(t *testing.T) {
	t.Parallel()

	ph, _ := newTestPermHandler(t, nil)
	ph.readOnlyDir = func(string) bool { return false }

	evidence := ph.handleLocalPermission(t.Context(), &actionCompletion{
		Path:       "bound.txt",
		ActionType: ActionDownload,
		Err:        &os.PathError{Op: "open", Path: "bound.txt", Err: syscall.EROFS},
	})

	assert.Equal(t, permissionEvidenceFileDenied, evidence.Kind)
	assert.Equal(t, issueLocalReadOnly, evidence.IssueType)
}
