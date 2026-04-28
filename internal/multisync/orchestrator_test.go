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
	"strings"
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

const (
	testChildRemoteDriveID = "remote-child-drive"
	testChildRemoteRootID  = "remote-child-root"
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
		Holder:                 holder,
		StandaloneMounts:       mounts,
		ReloadStandaloneMounts: testStandaloneMountsFromConfig,
		Runtime:                provider,
		DataDir:                t.TempDir(),
		Logger:                 slog.Default(),
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

func shortcutChildMountIDForTest(parent *StandaloneMountConfig, child *syncengine.ShortcutChildRunCommand) string {
	if parent == nil || child == nil {
		return ""
	}
	return child.ChildMountID
}

func seedShortcutChildStateArtifactsForTest(
	t *testing.T,
	dataDir string,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildRunCommand,
	includeSidecars bool,
) {
	t.Helper()
	childMountID := shortcutChildMountIDForTest(parent, child)
	require.NotEmpty(t, childMountID)
	require.NotNil(t, child)

	statePath := config.MountStatePathForDataDir(dataDir, childMountID)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o700))
	require.NoError(t, os.WriteFile(statePath, []byte("state"), 0o600))
	if includeSidecars {
		require.NoError(t, os.WriteFile(statePath+"-wal", []byte("wal"), 0o600))
		require.NoError(t, os.WriteFile(statePath+"-shm", []byte("shm"), 0o600))
		require.NoError(t, os.WriteFile(statePath+"-journal", []byte("journal"), 0o600))
	}
	childRoot := child.Engine.LocalRoot
	sessionStore := driveops.NewSessionStore(dataDir, slog.New(slog.DiscardHandler))
	require.NoError(t, sessionStore.Save(child.Engine.RemoteDriveID, "/queued-upload.bin", &driveops.SessionRecord{
		MountID:    childMountID,
		LocalRoot:  childRoot,
		SessionURL: "https://example.com/upload",
		FileHash:   "queued",
		FileSize:   42,
	}))
	require.NoError(t, sessionStore.Save(child.Engine.RemoteDriveID, "/root-only-upload.bin", &driveops.SessionRecord{
		MountID:    "legacy-missing-child-mount-tag",
		LocalRoot:  filepath.Join(childRoot, "nested"),
		SessionURL: "https://example.com/upload-root-only",
		FileHash:   "queued-root-only",
		FileSize:   43,
	}))
	require.NoError(t, config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		catalog.Drives[childMountID] = config.CatalogDrive{
			CanonicalID:   childMountID,
			DisplayName:   "Accidental child catalog record",
			RemoteDriveID: child.Engine.RemoteDriveID,
		}
		return nil
	}))
}

func assertShortcutChildArtifactsPurgedForTest(
	t *testing.T,
	dataDir string,
	parent *StandaloneMountConfig,
	child *syncengine.ShortcutChildRunCommand,
	includeSidecars bool,
) {
	t.Helper()
	childMountID := shortcutChildMountIDForTest(parent, child)
	require.NotEmpty(t, childMountID)
	require.NotNil(t, child)

	statePath := config.MountStatePathForDataDir(dataDir, childMountID)
	assert.NoFileExists(t, statePath)
	if includeSidecars {
		assert.NoFileExists(t, statePath+"-wal")
		assert.NoFileExists(t, statePath+"-shm")
		assert.NoFileExists(t, statePath+"-journal")
	}
	sessionStore := driveops.NewSessionStore(dataDir, slog.New(slog.DiscardHandler))
	_, found, err := sessionStore.Load(child.Engine.RemoteDriveID, "/queued-upload.bin")
	require.NoError(t, err)
	assert.False(t, found)
	_, found, err = sessionStore.Load(child.Engine.RemoteDriveID, "/root-only-upload.bin")
	require.NoError(t, err)
	assert.False(t, found)
	catalog, err := config.LoadCatalogForDataDir(dataDir)
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
	childID := child.ChildMountID
	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "A")
	require.NoError(t, os.MkdirAll(filepath.Dir(childRoot), 0o700))
	require.NoError(t, os.WriteFile(childRoot, []byte("not a directory"), 0o600))

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildRunCommand(orch, &parent, &child)

	decisions, err := orch.buildRunnerDecisionSet(t.Context(), cfg.StandaloneMounts, nil)

	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)
	assert.Equal(t, mountID(parent.CanonicalID.String()), decisions.Mounts[0].mountID)
	assert.Equal(t, mountID(childID), decisions.Mounts[1].mountID)
	assert.Empty(t, decisions.Skipped)
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
//
//nolint:funlen // Final-drain regression keeps parent and child lifecycle setup in one scenario.
func TestRunOnce_FinalDrainChildRunsBidirectionalFullReconcileAndReleasesAfterSuccess(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:final-drain@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	childID := child.ChildMountID
	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
	child.Engine.LocalRoot = childRoot
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "local-change.txt"), []byte("pending upload"), 0o600))
	seedShortcutChildStateArtifactsForTest(t, cfg.DataDir, &parent, &child, true)

	orch := NewOrchestrator(cfg)
	seedShortcutChildRunCommand(orch, &parent, &child)

	type runCall struct {
		mountID string
		mode    syncengine.SyncMode
		opts    syncengine.RunOptions
	}
	var callsMu sync.Mutex
	calls := make([]runCall, 0, 2)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		ackDrainFn := func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error) {
			return syncengine.ShortcutChildProcessSnapshot{}, nil
		}
		ackCleanupFn := func(context.Context, syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildProcessSnapshot, error) {
			return syncengine.ShortcutChildProcessSnapshot{}, nil
		}
		if req.Mount.projectionKind == MountProjectionStandalone {
			ackDrainFn = func(_ context.Context, ack syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error) {
				assert.False(t, ack.Ref.IsZero())
				require.NoError(t, os.RemoveAll(childRoot))
				snapshot := cleanupRequestSnapshot(
					parent.CanonicalID.String(),
					syncengine.ShortcutChildCleanupCommand{
						ChildMountID: childID,
						LocalRoot:    childRoot,
						Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
						AckRef:       syncengine.NewShortcutChildAckRef("binding-drain"),
					},
				)
				orch.receiveParentChildProcessSnapshot(mountID(parent.CanonicalID.String()), snapshot)
				return snapshot, nil
			}
			ackCleanupFn = func(_ context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildProcessSnapshot, error) {
				assert.False(t, ack.Ref.IsZero())
				snapshot := syncengine.ShortcutChildProcessSnapshot{NamespaceID: parent.CanonicalID.String()}
				orch.receiveParentChildProcessSnapshot(mountID(parent.CanonicalID.String()), snapshot)
				return snapshot, nil
			}
		}
		return &mockEngine{
			ackDrainFn:   ackDrainFn,
			ackCleanupFn: ackCleanupFn,
			runOnceFn: func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error) {
				if req.Mount.projectionKind == MountProjectionStandalone {
					require.NotNil(t, req.Mount.parentChildProcessSink)
					if err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
				}
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
	assertShortcutChildArtifactsPurgedForTest(t, cfg.DataDir, &parent, &child, true)

	publication := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	assert.Empty(t, publication.RunCommands)
	assert.Empty(t, publication.Cleanups)
}

