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
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// mockScopeManager — test double for the scopeManager interface
// ---------------------------------------------------------------------------

type mockScopeManager struct {
	scopeBlocks     map[synctypes.ScopeKey]*synctypes.ScopeBlock
	scopeClears     []synctypes.ScopeKey
	watchMode       bool
	setScopeCount   int
	clearScopeCount int
}

func newMockScopeManager(watchMode bool) *mockScopeManager {
	return &mockScopeManager{
		scopeBlocks: make(map[synctypes.ScopeKey]*synctypes.ScopeBlock),
		watchMode:   watchMode,
	}
}

func (m *mockScopeManager) setScopeBlock(key synctypes.ScopeKey, block *synctypes.ScopeBlock) {
	m.scopeBlocks[key] = block
	m.setScopeCount++
}

func (m *mockScopeManager) onScopeClear(_ context.Context, key synctypes.ScopeKey) {
	m.scopeClears = append(m.scopeClears, key)
	m.clearScopeCount++
}

func (m *mockScopeManager) isWatchMode() bool {
	return m.watchMode
}

// ---------------------------------------------------------------------------
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

func newTestPermHandler(t *testing.T, recorder *mockFailureRecorder, checker synctypes.PermissionChecker, sm *mockScopeManager) (*PermissionHandler, string) {
	t.Helper()

	syncRoot := filepath.Join(t.TempDir(), "sync")
	require.NoError(t, os.MkdirAll(syncRoot, 0o755))

	return &PermissionHandler{
		baseline:    recorder,
		permChecker: checker,
		permCache:   newPermissionCache(),
		syncRoot:    syncRoot,
		driveID:     driveid.New("test-drive"),
		logger:      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})),
		nowFn:       time.Now,
		scopeMgr:    sm,
	}, syncRoot
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NilChecker(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

	// nil permChecker → always returns false.
	result := ph.handle403(t.Context(), &synctypes.Baseline{}, "some/path.txt", nil)
	assert.False(t, result)
	assert.Empty(t, recorder.failures, "should not record any failure")
}

// Validates: R-2.14.1
func TestPermHandler_Handle403_NoShortcutMatch(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{perms: map[string][]graph.Permission{}}
	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, _ := newTestPermHandler(t, recorder, checker, sm)

	// No shortcuts → returns false.
	result := ph.handle403(t.Context(), &synctypes.Baseline{}, "unmatched/path.txt", nil)
	assert.False(t, result)
}

func TestPermHandler_HandlePermissionCheckError_NotFound(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

	// ErrNotFound → records failure and returns true.
	result := ph.handlePermissionCheckError(t.Context(), graph.ErrNotFound, "failed/file.txt", "failed")
	assert.True(t, result)
	require.Len(t, recorder.failures, 1)
	assert.Equal(t, "failed", recorder.failures[0].Path)
	assert.Equal(t, synctypes.IssuePermissionDenied, recorder.failures[0].IssueType)
}

func TestPermHandler_HandlePermissionCheckError_OtherError(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

	// Other errors → returns false, no failure recorded.
	result := ph.handlePermissionCheckError(t.Context(), errors.New("timeout"), "failed/file.txt", "failed")
	assert.False(t, result)
	assert.Empty(t, recorder.failures)
}

func TestPermHandler_HandleLocalPermission_SyncRootInaccessible(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, syncRoot := newTestPermHandler(t, recorder, nil, sm)

	// Make sync root inaccessible.
	require.NoError(t, os.Chmod(syncRoot, 0o000))
	t.Cleanup(func() { os.Chmod(syncRoot, 0o755) })
	r := &synctypes.WorkerResult{
		Path:       "file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	ph.handleLocalPermission(t.Context(), r)

	// Should record a top-level failure, not a scope block.
	require.Len(t, recorder.failures, 1)
	assert.Equal(t, synctypes.IssueLocalPermissionDenied, recorder.failures[0].IssueType)
	assert.Equal(t, synctypes.CategoryActionable, recorder.failures[0].Category)
	assert.Empty(t, sm.scopeBlocks, "no scope block for sync root failure")
}

func TestPermHandler_HandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, syncRoot := newTestPermHandler(t, recorder, nil, sm)

	// Create directory structure, then make subdir inaccessible.
	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.Chmod(subDir, 0o000))
	t.Cleanup(func() { os.Chmod(subDir, 0o755) })
	r := &synctypes.WorkerResult{
		Path:       "blocked/file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	ph.handleLocalPermission(t.Context(), r)

	// Should record failure at directory level and create scope block.
	require.Len(t, recorder.failures, 1)
	assert.Equal(t, "blocked", recorder.failures[0].Path)
	assert.Equal(t, synctypes.IssueLocalPermissionDenied, recorder.failures[0].IssueType)
	assert.Len(t, sm.scopeBlocks, 1, "should create scope block for directory")
}

