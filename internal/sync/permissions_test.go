package sync

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

const permissionsRemoteDriveID = "remote-drive-1"

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

func TestFindShortcutForPath_EmptyLocalPathMatchesConfiguredRoot(t *testing.T) {
	t.Parallel()

	shortcuts := []Shortcut{
		{ItemID: "nested", LocalPath: "Nested"},
		{ItemID: "root", LocalPath: ""},
	}

	require.NotNil(t, findShortcutForPath(shortcuts, "Nested/file.txt"))
	assert.Equal(t, "nested", findShortcutForPath(shortcuts, "Nested/file.txt").ItemID)

	require.NotNil(t, findShortcutForPath(shortcuts, "top-level.txt"))
	assert.Equal(t, "root", findShortcutForPath(shortcuts, "top-level.txt").ItemID)

	require.NotNil(t, findShortcutForPath(shortcuts, ""))
	assert.Equal(t, "root", findShortcutForPath(shortcuts, "").ItemID)
}

// ---------------------------------------------------------------------------
// handle403 tests
// ---------------------------------------------------------------------------

// newTestEngineWithPerms creates an engine with a mock permission checker
// and seeds baseline entries for the given paths.
func newTestEngineWithPerms(
	t *testing.T,
	checker PermissionChecker,
	shortcuts []Shortcut,
	baselineEntries []ActionOutcome,
) (*testEngine, *Baseline, string) {
	t.Helper()

	return newTestEngineWithPermsAndRoot(t, checker, shortcuts, baselineEntries, "")
}

func newTestEngineWithPermsAndRoot(
	t *testing.T,
	checker PermissionChecker,
	shortcuts []Shortcut,
	baselineEntries []ActionOutcome,
	rootItemID string,
) (*testEngine, *Baseline, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o700))

	mock := &engineMockClient{}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	driveID := driveid.New(engineTestDriveID)

	eng, err := newEngine(t.Context(), &engineInputs{
		DBPath:       dbPath,
		SyncRoot:     syncRoot,
		DriveID:      driveID,
		AccountEmail: "recipient@example.com",
		Fetcher:      mock,
		Items:        mock,
		Downloads:    mock,
		Uploads:      mock,
		RootItemID:   rootItemID,
		PermChecker:  checker,
		Logger:       logger,
	})
	require.NoError(t, err)
	testEng := newFlowBackedTestEngine(eng)

	ctx := t.Context()

	// Seed baseline entries.
	for i := range baselineEntries {
		require.NoError(t, eng.baseline.CommitMutation(ctx, mutationFromActionOutcome(&baselineEntries[i])))
	}

	// Register shortcuts.
	for i := range shortcuts {
		require.NoError(t, eng.baseline.UpsertShortcut(ctx, &shortcuts[i]))
	}

	// Load baseline after seeding so tests get a populated snapshot.
	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)

	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(t.Context()))
	})

	return testEng, bl, syncRoot
}

func listRemoteBlockedFailures(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
) []SyncFailureRow {
	t.Helper()

	rows, err := eng.baseline.ListRemoteBlockedFailures(ctx)
	require.NoError(t, err)

	return rows
}

func recordRemoteBlockedFailure(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	scopeKey ScopeKey,
	path string,
) {
	t.Helper()

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       path,
		DriveID:    eng.driveID,
		Direction:  DirectionUpload,
		ActionType: ActionUpload,
		Role:       FailureRoleHeld,
		Category:   CategoryTransient,
		IssueType:  IssueSharedFolderBlocked,
		ErrMsg:     "folder is read-only",
		HTTPStatus: http.StatusForbidden,
		ScopeKey:   scopeKey,
	}, nil))
}

