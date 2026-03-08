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
// isWriteSuppressed tests
// ---------------------------------------------------------------------------

func TestIsWriteSuppressed(t *testing.T) {
	t.Parallel()

	denied := []string{"Shared/ReadOnly", "Shared/Other/Private"}

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"exact denied folder", "Shared/ReadOnly", true},
		{"child of denied", "Shared/ReadOnly/sub/file.txt", true},
		{"different folder", "Shared/Writable/file.txt", false},
		{"partial prefix", "Shared/ReadOnlyExtra/file.txt", false},
		{"exact subfolder denied", "Shared/Other/Private", true},
		{"child of subfolder denied", "Shared/Other/Private/deep/file.txt", true},
		{"sibling of denied subfolder", "Shared/Other/Public/file.txt", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isWriteSuppressed(tt.path, denied))
		})
	}
}

// ---------------------------------------------------------------------------
// isRemoteWriteAction tests
// ---------------------------------------------------------------------------

func TestIsRemoteWriteAction(t *testing.T) {
	t.Parallel()

	tests := []struct {
		action ActionType
		want   bool
	}{
		{ActionUpload, true},
		{ActionRemoteDelete, true},
		{ActionRemoteMove, true},
		{ActionFolderCreate, true},
		{ActionDownload, false},
		{ActionLocalDelete, false},
		{ActionLocalMove, false},
		{ActionConflict, false},
		{ActionUpdateSynced, false},
		{ActionCleanup, false},
	}

	for _, tt := range tests {
		t.Run(tt.action.String(), func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, isRemoteWriteAction(tt.action))
		})
	}
}

// ---------------------------------------------------------------------------
// handle403 tests
// ---------------------------------------------------------------------------

// newTestEngineWithPerms creates an engine with a mock permission checker
// and seeds baseline entries for the given paths.
func newTestEngineWithPerms(t *testing.T, checker PermissionChecker, shortcuts []Shortcut, baselineEntries []Outcome) (*Engine, string) { //nolint:unparam // syncRoot useful for callers
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

	t.Cleanup(func() {
		assert.NoError(t, eng.Close())
	})

	return eng, syncRoot
}

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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// Should have recorded a permission_denied issue at the boundary (sub folder).
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs/sub", issues[0].Path)
}

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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// No issue should be recorded — transient 403.
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// Boundary should walk all the way up to the shortcut root.
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs", issues[0].Path)
}

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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// No issue — fail-open when API is unavailable.
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
}

func TestHandle403_NoShortcutMatch_Ignored(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	// Path not under any shortcut.
	eng.handle403(ctx, "Documents/file.txt", nil)

	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues)
	assert.Empty(t, checker.calls, "should not call API when no shortcut matches")
}

// ---------------------------------------------------------------------------
// recheckPermissions tests
// ---------------------------------------------------------------------------

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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	// Pre-record a permission_denied issue.
	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/TeamDocs/sub", IssuePermissionDenied,
		"folder is read-only", http.StatusForbidden, 0, "",
	))

	// Verify issue exists.
	before, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	require.Len(t, before, 1)

	eng.recheckPermissions(ctx, shortcuts)

	// Issue should be cleared.
	after, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/TeamDocs/sub", IssuePermissionDenied,
		"folder is read-only", http.StatusForbidden, 0, "",
	))

	eng.recheckPermissions(ctx, shortcuts)

	// Issue should remain.
	after, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
	require.NoError(t, err)
	assert.Len(t, after, 1)
}

func TestRecheckPermissions_NoIssues_NoAPICalls(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	eng.recheckPermissions(ctx, nil)

	assert.Empty(t, checker.calls, "should not call API when there are no issues")
}

// ---------------------------------------------------------------------------
// filterDeniedWrites tests
// ---------------------------------------------------------------------------

func TestFilterDeniedWrites_SuppressesWritesUnderDenied(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineWithPerms(t, &mockPermChecker{}, nil, nil)
	ctx := t.Context()

	// Record a denied folder.
	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/ReadOnly", IssuePermissionDenied,
		"read-only", http.StatusForbidden, 0, "",
	))

	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionUpload, Path: "Shared/ReadOnly/file.txt"},
			{Type: ActionDownload, Path: "Shared/ReadOnly/other.txt"},
			{Type: ActionUpload, Path: "Shared/Writable/doc.txt"},
			{Type: ActionRemoteDelete, Path: "Shared/ReadOnly/old.txt"},
		},
		Deps: [][]int{{}, {}, {}, {}},
	}

	filtered := eng.filterDeniedWrites(ctx, plan)

	// Only download and writable upload should remain.
	require.Len(t, filtered.Actions, 2)
	assert.Equal(t, ActionDownload, filtered.Actions[0].Type)
	assert.Equal(t, "Shared/ReadOnly/other.txt", filtered.Actions[0].Path)
	assert.Equal(t, ActionUpload, filtered.Actions[1].Type)
	assert.Equal(t, "Shared/Writable/doc.txt", filtered.Actions[1].Path)
}