// Validates: R-2.4.8
func TestRunOnce_FinalDrainChildFailureKeepsProjectionReserved(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:final-drain-fail@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain", "Shortcuts/Docs")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	childID := child.ChildMountID

	childRoot := filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")
	child.Engine.LocalRoot = childRoot
	require.NoError(t, os.MkdirAll(childRoot, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(childRoot, "local-change.txt"), []byte("pending upload"), 0o600))

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	seedShortcutChildStateArtifactsForTest(t, cfg.DataDir, &parent, &child, false)
	orch := NewOrchestrator(cfg)
	seedShortcutChildRunCommand(orch, &parent, &child)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, mode syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				if req.Mount.projectionKind == MountProjectionStandalone {
					require.NotNil(t, req.Mount.parentChildProcessSink)
					if err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
				}
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
	snapshot := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	require.Len(t, snapshot.RunCommands, 1)
	assert.Equal(t, syncengine.ShortcutChildRunModeFinalDrain, snapshot.RunCommands[0].Mode)
	assert.FileExists(t, config.MountStatePathForDataDir(cfg.DataDir, childID))
}

// Validates: R-2.4.8
func TestRunOnce_ParentCleanupRequestPurgesShortcutChildStateArtifacts(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:manual-discard@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-discard", "Shortcuts/Docs")
	child.Engine.LocalRoot = filepath.Join(parent.SyncRoot, "Shortcuts", "Docs")

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	seedShortcutChildStateArtifactsForTest(t, cfg.DataDir, &parent, &child, true)
	orch := NewOrchestrator(cfg)
	orch.receiveParentChildProcessSnapshot(mountID(parent.CanonicalID.String()), cleanupRequestSnapshot(
		parent.CanonicalID.String(),
		syncengine.ShortcutChildCleanupCommand{
			ChildMountID: child.ChildMountID,
			LocalRoot:    child.Engine.LocalRoot,
			Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
			AckRef:       syncengine.NewShortcutChildAckRef("binding-discard"),
		},
	))
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		assert.Equal(t, MountProjectionStandalone, req.Mount.projectionKind)
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
				if req.Mount.parentChildProcessSink == nil {
					return nil, errors.New("missing parent child process sink")
				}
				err := req.Mount.parentChildProcessSink(ctx, cleanupRequestSnapshot(
					parent.CanonicalID.String(),
					syncengine.ShortcutChildCleanupCommand{
						ChildMountID: child.ChildMountID,
						LocalRoot:    child.Engine.LocalRoot,
						Reason:       syncengine.ShortcutChildArtifactCleanupParentRemoved,
						AckRef:       syncengine.NewShortcutChildAckRef("binding-discard"),
					},
				))
				return &syncengine.Report{}, err
			},
			ackCleanupFn: func(_ context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildProcessSnapshot, error) {
				assert.False(t, ack.Ref.IsZero())
				snapshot := syncengine.ShortcutChildProcessSnapshot{NamespaceID: parent.CanonicalID.String()}
				orch.receiveParentChildProcessSnapshot(mountID(parent.CanonicalID.String()), snapshot)
				return snapshot, nil
			},
		}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.Len(t, result.Reports, 1)
	assertShortcutChildArtifactsPurgedForTest(t, cfg.DataDir, &parent, &child, true)
	publication := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	assert.Empty(t, publication.Cleanups)
}

