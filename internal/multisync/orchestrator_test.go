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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/synccontrol"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// --- helpers ---

// setupXDGIsolation sets XDG_DATA_HOME to a temp dir and creates drive
// metadata files for each CID. This gives buildResolvedDrive (called during
// control-socket reload → ResolveDrives) a non-zero DriveID, which is required
// for Session() to succeed.
func setupXDGIsolation(t *testing.T, cids ...driveid.CanonicalID) {
	t.Helper()

	xdgDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdgDir)

	for _, cid := range cids {
		require.NoError(t, config.SaveDriveMetadata(cid, &config.DriveMetadata{
			DriveID: "test-drive-id",
		}))
	}
}

func testOrchestratorConfig(t *testing.T, drives ...*config.ResolvedDrive) *OrchestratorConfig {
	t.Helper()

	return testOrchestratorConfigWithPath(t, "/tmp/test-config.toml", drives...)
}

// testOrchestratorConfigWithPath creates an OrchestratorConfig with a real
// config path. Use this when the test needs a writable config file (e.g.,
// control-socket reload tests that modify config on disk).
func testOrchestratorConfigWithPath(t *testing.T, cfgPath string, drives ...*config.ResolvedDrive) *OrchestratorConfig {
	t.Helper()

	holder := config.NewHolder(config.DefaultConfig(), cfgPath)
	provider := driveops.NewSessionProvider(
		holder,
		driveops.StaticClientResolver(&http.Client{}, &http.Client{}),
		"test/1.0",
		slog.Default(),
	)

	return &OrchestratorConfig{
		Holder:   holder,
		Drives:   drives,
		Provider: provider,
		Logger:   slog.Default(),
	}
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

func postControlJSONStatus(
	t *testing.T,
	socketPath string,
	path string,
	body []byte,
	wantStatus int,
) synccontrol.MutationResponse {
	t.Helper()

	client := controlTestClient(socketPath)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, synccontrol.HTTPBaseURL+path, bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// #nosec G704 -- fixed Unix-domain test socket client.
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, wantStatus, resp.StatusCode)

	var decoded synccontrol.MutationResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&decoded))
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

func testResolvedDrive(t *testing.T, cidStr, displayName string) *config.ResolvedDrive {
	t.Helper()

	cid := testCanonicalID(t, cidStr)

	return &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: displayName,
		SyncDir:     t.TempDir(),
		DriveID:     driveid.New("test-drive-id"),
	}
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

// --- RunOnce ---

// Validates: R-2.4
func TestRunOnce_ZeroDrives(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	assert.Empty(t, reports)
}

// Validates: R-2.4
func TestRunOnce_OneDrive_Success(t *testing.T) {
	rd := testResolvedDrive(t, "personal:test@example.com", "Test")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	expectedReport := &synctypes.SyncReport{
		Mode:      synctypes.SyncBidirectional,
		Downloads: 5,
	}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{report: expectedReport}, nil
	}

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 1)
	assert.Equal(t, rd.CanonicalID, reports[0].CanonicalID)
	assert.Equal(t, "Test", reports[0].DisplayName)
	require.NoError(t, reports[0].Err)
	require.NotNil(t, reports[0].Report)
	assert.Equal(t, 5, reports[0].Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_TwoDrives_OneFailsOneSucceeds(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:fail@example.com", "Failing")
	rd2 := testResolvedDrive(t, "personal:ok@example.com", "Working")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	errDelta := errors.New("delta gone")
	okReport := &synctypes.SyncReport{Mode: synctypes.SyncBidirectional, Uploads: 2}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{err: errDelta}, nil
		}

		return &mockEngine{report: okReport}, nil
	}

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 2)

	// Find each drive's report by canonical ID.
	var failReport, okDriveReport *synctypes.DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			failReport = reports[i]
		} else {
			okDriveReport = reports[i]
		}
	}

	require.NotNil(t, failReport)
	require.ErrorIs(t, failReport.Err, errDelta)
	assert.Nil(t, failReport.Report)

	require.NotNil(t, okDriveReport)
	require.NoError(t, okDriveReport.Err)
	assert.Equal(t, 2, okDriveReport.Report.Uploads)
}