func TestFilterDeniedWrites_NoDenied_PassThrough(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineWithPerms(t, &mockPermChecker{}, nil, nil)
	ctx := t.Context()

	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionUpload, Path: "docs/file.txt"},
			{Type: ActionDownload, Path: "docs/other.txt"},
		},
		Deps: [][]int{{}, {}},
	}

	filtered := eng.filterDeniedWrites(ctx, plan)

	// No denied folders — plan unchanged.
	assert.Len(t, filtered.Actions, 2)
}

func TestFilterDeniedWrites_RemapsDependencies(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineWithPerms(t, &mockPermChecker{}, nil, nil)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/ReadOnly", IssuePermissionDenied,
		"read-only", http.StatusForbidden, 0, "",
	))

	// Action 0: folder create (suppressed — under denied)
	// Action 1: upload depends on 0 (suppressed — under denied)
	// Action 2: download (kept)
	// Action 3: upload outside denied, depends on 2 (kept, dep remapped)
	plan := &ActionPlan{
		Actions: []Action{
			{Type: ActionFolderCreate, Path: "Shared/ReadOnly/newdir"},
			{Type: ActionUpload, Path: "Shared/ReadOnly/newdir/file.txt"},
			{Type: ActionDownload, Path: "Other/file.txt"},
			{Type: ActionUpload, Path: "Other/local.txt"},
		},
		Deps: [][]int{{}, {0}, {}, {2}},
	}

	filtered := eng.filterDeniedWrites(ctx, plan)

	require.Len(t, filtered.Actions, 2)
	assert.Equal(t, ActionDownload, filtered.Actions[0].Type)
	assert.Equal(t, ActionUpload, filtered.Actions[1].Type)

	// Action 3 (now index 1) depended on action 2 (now index 0).
	assert.Equal(t, []int{0}, filtered.Deps[1])

	// Action 2 (now index 0) had no deps.
	assert.Empty(t, filtered.Deps[0])
}

// ---------------------------------------------------------------------------
// loadDeniedPrefixes tests
// ---------------------------------------------------------------------------

func TestLoadDeniedPrefixes(t *testing.T) {
	t.Parallel()

	eng, _ := newTestEngineWithPerms(t, &mockPermChecker{}, nil, nil)
	ctx := t.Context()

	// No issues → empty.
	assert.Empty(t, eng.loadDeniedPrefixes(ctx))

	// Add some permission_denied issues.
	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/A", IssuePermissionDenied, "ro", http.StatusForbidden, 0, "",
	))
	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/B", IssuePermissionDenied, "ro", http.StatusForbidden, 0, "",
	))

	// Add a non-permission issue — should NOT appear.
	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Other/file.txt", "upload_failed", "err", 500, 0, "",
	))

	prefixes := eng.loadDeniedPrefixes(ctx)
	assert.Len(t, prefixes, 2)
	assert.Contains(t, prefixes, "Shared/A")
	assert.Contains(t, prefixes, "Shared/B")
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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	eng.handle403(ctx, "Shared/TeamDocs/sub/file.txt", shortcuts)

	// Should record an issue because the folder returned 404.
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	// File in a folder that's not in baseline yet.
	eng.handle403(ctx, "Shared/TeamDocs/newdir/file.txt", shortcuts)

	// Should fall back to root and record issue there.
	issues, err := eng.baseline.ListLocalIssuesByType(ctx, IssuePermissionDenied)
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

	eng, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	require.NoError(t, eng.baseline.RecordLocalIssue(
		ctx, "Shared/TeamDocs/sub", IssuePermissionDenied,
		"folder is read-only", http.StatusForbidden, 0, "",
	))

	eng.recheckPermissions(ctx, shortcuts)

	// Cache should have been populated.
	canWrite, ok := eng.permCache.get("Shared/TeamDocs/sub")
	assert.True(t, ok, "cache should contain the checked path")
	assert.False(t, canWrite, "should be cached as read-only")
}