// Validates: R-2.4.8
func TestRunOnce_DropsStaleChildSkipAfterParentPublishesRunnableChild(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:stale-child-skip@example.com", "Parent")
	runnableChild := testChildRecord(mountID(parent.CanonicalID.String()), "binding-stale", "Shortcuts/Docs")
	childID := runnableChild.ChildMountID

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	orch.receiveParentChildProcessSnapshot(mountID(parent.CanonicalID.String()), syncengine.ShortcutChildProcessSnapshot{
		NamespaceID: parent.CanonicalID.String(),
	})

	var childRan atomic.Bool
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.projectionKind {
		case MountProjectionStandalone:
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if req.Mount.parentChildProcessSink == nil {
						return nil, errors.New("missing parent child process sink")
					}
					err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), runnableChild))
					return &syncengine.Report{}, err
				},
			}, nil
		case MountProjectionChild:
			assert.Equal(t, childID, req.Mount.mountID.String())
			childRan.Store(true)
			return &mockEngine{report: &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}}, nil
		default:
			return nil, fmt.Errorf("unexpected projection %s", req.Mount.projectionKind)
		}
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	assert.True(t, childRan.Load())
	require.Len(t, result.Reports, 2)
	assert.Empty(t, result.Startup.SkippedResults())

	var childStartup *MountStartupResult
	for i := range result.Startup.Results {
		if result.Startup.Results[i].Identity.MountID == childID {
			childStartup = &result.Startup.Results[i]
			break
		}
	}
	require.NotNil(t, childStartup)
	assert.Equal(t, MountStartupRunnable, childStartup.Status)
	assert.NoError(t, childStartup.Err)
}

