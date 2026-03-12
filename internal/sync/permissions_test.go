package sync

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// ---------------------------------------------------------------------------
// Mock permission checker
// ---------------------------------------------------------------------------

type mockPermChecker struct {
	// Map of "driveID:itemID" → permissions
	perms      map[string][]graph.Permission
	err        error
	notFoundOn bool     // if true, return graph.ErrNotFound for unknown keys
	calls      []string // records "driveID:itemID" for each call
}

func (m *mockPermChecker) ListItemPermissions(_ context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
	key := driveID.String() + ":" + itemID
	m.calls = append(m.calls, key)

	if m.err != nil {
		return nil, m.err
	}

	if perms, ok := m.perms[key]; ok {
		return perms, nil
	}

	if m.notFoundOn {
		return nil, graph.ErrNotFound
	}

	return nil, fmt.Errorf("unknown key: %s", key)
}

// ---------------------------------------------------------------------------
// findShortcutForPath tests
// ---------------------------------------------------------------------------

func TestFindShortcutForPath(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
		{ItemID: "sc-1", LocalPath: "Shared/TeamDocs"},
		{ItemID: "sc-2", LocalPath: "Shared/Other"},
	}

	tests := []struct {
		name    string
		path    string
		wantID  string
		wantNil bool
	}{
		{"exact match", "Shared/TeamDocs", "sc-1", false},
		{"child path", "Shared/TeamDocs/sub/file.txt", "sc-1", false},
		{"different shortcut", "Shared/Other/doc.txt", "sc-2", false},
		{"no match", "Unrelated/path.txt", "", true},
		{"partial prefix mismatch", "Shared/TeamDocsExtra/file.txt", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := findShortcutForPath(shortcuts, tt.path)
			if tt.wantNil {
				assert.Nil(t, got)
			} else {
				require.NotNil(t, got)
				assert.Equal(t, tt.wantID, got.ItemID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// handle403 tests
// ---------------------------------------------------------------------------

// newTestEngineWithPerms creates an engine with a mock permission checker
// and seeds baseline entries for the given paths.
func newTestEngineWithPerms(t *testing.T, checker PermissionChecker, shortcuts []Shortcut, baselineEntries []Outcome) (*Engine, *Baseline, string) { //nolint:unparam // syncRoot useful for callers
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o755))

	mock := &engineMockClient{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(&EngineConfig{
		DBPath:      dbPath,
		SyncRoot:    syncRoot,
		DriveID:     driveID,
		Fetcher:     mock,
		Items:       mock,
		Downloads:   mock,
		Uploads:     mock,
		PermChecker: checker,
		Logger:      logger,
	})
	require.NoError(t, err)

	ctx := t.Context()

	// Seed baseline entries.
	for i := range baselineEntries {
		require.NoError(t, eng.baseline.CommitOutcome(ctx, &baselineEntries[i]))
	}

	// Register shortcuts.
	for i := range shortcuts {
		require.NoError(t, eng.baseline.UpsertShortcut(ctx, &shortcuts[i]))
	}

	// Load baseline after seeding so tests get a populated snapshot.
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		assert.NoError(t, eng.Close())
	})

	return eng, bl, syncRoot
}

// Validates: R-2.14.1
func TestHandle403_ReadOnlyFolder_RecordsIssueAtBoundary(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Parent folder is read-only.
			driveid.New(remoteDriveID).String() + ":parent-folder-id": {{ID: "p1", Roles: []string{"read"}}},
			// Grandparent (shortcut root) is writable.
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p2", Roles: []string{"write"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "parent-folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	result := eng.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)
	assert.True(t, result, "handle403 should return true for read-only folder")

	// Should have recorded a permission_denied issue at the boundary (sub folder).
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs/sub", issues[0].Path)

	// Should have populated the cache.
	canWrite, ok := eng.permCache.get("Shared/TeamDocs/sub")
	assert.True(t, ok, "cache should contain boundary path")
	assert.False(t, canWrite, "boundary should be cached as read-only")
}

