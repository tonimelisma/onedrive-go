package sync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// --- helpers ---

func testOrchestratorConfig(t *testing.T, drives ...*config.ResolvedDrive) *OrchestratorConfig {
	t.Helper()

	return &OrchestratorConfig{
		Config:       config.DefaultConfig(),
		Drives:       drives,
		ConfigPath:   "/tmp/test-config.toml",
		MetaHTTP:     &http.Client{},
		TransferHTTP: &http.Client{},
		UserAgent:    "test/1.0",
		Logger:       slog.Default(),
	}
}

func testResolvedDrive(t *testing.T, cidStr, displayName string) *config.ResolvedDrive {
	t.Helper()

	cid := testCanonicalID(t, cidStr)

	return &config.ResolvedDrive{
		CanonicalID: cid,
		DisplayName: displayName,
		SyncDir:     t.TempDir(),
	}
}

// stubTokenSource implements graph.TokenSource for tests.
type stubTokenSource struct{}

func (s *stubTokenSource) Token() (string, error) { return "test-token", nil }

// --- NewOrchestrator ---

func TestNewOrchestrator_ReturnsNonNil(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	require.NotNil(t, orch)
	assert.Equal(t, cfg, orch.cfg)
	assert.NotNil(t, orch.clients)
	assert.NotNil(t, orch.logger)
}

func TestNewOrchestrator_DefaultFactories(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	// engineFactory and tokenSourceFn are set to defaults (non-nil).
	assert.NotNil(t, orch.engineFactory)
	assert.NotNil(t, orch.tokenSourceFn)
}

// --- getOrCreateClient ---

func TestGetOrCreateClient_SamePathReturnsSamePointer(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	pair1, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)
	require.NotNil(t, pair1)

	pair2, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)

	// Same token path should yield the exact same pointer.
	assert.Same(t, pair1, pair2)
}

func TestGetOrCreateClient_DifferentPathReturnsDifferentPointer(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	pair1, err := orch.getOrCreateClient(context.Background(), "/tmp/token_a.json")
	require.NoError(t, err)

	pair2, err := orch.getOrCreateClient(context.Background(), "/tmp/token_b.json")
	require.NoError(t, err)

	assert.NotSame(t, pair1, pair2)
}

func TestGetOrCreateClient_TokenError(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("not logged in")
	}

	pair, err := orch.getOrCreateClient(context.Background(), "/tmp/no-token.json")
	assert.Error(t, err)
	assert.Nil(t, pair)
	assert.Contains(t, err.Error(), "not logged in")
}

// --- RunOnce ---

func TestRunOnce_ZeroDrives(t *testing.T) {
	cfg := testOrchestratorConfig(t)
	orch := NewOrchestrator(cfg)

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	assert.Empty(t, reports)
}

func TestRunOnce_OneDrive_Success(t *testing.T) {
	rd := testResolvedDrive(t, "personal:test@example.com", "Test")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	expectedReport := &SyncReport{
		Mode:      SyncBidirectional,
		Downloads: 5,
	}

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{report: expectedReport}, nil
	}

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Equal(t, rd.CanonicalID, reports[0].CanonicalID)
	assert.Equal(t, "Test", reports[0].DisplayName)
	assert.NoError(t, reports[0].Err)
	require.NotNil(t, reports[0].Report)
	assert.Equal(t, 5, reports[0].Report.Downloads)
}

