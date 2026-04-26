package multisync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	synccontrol "github.com/tonimelisma/onedrive-go/internal/synccontrol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

// --- helpers ---

// setupXDGIsolation sets XDG_DATA_HOME to a temp dir and creates catalog drive
// records for each CID. Reload tests use those records to compile non-zero
// remote drive IDs without routing through runtime mount construction.
func setupXDGIsolation(t *testing.T, cids ...driveid.CanonicalID) {
	t.Helper()

	xdgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgDir)

	for _, cid := range cids {
		require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
			catalog.UpsertDrive(&config.CatalogDrive{
				CanonicalID:           cid.String(),
				OwnerAccountCanonical: cid.String(),
				DriveType:             cid.DriveType(),
				RemoteDriveID:         "test-drive-id",
			})
			return nil
		}))
	}
}

func testOrchestratorConfig(t *testing.T, mounts ...StandaloneMountConfig) *OrchestratorConfig {
	t.Helper()

	return testOrchestratorConfigWithPath(t, "/tmp/test-config.toml", mounts...)
}

// testOrchestratorConfigWithPath creates an OrchestratorConfig with a real
// config path. Use this when the test needs a writable config file (e.g.,
// control-socket reload tests that modify config on disk).
func testOrchestratorConfigWithPath(t *testing.T, cfgPath string, mounts ...StandaloneMountConfig) *OrchestratorConfig {
	t.Helper()

	if _, err := os.Stat(cfgPath); err == nil {
		cfg, loadErr := config.LoadOrDefault(cfgPath, slog.Default())
		require.NoError(t, loadErr)
		configuredSelection, compileErr := testStandaloneMountsFromConfig(cfg)
		require.NoError(t, compileErr)

		configuredByID := make(map[driveid.CanonicalID]*StandaloneMountConfig, len(configuredSelection.Mounts))
		for i := range configuredSelection.Mounts {
			configuredByID[configuredSelection.Mounts[i].CanonicalID] = &configuredSelection.Mounts[i]
		}

		for i := range mounts {
			configured, ok := configuredByID[mounts[i].CanonicalID]
			if !ok {
				continue
			}
			overlayConfiguredTestMount(&mounts[i], configured)
		}
	}

	holder := config.NewHolder(config.DefaultConfig(), cfgPath)
	provider := driveops.NewSessionRuntime(holder, "test/1.0", slog.Default())

	return &OrchestratorConfig{
		Holder:                   holder,
		StandaloneMounts:         mounts,
		ReloadStandaloneMounts:   testStandaloneMountsFromConfig,
		Runtime:                  provider,
		Logger:                   slog.Default(),
		disableTopologyBootstrap: true,
	}
}

func overlayConfiguredTestMount(mount *StandaloneMountConfig, configured *StandaloneMountConfig) {
	if mount == nil || configured == nil {
		return
	}

	original := *mount
	mount.SyncRoot = configured.SyncRoot
	mount.StatePath = configured.StatePath
	mount.TokenOwnerCanonical = configured.TokenOwnerCanonical
	mount.AccountEmail = configured.AccountEmail
	mount.Paused = configured.Paused
	mount.EnableWebsocket = configured.EnableWebsocket
	mount.RemoteRootDeltaCapable = configured.RemoteRootDeltaCapable
	mount.TransferWorkers = configured.TransferWorkers
	mount.CheckWorkers = configured.CheckWorkers
	mount.MinFreeSpaceBytes = configured.MinFreeSpaceBytes
	if !configured.RemoteDriveID.IsZero() {
		mount.RemoteDriveID = configured.RemoteDriveID
	}
	if configured.RemoteRootItemID != "" {
		mount.RemoteRootItemID = configured.RemoteRootItemID
	}

	if original.DisplayName != "" {
		mount.DisplayName = original.DisplayName
	}
	if original.Paused {
		mount.Paused = true
	}
	if original.EnableWebsocket {
		mount.EnableWebsocket = true
	}
	if original.RemoteRootDeltaCapable {
		mount.RemoteRootDeltaCapable = true
	}
	if original.TransferWorkers != 0 {
		mount.TransferWorkers = original.TransferWorkers
	}
	if original.CheckWorkers != 0 {
		mount.CheckWorkers = original.CheckWorkers
	}
	if original.MinFreeSpaceBytes != 0 {
		mount.MinFreeSpaceBytes = original.MinFreeSpaceBytes
	}
	if !original.RemoteDriveID.IsZero() {
		mount.RemoteDriveID = original.RemoteDriveID
	}
	if original.RemoteRootItemID != "" {
		mount.RemoteRootItemID = original.RemoteRootItemID
	}
}

func testStandaloneMountsFromConfig(cfg *config.Config) (StandaloneMountSelection, error) {
	if cfg == nil || len(cfg.Drives) == 0 {
		return StandaloneMountSelection{}, nil
	}

	catalog, err := config.LoadCatalog()
	if err != nil {
		return StandaloneMountSelection{}, fmt.Errorf("loading catalog: %w", err)
	}

	cids := make([]driveid.CanonicalID, 0, len(cfg.Drives))
	for cid := range cfg.Drives {
		cids = append(cids, cid)
	}
	sort.Slice(cids, func(i, j int) bool {
		return cids[i].String() < cids[j].String()
	})

	mounts := make([]StandaloneMountConfig, 0, len(cids))
	for i, cid := range cids {
		drive := cfg.Drives[cid]
		tokenOwner, err := config.TokenAccountCanonicalID(cid)
		if err != nil {
			return StandaloneMountSelection{}, fmt.Errorf("token owner for %s: %w", cid, err)
		}

		remoteDriveID := driveid.ID{}
		if catalogDrive, found := catalog.DriveByCanonicalID(cid); found && catalogDrive.RemoteDriveID != "" {
			remoteDriveID = driveid.New(catalogDrive.RemoteDriveID)
		} else if cid.IsShared() {
			remoteDriveID = driveid.New(cid.SourceDriveID())
		}

		displayName := drive.DisplayName
		if displayName == "" {
			displayName = config.DefaultDisplayName(cid)
		}

		accountEmail := tokenOwner.Email()
		if accountEmail == "" {
			accountEmail = cid.Email()
		}
		minFreeSpace, err := config.ParseSize(cfg.MinFreeSpace)
		if err != nil {
			return StandaloneMountSelection{}, fmt.Errorf("parse min_free_space for %s: %w", cid, err)
		}

		mounts = append(mounts, StandaloneMountConfig{
			SelectionIndex:         i,
			CanonicalID:            cid,
			DisplayName:            displayName,
			SyncRoot:               drive.SyncDir,
			StatePath:              config.DriveStatePath(cid),
			RemoteDriveID:          remoteDriveID,
			RemoteRootItemID:       cid.SourceItemID(),
			TokenOwnerCanonical:    tokenOwner,
			AccountEmail:           accountEmail,
			Paused:                 drive.IsPaused(time.Now()),
			EnableWebsocket:        cfg.Websocket,
			RemoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
			TransferWorkers:        cfg.TransferWorkers,
			CheckWorkers:           cfg.CheckWorkers,
			MinFreeSpaceBytes:      minFreeSpace,
		})
	}

	return StandaloneMountSelection{Mounts: mounts}, nil
}