// Validates: R-2.4.8
func TestRunOnce_UsesFinalParentSnapshotInsteadOfIntermediateSkip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:intermediate-child-skip@example.com", "Parent")
	runnableChild := testChildRecord(mountID(parent.CanonicalID.String()), "binding-final", "Shortcuts/Docs")
	childID := runnableChild.ChildMountID

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	var childRan atomic.Bool
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.projectionKind {
		case MountProjectionStandalone:
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					require.NotNil(t, req.Mount.parentChildProcessSink)
					require.NoError(t, req.Mount.parentChildProcessSink(ctx, syncengine.ShortcutChildProcessSnapshot{
						NamespaceID: parent.CanonicalID.String(),
					}))
					require.NoError(t, req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), runnableChild)))
					return &syncengine.Report{}, nil
				},
			}, nil
		case MountProjectionChild:
			assert.Equal(t, childID, req.Mount.mountID.String())
			childRan.Store(true)
			return &mockEngine{report: &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}}, nil
		default:
			return nil, fmt.Errorf("unexpected projection %s", req.Mount.projectionKind)
		}
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	assert.True(t, childRan.Load())
	require.Len(t, result.Reports, 2)
	assert.Empty(t, result.Startup.SkippedResults())
	publication := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, syncengine.ShortcutChildRunModeNormal, publication.RunCommands[0].Mode)
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

	wr, err := orch.startWatchRunner(ctx, mounts[0], syncengine.SyncDownloadOnly, syncengine.WatchOptions{}, nil)
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
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	childID := child.ChildMountID
	topology := testParentTopologies(&parent, child)

	decisions, err := buildRunnerDecisions([]StandaloneMountConfig{parent}, topology, t.TempDir())
	require.NoError(t, err)
	var childMount *mountSpec
	for i := range decisions.Mounts {
		if decisions.Mounts[i].mountID.String() == childID {
			childMount = decisions.Mounts[i]
			break
		}
	}
	require.NotNil(t, childMount)
	require.True(t, childMount.isFinalDrainChild())

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
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	childID := child.ChildMountID

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)
	seedShortcutChildRunCommand(orch, &parent, &child)

	decisions, err := orch.buildRunnerDecisionsFromParentSnapshots(cfg.StandaloneMounts, cfg.InitialStartupResults)
	require.NoError(t, err)
	var parentMount, childMount *mountSpec
	for i := range decisions.Mounts {
		if decisions.Mounts[i].mountID.String() == parent.CanonicalID.String() {
			parentMount = decisions.Mounts[i]
		}
		if decisions.Mounts[i].mountID.String() == childID {
			childMount = decisions.Mounts[i]
		}
	}
	require.NotNil(t, parentMount)
	require.NotNil(t, childMount)

	ackCount := 0
	parentEngine := &mockEngine{
		ackDrainFn: func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error) {
			ackCount++
			return syncengine.ShortcutChildProcessSnapshot{}, nil
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
	publication := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, syncengine.ShortcutChildRunModeFinalDrain, publication.RunCommands[0].Mode)
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
func TestRunOnce_PublishesParentChildProcessSnapshotBeforeStartingChildren(t *testing.T) {
	parent := testStandaloneMount(t, "personal:bootstrap@example.com", "Bootstrap")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	bindingID := "binding-bootstrap"
	childID := config.ChildMountID(parent.CanonicalID.String(), bindingID)
	var startup atomic.Bool
	var parentRan atomic.Bool
	var runMountsMu sync.Mutex
	runMounts := make([]mountID, 0)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Mount.projectionKind == MountProjectionStandalone {
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					require.False(t, startup.Swap(true), "parent child process sink published more than once")
					require.NotNil(t, req.Mount.parentChildProcessSink)
					child := testChildRecord(req.Mount.mountID, bindingID, "Shortcuts/Bootstrap")
					child.Engine.LocalRoot = filepath.Join(parent.SyncRoot, "Shortcuts", "Bootstrap")
					child.Engine.RemoteDriveID = testChildRemoteDriveID
					child.Engine.RemoteItemID = testChildRemoteRootID
					require.NoError(t, req.Mount.parentChildProcessSink(ctx, processSnapshot(
						req.Mount.mountID.String(),
						child,
					)))
					runMountsMu.Lock()
					runMounts = append(runMounts, req.Mount.mountID)
					runMountsMu.Unlock()
					parentRan.Store(true)
					return &syncengine.Report{}, nil
				},
			}, nil
		}

		require.True(t, startup.Load(), "child engine started before parent snapshot")
		runMountsMu.Lock()
		runMounts = append(runMounts, req.Mount.mountID)
		runMountsMu.Unlock()
		return &mockEngine{report: &syncengine.Report{}}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	require.True(t, startup.Load())
	require.Len(t, result.Reports, 2)
	assert.True(t, parentRan.Load())
	runMountsMu.Lock()
	runMountsSnapshot := append([]mountID(nil), runMounts...)
	runMountsMu.Unlock()
	assert.Contains(t, runMountsSnapshot, mountID(parent.CanonicalID.String()))
	assert.Contains(t, runMountsSnapshot, mountID(childID))
	publication := orch.latestParentChildProcessSnapshotFor(mountID(parent.CanonicalID.String()))
	require.Len(t, publication.RunCommands, 1)
	assert.Equal(t, childID, publication.RunCommands[0].ChildMountID)
}

// Validates: R-2.4.8
func TestRunOnce_StartsParentChildrenWithoutWaitingForOtherParents(t *testing.T) {
	parentA := testStandaloneMount(t, "personal:parent-a@example.com", "ParentA")
	parentB := testStandaloneMount(t, "personal:parent-b@example.com", "ParentB")
	setupXDGIsolation(t, parentA.CanonicalID, parentB.CanonicalID)

	cfg := testOrchestratorConfig(t, parentA, parentB)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	child := testChildRecord(mountID(parentA.CanonicalID.String()), "binding-a", "Shortcuts/A")
	childID := child.ChildMountID
	parentBStarted := make(chan struct{})
	releaseParentB := make(chan struct{})
	childStarted := make(chan struct{})
	var parentBDone atomic.Bool
	var childStartedOnce sync.Once
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.mountID.String() {
		case parentA.CanonicalID.String():
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if req.Mount.parentChildProcessSink == nil {
						return nil, errors.New("missing parent child process sink")
					}
					err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parentA.CanonicalID.String(), child))
					return &syncengine.Report{}, err
				},
			}, nil
		case parentB.CanonicalID.String():
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					close(parentBStarted)
					select {
					case <-releaseParentB:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					parentBDone.Store(true)
					return &syncengine.Report{}, nil
				},
			}, nil
		case childID:
			return &mockEngine{
				runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
					assert.False(t, parentBDone.Load(), "child should start before unrelated parent finishes")
					childStartedOnce.Do(func() { close(childStarted) })
					return &syncengine.Report{}, nil
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected mount %s", req.Mount.mountID)
		}
	}

	resultCh := make(chan RunOnceResult, 1)
	go func() {
		resultCh <- orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	}()

	select {
	case <-parentBStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "parent B did not start")
	}
	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "parent A child did not start while parent B was still running")
	}
	close(releaseParentB)

	var result RunOnceResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunOnce did not finish")
	}
	require.Len(t, result.Reports, 3)
	assert.Empty(t, result.Startup.SkippedResults())
}