func TestRunOnce_TwoDrives_OneFailsOneSucceeds(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:fail@example.com", "Failing")
	rd2 := testResolvedDrive(t, "personal:ok@example.com", "Working")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	errDelta := errors.New("delta gone")
	okReport := &SyncReport{Mode: SyncBidirectional, Uploads: 2}

	orch.engineFactory = func(ecfg *EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{err: errDelta}, nil
		}

		return &mockEngine{report: okReport}, nil
	}

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	// RunOnce itself does not error — individual drives report their own errors.
	assert.NoError(t, err)
	require.Len(t, reports, 2)

	// Find each drive's report by canonical ID.
	var failReport, okDriveReport *DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			failReport = reports[i]
		} else {
			okDriveReport = reports[i]
		}
	}

	require.NotNil(t, failReport)
	assert.ErrorIs(t, failReport.Err, errDelta)
	assert.Nil(t, failReport.Report)

	require.NotNil(t, okDriveReport)
	assert.NoError(t, okDriveReport.Err)
	assert.Equal(t, 2, okDriveReport.Report.Uploads)
}

func TestRunOnce_PanicRecovery(t *testing.T) {
	rd1 := testResolvedDrive(t, "personal:panic@example.com", "Panicking")
	rd2 := testResolvedDrive(t, "personal:stable@example.com", "Stable")
	cfg := testOrchestratorConfig(t, rd1, rd2)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	stableReport := &SyncReport{Mode: SyncBidirectional, Downloads: 1}

	orch.engineFactory = func(ecfg *EngineConfig) (engineRunner, error) {
		if ecfg.SyncRoot == rd1.SyncDir {
			return &mockEngine{shouldPanic: true}, nil
		}

		return &mockEngine{report: stableReport}, nil
	}

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	require.Len(t, reports, 2)

	var panicReport, stableDriveReport *DriveReport
	for i := range reports {
		if reports[i].CanonicalID == rd1.CanonicalID {
			panicReport = reports[i]
		} else {
			stableDriveReport = reports[i]
		}
	}

	require.NotNil(t, panicReport)
	assert.Error(t, panicReport.Err)
	assert.Contains(t, panicReport.Err.Error(), "panic")
	assert.Nil(t, panicReport.Report)

	require.NotNil(t, stableDriveReport)
	assert.NoError(t, stableDriveReport.Err)
	assert.Equal(t, 1, stableDriveReport.Report.Downloads)
}

func TestRunOnce_ContextCanceled(t *testing.T) {
	rd := testResolvedDrive(t, "personal:cancel@example.com", "Cancel")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return &mockEngine{err: context.Canceled}, nil
	}

	reports, err := orch.RunOnce(ctx, SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	require.Len(t, reports, 1)
	assert.ErrorIs(t, reports[0].Err, context.Canceled)
}

func TestRunOnce_EngineFactoryError(t *testing.T) {
	rd := testResolvedDrive(t, "personal:factory-err@example.com", "FactoryErr")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return &stubTokenSource{}, nil
	}

	orch.engineFactory = func(_ *EngineConfig) (engineRunner, error) {
		return nil, errors.New("db init failed")
	}

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "db init failed")
}

func TestRunOnce_TokenError_ReportsPerDrive(t *testing.T) {
	rd := testResolvedDrive(t, "personal:notoken@example.com", "NoToken")
	cfg := testOrchestratorConfig(t, rd)
	orch := NewOrchestrator(cfg)
	orch.tokenSourceFn = func(_ context.Context, _ string, _ *slog.Logger) (graph.TokenSource, error) {
		return nil, errors.New("token file not found")
	}

	reports, err := orch.RunOnce(context.Background(), SyncBidirectional, RunOpts{})
	assert.NoError(t, err)
	require.Len(t, reports, 1)
	assert.Error(t, reports[0].Err)
	assert.Contains(t, reports[0].Err.Error(), "token")
}

// --- mockEngine ---

// mockEngine implements engineRunner for unit tests.
type mockEngine struct {
	report      *SyncReport
	err         error
	shouldPanic bool
	closed      bool
}

func (m *mockEngine) RunOnce(_ context.Context, _ SyncMode, _ RunOpts) (*SyncReport, error) {
	if m.shouldPanic {
		panic("mock engine panic")
	}

	return m.report, m.err
}

func (m *mockEngine) Close() error {
	m.closed = true
	return nil
}