func postControlReload(t *testing.T, socketPath string) {
	t.Helper()

	_ = postControlJSON(t, socketPath, synccontrol.PathReload, nil)
}

func getControlStatus(t *testing.T, socketPath string) synccontrol.StatusResponse {
	t.Helper()

	client := controlTestClient(socketPath)
	var decoded synccontrol.StatusResponse
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, synccontrol.HTTPBaseURL+synccontrol.PathStatus, http.NoBody)
		require.NoError(t, err)

		// #nosec G704 -- fixed Unix-domain test socket client.
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return false
		}
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return false
		}

		return true
	}, 5*time.Second, 10*time.Millisecond, "control socket status did not succeed")

	return decoded
}

func postControlJSON(t *testing.T, socketPath string, path string, body []byte) synccontrol.MutationResponse {
	t.Helper()

	client := controlTestClient(socketPath)
	var decoded synccontrol.MutationResponse
	require.Eventually(t, func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, synccontrol.HTTPBaseURL+path, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")

		// #nosec G704 -- fixed Unix-domain test socket client.
		resp, err := client.Do(req)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
			return false
		}

		return resp.StatusCode == http.StatusOK
	}, 5*time.Second, 10*time.Millisecond, "control socket request did not succeed")

	return decoded
}

func controlTestClient(socketPath string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}

	return &http.Client{Transport: transport}
}

func shortControlSocketPath(t *testing.T) string {
	t.Helper()

	path := filepath.Join(os.TempDir(), fmt.Sprintf("onedrive-go-%d.sock", time.Now().UnixNano()))
	t.Cleanup(func() {
		if err := localpath.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Logf("remove test control socket: %v", err)
		}
	})

	return path
}

// Validates: R-2.9.1
func TestControlSocketServer_PermissionsStaleCleanupAndRuntimeDirRemoval(t *testing.T) {
	t.Parallel()

	runtimeDir := filepath.Join(os.TempDir(), fmt.Sprintf("odgo-test-%d", time.Now().UnixNano()))
	socketPath := filepath.Join(runtimeDir, "control.sock")
	require.NoError(t, os.MkdirAll(runtimeDir, 0o700))
	require.NoError(t, os.WriteFile(socketPath, []byte("stale"), 0o600))
	t.Cleanup(func() {
		assert.NoError(t, os.RemoveAll(runtimeDir))
	})

	server, err := startControlSocketServer(
		t.Context(),
		socketPath,
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeJSON(w, http.StatusOK, synccontrol.StatusResponse{OwnerMode: synccontrol.OwnerModeWatch})
		}),
		slog.Default(),
	)
	require.NoError(t, err)

	info, err := os.Stat(socketPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, server.Close(t.Context()))
	assert.NoFileExists(t, socketPath)
	assert.NoDirExists(t, runtimeDir)
}

func testStandaloneMount(t *testing.T, cidStr, displayName string) StandaloneMountConfig {
	t.Helper()

	cid := testCanonicalID(t, cidStr)
	tokenOwner, err := config.TokenAccountCanonicalID(cid)
	require.NoError(t, err)

	accountEmail := cid.Email()
	if accountEmail == "" {
		accountEmail = tokenOwner.Email()
	}

	return StandaloneMountConfig{
		CanonicalID:            cid,
		DisplayName:            displayName,
		SyncRoot:               t.TempDir(),
		StatePath:              config.DriveStatePath(cid),
		RemoteDriveID:          driveid.New("test-drive-id"),
		TokenOwnerCanonical:    tokenOwner,
		AccountEmail:           accountEmail,
		RemoteRootDeltaCapable: config.RemoteRootDeltaCapableForTokenOwner(tokenOwner),
	}
}

func shortcutChildMountIDForTest(parent *StandaloneMountConfig, child *syncengine.ShortcutChildTopology) string {
	if parent == nil || child == nil {
		return ""
	}
	return config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)
}

func seedShortcutChildStateArtifactsForTest(
	t *testing.T,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildTopology,
	includeSidecars bool,
) {
	t.Helper()
	childMountID := shortcutChildMountIDForTest(parent, child)
	require.NotEmpty(t, childMountID)
	require.NotNil(t, child)

	statePath := config.MountStatePath(childMountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	if includeSidecars {
		require.NoError(t, os.WriteFile(statePath+"-wal", []byte("wal"), 0o600))
		require.NoError(t, os.WriteFile(statePath+"-shm", []byte("shm"), 0o600))
	}
	require.NoError(t, config.UpdateCatalog(func(catalog *config.Catalog) error {
		catalog.Drives[childMountID] = config.CatalogDrive{
			CanonicalID:   childMountID,
			DisplayName:   "Accidental child catalog record",
			RemoteDriveID: child.RemoteDriveID,
		}
		return nil
	}))
}

func assertShortcutChildArtifactsPurgedForTest(
	t *testing.T,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildTopology,
	includeSidecars bool,
) {
	t.Helper()
	childMountID := shortcutChildMountIDForTest(parent, child)
	require.NotEmpty(t, childMountID)
	require.NotNil(t, child)

	statePath := config.MountStatePath(childMountID)
	assert.NoFileExists(t, statePath)
	if includeSidecars {
		assert.NoFileExists(t, statePath+"-wal")
		assert.NoFileExists(t, statePath+"-shm")
	}
	catalog, err := config.LoadCatalog()
	require.NoError(t, err)
	assert.NotContains(t, catalog.Drives, childMountID)
}

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token() (string, error) { return "test-token", nil }

// stubTokenSourceFn returns a TokenSourceFn that always succeeds.
func stubTokenSourceFn(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
	return &stubTokenSource{}, nil
}

// --- NewOrchestrator ---

// Validates: R-2.4
func TestNewOrchestrator_ReturnsNonNil(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	require.NotNil(t, orch)
	assert.Equal(t, cfg, orch.cfg)
	assert.NotNil(t, orch.logger)
}

// Validates: R-2.4
func TestNewOrchestrator_DefaultFactories(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	// engineFactory is set to default (non-nil).
	assert.NotNil(t, orch.engineFactory)
}

// Validates: R-2.4.8
func TestBuildRuntimeMountSet_DoesNotInspectParentShortcutAliasRoot(t *testing.T) {
	xdgHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgHome)

	parent := testStandaloneMount(t, "personal:owner@example.com", "Parent")
	parent.RemoteDriveID = driveid.ID{}
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-a", "Shortcuts/A")
	childID := config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)
	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(filepath.Dir(childRoot), 0o700))
	require.NoError(t, os.WriteFile(childRoot, []byte("not a directory"), 0o600))

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildTopology(orch, &parent, &child)

	compiled, err := orch.buildRuntimeMountSet(t.Context(), cfg.StandaloneMounts, nil)

	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 2)
	assert.Equal(t, mountID(parent.CanonicalID.String()), compiled.Mounts[0].mountID)
	assert.Equal(t, mountID(childID), compiled.Mounts[1].mountID)
	assert.Empty(t, compiled.Skipped)
}