// Validates: R-2.14.1, R-2.10.23
func TestHandle403_ReadOnlyFolder_RecordsIssueAtBoundary(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
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
	newTestWatchState(t, eng)

	decision := applyRemote403Decision(t, eng, ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)
	assert.True(t, decision.Matched, "handle403 should match a confirmed read-only folder")
	assert.Equal(t, permissionCheckActivateDerivedScope, decision.Kind)

	// The blocked write itself is the only durable authority for the derived scope.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, driveid.New(remoteDriveID), issues[0].DriveID)
	assert.Equal(t, "Shared/TeamDocs/sub/file.txt", issues[0].Path)
	scopeKey := SKPermRemote("Shared/TeamDocs/sub")
	assert.Equal(t, scopeKey, issues[0].ScopeKey, "boundary issue should be scoped to the recursive remote permission boundary")
	assert.True(t, isTestScopeBlocked(eng, scopeKey), "watch mode should create a recursive remote permission scope")

	block, ok := getTestScopeBlock(eng, scopeKey)
	require.True(t, ok, "remote permission scope should be queryable from the active-scope working set")
	assert.Equal(t, IssueSharedFolderBlocked, block.IssueType)
	assert.True(t, block.NextTrialAt.IsZero(), "remote permission scopes should rely on recheckPermissions, not trial dispatch")

	nestedUpload := &TrackedAction{
		ID: 1,
		Action: Action{
			Type:    ActionUpload,
			Path:    "Shared/TeamDocs/sub/nested/file.txt",
			DriveID: driveid.New(remoteDriveID),
			ItemID:  "nested-upload",
		},
	}
	nestedDelete := &TrackedAction{
		ID: 2,
		Action: Action{
			Type:    ActionRemoteDelete,
			Path:    "Shared/TeamDocs/sub/nested/file.txt",
			DriveID: driveid.New(remoteDriveID),
			ItemID:  "nested-delete",
		},
	}
	nestedDownload := &TrackedAction{
		ID: 3,
		Action: Action{
			Type:    ActionDownload,
			Path:    "Shared/TeamDocs/sub/nested/file.txt",
			DriveID: driveid.New(remoteDriveID),
			ItemID:  "nested-download",
		},
	}
	siblingUpload := &TrackedAction{
		ID: 4,
		Action: Action{
			Type:    ActionUpload,
			Path:    "Shared/TeamDocs/other/file.txt",
			DriveID: driveid.New(remoteDriveID),
			ItemID:  "sibling-upload",
		},
	}

	assert.Equal(t, scopeKey, activeBlockingScopeForTest(t, eng, nestedUpload), "all uploads under the denied boundary should be blocked recursively")
	assert.Equal(t, scopeKey, activeBlockingScopeForTest(t, eng, nestedDelete), "all remote deletes under the denied boundary should be blocked recursively")
	assert.True(t, activeBlockingScopeForTest(t, eng, nestedDownload).IsZero(), "downloads must remain allowed in download-only mode")
	assert.True(t, activeBlockingScopeForTest(t, eng, siblingUpload).IsZero(), "siblings outside the denied boundary must remain admissible")
}

// Validates: R-2.14.1
func TestHandle403_TransientError_NoSuppression(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
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

	decision := eng.permHandler.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", ActionUpload, shortcuts)
	assert.False(t, decision.Matched, "handle403 should fail open for transient 403")

	// No issue should be recorded — transient 403.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, issues)
}

// Validates: R-2.14.1, R-2.10.25, R-2.10.40
func TestHandle403_WholeShareReadOnly_BoundaryAtRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
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

	decision := applyRemote403Decision(t, eng, ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)
	assert.True(t, decision.Matched)

	// Boundary should walk all the way up to the shortcut root.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs", issues[0].ScopeKey.RemotePath())
}

// Validates: R-2.14.1
func TestHandle403_APIFailure_FailOpen(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	checker := &mockPermChecker{
		err: fmt.Errorf("network error"),
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
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

	decision := eng.permHandler.handle403(ctx, bl, "Shared/TeamDocs/sub/file.txt", ActionUpload, shortcuts)
	assert.False(t, decision.Matched, "API failures should fail open")

	// No issue — fail-open when API is unavailable.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, issues)
}

func TestHandle403_NoShortcutMatch_Ignored(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	// Path not under any shortcut.
	decision := eng.permHandler.handle403(ctx, bl, "Documents/file.txt", ActionUpload, nil)
	assert.False(t, decision.Matched, "non-shortcut paths should not trigger permission suppression")

	issues := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, issues)
	assert.Empty(t, checker.calls, "should not call API when no shortcut matches")
}

// Validates: R-2.14.1, R-2.14.2
func TestHandle403_ConfiguredSharedRoot_RecordsRootBoundary(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(engineTestDriveID).String() + ":nested-id":      {{ID: "p1", Roles: []string{"read"}}},
			driveid.New(engineTestDriveID).String() + ":shared-root-id": {{ID: "p2", Roles: []string{"read"}}},
		},
	}

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "nested",
			DriveID: driveid.New(engineTestDriveID), ItemID: "nested-id", ParentID: "shared-root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPermsAndRoot(t, checker, nil, baselineEntries, "shared-root-id")
	ctx := t.Context()
	newTestWatchState(t, eng)

	decision := applyRemote403Decision(t, eng, ctx, bl, "nested/file.txt", nil)
	assert.True(t, decision.Matched)
	assert.Equal(t, permissionCheckActivateDerivedScope, decision.Kind)
	assert.Equal(t, SKPermRemote(""), decision.ScopeKey)

	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, "nested/file.txt", issues[0].Path)
	assert.Equal(t, SKPermRemote(""), issues[0].ScopeKey)
	assert.Equal(t, []string{""}, eng.permHandler.DeniedPrefixes(ctx))
}

