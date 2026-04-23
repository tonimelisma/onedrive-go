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
// records for each CID. This gives buildResolvedDrive (called during
// control-socket reload → ResolveDrives) a non-zero DriveID, which is required
// for Session() to succeed.
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
	provider := driveops.NewSessionRuntime(holder, "test/1.0", slog.Default())

	return &OrchestratorConfig{
		Holder:  holder,
		Drives:  drives,
		Runtime: provider,
		Logger:  slog.Default(),
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

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	assert.Empty(t, result.Reports)
	assert.Empty(t, result.Startup.Results)
}

// Validates: R-2.4
func TestRunOnce_OneDrive_Success(t *testing.T) {
	rd := testResolvedDrive(t, "personal:test@example.com", "Test")
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
	assert.Equal(t, DriveStartupRunnable, result.Startup.Results[0].Status)
	require.Len(t, result.Reports, 1)
	assert.Equal(t, rd.CanonicalID, result.Reports[0].CanonicalID)
	assert.Equal(t, "Test", result.Reports[0].DisplayName)
	require.NoError(t, result.Reports[0].Err)
	require.NotNil(t, result.Reports[0].Report)
	assert.Equal(t, 5, result.Reports[0].Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_TwoDrives_OneFailsOneSucceeds(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:fail@example.com", "Failing")
	rd2 := testResolvedDrive(t, "personal:ok@example.com", "Working")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	errDelta := errors.New("delta gone")
	okReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Uploads: 2}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Drive.SyncDir == rd1.SyncDir {
			return &mockEngine{err: errDelta}, nil
		}

		return &mockEngine{report: okReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 2)

	// Find each drive's report by canonical ID.
	var failReport, okDriveReport *DriveReport
	for i := range result.Reports {
		if result.Reports[i].CanonicalID == rd1.CanonicalID {
			failReport = result.Reports[i]
		} else {
			okDriveReport = result.Reports[i]
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
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	stableReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Drive.SyncDir == rd1.SyncDir {
			return &mockEngine{shouldPanic: true}, nil
		}

		return &mockEngine{report: stableReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 2)

	var panicReport, stableDriveReport *DriveReport
	for i := range result.Reports {
		if result.Reports[i].CanonicalID == rd1.CanonicalID {
			panicReport = result.Reports[i]
		} else {
			stableDriveReport = result.Reports[i]
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
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	orch := NewOrchestrator(cfg)
	var captured *engineFactoryRequest
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		captured = &req
		return &mockEngine{report: &syncengine.Report{}}, nil
	}

	work, summary, reports := orch.prepareRunOnceWork(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, work, 1)
	require.Len(t, summary.Results, 1)
	require.Len(t, reports, 1)
	require.Nil(t, reports[0])

	_, err := work[0].work.fn(t.Context())
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.Drive.Websocket)
	assert.True(t, captured.VerifyDrive)
	assert.NotNil(t, captured.Session)
	assert.NotNil(t, captured.Session.Meta)
}

// Validates: R-2.8.5
func TestStartWatchRunner_ThreadsWebsocketConfig(t *testing.T) {
	rd := testResolvedDrive(t, "personal:watch-websocket@example.com", "WatchWebsocket")
	rd.Websocket = true
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
	wr, err := orch.startWatchRunner(ctx, rd, syncengine.SyncDownloadOnly, syncengine.WatchOptions{})
	require.NoError(t, err)
	require.NotNil(t, captured)
	assert.True(t, captured.Drive.Websocket)
	assert.True(t, captured.VerifyDrive)
	assert.NotNil(t, captured.Session)
	assert.NotNil(t, captured.Session.Meta)
	assert.Equal(t, rd.RootItemID, captured.Drive.RootItemID)
	assert.Equal(t, rd.CanonicalID.Email(), captured.Drive.CanonicalID.Email())

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
	rd := testResolvedDrive(t, "personal:factory-err@example.com", "FactoryErr")
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
func TestRunOnce_EngineFactoryError_IsolatesAffectedDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:storefail@example.com", "StoreFail")
	rd2 := testResolvedDrive(t, "personal:healthy@example.com", "Healthy")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	cfg.Runtime.TokenSourceFn = stubTokenSourceFn

	okReport := &syncengine.Report{Mode: syncengine.SyncBidirectional, Downloads: 1}

	orch := NewOrchestrator(cfg)
	orch.engineFactory = func(_ context.Context, req engineFactoryRequest) (engineRunner, error) {
		if req.Drive.SyncDir == rd1.SyncDir {
			return nil, errors.New("open sync store: corrupted database")
		}

		return &mockEngine{report: okReport}, nil
	}

	result := orch.RunOnce(t.Context(), syncengine.SyncBidirectional, syncengine.RunOptions{})
	require.Len(t, result.Startup.Results, 2)
	require.Len(t, result.Reports, 1)

	var failedResult *DriveStartupResult
	var healthyReport *DriveReport
	for i := range result.Startup.Results {
		if result.Startup.Results[i].CanonicalID == rd1.CanonicalID {
			failedResult = &result.Startup.Results[i]
		}
	}
	for i := range result.Reports {
		if result.Reports[i].CanonicalID == rd2.CanonicalID {
			healthyReport = result.Reports[i]
		}
	}

	require.NotNil(t, failedResult)
	require.Error(t, failedResult.Err)
	assert.Contains(t, failedResult.Err.Error(), "open sync store")
	assert.Equal(t, DriveStartupFatal, failedResult.Status)

	require.NotNil(t, healthyReport)
	require.NoError(t, healthyReport.Err)
	require.NotNil(t, healthyReport.Report)
	assert.Equal(t, 1, healthyReport.Report.Downloads)
}

// Validates: R-2.4
func TestRunOnce_TokenError_ReportsPerDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:notoken@example.com", "NoToken")
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

	rd := &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: "ZeroID",
		SyncDir:     t.TempDir(),
		// DriveID intentionally zero — should produce an error, not trigger discovery.
	}
	require.True(t, rd.DriveID.IsZero())

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
	rd := testResolvedDrive(t, "personal:owner-lock@example.com", "OwnerLock")
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

// --- mockEngine ---

// mockEngine implements engineRunner for unit tests.
type mockEngine struct {
	report      *syncengine.Report
	err         error
	shouldPanic bool
	closed      bool
	runOnceFn   func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.RunOptions) (*syncengine.Report, error)
	runWatchFn  func(ctx context.Context, mode syncengine.SyncMode, opts syncengine.WatchOptions) error
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

// --- RunWatch ---

// Validates: R-2.4
func TestOrchestrator_RunWatch_SingleDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:watch1@example.com", "Watch1")
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
func TestOrchestrator_RunWatch_MultiDrive(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:multi1@example.com", "Multi1")
	rd2 := testResolvedDrive(t, "personal:multi2@example.com", "Multi2")
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

func TestOrchestrator_RunWatch_SkipsIncompatibleStoreDriveWhenAnotherDriveStarts(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:healthy@example.com", "Healthy")
	rd2 := testResolvedDrive(t, "personal:reset@example.com", "Reset")
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
		if req.Drive.CanonicalID == rd2.CanonicalID {
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
		require.Fail(t, "RunWatch did not start healthy drive in time")
	}

	select {
	case warning := <-warnings:
		results := warning.Summary.SkippedResults()
		require.Len(t, results, 1)
		assert.Equal(t, rd2.CanonicalID, results[0].CanonicalID)
		assert.Equal(t, DriveStartupIncompatibleStore, results[0].Status)
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

func TestOrchestrator_RunWatch_ReturnsStartupFailureWhenNoDriveStarts(t *testing.T) {
	rd := testResolvedDrive(t, "personal:reset@example.com", "Reset")
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
	assert.Equal(t, rd.CanonicalID, startupErr.Summary.Results[0].CanonicalID)
	assert.Equal(t, DriveStartupIncompatibleStore, startupErr.Summary.Results[0].Status)
}

func TestOrchestrator_RunWatch_ReturnsErrorWhenAllDrivesPaused(t *testing.T) {
	rd := testResolvedDrive(t, "personal:paused@example.com", "Paused")
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

// Validates: R-2.9.1, R-2.9.2
func TestOrchestrator_ControlSocket_StatusAndStop(t *testing.T) {
	rd := testResolvedDrive(t, "personal:control@example.com", "Control")
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

// Validates: R-2.9.1
func TestOrchestrator_OneShotControlSocket_StatusAndRejectsNonStatus(t *testing.T) {
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
	rd1 := testResolvedDrive(t, "personal:existing@example.com", "Existing")
	rd2CID := driveid.MustCanonicalID("personal:added@example.com")

	// XDG isolation so buildResolvedDrive finds catalog drive identity during reload.
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

	// Wait for drive to start.
	require.Eventually(t, func() bool {
		return started.Load() >= 1
	}, 5*time.Second, 10*time.Millisecond)

	// Write invalid TOML and request a control-socket reload; old state should remain.
	require.NoError(t, os.WriteFile(cfgPath, []byte("{{invalid toml"), 0o600))
	postControlReload(t, cfg.ControlSocketPath)

	assert.Never(t, func() bool {
		return started.Load() > 1
	}, 200*time.Millisecond, 10*time.Millisecond, "drive should still be running after invalid config reload")
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

	// Wait for drive to start.
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

	err := orch.RunWatch(t.Context(), syncengine.SyncBidirectional, syncengine.WatchOptions{})
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