// --- RunOnce ---

// Validates: R-2.4
func TestRunOnce_ZeroMounts(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	assert.Empty(t, result.Reports)
	assert.Empty(t, result.Startup.Results)
}

// Validates: R-2.4
func TestRunOnce_OneMount_Success(t *testing.T) {
	rd := testStandaloneMount(t, "personal:test@example.com", "Test")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	expectedReport := &syncengine.Report{
		Mode:      syncengine.SyncBidirectional,
		Downloads: 5,
	}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{report: expectedReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 1)
	assert.Equal(t, MountStartupRunnable, result.Startup.Results[0].Status)
	require.Len(t, result.Reports, 1)
	assert.Equal(t, testStandaloneMountIdentity(rd.CanonicalID), result.Reports[0].Identity)
	assert.Equal(t, "Test", result.Reports[0].DisplayName)
	require.NoError(t, result.Reports[0].Err)
	require.NotNil(t, result.Reports[0].Report)
	assert.Equal(t, 5, result.Reports[0].Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_TwoMounts_OneFailsOneSucceeds(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:fail@example.com", "Failing")
	rd2 := testStandaloneMount(t, "personal:ok@example.com", "Working")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	errDelta := errors.New("delta gone")
	okReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Uploads: 2}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.syncRoot == rd1.SyncRoot {
			return &mockEngine{err: errDelta}, nil
		}

		return &mockEngine{report: okReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 2)

	// Find each mount's report by canonical ID.
	var failReport, okMountReport *MountReport
	for i := range result.Reports {
		if result.Reports[i].Identity.CanonicalID == rd1.CanonicalID {
			failReport = result.Reports[i]
		} else {
			okMountReport = result.Reports[i]
		}
	}

	require.NotNil(t, failReport)
	require.ErrorIs(t, failReport.Err, errDelta)
	assert.Nil(t, failReport.Report)

	require.NotNil(t, okMountReport)
	require.NoError(t, okMountReport.Err)
	assert.Equal(t, 2, okMountReport.Report.Uploads)
}

// Validates: R-2.4.8
func TestRunOnce_FinalDrainChildRunsBidirectionalFullReconcileAndReleasesAfterSuccess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:final-drain@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain
	childID := config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)

	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "local-change.txt"), []byte("pending upload"), 0o600))
	seedShortcutChildStateArtifactsForTest(t, &parent, &child, true)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildTopology(orch, &parent, &child)

	type runCall struct {
		mountID string
		mode    syncengine.SyncMode
		opts    syncengine.RunOptions
	}
	var callsMu sync.Mutex
	calls := make([]runCall, 0, 2)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		ackDrainFn := func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildTopologySnapshot, error) {
			return syncengine.ShortcutChildTopologySnapshot{}, nil
		}
		if req.Mount.projectionKind == MountProjectionStandalone {
			ackDrainFn = func(_ context.Context, ack syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildTopologySnapshot, error) {
				assert.Equal(t, child.BindingItemID, ack.BindingItemID)
				require.NoError(t, os.RemoveAll(childRoot))
				snapshot := syncengine.ShortcutChildTopologySnapshot{NamespaceID: parent.CanonicalID.String()}
				orch.storeParentShortcutTopology(mountID(parent.CanonicalID.String()), snapshot)
				return snapshot, nil
			}
		}
		return &mockEngine{
			ackDrainFn: ackDrainFn,
			runOnceFn: func(_ context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error) {
				callsMu.Lock()
				calls = append(calls, runCall{
					mountID: req.Mount.mountID.String(),
					mode:    mode,
					opts:    opts,
				})
				callsMu.Unlock()
				return &syncengine.Report{Mode: mode, Succeeded: 1}, nil
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncDownloadOnly, syncengine.RunOptions{})

	require.Len(t, result.Reports, 2)
	callsMu.Lock()
	callsSnapshot := append([]runCall(nil), calls...)
	callsMu.Unlock()
	require.Len(t, callsSnapshot, 2)
	var parentCall, childCall *runCall
	for i := range callsSnapshot {
		if callsSnapshot[i].mountID == parent.CanonicalID.String() {
			parentCall = &callsSnapshot[i]
		}
		if callsSnapshot[i].mountID == childID {
			childCall = &callsSnapshot[i]
		}
	}
	require.NotNil(t, parentCall)
	assert.Equal(t, syncengine.SyncDownloadOnly, parentCall.mode)
	assert.False(t, parentCall.opts.FullReconcile)
	require.NotNil(t, childCall)
	assert.Equal(t, syncengine.SyncBidirectional, childCall.mode)
	assert.True(t, childCall.opts.FullReconcile)
	assert.NoDirExists(t, childRoot)
	assertShortcutChildArtifactsPurgedForTest(t, &parent, &child, true)

	publication := orch.parentShortcutTopologyFor(mountID(parent.CanonicalID.String()))
	assert.Empty(t, publication.Children)
	assert.Empty(t, publication.Released)
}

// Validates: R-2.4.8
func TestRunOnce_FinalDrainChildFailureKeepsProjectionReserved(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:final-drain-fail@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain
	childID := config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)

	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "local-change.txt"), []byte("pending upload"), 0o600))
	seedShortcutChildStateArtifactsForTest(t, &parent, &child, false)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildTopology(orch, &parent, &child)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(_ context.Context, mode syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				report := &syncengine.Report{Mode: mode}
				if req.Mount.mountID.String() == childID {
					report.Failed = 1
					report.Errors = []error{errors.New("upload blocked")}
				}
				return report, nil
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Len(t, result.Reports, 2)
	assert.DirExists(t, childRoot)
	snapshot := orch.parentShortcutTopologyFor(mountID(parent.CanonicalID.String()))
	require.Len(t, snapshot.Children, 1)
	assert.Equal(t, syncengine.ShortcutChildActionFinalDrain, snapshot.Children[0].RunnerAction)
	assert.FileExists(t, config.MountStatePath(childID))
}

// Validates: R-2.4.8
func TestRunOnce_ReleasedShortcutChildPurgesStateArtifacts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:manual-discard@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-discard", "Shortcuts/Docs")
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain

	seedShortcutChildStateArtifactsForTest(t, &parent, &child, true)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildTopology(orch, &parent, &child)
	orch.storeParentShortcutTopology(mountID(parent.CanonicalID.String()), syncengine.ShortcutChildTopologyPublication{
		NamespaceID: parent.CanonicalID.String(),
	})
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		assert.Equal(t, MountProjectionStandalone, req.Mount.projectionKind)
		return &mockEngine{report: &syncengine.Report{}}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Len(t, result.Reports, 1)
	assertShortcutChildArtifactsPurgedForTest(t, &parent, &child, true)
	publication := orch.parentShortcutTopologyFor(mountID(parent.CanonicalID.String()))
	assert.Empty(t, publication.Released)
}