// ---------------------------------------------------------------------------
// recheckPermissions tests
// ---------------------------------------------------------------------------

// Validates: R-2.14.1, R-2.10.25
func TestRecheckPermissions_GrantDetected_IssueCleared(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	newTestWatchState(t, eng)
	scopeKey := SKPermRemote("Shared/TeamDocs/sub")

	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, "Shared/TeamDocs/sub/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	// Verify issue exists.
	before := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, before, 1)

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	// Issue should be cleared.
	after := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, after)
	assert.False(t, isTestScopeBlocked(eng, scopeKey), "grant detection should release the recursive remote scope immediately")

	retryable, err := eng.baseline.ListSyncFailuresForRetry(ctx, eng.nowFn())
	require.NoError(t, err)
	require.Len(t, retryable, 1)
	assert.Equal(t, "Shared/TeamDocs/sub/file.txt", retryable[0].Path, "held child work should become retryable immediately when permissions return")

	select {
	case <-testWatchRuntime(t, eng).retryTimerCh:
	case <-time.After(time.Second):
		require.Fail(t, "recheckPermissions should signal retryTimerCh when a remote scope clears")
	}

	admitted := activeBlockingScopeForTest(t, eng, &TrackedAction{
		ID: 10,
		Action: Action{
			Type:    ActionUpload,
			Path:    "Shared/TeamDocs/sub/file.txt",
			DriveID: driveid.New(remoteDriveID),
			ItemID:  "folder-id",
		},
	})
	assert.True(t, admitted.IsZero(), "child uploads should be admissible immediately after permission restoration")
}

// Validates: R-2.10.24, R-2.14.1
func TestHandle403_ExistingRemoteScope_AvoidsAPICall(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID
	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":parent-folder-id": {{ID: "p1", Roles: []string{"read"}}},
			driveid.New(remoteDriveID).String() + ":root-id":          {{ID: "p2", Roles: []string{"write"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
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
	newTestWatchState(t, eng)

	first := applyRemote403Decision(t, eng, ctx, bl, "Shared/TeamDocs/sub/file-a.txt", shortcuts)
	require.True(t, first.Matched)
	callCount := len(checker.calls)
	require.NotZero(t, callCount, "initial 403 should consult Graph permissions")

	second := eng.permHandler.handle403(ctx, bl, "Shared/TeamDocs/sub/deeper/file-b.txt", ActionUpload, shortcuts)
	require.True(t, second.Matched)
	assert.Equal(t, permissionCheckNone, second.Kind, "known denied boundary should short-circuit to a no-op decision")
	assert.Len(t, checker.calls, callCount, "known denied boundary should short-circuit further permission API calls")

	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, SKPermRemote("Shared/TeamDocs/sub"), issues[0].ScopeKey)
}

// Validates: R-2.10.9, R-2.10.11, R-2.14.4
func TestRecheckPermissions_APIFailure_FailsOpenAndReleasesScope(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID
	checker := &mockPermChecker{
		err: fmt.Errorf("graph unavailable"),
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	newTestWatchState(t, eng)
	scopeKey := SKPermRemote("Shared/TeamDocs/sub")

	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, "Shared/TeamDocs/sub/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	issues := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, issues, "inconclusive recheck must fail open rather than keep suppressing writes")
	assert.False(t, isTestScopeBlocked(eng, scopeKey), "fail-open recheck should release the remote scope")

	retryable, err := eng.baseline.ListSyncFailuresForRetry(ctx, eng.nowFn())
	require.NoError(t, err)
	require.Len(t, retryable, 1)
	assert.Equal(t, "Shared/TeamDocs/sub/file.txt", retryable[0].Path)
}

func TestRecheckPermissions_StillDenied_NoChange(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	scopeKey := SKPermRemote("Shared/TeamDocs/sub")
	newTestWatchState(t, eng)

	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, "Shared/TeamDocs/sub/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckKeepScope)

	// Issue should remain.
	after := listRemoteBlockedFailures(t, eng, ctx)
	assert.Len(t, after, 1)
	assert.Equal(t, []string{"Shared/TeamDocs/sub"}, eng.permHandler.DeniedPrefixes(ctx))
	assert.True(t, isTestScopeBlocked(eng, scopeKey), "still-denied remote boundary should remain blocked after recheck")
}

func TestRecheckPermissions_NoIssues_NoAPICalls(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, nil)
	assert.Empty(t, decisions)

	assert.Empty(t, checker.calls, "should not call API when there are no issues")
}

// ---------------------------------------------------------------------------
// Recheck fail-open and denied-prefix contract tests
// ---------------------------------------------------------------------------

// Validates: R-2.14.4
func TestRecheckPermissions_UnresolvableIssues_FailOpenClearsStaleBoundaries(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}

	// No shortcuts registered — issues won't match any shortcut.
	eng, bl, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	recordRemoteBlockedFailure(t, eng, ctx, SKPermRemote("Shared/NoShortcut/sub"), "Shared/NoShortcut/sub/file.txt")
	recordRemoteBlockedFailure(t, eng, ctx, SKPermRemote("Shared/Other/locked"), "Shared/Other/locked/file.txt")

	// Recheck with no shortcuts — both issues have sc == nil.
	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, nil)
	require.Len(t, decisions, 2)

	remaining := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, remaining, "stale remote permission boundaries should be cleared when no shortcut can resolve them")
	assert.Empty(t, checker.calls, "should not call API when no shortcut matches")
	assert.Empty(t, eng.permHandler.DeniedPrefixes(ctx), "cleared stale boundaries must not continue suppressing planning")
}