func TestPermHandler_HandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, syncRoot := newTestPermHandler(t, recorder, nil, sm)

	// Create directory (accessible) with a file reference.
	subDir := filepath.Join(syncRoot, "accessible")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	r := &synctypes.WorkerResult{
		Path:       "accessible/file.txt",
		ActionType: synctypes.ActionDownload,
		ErrMsg:     "permission denied",
	}

	ph.handleLocalPermission(t.Context(), r)

	// File-level: no scope block, failure at file level.
	require.Len(t, recorder.failures, 1)
	assert.Equal(t, "accessible/file.txt", recorder.failures[0].Path)
	assert.Empty(t, sm.scopeBlocks, "no scope block for file-level issues")
}

func TestPermHandler_RecheckLocalPermissions_Restored(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(true) // watch mode
	ph, syncRoot := newTestPermHandler(t, recorder, nil, sm)

	// Create the directory (accessible).
	subDir := filepath.Join(syncRoot, "restored")
	require.NoError(t, os.MkdirAll(subDir, 0o755))

	scopeKey := synctypes.SKPermDir("restored")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "restored",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	ph.recheckLocalPermissions(t.Context())

	// Should clear the failure and invoke onScopeClear.
	require.Len(t, recorder.clearPaths, 1)
	assert.Equal(t, "restored", recorder.clearPaths[0])
	require.Len(t, sm.scopeClears, 1)
	assert.Equal(t, scopeKey, sm.scopeClears[0])
}

func TestPermHandler_RecheckLocalPermissions_StillDenied(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(true)
	ph, syncRoot := newTestPermHandler(t, recorder, nil, sm)

	// Create and immediately make inaccessible.
	subDir := filepath.Join(syncRoot, "blocked")
	require.NoError(t, os.MkdirAll(subDir, 0o755))
	require.NoError(t, os.Chmod(subDir, 0o000))
	t.Cleanup(func() { os.Chmod(subDir, 0o755) })
	scopeKey := synctypes.SKPermDir("blocked")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "blocked",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	ph.recheckLocalPermissions(t.Context())

	// Should NOT clear — directory still inaccessible.
	assert.Empty(t, recorder.clearPaths)
	assert.Empty(t, sm.scopeClears)
}

func TestPermHandler_ClearScannerResolved_FileLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false)
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:    "docs/file.txt",
			DriveID: driveid.New("test-drive"),
		},
	}

	observed := map[string]bool{"docs/file.txt": true}
	ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, recorder.clearPaths, 1)
	assert.Equal(t, "docs/file.txt", recorder.clearPaths[0])
}

func TestPermHandler_ClearScannerResolved_DirLevel(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(true) // watch mode for onScopeClear
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

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
	ph.clearScannerResolvedPermissions(t.Context(), observed)

	require.Len(t, recorder.clearPaths, 1)
	assert.Equal(t, "blocked", recorder.clearPaths[0])
	require.Len(t, sm.scopeClears, 1)
	assert.Equal(t, scopeKey, sm.scopeClears[0])
}

func TestPermHandler_ClearScannerResolved_NotWatchMode(t *testing.T) {
	t.Parallel()

	recorder := newMockFailureRecorder()
	sm := newMockScopeManager(false) // NOT watch mode
	ph, _ := newTestPermHandler(t, recorder, nil, sm)

	scopeKey := synctypes.SKPermDir("blocked")
	recorder.byIssueType[synctypes.IssueLocalPermissionDenied] = []synctypes.SyncFailureRow{
		{
			Path:     "blocked",
			DriveID:  driveid.New("test-drive"),
			ScopeKey: scopeKey,
		},
	}

	observed := map[string]bool{"blocked/child.txt": true}
	ph.clearScannerResolvedPermissions(t.Context(), observed)

	// Failure cleared, but no onScopeClear call (not watch mode).
	require.Len(t, recorder.clearPaths, 1)
	assert.Empty(t, sm.scopeClears, "onScopeClear not called in non-watch mode")
}