// Validates: R-2.4
func TestRunOnce_InitialStartupFailureDoesNotBlockRunnableMount(t *testing.T) {
	rd := testStandaloneMount(t, "personal:healthy@example.com", "Healthy")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	cfg.InitialStartupResults = []MountStartupResult{{
		SelectionIndex: 1,
		DisplayName:    "Bad",
		Status:         MountStartupFatal,
		Err:            errors.New("compile standalone mount config: bad min_free_space"),
	}}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{report: &syncengine.Report{Mode: syncengine.SyncBidirectional, Uploads: 1}}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 1)
	assert.Equal(t, MountStartupFatal, result.Startup.Results[0].Status)
	require.Error(t, result.Startup.Results[0].Err)
	assert.Contains(t, result.Startup.Results[0].Err.Error(), "bad min_free_space")
	assert.Equal(t, MountStartupRunnable, result.Startup.Results[1].Status)
	assert.NotEqual(t, result.Startup.Results[0].SelectionIndex, result.Startup.Results[1].SelectionIndex)
	assert.Equal(t, testStandaloneMountIdentity(rd.CanonicalID), result.Reports[0].Identity)
	assert.Equal(t, result.Startup.Results[1].SelectionIndex, result.Reports[0].SelectionIndex)
}

// Validates: R-2.4, R-6.8
func TestRunOnce_PanicRecovery(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:panic@example.com", "Panicking")
	rd2 := testStandaloneMount(t, "personal:stable@example.com", "Stable")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	stableReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.syncRoot == rd1.SyncRoot {
			return &mockEngine{shouldPanic: true}, nil
		}

		return &mockEngine{report: stableReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 2)

	var panicReport, stableMountReport *MountReport
	for i := range result.Reports {
		if result.Reports[i].Identity.CanonicalID == rd1.CanonicalID {
			panicReport = result.Reports[i]
		} else {
			stableMountReport = result.Reports[i]
		}
	}

	require.NotNil(t, panicReport)
	require.Error(t, panicReport.Err)
	assert.Contains(t, panicReport.Err.Error(), "panic")
	assert.Nil(t, panicReport.Report)

	require.NotNil(t, stableMountReport)
	require.NoError(t, stableMountReport.Err)
	assert.Equal(t, 1, stableMountReport.Report.Downloads)
}

// Validates: R-2.8.5
func TestPrepareMountWork_ThreadsWebsocketConfig(t *testing.T) {
	rd := testStandaloneMount(t, "personal:websocket@example.com", "Websocket")
	rd.EnableWebsocket = true
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	var captured *engineFactoryRequest
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		captured = &req
		return &mockEngine{report: &syncengine.Report{}}, nil
	}

	mounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)

	work, summary, reports := orch.prepareRunOnceWork(
		t.Context(),
		syncengine.SyncBidirectional,
		mounts,
		nil,
		syncengine.RunOptions{},
		nil,
	)
	require.Len(t, work, 1)
	require.Len(t, summary.Results, 1)
	require.Len(t, reports, 1)
	require.Nil(t, reports[0])

	_, err = work[0].work.fn(t.Context())
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.Mount.enableWebsocket)
	assert.True(t, captured.VerifyDrive)
	assert.NotNil(t, captured.Session)
	assert.NotNil(t, captured.Session.Meta)
}

// Validates: R-2.8.5
func TestStartWatchRunner_ThreadsWebsocketConfig(t *testing.T) {
	rd := testStandaloneMount(t, "personal:watch-websocket@example.com", "WatchWebsocket")
	rd.EnableWebsocket = true
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	var captured *engineFactoryRequest
	started := make(chan struct{}, 1)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		captured = &req
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				select {
				case started <- struct{}{}:
				default:
				}
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	mounts, err := buildStandaloneMountSpecs([]StandaloneMountConfig{rd})
	require.NoError(t, err)

	wr, err := orch.startWatchRunner(ctx, mounts[0], syncengine.SyncDownloadOnly, syncengine.WatchOptions{}, nil, nil)
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.Mount.enableWebsocket)
	assert.True(t, captured.VerifyDrive)
	assert.NotNil(t, captured.Session)
	assert.NotNil(t, captured.Session.Meta)
	assert.Equal(t, rd.RemoteRootItemID, captured.Mount.remoteRootItemID)
	assert.Equal(t, rd.CanonicalID.Email(), captured.Mount.accountEmail)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "watch runner did not start")
	}

	wr.cancel()
	<-wr.done
	require.NoError(t, wr.engine.Close(t.Context()))
	cancel()
}

// Validates: R-2.4.8
func TestStartWatchRunner_FinalDrainRunsOnceBidirectionalFullReconcile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:watch-final-drain@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain
	childID := config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)
	topology := testParentTopologies(&parent, child)

	compiled, err := compileRuntimeMounts([]StandaloneMountConfig{parent}, topology)
	require.NoError(t, err)
	var childMount *mountSpec
	for i := range compiled.Mounts {
		if compiled.Mounts[i].mountID.String() == childID {
			childMount = compiled.Mounts[i]
			break
		}
	}
	require.NotNil(t, childMount)
	require.True(t, childMount.finalDrain)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	captured := make(chan struct {
		mode syncengine.SyncMode
		opts syncengine.RunOptions
	}, 1)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(_ context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error) {
				captured <- struct {
					mode syncengine.SyncMode
					opts syncengine.RunOptions
				}{mode: mode, opts: opts}
				return &syncengine.Report{Mode: mode}, nil
			},
		}, nil
	}

	runnerEvents := make(chan watchRunnerEvent, 1)
	wr, err := orch.startWatchRunner(
		t.Context(),
		childMount,
		syncengine.SyncDownloadOnly,
		syncengine.WatchOptions{},
		runnerEvents,
		nil,
	)
	require.NoError(t, err)
	defer func() {
		require.NoError(t, wr.engine.Close(t.Context()))
	}()

	select {
	case call := <-captured:
		assert.Equal(t, syncengine.SyncBidirectional, call.mode)
		assert.True(t, call.opts.FullReconcile)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "final-drain runner did not run")
	}
	select {
	case event := <-runnerEvents:
		assert.Equal(t, mountID(childID), event.mountID)
		require.NoError(t, event.err)
		require.NotNil(t, event.report)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "final-drain runner did not report completion")
	}
	<-wr.done
}

// Validates: R-2.4.8
func TestHandleFinalDrainWatchRunnerEvent_DoesNotAckParentWhenDrainErrs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:watch-final-drain-error@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.RunnerAction = syncengine.ShortcutChildActionFinalDrain
	childID := config.ChildMountID(parent.CanonicalID.String(), child.BindingItemID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildTopology(orch, &parent, &child)

	compiled, err := orch.compileRuntimeMountSetFromTopology(cfg.StandaloneMounts, cfg.InitialStartupResults)
	require.NoError(t, err)
	var parentMount, childMount *mountSpec
	for i := range compiled.Mounts {
		if compiled.Mounts[i].mountID.String() == parent.CanonicalID.String() {
			parentMount = compiled.Mounts[i]
		}
		if compiled.Mounts[i].mountID.String() == childID {
			childMount = compiled.Mounts[i]
		}
	}
	require.NotNil(t, parentMount)
	require.NotNil(t, childMount)

	ackCount := 0
	parentEngine := &mockEngine{
		ackDrainFn: func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildTopologySnapshot, error) {
			ackCount++
			return syncengine.ShortcutChildTopologySnapshot{}, nil
		},
	}
	runners := map[mountID]*watchRunner{
		parentMount.mountID: {
			mount:  parentMount,
			engine: parentEngine,
		},
	}
	wr := &watchRunner{
		mount: childMount,
	}

	orch.handleFinalDrainWatchRunnerEvent(t.Context(), runners, wr, watchRunnerEvent{
		mountID: childMount.mountID,
		report:  &syncengine.Report{},
		err:     fmt.Errorf("opening child root: %w", syncengine.ErrMountRootUnavailable),
	})

	assert.Equal(t, 0, ackCount)
	publication := orch.parentShortcutTopologyFor(mountID(parent.CanonicalID.String()))
	require.Len(t, publication.Children, 1)
	assert.Equal(t, syncengine.ShortcutChildActionFinalDrain, publication.Children[0].RunnerAction)
}