// Validates: R-2.14.4
func TestRecheckPermissions_UnresolvedItemID_FailOpenClearsStaleBoundary(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	checker := &mockPermChecker{}

	// Shortcut exists but the issue path is NOT in baseline → remoteItemID == "".
	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	// No baseline entries for the issue path.
	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, nil)
	ctx := t.Context()

	recordRemoteBlockedFailure(t, eng, ctx, SKPermRemote("Shared/TeamDocs/missing"), "Shared/TeamDocs/missing/file.txt")

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	remaining := listRemoteBlockedFailures(t, eng, ctx)
	assert.Empty(t, remaining, "unresolvable remote boundaries should not stay suppressed on stale evidence")
	assert.Empty(t, checker.calls)
	assert.Empty(t, eng.permHandler.DeniedPrefixes(ctx))
}

// Validates: R-2.14.4
func TestRecheckPermissions_ConfiguredSharedRootBoundary_UsesRootItemID(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(engineTestDriveID).String() + ":shared-root-id": {{ID: "p1", Roles: []string{"write"}}},
		},
	}

	eng, bl, _ := newTestEngineWithPermsAndRoot(t, checker, nil, nil, "shared-root-id")
	ctx := t.Context()
	scopeKey := SKPermRemote("")
	newTestWatchState(t, eng)

	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, "nested/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, nil)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)
	assert.Empty(t, listRemoteBlockedFailures(t, eng, ctx))
	assert.Equal(t, []string{driveid.New(engineTestDriveID).String() + ":shared-root-id"}, checker.calls)
}

// Validates: R-2.14.2
func TestDeniedPrefixes_RemoteScopesOnly(t *testing.T) {
	t.Parallel()

	checker := &mockPermChecker{}
	eng, _, _ := newTestEngineWithPerms(t, checker, nil, nil)
	ctx := t.Context()

	recordRemoteBlockedFailure(t, eng, ctx, SKPermRemote("Shared/TeamDocs"), "Shared/TeamDocs/file.txt")
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:       "file.txt",
		Direction:  DirectionUpload,
		Category:   CategoryActionable,
		IssueType:  IssuePermissionDenied,
		ErrMsg:     "single-file 403",
		HTTPStatus: http.StatusForbidden,
	}, nil))
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		Direction: DirectionUpload,
		Role:      FailureRoleBoundary,
		Category:  CategoryActionable,
		IssueType: IssueLocalPermissionDenied,
		ErrMsg:    "local permission denied",
		ScopeKey:  SKPermDir("Private"),
	}, nil))

	assert.Equal(t, []string{"Shared/TeamDocs"}, eng.permHandler.DeniedPrefixes(ctx))
}

// ---------------------------------------------------------------------------
// handle403 404 fallback test
// ---------------------------------------------------------------------------

