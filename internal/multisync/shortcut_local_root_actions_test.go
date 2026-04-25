package multisync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
)

const (
	localRootActionBindingID   = "binding-a"
	localRootActionOldPath     = "Shortcuts/Old"
	localRootActionRenamedPath = "Shortcuts/Renamed"
)

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildRootLifecycleActions_RenamePatchesPlaceholderAndPreservesMount(t *testing.T) {
	parent, child := localRootActionTestMounts(t, localRootActionOldPath)
	newRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(newRoot, 0o700))
	child.ReservedLocalPaths = []string{localRootActionRenamedPath}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	var patchName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/0000parent-drive/items/"+localRootActionBindingID, r.URL.Path)
		var body struct {
			Name string `json:"name"`
		}
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
			assert.NoError(t, decodeErr)
			http.Error(w, decodeErr.Error(), http.StatusBadRequest)
			return
		}
		patchName = body.Name
		_, writeErr := w.Write([]byte(`{"id":"` + localRootActionBindingID + `","name":"Renamed","folder":{}}`))
		assert.NoError(t, writeErr)
	}))
	defer server.Close()

	compiled := &compiledMountSet{LocalRootActions: []childRootLifecycleAction{{
		kind:                  childRootLifecycleActionRename,
		mountID:               mountID(child.MountID),
		parent:                parent,
		bindingItemID:         child.BindingItemID,
		fromRelativeLocalPath: child.RelativeLocalPath,
		toRelativeLocalPath:   localRootActionRenamedPath,
	}}}

	changed := applyChildRootLifecycleActions(t.Context(), localRootActionTestOrchestrator(t, server.URL), compiled, nil)

	require.True(t, changed)
	assert.Equal(t, "Renamed", patchName)
	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	record := loaded.Mounts[child.MountID]
	assert.Equal(t, "Renamed", record.LocalAlias)
	assert.Equal(t, localRootActionRenamedPath, record.RelativeLocalPath)
	assert.Empty(t, record.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, record.State)
	assert.Empty(t, record.StateReason)
	assert.NotNil(t, record.LocalRootIdentity)
	assert.Empty(t, compiled.RemovedMountIDs)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildRootLifecycleActions_DeleteDeletesPlaceholderAndQueuesStatePurge(t *testing.T) {
	parent, child := localRootActionTestMounts(t, localRootActionOldPath)
	child.LocalRootMaterialized = true
	child.LocalRootIdentity = &config.RootIdentity{Device: 1, Inode: 2}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/drives/0000parent-drive/items/"+localRootActionBindingID, r.URL.Path)
		deleteCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	compiled := &compiledMountSet{LocalRootActions: []childRootLifecycleAction{{
		kind:                  childRootLifecycleActionDelete,
		mountID:               mountID(child.MountID),
		parent:                parent,
		bindingItemID:         child.BindingItemID,
		fromRelativeLocalPath: child.RelativeLocalPath,
	}}}

	changed := applyChildRootLifecycleActions(t.Context(), localRootActionTestOrchestrator(t, server.URL), compiled, nil)

	require.True(t, changed)
	assert.True(t, deleteCalled)
	assert.Equal(t, []string{child.MountID}, compiled.RemovedMountIDs)
	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	record := loaded.Mounts[child.MountID]
	assert.Equal(t, config.MountStatePendingRemoval, record.State)
	assert.Equal(t, config.MountStateReasonShortcutRemoved, record.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestFinalizeRuntimeMountSetLifecycle_DeleteRecompilesBeforePendingRemovalCleanup(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))

	parentMount := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parentMount.SyncRoot = filepath.Join(t.TempDir(), "parent")
	require.NoError(t, os.MkdirAll(parentMount.SyncRoot, 0o700))
	parentSpecs, err := buildStandaloneMountSpecs([]StandaloneMountConfig{parentMount})
	require.NoError(t, err)
	require.Len(t, parentSpecs, 1)
	parent := parentSpecs[0]

	child := testMountRecordForParent(&parentMount)
	child.BindingItemID = localRootActionBindingID
	child.RelativeLocalPath = localRootActionOldPath
	child.LocalRootMaterialized = true
	child.LocalRootIdentity = &config.RootIdentity{Device: 1, Inode: 2}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/drives/"+parent.remoteDriveID.String()+"/items/"+localRootActionBindingID, r.URL.Path)
		deleteCalled = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	compiled := &compiledMountSet{
		Mounts: []*mountSpec{parent},
		LocalRootActions: []childRootLifecycleAction{{
			kind:                  childRootLifecycleActionDelete,
			mountID:               mountID(child.MountID),
			parent:                parent,
			bindingItemID:         child.BindingItemID,
			fromRelativeLocalPath: child.RelativeLocalPath,
		}},
	}

	refreshed, err := localRootActionTestOrchestrator(t, server.URL).finalizeRuntimeMountSetLifecycle(
		t.Context(),
		compiled,
		[]StandaloneMountConfig{parentMount},
		nil,
		"test",
		true,
	)
	require.NoError(t, err)

	assert.True(t, deleteCalled)
	assert.NotContains(t, refreshed.RemovedMountIDs, child.MountID)
	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	assert.NotContains(t, loaded.Mounts, child.MountID)
}