// Validates: R-2.4
func TestRunOnce_ContextCanceled(t *testing.T) {
	rd := testStandaloneMount(t, "personal:cancel@example.com", "Cancel")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{err: context.Canceled}, nil
	}

	result := orch.RunOnce(ctx, syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Reports, 1)
	assert.ErrorIs(t, result.Reports[0].Err, context.Canceled)
}

// Validates: R-2.4
func TestRunOnce_EngineFactoryError(t *testing.T) {
	rd := testStandaloneMount(t, "personal:factory-err@example.com", "FactoryErr")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return nil, errors.New("db init failed")
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.SkippedResults(), 1)
	require.Empty(t, result.Reports)
	require.Error(t, result.Startup.SkippedResults()[0].Err)
	assert.Contains(t, result.Startup.SkippedResults()[0].Err.Error(), "db init failed")
}

// Validates: R-6.10.7
func TestRunOnce_EngineFactoryError_IsolatesAffectedMount(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:storefail@example.com", "StoreFail")
	rd2 := testStandaloneMount(t, "personal:healthy@example.com", "Healthy")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	okReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.syncRoot == rd1.SyncRoot {
			return nil, errors.New("open sync store: corrupted database")
		}

		return &mockEngine{report: okReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 1)

	var failedResult *MountStartupResult
	var healthyReport *MountReport
	for i := range result.Startup.Results {
		if result.Startup.Results[i].Identity.CanonicalID == rd1.CanonicalID {
			failedResult = &result.Startup.Results[i]
		}
	}
	for i := range result.Reports {
		if result.Reports[i].Identity.CanonicalID == rd2.CanonicalID {
			healthyReport = result.Reports[i]
		}
	}

	require.NotNil(t, failedResult)
	require.Error(t, failedResult.Err)
	assert.Contains(t, failedResult.Err.Error(), "open sync store")
	assert.Equal(t, MountStartupFatal, failedResult.Status)

	require.NotNil(t, healthyReport)
	require.NoError(t, healthyReport.Err)
	require.NotNil(t, healthyReport.Report)
	assert.Equal(t, 1, healthyReport.Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_BootstrapsParentShortcutTopologyBeforeStartingChildren(t *testing.T) {
	parent := testStandaloneMount(t, "personal:bootstrap@example.com", "Bootstrap")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.disableTopologyBootstrap = false
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	bindingID := "binding-bootstrap"
	childID := config.ChildMountID(parent.CanonicalID.String(), bindingID)
	standaloneCreations := 0
	runMounts := make([]mountID, 0)
	parentRan := false
	bootstrapParentRan := false
	var bootstrapEngine *topologyBootstrapEngine
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.projectionKind == MountProjectionStandalone {
			standaloneCreations++
			if standaloneCreations == 1 {
				bootstrapEngine = &topologyBootstrapEngine{
					mockEngine: &mockEngine{
						runOnceFn: func(_ context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
							bootstrapParentRan = true
							return &syncengine.Report{}, nil
						},
					},
					prepareFn: func(context.Context) (syncengine.ShortcutChildTopologySnapshot, error) {
						return syncengine.ShortcutChildTopologySnapshot{
							NamespaceID: req.Mount.mountID.String(),
							Children: []syncengine.ShortcutChildTopology{{
								BindingItemID:     bindingID,
								RelativeLocalPath: "Shortcuts/Bootstrap",
								LocalAlias:        "Bootstrap",
								RemoteDriveID:     "remote-child-drive",
								RemoteItemID:      "remote-child-root",
								RemoteIsFolder:    true,
								RunnerAction:      syncengine.ShortcutChildActionRun,
							}},
						}, nil
					},
				}
				return bootstrapEngine, nil
			}

			runMounts = append(runMounts, req.Mount.mountID)
			return &mockEngine{
				runOnceFn: func(_ context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					parentRan = true
					return &syncengine.Report{}, nil
				},
			}, nil
		}

		require.Positive(t, standaloneCreations, "child engine started before parent shortcut bootstrap")
		runMounts = append(runMounts, req.Mount.mountID)
		return &mockEngine{report: &syncengine.Report{}}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Equal(t, 2, standaloneCreations)
	require.NotNil(t, bootstrapEngine)
	assert.True(t, bootstrapEngine.closed)
	assert.False(t, bootstrapParentRan)
	require.Len(t, result.Reports, 2)
	assert.True(t, parentRan)
	assert.Contains(t, runMounts, mountID(parent.CanonicalID.String()))
	assert.Contains(t, runMounts, mountID(childID))
	publication := orch.parentShortcutTopologyFor(mountID(parent.CanonicalID.String()))
	require.Len(t, publication.Children, 1)
	assert.Equal(t, childID, config.ChildMountID(publication.NamespaceID, publication.Children[0].BindingItemID))
}

// Validates: R-2.4
func TestBootstrapShortcutTopologyReusesActiveWatchParentRunner(t *testing.T) {
	parent := testStandaloneMount(t, "personal:bootstrap-watch@example.com", "BootstrapWatch")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.disableTopologyBootstrap = false
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		require.FailNow(t, "bootstrap opened a second parent engine for an unchanged active watch runner")
		return nil, assert.AnError
	}

	compiled, err := orch.buildRuntimeMountSet(t.Context(), cfg.StandaloneMounts, cfg.InitialStartupResults)
	require.NoError(t, err)
	require.Len(t, compiled.Mounts, 1)

	existingRunner := &watchRunner{
		mount:  compiled.Mounts[0],
		engine: &mockEngine{},
	}
	refreshed, bootstrapped, err := orch.bootstrapShortcutTopology(
		t.Context(),
		compiled,
		cfg.StandaloneMounts,
		cfg.InitialStartupResults,
		map[mountID]*watchRunner{compiled.Mounts[0].mountID: existingRunner},
	)

	require.NoError(t, err)
	assert.Same(t, compiled, refreshed)
	assert.Empty(t, bootstrapped)
}

// Validates: R-2.4
func TestRunWatch_BootstrapsParentShortcutTopologyBeforeStartingChildren(t *testing.T) {
	parent := testStandaloneMount(t, "personal:watch-bootstrap@example.com", "WatchBootstrap")
	cfgPath := writeTestConfig(t, parent.CanonicalID)
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfigWithPath(t, cfgPath, parent)
	cfg.disableTopologyBootstrap = false
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	bindingID := "binding-watch-bootstrap"
	childID := config.ChildMountID(parent.CanonicalID.String(), bindingID)
	prepared := false
	childStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.projectionKind {
		case MountProjectionStandalone:
			return &topologyBootstrapEngine{
				mockEngine: &mockEngine{},
				prepareFn: func(context.Context) (syncengine.ShortcutChildTopologySnapshot, error) {
					prepared = true
					return syncengine.ShortcutChildTopologySnapshot{
						NamespaceID: req.Mount.mountID.String(),
						Children: []syncengine.ShortcutChildTopology{{
							BindingItemID:     bindingID,
							RelativeLocalPath: "Shortcuts/WatchBootstrap",
							LocalAlias:        "WatchBootstrap",
							RemoteDriveID:     "remote-child-drive",
							RemoteItemID:      "remote-child-root",
							RemoteIsFolder:    true,
							RunnerAction:      syncengine.ShortcutChildActionRun,
						}},
					}, nil
				},
			}, nil
		case MountProjectionChild:
			require.True(t, prepared, "child engine started before parent shortcut bootstrap")
			assert.Equal(t, mountID(childID), req.Mount.mountID)
			return &mockEngine{
				runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
					close(childStarted)
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		default:
			require.FailNow(t, "unexpected mount projection")
		}
		return nil, assert.AnError
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not start bootstrapped child in time")
	}

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestApplyWatchMountSet_StopsStaleChildBeforeStartingReplacement(t *testing.T) {
	parent := testStandaloneMount(t, "personal:watch-replace@example.com", "WatchReplace")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	parentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	child := testPublishedShortcutChild()
	child.LocalRootIdentity = &syncengine.ShortcutRootIdentity{Device: 1, Inode: 2}
	oldCompiled, err := compileRuntimeMountsForParents(parentMounts, testParentTopologies(&parent, child), nil)
	require.NoError(t, err)

	nextChild := child
	nextChild.LocalRootIdentity = &syncengine.ShortcutRootIdentity{Device: 3, Inode: 4}
	nextParentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	newCompiled, err := compileRuntimeMountsForParents(nextParentMounts, testParentTopologies(&parent, nextChild), nil)
	require.NoError(t, err)

	var oldChildMount *mountSpec
	for i := range oldCompiled.Mounts {
		if oldCompiled.Mounts[i].projectionKind == MountProjectionChild {
			oldChildMount = oldCompiled.Mounts[i]
			break
		}
	}
	require.NotNil(t, oldChildMount)

	events := make([]string, 0)
	oldDone := make(chan struct{})
	runners := map[mountID]*watchRunner{
		oldChildMount.mountID: {
			mount:  oldChildMount,
			engine: &mockEngine{},
			cancel: func() {
				events = append(events, "stop-child")
				close(oldDone)
			},
			done: oldDone,
		},
	}
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.projectionKind == MountProjectionChild {
			require.Equal(t, []string{"stop-child"}, events)
			events = append(events, "start-child")
		}
		return &mockEngine{}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stopped, started, startResults := orch.applyWatchMountSet(
		ctx,
		runners,
		newCompiled,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		nil,
		nil,
	)

	assert.Empty(t, summarizeStartupResults(startResults).SkippedResults())
	assert.Equal(t, 1, stopped)
	assert.Equal(t, 2, started)
	assert.Equal(t, []string{"stop-child", "start-child"}, events)

	cancel()
	for _, runner := range runners {
		<-runner.done
	}
}

// Validates: R-2.4
func TestRunOnce_TokenError_ReportsPerMount(t *testing.T) {
	rd := testStandaloneMount(t, "personal:notoken@example.com", "NoToken")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("token file not found")
	}

	orch := NewOrchestrator(cfg)

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.SkippedResults(), 1)
	require.Empty(t, result.Reports)
	require.Error(t, result.Startup.SkippedResults()[0].Err)
	assert.Contains(t, result.Startup.SkippedResults()[0].Err.Error(), "token")
}