func TestHandle403_FolderNotFound_RecordsIssue(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	checker := &mockPermChecker{
		perms:      map[string][]graph.Permission{},
		notFoundOn: true, // Return graph.ErrNotFound for unknown keys.
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
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

	decision := applyRemote403Decision(t, eng, ctx, bl, "Shared/TeamDocs/sub/file.txt", shortcuts)
	assert.True(t, decision.Matched)

	// Should record an issue because the folder returned 404.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs/sub/file.txt", issues[0].Path)
	assert.Contains(t, issues[0].LastError, "not found")
}

// ---------------------------------------------------------------------------
// handle403 with unresolved parent (falls back to shortcut root)
// ---------------------------------------------------------------------------

func TestHandle403_UnresolvedParent_FallsBackToRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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
	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	// File in a folder that's not in baseline yet.
	decision := applyRemote403Decision(t, eng, ctx, bl, "Shared/TeamDocs/newdir/file.txt", shortcuts)
	assert.True(t, decision.Matched)

	// Should fall back to root and record issue there.
	issues := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, issues, 1)
	assert.Equal(t, "Shared/TeamDocs", issues[0].ScopeKey.RemotePath())
}

// ---------------------------------------------------------------------------
// recheckPermissions preserves active denied boundaries
// ---------------------------------------------------------------------------

// Validates: R-2.10.25
func TestRecheckPermissions_StillDenied_KeepsDeniedPrefix(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

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

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	newTestWatchState(t, eng)
	scopeKey := SKPermRemote("Shared/TeamDocs/sub")

	recordRemoteBlockedFailure(t, eng, ctx, scopeKey, "Shared/TeamDocs/sub/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeKey,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	requireSinglePermissionDecision(t, decisions, permissionRecheckKeepScope)

	remaining := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, remaining, 1)
	assert.Equal(t, scopeKey, remaining[0].ScopeKey)
	assert.Equal(t, []string{"Shared/TeamDocs/sub"}, eng.permHandler.DeniedPrefixes(ctx))
	assert.True(t, isTestScopeBlocked(eng, scopeKey), "watch-mode recheck should keep the recursive remote scope active while still denied")
}

// ---------------------------------------------------------------------------
// R-2.10.21: handle403 uses shortcut's RemoteDriveID for permission queries
// ---------------------------------------------------------------------------

// Validates: R-2.10.21
func TestHandle403_ShortcutUsesRemoteDrive(t *testing.T) {
	t.Parallel()

	remoteDriveID := "remote-drive-special"

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Parent folder on the shortcut's remote drive is read-only.
			driveid.New(remoteDriveID).String() + ":parent-id": {{ID: "p1", Roles: []string{"read"}}},
			// Shortcut root is also read-only.
			driveid.New(remoteDriveID).String() + ":root-id": {{ID: "p2", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/Special", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/Special",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/Special/sub",
			DriveID: driveid.New(remoteDriveID), ItemID: "parent-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()

	decision := eng.permHandler.handle403(ctx, bl, "Shared/Special/sub/file.txt", ActionUpload, shortcuts)
	assert.True(t, decision.Matched, "handle403 should match a read-only shortcut folder")
	assert.Equal(t, permissionCheckActivateDerivedScope, decision.Kind)

	// Verify ALL API calls used the shortcut's remote drive, not the engine's primary drive.
	remoteDriveStr := driveid.New(remoteDriveID).String()
	for _, call := range checker.calls {
		assert.Contains(t, call, remoteDriveStr,
			"every ListItemPermissions call should use the shortcut's RemoteDriveID, got: %s", call)
	}

	// Verify at least one API call was made.
	assert.NotEmpty(t, checker.calls, "should have made at least one API call")
}

// ---------------------------------------------------------------------------
// R-2.10.40: walkPermissionBoundary stops at shortcut root
// ---------------------------------------------------------------------------

// Validates: R-2.10.40
func TestWalkPermissionBoundary_StopsAtShortcutRoot(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	// All folders including root are read-only. The walk MUST stop at the
	// shortcut root and not try to go above it.
	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			driveid.New(remoteDriveID).String() + ":deep-id":    {{ID: "p1", Roles: []string{"read"}}},
			driveid.New(remoteDriveID).String() + ":mid-id":     {{ID: "p2", Roles: []string{"read"}}},
			driveid.New(remoteDriveID).String() + ":root-id":    {{ID: "p3", Roles: []string{"read"}}},
			driveid.New(remoteDriveID).String() + ":above-root": {{ID: "p4", Roles: []string{"read"}}},
		},
	}

	sc := &Shortcut{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs",
			DriveID: driveid.New(remoteDriveID), ItemID: "root-id", ParentID: "root", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/mid",
			DriveID: driveid.New(remoteDriveID), ItemID: "mid-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/mid/deep",
			DriveID: driveid.New(remoteDriveID), ItemID: "deep-id", ParentID: "mid-id", ItemType: ItemTypeFolder,
		},
		// Parent above the shortcut root — should never be queried.
		{
			Action: ActionDownload, Success: true, Path: "Shared",
			DriveID: driveid.New(remoteDriveID), ItemID: "above-root", ParentID: "root", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, []Shortcut{*sc}, baselineEntries)
	ctx := t.Context()

	boundary := eng.permHandler.walkPermissionBoundary(ctx, bl, "Shared/TeamDocs/mid/deep", sc, driveid.New(remoteDriveID))

	// Boundary should be the shortcut root, NOT "Shared" (above root).
	assert.Equal(t, "Shared/TeamDocs", boundary, "walk must stop at shortcut root")

	// Verify that the "above-root" key was never queried.
	aboveRootKey := driveid.New(remoteDriveID).String() + ":above-root"
	for _, call := range checker.calls {
		assert.NotEqual(t, aboveRootKey, call,
			"walkPermissionBoundary must not query above the shortcut root")
	}
}