// Validates: R-2.8.1, R-4.1.4
func TestFinalizeRuntimeMountSetLifecycle_RecompilesAfterLocalAliasRenameBeforeProjectionMoves(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))

	parentMount := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parentMount.SyncRoot = filepath.Join(t.TempDir(), "parent")
	require.NoError(t, os.MkdirAll(parentMount.SyncRoot, 0o700))
	parentSpecs, err := buildStandaloneMountSpecs([]StandaloneMountConfig{parentMount})
	require.NoError(t, err)
	require.Len(t, parentSpecs, 1)
	parent := parentSpecs[0]

	oldPath := localRootActionOldPath
	newPath := localRootActionRenamedPath
	newRoot := filepath.Join(parentMount.SyncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(newRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(newRoot, "kept.txt"), []byte("stateful"), 0o600))

	child := testMountRecordForParent(&parentMount)
	child.BindingItemID = localRootActionBindingID
	child.RelativeLocalPath = oldPath
	child.ReservedLocalPaths = []string{newPath}
	child.LocalRootMaterialized = true
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	patchCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/"+parent.remoteDriveID.String()+"/items/"+localRootActionBindingID, r.URL.Path)
		patchCalled = true
		_, writeErr := w.Write([]byte(`{"id":"` + localRootActionBindingID + `","name":"Renamed","folder":{}}`))
		assert.NoError(t, writeErr)
	}))
	defer server.Close()

	compiled := &compiledMountSet{
		Mounts: []*mountSpec{parent},
		LocalRootActions: []childRootLifecycleAction{{
			kind:                  childRootLifecycleActionRename,
			mountID:               mountID(child.MountID),
			parent:                parent,
			bindingItemID:         child.BindingItemID,
			fromRelativeLocalPath: oldPath,
			toRelativeLocalPath:   newPath,
		}},
		ProjectionMoves: []childProjectionMove{{
			mountID:               mountID(child.MountID),
			parentSyncRoot:        parentMount.SyncRoot,
			fromRelativeLocalPath: newPath,
			toRelativeLocalPath:   oldPath,
		}},
	}

	refreshed, err := localRootActionTestOrchestrator(t, server.URL).finalizeRuntimeMountSetLifecycle(
		t.Context(),
		compiled,
		[]StandaloneMountConfig{parentMount},
		nil,
		"test",
		true,
	)
	require.NoError(t, err)

	assert.True(t, patchCalled)
	assert.DirExists(t, newRoot)
	assert.FileExists(t, filepath.Join(newRoot, "kept.txt"))
	assert.NoDirExists(t, filepath.Join(parentMount.SyncRoot, "Shortcuts", "Old"))
	assert.Empty(t, refreshed.ProjectionMoves)

	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	record := loaded.Mounts[child.MountID]
	assert.Equal(t, newPath, record.RelativeLocalPath)
	assert.Empty(t, record.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, record.State)
	assert.Empty(t, record.StateReason)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildRootLifecycleActions_FailureFiltersStaleProjectionMoves(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))

	parentMount := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parentMount.SyncRoot = filepath.Join(t.TempDir(), "parent")
	require.NoError(t, os.MkdirAll(parentMount.SyncRoot, 0o700))
	parentSpecs, err := buildStandaloneMountSpecs([]StandaloneMountConfig{parentMount})
	require.NoError(t, err)
	require.Len(t, parentSpecs, 1)
	parent := parentSpecs[0]

	child := testMountRecordForParent(&parentMount)
	child.BindingItemID = localRootActionBindingID
	child.RelativeLocalPath = localRootActionOldPath
	child.ReservedLocalPaths = []string{localRootActionRenamedPath}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rename rejected", http.StatusForbidden)
	}))
	defer server.Close()

	compiled := &compiledMountSet{
		Mounts: []*mountSpec{parent},
		LocalRootActions: []childRootLifecycleAction{{
			kind:                  childRootLifecycleActionRename,
			mountID:               mountID(child.MountID),
			parent:                parent,
			bindingItemID:         child.BindingItemID,
			fromRelativeLocalPath: localRootActionOldPath,
			toRelativeLocalPath:   localRootActionRenamedPath,
		}},
		ProjectionMoves: []childProjectionMove{{
			mountID:               mountID(child.MountID),
			parentSyncRoot:        parentMount.SyncRoot,
			fromRelativeLocalPath: localRootActionRenamedPath,
			toRelativeLocalPath:   localRootActionOldPath,
		}},
	}

	changed := applyChildRootLifecycleActions(t.Context(), localRootActionTestOrchestrator(t, server.URL), compiled, nil)

	assert.True(t, changed)
	assert.Empty(t, compiled.ProjectionMoves)
	require.Len(t, compiled.Skipped, 1)
	assert.Contains(t, compiled.Skipped[0].Err.Error(), config.MountStateReasonLocalAliasRenameUnavailable)
}

func localRootActionTestMounts(t *testing.T, relativePath string) (*mountSpec, config.MountRecord) {
	t.Helper()

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))
	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	parent.statePath = filepath.Join(config.DefaultDataDir(), "parent-shortcut-actions.db")
	parent.accountEmail = parent.tokenOwnerCanonical.Email()
	child := testChildRecord(parent.mountID, localRootActionBindingID, relativePath)

	return parent, child
}

func localRootActionTestOrchestrator(t *testing.T, graphBaseURL string) *Orchestrator {
	t.Helper()

	cfg := testOrchestratorConfig(t)
	cfg.Runtime = driveops.NewSessionRuntime(cfg.Holder, "test/1.0", cfg.Logger)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.Runtime.GraphBaseURL = graphBaseURL
	orch := NewOrchestrator(cfg)

	return orch
}