// Validates: R-2.4.8
func TestRunOnce_StartsParentChildrenAfterPublishingParentReturns(t *testing.T) {
	parent := testStandaloneMount(t, "personal:parent-publishing@example.com", "Parent")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-publish", "Shortcuts/Publish")
	childID := child.ChildMountID
	releaseParent := make(chan struct{})
	childStarted := make(chan struct{})
	var parentReturned atomic.Bool
	var childStartedOnce sync.Once
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.mountID.String() {
		case parent.CanonicalID.String():
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if req.Mount.parentChildProcessSink == nil {
						return nil, errors.New("missing parent child process sink")
					}
					if err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
					select {
					case <-releaseParent:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					parentReturned.Store(true)
					return &syncengine.Report{}, nil
				},
			}, nil
		case childID:
			return &mockEngine{
				runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
					assert.True(t, parentReturned.Load(), "child should start only after publishing parent returns")
					childStartedOnce.Do(func() { close(childStarted) })
					return &syncengine.Report{}, nil
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected mount %s", req.Mount.mountID)
		}
	}

	resultCh := make(chan RunOnceResult, 1)
	go func() {
		resultCh <- orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	}()

	select {
	case <-childStarted:
		require.FailNow(t, "child started before parent returned")
	default:
	}
	select {
	case releaseParent <- struct{}{}:
	default:
		close(releaseParent)
	}
	select {
	case <-childStarted:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "child did not start after parent returned")
	}

	var result RunOnceResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunOnce did not finish")
	}
	require.Len(t, result.Reports, 2)
}

// Validates: R-2.4.8
func TestRunOnce_EmptySnapshotDoesNotConsumeOneShotChildStart(t *testing.T) {
	parent := testStandaloneMount(t, "personal:empty-then-child@example.com", "Parent")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-later", "Shortcuts/Later")
	childID := child.ChildMountID
	childStarted := make(chan struct{})
	var childStartedOnce sync.Once
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.mountID.String() {
		case parent.CanonicalID.String():
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if req.Mount.parentChildProcessSink == nil {
						return nil, errors.New("missing parent child process sink")
					}
					if err := req.Mount.parentChildProcessSink(ctx, syncengine.ShortcutChildProcessSnapshot{
						NamespaceID: parent.CanonicalID.String(),
					}); err != nil {
						return nil, err
					}
					if err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
					return &syncengine.Report{}, nil
				},
			}, nil
		case childID:
			return &mockEngine{
				runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
					childStartedOnce.Do(func() { close(childStarted) })
					return &syncengine.Report{}, nil
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected mount %s", req.Mount.mountID)
		}
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})

	select {
	case <-childStarted:
	default:
		require.Fail(t, "child did not start after later non-empty publication")
	}
	require.Len(t, result.Reports, 2)
}

// Validates: R-2.4.8
func TestRunOnce_DelaysFinalDrainStartUntilPublishingParentSafePoint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	parent := testStandaloneMount(t, "personal:final-drain-safe-point@example.com", "Parent")
	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-drain-safe", "Shortcuts/Docs")
	child.Mode = syncengine.ShortcutChildRunModeFinalDrain
	childID := child.ChildMountID

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	seedShortcutChildStateArtifactsForTest(t, cfg.DataDir, &parent, &child, true)
	orch := NewOrchestrator(cfg)

	releaseParent := make(chan struct{})
	childCompleted := make(chan struct{})
	acked := make(chan struct{})
	var childCompletedOnce sync.Once
	var ackedOnce sync.Once
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.mountID.String() {
		case parent.CanonicalID.String():
			return &mockEngine{
				runOnceFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.RunOptions) (*syncengine.Report, error) {
					if req.Mount.parentChildProcessSink == nil {
						return nil, errors.New("missing parent child process sink")
					}
					if err := req.Mount.parentChildProcessSink(ctx, processSnapshot(parent.CanonicalID.String(), child)); err != nil {
						return nil, err
					}
					select {
					case <-releaseParent:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					return &syncengine.Report{}, nil
				},
				ackDrainFn: func(context.Context, syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error) {
					ackedOnce.Do(func() { close(acked) })
					return syncengine.ShortcutChildProcessSnapshot{}, nil
				},
			}, nil
		case childID:
			return &mockEngine{
				runOnceFn: func(context.Context, syncengine.SyncMode, syncengine.RunOptions) (*syncengine.Report, error) {
					childCompletedOnce.Do(func() { close(childCompleted) })
					return &syncengine.Report{Succeeded: 1}, nil
				},
			}, nil
		default:
			return nil, fmt.Errorf("unexpected mount %s", req.Mount.mountID)
		}
	}

	resultCh := make(chan RunOnceResult, 1)
	go func() {
		resultCh <- orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	}()

	select {
	case <-childCompleted:
		require.FailNow(t, "final-drain child started before parent safe point")
	default:
	}
	select {
	case <-acked:
		require.FailNow(t, "final-drain ack happened before parent safe point")
	default:
	}
	close(releaseParent)
	select {
	case <-acked:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "final-drain ack did not happen after parent safe point")
	}
	select {
	case result := <-resultCh:
		require.Len(t, result.Reports, 2)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "RunOnce did not finish")
	}
}