// ---------------------------------------------------------------------------
// R-2.10.25: recheckPermissions re-queries and clears/releases boundaries
// ---------------------------------------------------------------------------

// Validates: R-2.10.25
func TestRecheckPermissions_MultipleIssues_PartialResolution(t *testing.T) {
	t.Parallel()

	remoteDriveID := permissionsRemoteDriveID

	checker := &mockPermChecker{
		perms: map[string][]graph.Permission{
			// Folder A is now writable — permission restored.
			driveid.New(remoteDriveID).String() + ":folder-a-id": {{ID: "p1", Roles: []string{"write"}}},
			// Folder B is still read-only.
			driveid.New(remoteDriveID).String() + ":folder-b-id": {{ID: "p2", Roles: []string{"read"}}},
		},
	}

	shortcuts := []Shortcut{{
		ItemID: "sc-1", RemoteDrive: remoteDriveID, RemoteItem: "root-id",
		LocalPath: "Shared/TeamDocs", Observation: ObservationDelta, DiscoveredAt: 1000,
	}}

	baselineEntries := []ActionOutcome{
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/folderA",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-a-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
		{
			Action: ActionDownload, Success: true, Path: "Shared/TeamDocs/folderB",
			DriveID: driveid.New(remoteDriveID), ItemID: "folder-b-id", ParentID: "root-id", ItemType: ItemTypeFolder,
		},
	}

	eng, bl, _ := newTestEngineWithPerms(t, checker, shortcuts, baselineEntries)
	ctx := t.Context()
	scopeA := SKPermRemote("Shared/TeamDocs/folderA")
	scopeB := SKPermRemote("Shared/TeamDocs/folderB")
	newTestWatchState(t, eng)

	recordRemoteBlockedFailure(t, eng, ctx, scopeA, "Shared/TeamDocs/folderA/file.txt")
	recordRemoteBlockedFailure(t, eng, ctx, scopeB, "Shared/TeamDocs/folderB/file.txt")
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeA,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})
	setTestScopeBlock(t, eng, &ScopeBlock{
		Key:       scopeB,
		IssueType: IssueSharedFolderBlocked,
		BlockedAt: eng.nowFn().Add(-time.Minute),
	})

	decisions := applyRemotePermissionRecheck(t, eng, ctx, bl, shortcuts)
	require.Len(t, decisions, 2)

	// Folder A should be cleared (now writable).
	// Folder B should remain (still read-only).
	remaining := listRemoteBlockedFailures(t, eng, ctx)
	require.Len(t, remaining, 1)
	assert.Equal(t, "Shared/TeamDocs/folderB/file.txt", remaining[0].Path)
	assert.Equal(t, []string{"Shared/TeamDocs/folderB"}, eng.permHandler.DeniedPrefixes(ctx))
	assert.False(t, isTestScopeBlocked(eng, scopeA), "resolved boundary should be released")
	assert.True(t, isTestScopeBlocked(eng, scopeB), "still-denied boundary should remain blocked")
}

// ---------------------------------------------------------------------------
// handleLocalPermission tests (R-2.10.12)
// ---------------------------------------------------------------------------