// Validates: R-2.14.1
func TestHandle403_TransientError_NoSuppression(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Parent folder is actually writable — transient 403.
			driveid.New(remoteDriveID).String() + ":parent-folder-id": {{ID: "p1", Roles: []string{"write"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "parent-folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	result := eng.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)
	assert.False(t, result, "handle403 should return false for transient 403")

	// No issue should be recorded — transient 403.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

// Validates: R-2.14.1
func TestHandle403_WholeShareReadOnly_BoundaryAtRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Both parent and root are read-only.
			driveid.New(remoteDriveID).String() + ":sub-id":  {{ID: "p1", Roles: []string{"read"}}},
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p2", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "sub-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// Boundary should walk all the way up to the shortcut root.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs", issues[0].Path)
}

// Validates: R-2.14.1
func TestHandle403_APIFailure_FailOpen(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		err: fmt.Errorf("network error"),
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "sub-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// No issue — fail-open when API is unavailable.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestHandle403_NoShortcutMatch_Ignored(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	// Path not under any shortcut.
	eng.handle403(ctx, bl, "Documents/file.txt", nil)

	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
	assert.Empty(t, checker.calls, "should not call API when no shortcut matches")
}

// ---------------------------------------------------------------------------
// recheckPermissions tests
// ---------------------------------------------------------------------------

// Validates: R-2.14.1
func TestRecheckPermissions_GrantDetected_IssueCleared(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Folder is now writable.
			driveid.New(remoteDriveID).String() + ":folder-id": {{ID: "p1", Roles: []string{"write"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	// Pre-record a permission_denied issue.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/TeamDocs/sub",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))

	// Verify issue exists.
	before, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, before, 1)

	eng.recheckPermissions(ctx, bl, shortcuts)

	// Issue should be cleared.
	after, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, after)
}

func TestRecheckPermissions_StillDenied_NoChange(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Still read-only.
			driveid.New(remoteDriveID).String() + ":folder-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/TeamDocs/sub",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))

	eng.recheckPermissions(ctx, bl, shortcuts)

	// Issue should remain.
	after, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Len(t, after, 1)
}

func TestRecheckPermissions_NoIssues_NoAPICalls(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	eng.recheckPermissions(ctx, bl, nil)

	assert.Empty(t, checker.calls, "should not call API when there are no issues")
}

// ---------------------------------------------------------------------------
// Fix #7: recheckPermissions caches unresolvable issues as denied.
// ---------------------------------------------------------------------------

func TestRecheckPermissions_UnresolvableIssues_CachedAsDenied(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	// No shortcuts registered — issues won't match any shortcut.
	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	// Record two permission_denied issues.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/NoShortcut/sub",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/Other/locked",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))

	// Recheck with no shortcuts — both issues have sc == nil.
	eng.recheckPermissions(ctx, bl, nil)

	// Both should be cached as denied (canWrite == false).
	canWrite, ok := eng.permCache.get("Shared/NoShortcut/sub")
	assert.True(t, ok, "unresolvable issue should be cached")
	assert.False(t, canWrite, "unresolvable issue should be cached as denied")

	canWrite, ok = eng.permCache.get("Shared/Other/locked")
	assert.True(t, ok, "unresolvable issue should be cached")
	assert.False(t, canWrite, "unresolvable issue should be cached as denied")

	// No API calls — can't resolve without shortcuts.
	assert.Empty(t, checker.calls, "should not call API when no shortcut matches")

	// deniedPrefixes should return both.
	prefixes := eng.permCache.deniedPrefixes()
	assert.Len(t, prefixes, 2)
	assert.Contains(t, prefixes, "Shared/NoShortcut/sub")
	assert.Contains(t, prefixes, "Shared/Other/locked")
}

func TestRecheckPermissions_UnresolvedItemID_CachedAsDenied(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{}

	// Shortcut exists but the issue path is NOT in baseline → remoteItemID == "".
	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	// No baseline entries for the issue path.
	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, nil)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/TeamDocs/missing",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))

	eng.recheckPermissions(ctx, bl, shortcuts)

	// Should be cached as denied even though item ID can't be resolved.
	canWrite, ok := eng.permCache.get("Shared/TeamDocs/missing")
	assert.True(t, ok, "unresolved item ID should be cached")
	assert.False(t, canWrite, "unresolved item ID should be cached as denied")

	// No API calls — can't query without item ID.
	assert.Empty(t, checker.calls)
}

// ---------------------------------------------------------------------------
// permissionCache tests
// ---------------------------------------------------------------------------

func TestPermissionCache(t *testing.T) {
	t.Parallel()

	pc := newPermissionCache()

	// Miss.
	_, ok := pc.get("folder")
	assert.False(t, ok)

	// Set and hit.
	pc.set("folder", true)

	canWrite, ok := pc.get("folder")
	assert.True(t, ok)
	assert.True(t, canWrite)

	// Set read-only.
	pc.set("other", false)

	canWrite, ok = pc.get("other")
	assert.True(t, ok)
	assert.False(t, canWrite)
}

func TestPermissionCache_Reset(t *testing.T) {
	t.Parallel()

	pc := newPermissionCache()

	pc.set("folder", true)
	pc.set("other", false)

	pc.reset()

	_, ok := pc.get("folder")
	assert.False(t, ok, "entries should be cleared after reset")

	_, ok = pc.get("other")
	assert.False(t, ok, "entries should be cleared after reset")
}

func TestPermissionCache_DeniedPrefixes(t *testing.T) {
	t.Parallel()

	pc := newPermissionCache()

	pc.set("Shared/ReadOnly", false)
	pc.set("Shared/Writable", true)
	pc.set("Shared/Other", false)

	prefixes := pc.deniedPrefixes()
	assert.Len(t, prefixes, 2)
	assert.Contains(t, prefixes, "Shared/ReadOnly")
	assert.Contains(t, prefixes, "Shared/Other")
}

func TestPermissionCache_NilSafe(t *testing.T) {
	t.Parallel()

	var pc *permissionCache

	// None of these should panic.
	pc.reset()
	pc.set("folder", true)

	_, ok := pc.get("folder")
	assert.False(t, ok)

	assert.Nil(t, pc.deniedPrefixes())
}

func TestPermissionCache_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	pc := newPermissionCache()

	// Hammer the cache from multiple goroutines to verify thread safety.
	// The race detector will catch unprotected map access.
	done := make(chan struct{})
	for i := range 10 {
		go func(id int) {
			defer func() { done <- struct{}{} }()
			path := fmt.Sprintf("folder/%d", id)

			for range 100 {
				pc.set(path, id%2 == 0)
				pc.get(path)
				pc.deniedPrefixes()
			}
		}(i)
	}

	// One goroutine doing resets.
	go func() {
		defer func() { done <- struct{}{} }()

		for range 50 {
			pc.reset()
		}
	}()

	for range 11 {
		<-done
	}
}