// Validates: R-2.4
func TestRunWatch_PublishesParentChildProcessSnapshotBeforeStartingChildren(t *testing.T) {
	parent := testStandaloneMount(t, "personal:watch-bootstrap@example.com", "WatchBootstrap")
	cfgPath := writeTestConfig(t, parent.CanonicalID)
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfigWithPath(t, cfgPath, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	bindingID := "binding-watch-bootstrap"
	childID := config.ChildMountID(parent.CanonicalID.String(), bindingID)
	startup := false
	childStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.projectionKind {
		case MountProjectionStandalone:
			return &mockEngine{
				runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
					startup = true
					require.NotNil(t, req.Mount.parentChildProcessSink)
					child := testChildRecord(req.Mount.mountID, bindingID, "Shortcuts/WatchBootstrap")
					child.Engine.LocalRoot = filepath.Join(parent.SyncRoot, "Shortcuts", "WatchBootstrap")
					child.Engine.RemoteDriveID = testChildRemoteDriveID
					child.Engine.RemoteItemID = testChildRemoteRootID
					require.NoError(t, req.Mount.parentChildProcessSink(ctx, processSnapshot(
						req.Mount.mountID.String(),
						child,
					)))
					<-ctx.Done()
					return ctx.Err()
				},
			}, nil
		case MountProjectionChild:
			require.True(t, startup, "child engine started before parent snapshot")
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
func TestRunWatch_ReconcilesChildRunnersFromLiveParentSnapshot(t *testing.T) {
	parent := testStandaloneMount(t, "personal:watch-live-pub@example.com", "WatchLivePub")
	cfgPath := writeTestConfig(t, parent.CanonicalID)
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfigWithPath(t, cfgPath, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	harness := newLiveParentSnapshotWatchHarness(t, orch, &parent, "binding-first", "binding-second")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(ctx, syncengine.SyncBidirectional, syncengine.WatchOptions{})
	}()

	select {
	case <-harness.parentStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "parent did not start")
	}
	select {
	case <-harness.secondStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "second child did not start from live parent snapshot")
	}
	assert.Equal(t, int32(1), harness.parentRunCount.Load())
	close(harness.parentRelease)
	cancel()

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

type liveParentSnapshotWatchHarness struct {
	firstStarted   chan struct{}
	secondStarted  chan struct{}
	parentStarted  chan struct{}
	parentRelease  chan struct{}
	parentRunCount atomic.Int32
}

func newLiveParentSnapshotWatchHarness(
	t *testing.T,
	orch *Orchestrator,
	parent *StandaloneMountConfig,
	firstBinding string,
	secondBinding string,
) *liveParentSnapshotWatchHarness {
	t.Helper()

	require.NotNil(t, parent)
	firstChildID := config.ChildMountID(parent.CanonicalID.String(), firstBinding)
	secondChildID := config.ChildMountID(parent.CanonicalID.String(), secondBinding)
	harness := &liveParentSnapshotWatchHarness{
		firstStarted:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		parentStarted: make(chan struct{}),
		parentRelease: make(chan struct{}),
	}
	var firstStartedOnce sync.Once
	var secondStartedOnce sync.Once
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		switch req.Mount.projectionKind {
		case MountProjectionStandalone:
			return harness.parentSnapshotEngine(t, req.Mount, firstBinding, secondBinding), nil
		case MountProjectionChild:
			return harness.childSnapshotEngine(t, req.Mount.mountID.String(), firstChildID, secondChildID, &firstStartedOnce, &secondStartedOnce), nil
		default:
			require.FailNow(t, "unexpected mount projection")
		}
		return nil, assert.AnError
	}
	return harness
}

func (h *liveParentSnapshotWatchHarness) parentSnapshotEngine(
	t *testing.T,
	mount *mountSpec,
	firstBinding string,
	secondBinding string,
) *mockEngine {
	t.Helper()

	return &mockEngine{
		runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
			h.parentRunCount.Add(1)
			close(h.parentStarted)
			require.NotNil(t, mount.parentChildProcessSink)
			require.NoError(t, mount.parentChildProcessSink(ctx, shortcutSnapshot(mount.mountID.String(), firstBinding, "First")))
			select {
			case <-h.firstStarted:
			case <-ctx.Done():
				return ctx.Err()
			}
			require.NoError(t, mount.parentChildProcessSink(ctx, shortcutSnapshot(mount.mountID.String(), secondBinding, "Second")))
			<-h.parentRelease
			return nil
		},
	}
}

func (h *liveParentSnapshotWatchHarness) childSnapshotEngine(
	t *testing.T,
	childID string,
	firstChildID string,
	secondChildID string,
	firstStartedOnce *sync.Once,
	secondStartedOnce *sync.Once,
) *mockEngine {
	t.Helper()

	return &mockEngine{
		runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
			switch childID {
			case firstChildID:
				firstStartedOnce.Do(func() { close(h.firstStarted) })
			case secondChildID:
				secondStartedOnce.Do(func() { close(h.secondStarted) })
			default:
				require.FailNow(t, "unexpected child mount", childID)
			}
			<-ctx.Done()
			return ctx.Err()
		},
	}
}