// Validates: R-2.10.12
func TestHandleLocalPermission_DirectoryLevel(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Create a directory, then make it inaccessible.
	deniedDir := filepath.Join(syncRoot, "Private")
	require.NoError(t, os.MkdirAll(deniedDir, 0o700))
	setTestDirPermissions(t, deniedDir, 0o000)

	t.Cleanup(func() {
		// Restore permissions so t.TempDir cleanup works.
		setTestDirPermissions(t, deniedDir, 0o700)
	})

	// Set up watch state so the test can install an active scope block.
	newTestWatchState(t, eng)

	// Simulate a worker result with os.ErrPermission.
	r := &WorkerResult{
		Path:       "Private/file.txt",
		ActionType: ActionDownload,
		Err:        os.ErrPermission,
		ErrMsg:     "permission denied",
	}

	decision := applyLocalPermissionDecision(t, eng, ctx, r)
	assert.True(t, decision.Matched)
	assert.Equal(t, permissionCheckActivateBoundaryScope, decision.Kind)

	// Should have recorded a directory-level local_permission_denied.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Private", issues[0].Path)
	assert.Equal(t, SKPermDir("Private"), issues[0].ScopeKey)

	// Should have created a scope block.
	assert.True(t, isTestScopeBlocked(eng, SKPermDir("Private")), "should create a scope block for the denied directory")
}

// Validates: R-2.10.12
func TestHandleLocalPermission_FileLevel(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	// Create a directory (accessible) with an inaccessible file.
	dir := filepath.Join(syncRoot, "Docs")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// Parent dir is accessible — this should be file-level only.
	r := &WorkerResult{
		Path:       "Docs/secret.txt",
		ActionType: ActionUpload,
		Err:        os.ErrPermission,
		ErrMsg:     "permission denied",
	}

	decision := applyLocalPermissionDecision(t, eng, ctx, r)
	assert.True(t, decision.Matched)
	assert.Equal(t, permissionCheckRecordFileFailure, decision.Kind)

	// Should have recorded a file-level local_permission_denied (no scope key).
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	require.Len(t, issues, 1)
	assert.Equal(t, "Docs/secret.txt", issues[0].Path)
	assert.True(t, issues[0].ScopeKey.IsZero(), "file-level issues should have no scope key")
}

// ---------------------------------------------------------------------------
// recheckLocalPermissions tests (R-2.10.13)
// ---------------------------------------------------------------------------

// Validates: R-2.10.13
func TestRecheckLocalPermissions_Restored(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	deniedDir := filepath.Join(syncRoot, "Private")
	require.NoError(t, os.MkdirAll(deniedDir, 0o700))

	// Set up watch state so the test can install an active scope block.
	newTestWatchState(t, eng)

	scopeKey := SKPermDir("Private")

	// Simulate a prior denial: record failure + scope block.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleBoundary,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key: scopeKey, IssueType: IssueLocalPermissionDenied,
	})

	// Directory is now accessible (we didn't chmod 000 it).
	decisions := applyLocalPermissionRecheck(t, eng, ctx)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	// Failure should be cleared.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues, "failure should be cleared when directory is accessible")

	// Scope block should be released.
	assert.False(t, isTestScopeBlocked(eng, scopeKey), "scope block should be released when directory is accessible")
}

// Validates: R-2.10.13
func TestRecheckLocalPermissions_StillDenied(t *testing.T) {
	t.Parallel()

	if os.Getuid() == 0 {
		t.Skip("root bypasses permission checks")
	}

	mock := &engineMockClient{}
	eng, syncRoot := newTestEngine(t, mock)
	ctx := t.Context()

	deniedDir := filepath.Join(syncRoot, "Private")
	require.NoError(t, os.MkdirAll(deniedDir, 0o700))
	setTestDirPermissions(t, deniedDir, 0o000)

	t.Cleanup(func() {
		setTestDirPermissions(t, deniedDir, 0o700)
	})

	newTestWatchState(t, eng)

	scopeKey := SKPermDir("Private")

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleBoundary,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key: scopeKey, IssueType: IssueLocalPermissionDenied,
	})

	decisions := applyLocalPermissionRecheck(t, eng, ctx)
	requireSinglePermissionDecision(t, decisions, permissionRecheckKeepScope)

	// Failure should remain.
	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	assert.Len(t, issues, 1, "failure should remain when directory is still inaccessible")

	// Scope block should still be active.
	assert.True(t, isTestScopeBlocked(eng, scopeKey), "scope block should remain when directory is still inaccessible")
}

// ---------------------------------------------------------------------------
// clearScannerResolvedPermissions tests (R-2.10.10)
// ---------------------------------------------------------------------------