// ---------------------------------------------------------------------------
// handle403 404 fallback test
// ---------------------------------------------------------------------------

func TestHandle403_FolderNotFound_RecordsIssue(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms:      map[string][]graph.Permission{},
		notFoundOn: true, // Return graph.ErrNotFound for unknown keys.
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "sub-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// Should record an issue because the folder returned 404.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs/sub", issues[0].Path)
	assert.Contains(t, issues[0].LastError, "not found")
}

// ---------------------------------------------------------------------------
// handle403 with unresolved parent (falls back to shortcut root)
// ---------------------------------------------------------------------------

func TestHandle403_UnresolvedParent_FallsBackToRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Root is read-only.
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	// Only root is in baseline — parent folder "Shared/TeamDocs/newdir" is NOT.
	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	// File in a folder that's not in baseline yet.
	eng.handle403(ctx, bl, "Shared/TeamDocs/newdir/file.txt", shortcuts)

	// Should fall back to root and record issue there.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs", issues[0].Path)
}

// ---------------------------------------------------------------------------
// recheckPermissions populates cache
// ---------------------------------------------------------------------------

func TestRecheckPermissions_PopulatesCache(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-1"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Still read-only.
			driveid.New(remoteDriveID).String() + ":folder-id": {{ID: "p1", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []Outcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "Shared/TeamDocs/sub",
		Direction:  "upload",
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
	}, nil))

	eng.recheckPermissions(ctx, bl, shortcuts)

	// Cache should have been populated.
	canWrite, ok := eng.permCache.get("Shared/TeamDocs/sub")
	assert.True(t, ok, "cache should contain the checked path")
	assert.False(t, canWrite, "should be cached as read-only")
}