func shortcutSnapshot(namespaceID, bindingItemID, alias string) syncengine.ShortcutChildProcessSnapshot {
	return processSnapshot(
		namespaceID,
		syncengine.ShortcutChildRunCommand{
			ChildMountID: config.ChildMountID(namespaceID, bindingItemID),
			DisplayName:  alias,
			Engine: syncengine.ShortcutChildEngineSpec{
				LocalRoot:     filepath.Join(os.TempDir(), "parent", "Shortcuts", alias),
				RemoteDriveID: testChildRemoteDriveID,
				RemoteItemID:  "remote-child-" + strings.ToLower(alias),
			},
			Mode:   syncengine.ShortcutChildRunModeNormal,
			AckRef: syncengine.NewShortcutChildAckRef(bindingItemID),
		},
	)
}

// Validates: R-2.4
func TestReconcileWatchRunnersForParentDoesNotTouchOtherParents(t *testing.T) {
	firstParent := testStandaloneMount(t, "personal:watch-scope-a@example.com", "ScopeA")
	secondParent := testStandaloneMount(t, "personal:watch-scope-b@example.com", "ScopeB")
	setupXDGIsolation(t, firstParent.CanonicalID, secondParent.CanonicalID)

	cfg := testOrchestratorConfig(t, firstParent, secondParent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	firstOld := testChildRecord(mountID(firstParent.CanonicalID.String()), "binding-old", "Shortcuts/Old")
	firstNew := testChildRecord(mountID(firstParent.CanonicalID.String()), "binding-new", "Shortcuts/New")
	secondChild := testChildRecord(mountID(secondParent.CanonicalID.String()), "binding-keep", "Shortcuts/Keep")
	orch.receiveParentChildProcessSnapshot(mountID(firstParent.CanonicalID.String()), processSnapshot(firstParent.CanonicalID.String(), firstOld))
	orch.receiveParentChildProcessSnapshot(mountID(secondParent.CanonicalID.String()), processSnapshot(secondParent.CanonicalID.String(), secondChild))

	parentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	initialDecisions, err := buildRunnerDecisionsForParents(
		parentMounts,
		orch.latestParentChildProcessSnapshotsFor(parentMounts),
		t.TempDir(),
		nil,
	)
	require.NoError(t, err)

	var firstOldMount, secondChildMount *mountSpec
	for i := range initialDecisions.Mounts {
		mount := initialDecisions.Mounts[i]
		if mount.projectionKind != MountProjectionChild {
			continue
		}
		switch mount.mountID.String() {
		case firstOld.ChildMountID:
			firstOldMount = mount
		case secondChild.ChildMountID:
			secondChildMount = mount
		}
	}
	require.NotNil(t, firstOldMount)
	require.NotNil(t, secondChildMount)

	events := make([]string, 0)
	firstDone := make(chan struct{})
	secondDone := make(chan struct{})
	runners := map[mountID]*watchRunner{
		firstOldMount.mountID: {
			mount:  firstOldMount,
			engine: &mockEngine{},
			cancel: func() {
				events = append(events, "stop-first")
				close(firstDone)
			},
			done: firstDone,
		},
		secondChildMount.mountID: {
			mount:  secondChildMount,
			engine: &mockEngine{},
			cancel: func() {
				events = append(events, "stop-second")
				close(secondDone)
			},
			done: secondDone,
		},
	}
	orch.receiveParentChildProcessSnapshot(mountID(firstParent.CanonicalID.String()), processSnapshot(firstParent.CanonicalID.String(), firstNew))
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		require.Equal(t, MountProjectionChild, req.Mount.projectionKind)
		assert.Equal(t, firstNew.ChildMountID, req.Mount.mountID.String())
		events = append(events, "start-first")
		return &mockEngine{}, nil
	}

	orch.reconcileWatchRunnersForParent(
		t.Context(),
		mountID(firstParent.CanonicalID.String()),
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		runners,
		nil,
	)

	assert.Equal(t, []string{"stop-first", "start-first"}, events)
	assert.Contains(t, runners, secondChildMount.mountID)
	assert.NotContains(t, runners, firstOldMount.mountID)
	assert.Contains(t, runners, mountID(firstNew.ChildMountID))
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
	child.Engine.LocalRootIdentity = &syncengine.ShortcutRootIdentity{Device: 1, Inode: 2}
	oldDecisions, err := buildRunnerDecisionsForParents(parentMounts, testParentTopologies(&parent, child), t.TempDir(), nil)
	require.NoError(t, err)

	nextChild := child
	nextChild.Engine.LocalRootIdentity = &syncengine.ShortcutRootIdentity{Device: 3, Inode: 4}
	nextParentMounts, err := buildStandaloneMountSpecs(cfg.StandaloneMounts)
	require.NoError(t, err)
	newDecisions, err := buildRunnerDecisionsForParents(nextParentMounts, testParentTopologies(&parent, nextChild), t.TempDir(), nil)
	require.NoError(t, err)

	var oldChildMount *mountSpec
	for i := range oldDecisions.Mounts {
		if oldDecisions.Mounts[i].projectionKind == MountProjectionChild {
			oldChildMount = oldDecisions.Mounts[i]
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
		newDecisions,
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
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
func TestHandleWatchRunnerEvent_ParentExitStopsChildrenAndForgetsCachedSnapshot(t *testing.T) {
	parent := testStandaloneMount(t, "personal:parent-exit@example.com", "ParentExit")
	setupXDGIsolation(t, parent.CanonicalID)

	cfg := testOrchestratorConfig(t, parent)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn
	orch := NewOrchestrator(cfg)

	child := testChildRecord(mountID(parent.CanonicalID.String()), "binding-exit", "Shortcuts/Exit")
	seedShortcutChildRunCommand(orch, &parent, &child)
	decisions, err := orch.buildRunnerDecisionSet(t.Context(), cfg.StandaloneMounts, cfg.InitialStartupResults)
	require.NoError(t, err)
	require.Len(t, decisions.Mounts, 2)

	var parentMount, childMount *mountSpec
	for _, mount := range decisions.Mounts {
		switch mount.projectionKind {
		case MountProjectionStandalone:
			parentMount = mount
		case MountProjectionChild:
			childMount = mount
		}
	}
	require.NotNil(t, parentMount)
	require.NotNil(t, childMount)

	parentRunnerDone := make(chan struct{})
	close(parentRunnerDone)
	childDone := make(chan struct{})
	childCanceled := false
	runners := map[mountID]*watchRunner{
		parentMount.mountID: {
			mount:  parentMount,
			engine: &mockEngine{},
			cancel: func() {},
			done:   parentRunnerDone,
		},
		childMount.mountID: {
			mount:  childMount,
			engine: &mockEngine{},
			cancel: func() {
				childCanceled = true
				close(childDone)
			},
			done: childDone,
		},
	}

	var restarted []mountID
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		require.Equal(t, MountProjectionStandalone, req.Mount.projectionKind)
		restarted = append(restarted, req.Mount.mountID)
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ syncengine.SyncMode, _ syncengine.WatchOptions) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	orch.handleWatchRunnerEvent(
		ctx,
		watchRunnerEvent{mountID: parentMount.mountID, err: errors.New("parent stopped")},
		syncengine.SyncBidirectional,
		syncengine.WatchOptions{},
		runners,
		make(chan watchRunnerEvent, 4),
	)

	require.True(t, childCanceled)
	assert.NotContains(t, runners, childMount.mountID)
	assert.Empty(t, orch.latestParentChildProcessSnapshotFor(parentMount.mountID).RunCommands)
	assert.Equal(t, []mountID{parentMount.mountID}, restarted)

	cancel()
	for _, runner := range runners {
		runner.cancel()
		<-runner.done
		assert.NoError(t, runner.engine.Close(context.Background()))
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
	report       *syncengine.Report
	err          error
	shouldPanic  bool
	closed       bool
	runOnceFn    func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	runWatchFn   func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
	ackDrainFn   func(ctx context.Context, ack syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error)
	ackCleanupFn func(ctx context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildProcessSnapshot, error)
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

func (m *mockEngine) ShortcutChildAckHandle() shortcutChildAckHandle {
	if m.ackDrainFn == nil && m.ackCleanupFn == nil {
		return nil
	}
	return mockShortcutChildAckHandle{
		ackDrainFn:   m.ackDrainFn,
		ackCleanupFn: m.ackCleanupFn,
	}
}

type mockShortcutChildAckHandle struct {
	ackDrainFn   func(ctx context.Context, ack syncengine.ShortcutChildDrainAck) (syncengine.ShortcutChildProcessSnapshot, error)
	ackCleanupFn func(ctx context.Context, ack syncengine.ShortcutChildArtifactCleanupAck) (syncengine.ShortcutChildProcessSnapshot, error)
}

func (h mockShortcutChildAckHandle) IsZero() bool {
	return h.ackDrainFn == nil && h.ackCleanupFn == nil
}

func (h mockShortcutChildAckHandle) AcknowledgeChildFinalDrain(
	ctx context.Context,
	ack syncengine.ShortcutChildDrainAck,
) (syncengine.ShortcutChildProcessSnapshot, error) {
	if h.ackDrainFn == nil {
		return syncengine.ShortcutChildProcessSnapshot{}, nil
	}
	return h.ackDrainFn(ctx, ack)
}

func (h mockShortcutChildAckHandle) AcknowledgeChildArtifactsPurged(
	ctx context.Context,
	ack syncengine.ShortcutChildArtifactCleanupAck,
) (syncengine.ShortcutChildProcessSnapshot, error) {
	if h.ackCleanupFn == nil {
		return syncengine.ShortcutChildProcessSnapshot{}, nil
	}
	return h.ackCleanupFn(ctx, ack)
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