// Validates: R-2.4, R-6.8
func TestRunOnce_PanicRecovery(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:panic@example.com", "Panicking")
	rd2 := testResolvedDrive(t, "personal:stable@example.com", "Stable")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	stableReport := &synctypes.SyncReport{Mode: synctypes.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{shouldPanic: true}, nil
		}

		return &mockEngine{report: stableReport}, nil
	}

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 2)

	var panicReport, stableDriveReport *synctypes.DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			panicReport = reports[i]
		} else {
			stableDriveReport = reports[i]
		}
	}

	require.NotNil(t, panicReport)
	require.Error(t, panicReport.Err)
	assert.Contains(t, panicReport.Err.Error(), "panic")
	assert.Nil(t, panicReport.Report)

	require.NotNil(t, stableDriveReport)
	require.NoError(t, stableDriveReport.Err)
	assert.Equal(t, 1, stableDriveReport.Report.Downloads)
}

// Validates: R-2.8.5
func TestPrepareDriveWork_ThreadsWebsocketConfig(t *testing.T) {
	rd := testResolvedDrive(t, "personal:websocket@example.com", "Websocket")
	rd.Websocket = true
	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	var captured *synctypes.EngineConfig
	orch.engineFactory = func(_ context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
		captured = ecfg
		return &mockEngine{report: &synctypes.SyncReport{}}, nil
	}

	work := orch.prepareDriveWork(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, work, 1)

	_, err := work[0].fn(t.Context())
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.EnableWebsocket)
	assert.NotNil(t, captured.SocketIOFetcher)
}

// Validates: R-2.8.5
func TestStartWatchRunner_ThreadsWebsocketConfig(t *testing.T) {
	rd := testResolvedDrive(t, "personal:watch-websocket@example.com", "WatchWebsocket")
	rd.Websocket = true
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	var captured *synctypes.EngineConfig
	started := make(chan struct{}, 1)
	orch.engineFactory = func(_ context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
		captured = ecfg
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
	wr, err := orch.startWatchRunner(ctx, rd, synctypes.SyncDownloadOnly, synctypes.WatchOpts{})
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.EnableWebsocket)
	assert.NotNil(t, captured.SocketIOFetcher)
	assert.Equal(t, rd.RootItemID, captured.RootItemID)
	assert.Equal(t, rd.CanonicalID.Email(), captured.AccountEmail)

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

// Validates: R-2.4
func TestRunOnce_ContextCanceled(t *testing.T) {
	rd := testResolvedDrive(t, "personal:cancel@example.com", "Cancel")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{err: context.Canceled}, nil
	}

	reports := orch.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 1)
	assert.ErrorIs(t, reports[0].Err, context.Canceled)
}

// Validates: R-2.4
func TestRunOnce_EngineFactoryError(t *testing.T) {
	rd := testResolvedDrive(t, "personal:factory-err@example.com", "FactoryErr")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return nil, errors.New("db init failed")
	}

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 1)
	require.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "db init failed")
}

// Validates: R-6.10.7
func TestRunOnce_EngineFactoryError_IsolatesAffectedDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:storefail@example.com", "StoreFail")
	rd2 := testResolvedDrive(t, "personal:healthy@example.com", "Healthy")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	okReport := &synctypes.SyncReport{Mode: synctypes.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, ecfg *synctypes.EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return nil, errors.New("open sync store: corrupted database")
		}

		return &mockEngine{report: okReport}, nil
	}

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 2)

	var failedReport, healthyReport *synctypes.DriveReport
	for i := range reports {
		switch reports[i].CanonicalID {
		case rd1.CanonicalID:
			failedReport = reports[i]
		case rd2.CanonicalID:
			healthyReport = reports[i]
		}
	}

	require.NotNil(t, failedReport)
	require.Error(t, failedReport.Err)
	assert.Contains(t, failedReport.Err.Error(), "open sync store")
	assert.Nil(t, failedReport.Report)

	require.NotNil(t, healthyReport)
	require.NoError(t, healthyReport.Err)
	require.NotNil(t, healthyReport.Report)
	assert.Equal(t, 1, healthyReport.Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_TokenError_ReportsPerDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:notoken@example.com", "NoToken")
	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("token file not found")
	}

	orch := NewOrchestrator(cfg)

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 1)
	require.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "token")
}

// --- zero DriveID ---

// Validates: R-2.4
func TestRunOnce_ZeroDriveID_ReportsError(t *testing.T) {
	cid := testCanonicalID(t, "personal:zero-id@example.com")

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: "ZeroID",
		SyncDir:     t.TempDir(),
		// DriveID intentionally zero — should produce an error, not trigger discovery.
	}
	require.True(t, rd.DriveID.IsZero())

	cfg := testOrchestratorConfig(t, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	reports := orch.RunOnce(t.Context(), synctypes.SyncBidirectional, synctypes.RunOpts{})
	require.Len(t, reports, 1)
	require.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "drive ID not resolved")
	assert.Contains(t, reports[0].Err.Error(), "login")
}

