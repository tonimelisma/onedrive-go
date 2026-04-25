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

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildRootLifecycleActions_RenamePatchesPlaceholderAndPreservesMount(t *testing.T) {
	parent, child := localRootActionTestMounts(t, "Shortcuts/Old")
	newRoot := filepath.Join(parent.syncRoot, "Shortcuts", "Renamed")
	require.NoError(t, os.MkdirAll(newRoot, 0o700))
	child.ReservedLocalPaths = []string{"Shortcuts/Renamed"}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	var patchName string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPatch, r.Method)
		assert.Equal(t, "/drives/0000parent-drive/items/binding-a", r.URL.Path)
		var body struct {
			Name string `json:"name"`
		}
		if decodeErr := json.NewDecoder(r.Body).Decode(&body); decodeErr != nil {
			assert.NoError(t, decodeErr)
			http.Error(w, decodeErr.Error(), http.StatusBadRequest)
			return
		}
		patchName = body.Name
		_, writeErr := w.Write([]byte(`{"id":"binding-a","name":"Renamed","folder":{}}`))
		assert.NoError(t, writeErr)
	}))
	defer server.Close()

	compiled := &compiledMountSet{LocalRootActions: []childRootLifecycleAction{{
		kind:                  childRootLifecycleActionRename,
		mountID:               mountID(child.MountID),
		parent:                parent,
		bindingItemID:         child.BindingItemID,
		fromRelativeLocalPath: child.RelativeLocalPath,
		toRelativeLocalPath:   "Shortcuts/Renamed",
	}}}

	changed := applyChildRootLifecycleActions(t.Context(), localRootActionTestOrchestrator(t, server.URL), compiled, nil)

	require.True(t, changed)
	assert.Equal(t, "Renamed", patchName)
	loaded, err := config.LoadMountInventory()
	require.NoError(t, err)
	record := loaded.Mounts[child.MountID]
	assert.Equal(t, "Renamed", record.LocalAlias)
	assert.Equal(t, "Shortcuts/Renamed", record.RelativeLocalPath)
	assert.Empty(t, record.ReservedLocalPaths)
	assert.Equal(t, config.MountStateActive, record.State)
	assert.Empty(t, record.StateReason)
	assert.NotNil(t, record.LocalRootIdentity)
	assert.Empty(t, compiled.RemovedMountIDs)
}

// Validates: R-2.8.1, R-4.1.4
func TestApplyChildRootLifecycleActions_DeleteDeletesPlaceholderAndQueuesStatePurge(t *testing.T) {
	parent, child := localRootActionTestMounts(t, "Shortcuts/Old")
	child.LocalRootMaterialized = true
	child.LocalRootIdentity = &config.RootIdentity{Device: 1, Inode: 2}
	require.NoError(t, config.SaveMountInventory(&config.MountInventory{
		Mounts: map[string]config.MountRecord{child.MountID: child},
	}))

	deleteCalled := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodDelete, r.Method)
		assert.Equal(t, "/drives/0000parent-drive/items/binding-a", r.URL.Path)
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

func localRootActionTestMounts(t *testing.T, relativePath string) (*mountSpec, config.MountRecord) {
	t.Helper()

	t.Setenv("XDG_DATA_HOME", t.TempDir())
	require.NoError(t, os.MkdirAll(config.DefaultDataDir(), 0o700))
	parent := testParentMountSpec()
	parent.syncRoot = t.TempDir()
	parent.accountEmail = parent.tokenOwnerCanonical.Email()
	child := testChildRecord(parent.mountID, "binding-a", relativePath)

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
