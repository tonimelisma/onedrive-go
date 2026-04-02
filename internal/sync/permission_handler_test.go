package sync

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// mockFailureRecorder — minimal SyncFailureRecorder for direct tests
// ---------------------------------------------------------------------------

type mockFailureRecorder struct {
	failures    []synctypes.SyncFailureParams
	failureRows []synctypes.SyncFailureRow
	clearPaths  []string
	actionable  []synctypes.SyncFailureRow
	byIssueType map[string][]synctypes.SyncFailureRow
}

func newMockFailureRecorder() *mockFailureRecorder {
	return &mockFailureRecorder{
		byIssueType: make(map[string][]synctypes.SyncFailureRow),
	}
}

func (m *mockFailureRecorder) RecordFailure(_ context.Context, p *synctypes.SyncFailureParams, _ func(int) time.Duration) error {
	m.failures = append(m.failures, *p)
	return nil
}

func (m *mockFailureRecorder) ListSyncFailures(_ context.Context) ([]synctypes.SyncFailureRow, error) {
	return m.failureRows, nil
}

func (m *mockFailureRecorder) ListSyncFailuresByIssueType(_ context.Context, issueType string) ([]synctypes.SyncFailureRow, error) {
	return m.byIssueType[issueType], nil
}

func (m *mockFailureRecorder) ListActionableFailures(_ context.Context) ([]synctypes.SyncFailureRow, error) {
	return m.actionable, nil
}

func (m *mockFailureRecorder) ClearSyncFailure(_ context.Context, path string, _ driveid.ID) error {
	m.clearPaths = append(m.clearPaths, path)
	return nil
}

func (m *mockFailureRecorder) ClearActionableSyncFailures(_ context.Context) error { return nil }

func (m *mockFailureRecorder) MarkSyncFailureActionable(_ context.Context, _ string, _ driveid.ID) error {
	return nil
}

func (m *mockFailureRecorder) UpsertActionableFailures(_ context.Context, _ []synctypes.ActionableFailure) error {
	return nil
}

func (m *mockFailureRecorder) ClearResolvedActionableFailures(_ context.Context, _ string, _ []string) error {
	return nil
}

func (m *mockFailureRecorder) ResetRetryTimesForScope(_ context.Context, _ synctypes.ScopeKey, _ time.Time) error {
	return nil
}

// ---------------------------------------------------------------------------
// Direct PermissionHandler unit tests
// ---------------------------------------------------------------------------

func newTestPermHandler(t *testing.T, recorder *mockFailureRecorder, checker synctypes.PermissionChecker) (*PermissionHandler, string) {
	t.Helper()

	syncRoot := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o750))
	tree, err := synctree.Open(syncRoot)
	require.NoError(t, err)

	return &PermissionHandler{
		baseline:    recorder,
		permChecker: checker,
		syncTree:    tree,
		driveID:     driveid.New("test-drive"),
		logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		nowFn:       time.Now,
	}, syncRoot
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NilChecker(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	// nil permChecker → always returns false.
	result := ph.handle403(t.Context(), &synctypes.Baseline{}, "some/path.txt", nil)
	assert.False(t, result.Matched)
	assert.Empty(t, recorder.failures, "should not record any failure")
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoShortcutMatch(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, checker)

	// No shortcuts → returns false.
	result := ph.handle403(t.Context(), &synctypes.Baseline{}, "unmatched/path.txt", nil)
	assert.False(t, result.Matched)
}

func TestPermHandler_HandlePermissionCheckError_NotFound(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	// ErrNotFound → records failure and returns true.
	result := ph.handlePermissionCheckError(t.Context(), graph.ErrNotFound, "failed/file.txt", "failed")
	assert.True(t, result.Matched)
	assert.Equal(t, permissionCheckActivateBoundaryScope, result.Kind)
	assert.Equal(t, "failed", result.Failure.Path)
	assert.Equal(t, synctypes.IssuePermissionDenied, result.Failure.IssueType)
}

func TestPermHandler_HandlePermissionCheckError_OtherError(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	// Other errors → returns false, no failure recorded.
	result := ph.handlePermissionCheckError(t.Context(), errors.New("timeout"), "failed/file.txt", "failed")
	assert.False(t, result.Matched)
	assert.Empty(t, recorder.failures)
}

func TestPermHandler_HandleLocalPermission_SyncRootInaccessible(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, syncRoot := newTestPermHandler(t, recorder, nil)

	// Make sync root inaccessible.
	require.NoError(t, os.Chmod(syncRoot, 0o000))
	r := &synctypes.WorkerResult{
		Path:       "file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	assert.Equal(t, synctypes.IssueLocalPermissionDenied, decision.Failure.IssueType)
	assert.Equal(t, synctypes.CategoryActionable, decision.Failure.Category)
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, syncRoot := newTestPermHandler(t, recorder, nil)

	// Create directory structure, then make subdir inaccessible.
	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	r := &synctypes.WorkerResult{
		Path:       "blocked/file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckActivateBoundaryScope, decision.Kind)
	assert.Equal(t, "blocked", decision.Failure.Path)
	assert.Equal(t, synctypes.IssueLocalPermissionDenied, decision.Failure.IssueType)
	assert.Equal(t, synctypes.SKPermDir("blocked"), decision.ScopeBlock.Key)
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, syncRoot := newTestPermHandler(t, recorder, nil)

	// Create directory (accessible) with a file reference.
	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	r := &synctypes.WorkerResult{
		Path:       "accessible/file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	decision := ph.handleLocalPermission(t.Context(), r)

	require.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)
	assert.Equal(t, "accessible/file.txt", decision.Failure.Path)
}

func TestPermHandler_RecheckLocalPermissions_Restored(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, syncRoot := newTestPermHandler(t, recorder, nil)

	// Create the directory (accessible).
	subDir := filepath.Join(syncRoot, "restored")
	require.NoError(t, os.MkdirAll(subDir, 0o750))

	scopeKey := synctypes.SKPermDir("restored")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "restored",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	decisions := ph.recheckLocalPermissions(t.Context())

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey)
}

func TestPermHandler_RecheckLocalPermissions_StillDenied(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, syncRoot := newTestPermHandler(t, recorder, nil)

	// Create and immediately make inaccessible.
	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o750))
	require.NoError(t, os.Chmod(subDir, 0o000))
	scopeKey := synctypes.SKPermDir("blocked")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "blocked",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	decisions := ph.recheckLocalPermissions(t.Context())

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckKeepScope, decisions[0].Kind)
}

func TestPermHandler_ClearScannerResolved_FileLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:    "docs/file.txt",
			DriveID: driveid.New("test-drive"),
		},
	}

	observed := map[string]bool{"docs/file.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckClearFileFailure, decisions[0].Kind)
	assert.Equal(t, "docs/file.txt", decisions[0].Path)
}

func TestPermHandler_ClearScannerResolved_DirLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	scopeKey := synctypes.SKPermDir("blocked")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "blocked",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	// Observed a path under the blocked directory.
	observed := map[string]bool{"blocked/child.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey)
}

func TestPermHandler_ClearScannerResolved_ReleasesScopedIssueInOneShotMode(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	ph, _ := newTestPermHandler(t, recorder, nil)

	scopeKey := synctypes.SKPermDir("blocked")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "blocked",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	observed := map[string]bool{"blocked/child.txt": true}
	decisions := ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, decisions, 1)
	assert.Equal(t, permissionRecheckReleaseScope, decisions[0].Kind)
	assert.Equal(t, scopeKey, decisions[0].ScopeKey, "scoped permission recovery should release the scope in one-shot mode too")
}