// Validates: R-2.9.1
func TestRunOnce_ControlSocketBlocksWatchOwner(t *testing.T) {
	rd := testResolvedDrive(t, "personal:owner-lock@example.com", "OwnerLock")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	started := make(chan struct{})
	release := make(chan struct{})

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runOnceFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.RunOpts) (*synctypes.SyncReport, error) {
				close(started)
				select {
				case <-release:
					return &synctypes.SyncReport{Mode: synctypes.SyncBidirectional}, nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reportsCh := make(chan []*synctypes.DriveReport, 1)
	go func() {
		reportsCh <- orch.RunOnce(ctx, synctypes.SyncBidirectional, synctypes.RunOpts{})
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunOnce did not start in time")
	}
	status := getControlStatus(t, cfg.ControlSocketPath)
	assert.Equal(t, synccontrol.OwnerModeOneShot, status.OwnerMode)

	watchErr := orch.RunWatch(t.Context(), synctypes.SyncBidirectional, synctypes.WatchOpts{})
	require.Error(t, watchErr)
	var inUse *ControlSocketInUseError
	require.ErrorAs(t, watchErr, &inUse)

	close(release)
	select {
	case reports := <-reportsCh:
		require.Len(t, reports, 1)
		require.NoError(t, reports[0].Err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunOnce did not stop in time")
	}
}

// --- mockEngine ---

// mockEngine implements engineRunner for unit tests.
type mockEngine struct {
	report      *synctypes.SyncReport
	err         error
	shouldPanic bool
	closed      bool
	runOnceFn   func(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) (*synctypes.SyncReport, error)
	runWatchFn  func(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error
}

func (m *mockEngine) RunOnce(ctx context.Context, mode synctypes.SyncMode, opts synctypes.RunOpts) (*synctypes.SyncReport, error) {
	if m.shouldPanic {
		panic("mock engine panic")
	}
	if m.runOnceFn != nil {
		return m.runOnceFn(ctx, mode, opts)
	}

	return m.report, m.err
}

func (m *mockEngine) RunWatch(ctx context.Context, mode synctypes.SyncMode, opts synctypes.WatchOpts) error {
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

// --- RunWatch ---

// Validates: R-2.4
func TestOrchestrator_RunWatch_SingleDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:watch1@example.com", "Watch1")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	watchStarted := make(chan struct{})

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
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
func TestOrchestrator_RunWatch_MultiDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:multi1@example.com", "Multi1")
	rd2 := testResolvedDrive(t, "personal:multi2@example.com", "Multi2")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1, rd2)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait until both drives have started.
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

// Validates: R-2.9.1, R-2.9.2
func TestOrchestrator_ControlSocket_StatusAndStop(t *testing.T) {
	rd := testResolvedDrive(t, "personal:control@example.com", "Control")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	watchStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(t.Context(), synctypes.SyncBidirectional, synctypes.WatchOpts{})
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
	assert.Equal(t, []string{rd.CanonicalID.String()}, status.Drives)

	stop := postControlJSON(t, cfg.ControlSocketPath, synccontrol.PathStop, nil)
	assert.Equal(t, synccontrol.StatusStopping, stop.Status)

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

// Validates: R-2.9.1, R-2.9.3
func TestOrchestrator_OneShotControlSocket_StatusAndMutationConflict(t *testing.T) {
	rd := testResolvedDrive(t, "personal:oneshot@example.com", "OneShot")
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
	assert.Equal(t, []string{rd.CanonicalID.String()}, status.Drives)
	assert.Zero(t, status.PendingHeldDeleteApprovals)
	assert.Zero(t, status.PendingConflictRequests)
	assert.Zero(t, status.ResolvingConflictRequests)
	assert.Zero(t, status.FailedConflictRequests)

	client := controlTestClient(cfg.ControlSocketPath)
	req, err := http.NewRequestWithContext(
		t.Context(),
		http.MethodPost,
		synccontrol.HTTPBaseURL+synccontrol.HeldDeletesApprovePath(rd.CanonicalID.String()),
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

// Validates: R-2.3.5, R-2.3.6, R-2.9.3
func TestOrchestrator_ControlSocket_QueuesDurableUserIntent(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	rd := testResolvedDrive(t, "personal:intent@example.com", "Intent")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	seedControlSocketIntentStore(t, rd)

	orch := NewOrchestrator(cfg)
	watchStarted := make(chan struct{})
	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
				close(watchStarted)
				<-ctx.Done()
				return ctx.Err()
			},
		}, nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- orch.RunWatch(t.Context(), synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	select {
	case <-watchStarted:
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not start in time")
	}

	approvePath := synccontrol.HeldDeletesApprovePath(rd.CanonicalID.String())
	approve := postControlJSON(t, cfg.ControlSocketPath, approvePath, nil)
	assert.Equal(t, synccontrol.StatusApproved, approve.Status)

	requestPath := synccontrol.ConflictResolutionRequestPath(rd.CanonicalID.String(), "conflict-1")
	request := postControlJSON(t, cfg.ControlSocketPath, requestPath, []byte(`{"resolution":"keep_local"}`))
	assert.Equal(t, synccontrol.Status(syncstore.ConflictRequestQueued), request.Status)

	assertControlStatusCounts(t, cfg.ControlSocketPath)
	assertControlTypedIntentErrors(t, cfg.ControlSocketPath, rd.CanonicalID.String())
	assertDurableIntentStoreUpdated(t, rd)

	stop := postControlJSON(t, cfg.ControlSocketPath, synccontrol.PathStop, nil)
	assert.Equal(t, synccontrol.StatusStopping, stop.Status)

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.Fail(t, "RunWatch did not stop in time")
	}
}

func assertControlStatusCounts(t *testing.T, socketPath string) {
	t.Helper()

	status := getControlStatus(t, socketPath)
	assert.Equal(t, 1, status.PendingHeldDeleteApprovals)
	assert.Equal(t, 1, status.PendingConflictRequests)
	assert.Zero(t, status.ResolvingConflictRequests)
	assert.Zero(t, status.FailedConflictRequests)
}

func assertControlTypedIntentErrors(t *testing.T, socketPath, cid string) {
	t.Helper()

	unknownDrive := postControlJSONStatus(
		t,
		socketPath,
		synccontrol.HeldDeletesApprovePath("personal:unknown@example.com"),
		nil,
		http.StatusNotFound,
	)
	assert.Equal(t, synccontrol.ErrorDriveNotManaged, unknownDrive.Code)

	invalidResolution := postControlJSONStatus(
		t,
		socketPath,
		synccontrol.ConflictResolutionRequestPath(cid, "conflict-1"),
		[]byte(`{"resolution":"not_a_strategy"}`),
		http.StatusBadRequest,
	)
	assert.Equal(t, synccontrol.ErrorInvalidResolution, invalidResolution.Code)

	missingConflict := postControlJSONStatus(
		t,
		socketPath,
		synccontrol.ConflictResolutionRequestPath(cid, "missing-conflict"),
		[]byte(`{"resolution":"keep_local"}`),
		http.StatusNotFound,
	)
	assert.Equal(t, synccontrol.ErrorConflictNotFound, missingConflict.Code)

	differentStrategy := postControlJSONStatus(
		t,
		socketPath,
		synccontrol.ConflictResolutionRequestPath(cid, "conflict-1"),
		[]byte(`{"resolution":"keep_remote"}`),
		http.StatusConflict,
	)
	assert.Equal(t, synccontrol.ErrorConflictDifferentStrategy, differentStrategy.Code)
}

func assertDurableIntentStoreUpdated(t *testing.T, rd *config.ResolvedDrive) {
	t.Helper()

	reopened, err := syncstore.NewSyncStore(t.Context(), rd.StatePath(), slog.Default())
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, reopened.Close(context.Background()))
	})

	held, err := reopened.ListHeldDeletesByState(t.Context(), synctypes.HeldDeleteStateHeld)
	require.NoError(t, err)
	assert.Empty(t, held)
	approved, err := reopened.ListHeldDeletesByState(t.Context(), synctypes.HeldDeleteStateApproved)
	require.NoError(t, err)
	require.Len(t, approved, 1)

	conflict, err := reopened.GetConflictRequest(t.Context(), "conflict-1")
	require.NoError(t, err)
	assert.Equal(t, synctypes.ConflictStateResolutionRequested, conflict.State)
	assert.Equal(t, synctypes.ResolutionKeepLocal, conflict.RequestedResolution)
}

func seedControlSocketIntentStore(t *testing.T, rd *config.ResolvedDrive) {
	t.Helper()

	store, err := syncstore.NewSyncStore(t.Context(), rd.StatePath(), slog.Default())
	require.NoError(t, err)
	require.NoError(t, store.UpsertHeldDeletes(t.Context(), []synctypes.HeldDeleteRecord{{
		DriveID:       rd.DriveID,
		ItemID:        "item-delete",
		Path:          "delete-me.txt",
		ActionType:    synctypes.ActionRemoteDelete,
		State:         synctypes.HeldDeleteStateHeld,
		HeldAt:        1,
		LastPlannedAt: 1,
		LastError:     "held",
	}}))
	_, err = store.DB().ExecContext(t.Context(), `INSERT INTO conflicts
		(id, drive_id, item_id, path, conflict_type, detected_at, resolution)
		VALUES ('conflict-1', ?, 'item-conflict', 'conflict.txt', 'edit_edit', 1, 'unresolved')`,
		rd.DriveID.String(),
	)
	require.NoError(t, err)
	require.NoError(t, store.Close(t.Context()))
}

// Validates: R-2.4
func TestOrchestrator_Reload_AddDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:existing@example.com", "Existing")
	rd2CID := driveid.MustCanonicalID("personal:added@example.com")

	// XDG isolation so buildResolvedDrive finds drive metadata during reload.
	setupXDGIsolation(t, rd1.CanonicalID, rd2CID)

	cfgPath := writeTestConfig(t, rd1.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait for first drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Add a second drive to the config and request a control-socket reload.
	writeTestConfigMulti(t, cfgPath, rd1.CanonicalID, rd1.SyncDir, rd2CID, t.TempDir())
	postControlReload(t, cfg.ControlSocketPath)

	// Wait for the second drive to start.
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
func TestOrchestrator_Reload_RemoveDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:keep@example.com", "Keep")
	rd2 := testResolvedDrive(t, "personal:remove@example.com", "Remove")
	cfgPath := writeTestConfig(t, rd1.CanonicalID, rd2.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd1, rd2)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait for both drives to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 2
	}, 5*time.Second, 10*time.Millisecond)

	// Remove rd2 from config and request a control-socket reload.
	writeTestConfigSingle(t, cfgPath, rd1.CanonicalID, rd1.SyncDir)
	postControlReload(t, cfg.ControlSocketPath)

	// Wait for one runner to stop (the removed drive).
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
func TestOrchestrator_Reload_PausedDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:pausetest@example.com", "PauseTest")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Pause the drive and request a control-socket reload.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	postControlReload(t, cfg.ControlSocketPath)

	// The drive runner should stop.
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
	rd := testResolvedDrive(t, "personal:invalidcfg@example.com", "InvalidCfg")
	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Write invalid TOML and request a control-socket reload; old state should remain.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	postControlReload(t, cfg.ControlSocketPath)

	// Give reload time to process — the drive should still be running.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(1), started.Load(), "drive should still be running after invalid config reload")

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
	rd := testResolvedDrive(t, "personal:timedpause@example.com", "TimedPause")

	// XDG isolation — defensive, so buildResolvedDrive during reload can
	// find metadata if needed.
	setupXDGIsolation(t, rd.CanonicalID)

	cfgPath := writeTestConfig(t, rd.CanonicalID)
	cfg := testOrchestratorConfigWithPath(t, cfgPath, rd)
	cfg.ControlSocketPath = shortControlSocketPath(t)
	cfg.Provider.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)

	var started atomic.Int32
	var stopped atomic.Int32

	orch.engineFactory = func(_ context.Context, _ *synctypes.EngineConfig) (engineRunner, error) {
		return &mockEngine{
			runWatchFn: func(ctx context.Context, _ synctypes.SyncMode, _ synctypes.WatchOpts) error {
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
		errCh <- orch.RunWatch(ctx, synctypes.SyncBidirectional, synctypes.WatchOpts{})
	}()

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Set an already-expired timed pause and request a control-socket reload.
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused", "true"))
	require.NoError(t, config.SetDriveKey(cfgPath, rd.CanonicalID, "paused_until", "2000-01-01T00:00:00Z"))
	postControlReload(t, cfg.ControlSocketPath)

	// The expired timed pause is cleared by ClearExpiredPauses during reload.
	// The drive is already running and remains in newActive, so it is NOT
	// stopped and NOT restarted — avoiding unnecessary downtime.
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), stopped.Load(), "drive should NOT be stopped — expired pause is cleared, drive stays running")
	assert.Equal(t, int32(1), started.Load(), "drive should NOT be restarted — it was already running")

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
func TestOrchestrator_RunWatch_ZeroDrives(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	err := orch.RunWatch(t.Context(), synctypes.SyncBidirectional, synctypes.WatchOpts{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no drives")
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