// --- zero DriveID ---

// Validates: R-2.4
func TestRunOnce_ZeroDriveID_ReportsError(t *testing.T) {
	cid := testCanonicalID(t, "personal:zero-id@example.com")

	tokenOwner, err := config.TokenAccountCanonicalID(cid)
	require.NoError(t, err)
	rd := StandaloneMountConfig{
		CanonicalID:         cid,
		DisplayName:         "ZeroID",
		SyncRoot:            t.TempDir(),
		StatePath:           config.DriveStatePath(cid),
		TokenOwnerCanonical: tokenOwner,
		AccountEmail:        cid.Email(),
		// RemoteDriveID intentionally zero — should produce an error, not trigger discovery.
	}
	require.True(t, rd.RemoteDriveID.IsZero())

	cfg := testOrchestratorConfig(t, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.SkippedResults(), 1)
	require.Empty(t, result.Reports)
	require.Error(t, result.Startup.SkippedResults()[0].Err)
	assert.Contains(t, result.Startup.SkippedResults()[0].Err.Error(), "drive ID not resolved")
	assert.Contains(t, result.Startup.SkippedResults()[0].Err.Error(), "login")
}

// Validates: R-2.9.1
func TestRunOnce_ControlSocketBlocksWatchOwner(t *testing.T) {
	rd := testStandaloneMount(t, "personal:owner-lock@example.com", "OwnerLock")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	started := make(chan struct{})
	release := make(chan struct{})

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				close(started)
				select {
				case <-release:
					return &syncengine.Report{Mode: syncengine.SyncBidirectional}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reportsCh := make(chan RunOnceResult, 1)
	go func() {
		reportsCh <- orch.RunOnce(ctx, syncengine.SyncBidirectional, syncengine.RunOptions{})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunOnce did not start in time")
	}
	status := getControlStatus(t, cfg.ControlSocketPath)
	assert.Equal(t, synccontrol.OwnerModeOneShot, status.OwnerMode)

	watchErr := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.Error(t, watchErr)
	var inUse *ControlSocketInUseError
	require.ErrorAs(t, watchErr, &inUse)

	close(release)
	select {
	case result := <-reportsCh:
		require.Len(t, result.Reports, 1)
		require.NoError(t, result.Reports[0].Err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunOnce did not stop in time")
	}
}

// Validates: R-2.9.1
func TestRunOnce_BindsControlSocketBeforeEngineStartup(t *testing.T) {
	rd := testStandaloneMount(t, "personal:owner-lock-first@example.com", "OwnerLockFirst")
	setupXDGIsolation(t, rd.CanonicalID)

	socketPath := shortControlSocketPath(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := conn.Close(); closeErr != nil {
				return
			}
		}
	}()

	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = socketPath
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		require.Fail(t, "engine factory should not run when control socket is already owned")
		return nil, assert.AnError
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Len(t, result.Startup.Results, 1)
	var inUse *ControlSocketInUseError
	require.ErrorAs(t, result.Startup.Results[0].Err, &inUse)
	assert.Equal(t, MountStartupFatal, result.Startup.Results[0].Status)

	require.NoError(t, listener.Close())
	<-acceptDone
}

// Validates: R-2.9.1
func TestRunWatch_BindsControlSocketBeforeEngineStartup(t *testing.T) {
	rd := testStandaloneMount(t, "personal:watch-owner-lock-first@example.com", "WatchOwnerLockFirst")
	setupXDGIsolation(t, rd.CanonicalID)

	socketPath := shortControlSocketPath(t)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", socketPath)
	require.NoError(t, err)

	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			if closeErr := conn.Close(); closeErr != nil {
				return
			}
		}
	}()

	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = socketPath
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(context.Context, engineFactoryRequest) (engineRunner, error) {
		require.Fail(t, "engine factory should not run when control socket is already owned")
		return nil, assert.AnError
	}

	err = orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})

	var inUse *ControlSocketInUseError
	require.ErrorAs(t, err, &inUse)

	require.NoError(t, listener.Close())
	<-acceptDone
}