// Validates: R-2.10.10
func TestClearScannerResolvedPermissions_FileLevel(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	// Record a file-level permission failure (no scope key = file-level).
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "secret.txt",
		DriveID:   eng.driveID,
		Direction: DirectionUpload,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "permission denied",
	}, nil))

	// Scanner observes the file — proof of accessibility.
	observed := map[string]bool{"secret.txt": true}
	decisions := applyScannerResolvedPermissions(t, eng, ctx, observed)
	requireSinglePermissionDecision(t, decisions, permissionRecheckClearFileFailure)

	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues, "file-level failure should be cleared when scanner observes the file")
}

// Validates: R-2.10.10
func TestClearScannerResolvedPermissions_DirLevel(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	newTestWatchState(t, eng)

	scopeKey := SKPermDir("Private")

	// Record a directory-level permission failure with scope block.
	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleBoundary,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key: scopeKey, IssueType: IssueLocalPermissionDenied,
	})

	// Scanner observes a file under the blocked directory — proof that the
	// directory is now traversable.
	observed := map[string]bool{"Private/doc.txt": true}
	decisions := applyScannerResolvedPermissions(t, eng, ctx, observed)
	requireSinglePermissionDecision(t, decisions, permissionRecheckReleaseScope)

	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	assert.Empty(t, issues, "dir-level failure should be cleared when scanner observes a child path")

	assert.False(t, isTestScopeBlocked(eng, scopeKey), "scope block should be released when scanner proves directory is accessible")
}

// Validates: R-2.10.10
func TestClearScannerResolvedPermissions_NoFalsePositives(t *testing.T) {
	t.Parallel()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	ctx := t.Context()

	newTestWatchState(t, eng)

	scopeKey := SKPermDir("Private")

	require.NoError(t, eng.baseline.RecordFailure(ctx, &SyncFailureParams{
		Path:      "Private",
		DriveID:   eng.driveID,
		Direction: DirectionDownload,
		Role:      FailureRoleBoundary,
		IssueType: IssueLocalPermissionDenied,
		Category:  CategoryActionable,
		ErrMsg:    "directory not accessible",
		ScopeKey:  scopeKey,
	}, nil))

	setTestScopeBlock(t, eng, &ScopeBlock{
		Key: scopeKey, IssueType: IssueLocalPermissionDenied,
	})

	// Scanner observes an unrelated path — should NOT clear the permission failure.
	observed := map[string]bool{"Public/readme.txt": true}
	decisions := applyScannerResolvedPermissions(t, eng, ctx, observed)
	assert.Empty(t, decisions)

	issues, err := eng.baseline.ListSyncFailuresByIssueType(ctx, IssueLocalPermissionDenied)
	require.NoError(t, err)
	assert.Len(t, issues, 1, "failure should remain when scanner didn't observe the blocked path")

	assert.True(t, isTestScopeBlocked(eng, scopeKey), "scope block should remain when scanner didn't observe the blocked path")
}

func TestPathSetFromEvents(t *testing.T) {
	t.Parallel()

	t.Run("nil_events", func(t *testing.T) {
		result := pathSetFromEvents(nil)
		assert.Nil(t, result)
	})

	t.Run("empty_events", func(t *testing.T) {
		result := pathSetFromEvents([]ChangeEvent{})
		assert.Nil(t, result)
	})

	t.Run("extracts_paths", func(t *testing.T) {
		events := []ChangeEvent{
			{Path: "a.txt"},
			{Path: "dir/b.txt"},
			{Path: ""}, // empty path should be skipped
			{Path: "c.txt"},
		}
		result := pathSetFromEvents(events)
		assert.Equal(t, map[string]bool{
			"a.txt":     true,
			"dir/b.txt": true,
			"c.txt":     true,
		}, result)
	})
}

func TestPathSetFromBatch(t *testing.T) {
	t.Parallel()

	t.Run("nil_batch", func(t *testing.T) {
		result := pathSetFromBatch(nil)
		assert.Nil(t, result)
	})

	t.Run("empty_batch", func(t *testing.T) {
		result := pathSetFromBatch([]PathChanges{})
		assert.Nil(t, result)
	})

	t.Run("extracts_paths", func(t *testing.T) {
		batch := []PathChanges{
			{Path: "x.txt"},
			{Path: ""},
			{Path: "y/z.txt"},
		}
		result := pathSetFromBatch(batch)
		assert.Equal(t, map[string]bool{
			"x.txt":   true,
			"y/z.txt": true,
		}, result)
	})
}