// --- mockEngine ---

// mockEngine implements engineRunner for unit tests.
type mockEngine struct {
	report      *syncengine.Report
	err         error
	shouldPanic bool
	closed      bool
	runOnceFn   func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	runWatchFn  func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
	ackDrainFn  func(ctx context.Context, ack syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildTopologySnapshot, error)
}

func (m *mockEngine) RunOnce(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error) {
	if m.shouldPanic {
		panic("mock engine panic")
	}
	if m.runOnceFn != nil {
		return m.runOnceFn(ctx, mode, opts)
	}

	return m.report, m.err
}

func (m *mockEngine) RunWatch(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error {
	if m.runWatchFn != nil {
		return m.runWatchFn(ctx, mode, opts)
	}

	// Default: block until context is canceled.
	<-ctx.Done()

	return fmt.Errorf("watch context: %w", ctx.Err())
}

func (m *mockEngine) Close(context.Context) error {
	m.closed = true
	return nil
}

func (m *mockEngine) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack syncengine.ShortcutChildDrainAck,
) (syncengine.ShortcutChildTopologySnapshot, error) {
	if m.ackDrainFn != nil {
		return m.ackDrainFn(ctx, ack)
	}
	return syncengine.ShortcutChildTopologySnapshot{}, nil
}

type topologyBootstrapEngine struct {
	*mockEngine
	prepareFn func(context.Context) (syncengine.ShortcutChildTopologySnapshot, error)
}

func (m *topologyBootstrapEngine) PrepareShortcutChildren(ctx context.Context) (
	syncengine.ShortcutChildTopologySnapshot,
	error,
) {
	if m.prepareFn == nil {
		return syncengine.ShortcutChildTopologySnapshot{}, nil
	}
	return m.prepareFn(ctx)
}

// --- RunWatch ---

// Validates: R-2.4
func TestOrchestrator_RunWatch_SingleMount(t *testing.T) {
	rd := testStandaloneMount(t, "personal:watch1@example.com", "Watch1")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	watchStarted := make(chan struct{})

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for watch to start, then shut down.
	select {
	case <-watchStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not start in time")
	}

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_RunWatch_MultiMount(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:multi1@example.com", "Multi1")
	rd2 := testStandaloneMount(t, "personal:multi2@example.com", "Multi2")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait until both mounts have started.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

func TestOrchestrator_RunWatch_SkipsIncompatibleStoreMountWhenAnotherMountStarts(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:healthy@example.com", "Healthy")
	rd2 := testStandaloneMount(t, "personal:reset@example.com", "Reset")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	warnings := make(chan StartupWarning, 1)
	cfg.StartWarning = func(warning StartupWarning) {
		warnings <- warning
	}

	orch := NewOrchestrator(cfg)

	watchStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.canonicalID == rd2.CanonicalID {
			return nil, &syncengine.StateStoreIncompatibleError{Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema}
		}

		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case <-watchStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not start healthy mount in time")
	}

	select {
	case warning := <-warnings:
		results := warning.Summary.SkippedResults()
		require.Len(t, results, 1)
		assert.Equal(t, testStandaloneMountIdentity(rd2.CanonicalID), results[0].Identity)
		assert.Equal(t, MountStartupIncompatibleStore, results[0].Status)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not emit startup warning")
	}

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

func TestOrchestrator_RunWatch_ReturnsStartupFailureWhenNoMountStarts(t *testing.T) {
	rd := testStandaloneMount(t, "personal:reset@example.com", "Reset")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return nil, &syncengine.StateStoreIncompatibleError{Reason: syncengine.StateStoreIncompatibleReasonIncompatibleSchema}
	}

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.Error(t, err)

	var startupErr *WatchStartupError
	require.ErrorAs(t, err, &startupErr)
	require.Len(t, startupErr.Summary.Results, 1)
	assert.Equal(t, testStandaloneMountIdentity(rd.CanonicalID), startupErr.Summary.Results[0].Identity)
	assert.Equal(t, MountStartupIncompatibleStore, startupErr.Summary.Results[0].Status)
}

func TestOrchestrator_RunWatch_ReturnsErrorWhenAllMountsPaused(t *testing.T) {
	rd := testStandaloneMount(t, "personal:paused@example.com", "Paused")
	rd.Paused = true
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.Error(t, err)
	var startupErr *WatchStartupError
	require.ErrorAs(t, err, &startupErr)
	assert.True(t, startupErr.Summary.AllPaused())
}

// Validates: R-2.9.1
func TestOrchestrator_RunWatch_ClosesControlSocketWhenStartupValidationFails(t *testing.T) {
	rd := testStandaloneMount(t, "personal:paused-close@example.com", "PausedClose")
	rd.Paused = true
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)

	orch := NewOrchestrator(cfg)

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.Error(t, err)
	assert.NoFileExists(t, cfg.ControlSocketPath)
}

// Validates: R-2.9.1, R-2.9.2
func TestOrchestrator_ControlSocket_StatusAndStop(t *testing.T) {
	rd := testStandaloneMount(t, "personal:control@example.com", "Control")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	watchStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case <-watchStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not start in time")
	}

	client := controlTestClient(cfg.ControlSocketPath)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, synccontrol.HTTPBaseURL+synccontrol.PathStatus, http.NoBody)
	require.NoError(t, err)
	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var status synccontrol.StatusResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&status))
	assert.Equal(t, synccontrol.OwnerModeWatch, status.OwnerMode)
	assert.Equal(t, []string{rd.CanonicalID.String()}, status.Mounts)

	stop := postControlJSON(t, cfg.ControlSocketPath, synccontrol.PathStop, nil)
	assert.Equal(t, synccontrol.StatusStopping, stop.Status)

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.9.1
func TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus(t *testing.T) {
	rd := testStandaloneMount(t, "personal:oneshot@example.com", "OneShot")
	cfg := testOrchestratorConfig(t, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	orch := NewOrchestrator(cfg)

	control, err := orch.startControlServer(t.Context(), synccontrol.OwnerModeOneShot, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, control.Close(context.Background()))
	})

	status := getControlStatus(t, cfg.ControlSocketPath)
	assert.Equal(t, synccontrol.OwnerModeOneShot, status.OwnerMode)
	assert.Equal(t, []string{rd.CanonicalID.String()}, status.Mounts)

	client := controlTestClient(cfg.ControlSocketPath)
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		synccontrol.HTTPBaseURL+synccontrol.PathReload,
		http.NoBody,
	)
	require.NoError(t, err)

	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)

	var decoded synccontrol.MutationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
	assert.Equal(t, synccontrol.StatusError, decoded.Status)
	assert.Equal(t, synccontrol.ErrorForegroundSyncRunning, decoded.Code)
	assert.Contains(t, decoded.Message, "foreground sync")
}

// Validates: R-2.4
func TestOrchestrator_Reload_AddDrive(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:existing@example.com", "Existing")
	rd2CID := driveid.MustCanonicalID("personal:added@example.com")

	// XDG isolation so reload mount compilation finds catalog drive identity.
	setupXDGIsolation(t, rd1.CanonicalID, rd2CID)

	cfgPath := writeTestConfig(t, rd1.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for first mount to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Add a second mount to the config and request a control-socket reload.
	writeTestConfigMulti(t, cfgPath, rd1.CanonicalID, rd1.SyncRoot, rd2CID, t.TempDir())
	postControlReload(t, cfg.ControlSocketPath)

	// Wait for the second mount to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_Reload_RemoveMount(t *testing.T) {
	rd1 := testStandaloneMount(t, "personal:keep@example.com", "Keep")
	rd2 := testStandaloneMount(t, "personal:remove@example.com", "Remove")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1, rd2)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for both mounts to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	// Remove rd2 from config and request a control-socket reload.
	writeTestConfigSingle(t, cfgPath, rd1.CanonicalID, rd1.SyncRoot)
	postControlReload(t, cfg.ControlSocketPath)

	// Wait for one runner to stop (the removed mount).
	require.Eventually(t, func() bool {
		return stopped.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_Reload_PausedMount(t *testing.T) {
	rd := testStandaloneMount(t, "personal:pausetest@example.com", "PauseTest")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for mount to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Pause the mount and request a control-socket reload.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	postControlReload(t, cfg.ControlSocketPath)

	// The mount runner should stop.
	require.Eventually(t, func() bool {
		return stopped.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_Reload_InvalidConfig(t *testing.T) {
	rd := testStandaloneMount(t, "personal:invalidcfg@example.com", "InvalidCfg")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for mount to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Write invalid TOML and request a control-socket reload; old state should remain.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	postControlReload(t, cfg.ControlSocketPath)

	assert.Never(t, func() bool {
		return started.Load() > 1
	}, 200*time.Millisecond, 10*time.Millisecond, "mount should still be running after invalid config reload")
	assert.Equal(t, int32(1), started.Load(), "mount should still be running after invalid config reload")

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_Reload_EmitsMountLocalCompilationFailures(t *testing.T) {
	rd := testStandaloneMount(t, "personal:reload-healthy@example.com", "ReloadHealthy")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	currentMount := cfg.StandaloneMounts[0]
	reloadErr := errors.New("compile standalone mount config: token owner")
	cfg.ReloadStandaloneMounts = func(*config.Config) (StandaloneMountSelection, error) {
		return StandaloneMountSelection{
			Mounts: []StandaloneMountConfig{currentMount},
			StartupResults: []MountStartupResult{{
				SelectionIndex: 1,
				DisplayName:    "BadReloadMount",
				Status:         MountStartupFatal,
				Err:            reloadErr,
			}},
		}, nil
	}
	warnings := make(chan StartupWarning, 1)
	cfg.StartWarning = func(warning StartupWarning) {
		warnings <- warning
	}

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	postControlReload(t, cfg.ControlSocketPath)

	select {
	case warning := <-warnings:
		results := warning.Summary.SkippedResults()
		require.Len(t, results, 1)
		assert.Equal(t, MountStartupFatal, results[0].Status)
		require.ErrorIs(t, results[0].Err, reloadErr)
	case <-time.After(5 * time.Second):
		require.Fail(t, "reload warning was not emitted")
	}

	assert.Never(t, func() bool {
		return started.Load() > 1
	}, 200*time.Millisecond, 10*time.Millisecond)

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_Reload_TimedPauseExpiry(t *testing.T) {
	rd := testStandaloneMount(t, "personal:timedpause@example.com", "TimedPause")

	// XDG isolation — defensive, so reload mount compilation can find metadata
	// if needed.
	setupXDGIsolation(t, rd.CanonicalID)

	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				started.Add(1)
				<-ctx.Done()
				stopped.Add(1)
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	// Wait for mount to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Set an already-expired timed pause and request a control-socket reload.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", "2000-01-01T00:00:00Z"))
	postControlReload(t, cfg.ControlSocketPath)

	require.Eventually(t, func() bool {
		reloadedCfg, err := config.LoadOrDefault(cfgPath, slog.Default())
		if err != nil {
			return false
		}
		d := reloadedCfg.Drives[rd.CanonicalID]
		return d.Paused == nil && d.PausedUntil == nil
	}, 5*time.Second, 10*time.Millisecond)

	assert.Equal(t, int32(0), stopped.Load(), "mount should NOT be stopped — expired pause is cleared, mount stays running")
	assert.Equal(t, int32(1), started.Load(), "mount should NOT be restarted — the mount was already running")

	// Verify paused keys were cleared from config file.
	reloadedCfg, err := config.LoadOrDefault(cfgPath, slog.Default())
	require.NoError(t, err)
	d := reloadedCfg.Drives[rd.CanonicalID]
	assert.Nil(t, d.Paused, "paused should be cleared after timed pause expires")
	assert.Nil(t, d.PausedUntil, "paused_until should be cleared after timed pause expires")

	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.4
func TestOrchestrator_RunWatch_ZeroMounts(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no standalone mounts")
}

// --- test config helpers ---

// writeTestConfig creates a config file with the given drives using the
// real config API to ensure correct TOML format. Each call creates a fresh
// file (AppendDriveSection creates from template if the file doesn't exist).
func writeTestConfig(t *testing.T, cids ...driveid.CanonicalID) string {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")

	require.NotEmpty(t, cids, "writeTestConfig requires at least one CID")

	for _, cid := range cids {
		require.NoError(t, config.AppendDriveSection(cfgPath, cid, t.TempDir()))
	}

	return cfgPath
}

// writeTestConfigSingle overwrites a config file with a single drive.
// Removes any existing file first to ensure a clean slate.
func writeTestConfigSingle(t *testing.T, cfgPath string, cid driveid.CanonicalID, syncDir string) {
	t.Helper()

	require.NoError(t, os.Remove(cfgPath))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid, syncDir))
}

// writeTestConfigMulti overwrites a config file with two drives.
// Removes any existing file first to ensure a clean slate.
func writeTestConfigMulti(t *testing.T, cfgPath string, cid1 driveid.CanonicalID, dir1 string, cid2 driveid.CanonicalID, dir2 string) {
	t.Helper()

	require.NoError(t, os.Remove(cfgPath))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid1, dir1))
	require.NoError(t, config.AppendDriveSection(cfgPath, cid2, dir2))
}
