package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func newSingleOwnerEngine(t *testing.T) *testEngine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.clock = newManualClock(time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC))
	setupWatchEngine(t, eng)

	return eng
}

// ---------------------------------------------------------------------------
// Composite mock implementing DeltaFetcher + ItemClient + Downloader + Uploader
//
// Engine requires all 4 interfaces (unlike Executor, which takes them
// individually), so a single mock is pragmatic here. Executor tests split
// mocks by interface because each test exercises only 1-2 interfaces.
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ DeltaFetcher            = (*engineMockClient)(nil)
	_ SocketIOEndpointFetcher = (*engineMockClient)(nil)
	_ ItemClient              = (*engineMockClient)(nil)
	_ driveops.Downloader     = (*engineMockClient)(nil)
	_ driveops.Uploader       = (*engineMockClient)(nil)
	_ driveops.ItemUploader   = (*engineMockClient)(nil)
	_ FolderDeltaFetcher      = (*engineMockClient)(nil)
	_ RecursiveLister         = (*engineMockClient)(nil)
	_ PermissionChecker       = (*engineMockClient)(nil)
	_ DriveVerifier           = (*engineMockClient)(nil)
)

// The transfer manager type-asserts the capability, so losing it would fail
// only the lookups that need it, at run time.
var _ driveops.ItemReader = (*engineMockClient)(nil)

const (
	engineTestEmail = "sync-user@example.com"
)

func testThrottleDriveID() driveid.ID {
	return driveid.New(engineTestDriveID)
}

func testThrottleScope() ScopeKey {
	return SKThrottleDrive(testThrottleDriveID())
}

type testEngine struct {
	*Engine
	runtime *watchRuntime
	flow    *engineFlow
}

type enginePathConvergenceStub struct{}

func (s *enginePathConvergenceStub) WaitPathVisible(_ context.Context, _ string) (*graph.Item, error) {
	return &graph.Item{ID: "visible-item-id"}, nil
}

func (s *enginePathConvergenceStub) DeleteResolvedPath(_ context.Context, _, _ string) error {
	return nil
}

func (s *enginePathConvergenceStub) PermanentDeleteResolvedPath(_ context.Context, _, _ string) error {
	return nil
}

func newFlowBackedTestEngine(engine *Engine) *testEngine {
	flow := newEngineFlow(engine)

	return &testEngine{
		Engine: engine,
		flow:   flow,
	}
}

// newTestEngine creates an Engine backed by a temp dir with real SQLite
// and the given mock client. Returns the engine and sync root path.
func newTestEngine(t *testing.T, mock *engineMockClient) (*testEngine, string) {
	t.Helper()

	return newTestEngineWithContext(t, t.Context(), mock)
}

// newTestEngineWithContext is like newTestEngine but lets callers thread a
// specific context through engine construction and cleanup-sensitive helpers.
func newTestEngineWithContext(t *testing.T, ctx context.Context, mock *engineMockClient) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDir(t, ctx, mock, t.TempDir())
}

// newTestEngineInDir builds a test engine on tmpDir's data, test.db and sync,
// creating whichever are missing: a fresh directory gives a fresh engine, one
// an earlier engine left behind reopens its state (crash recovery tests).
func newTestEngineInDir(t *testing.T, ctx context.Context, mock *engineMockClient, tmpDir string) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, ctx, mock, tmpDir, nil)
}

// newTestEngineOnFS builds a test engine whose sync tree runs through
// interpose: an emulated filesystem (internal/synctest, D-094).
func newTestEngineOnFS(
	t *testing.T, mock *engineMockClient, interpose func(synctree.Handle) synctree.Handle,
) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.interposeSyncTree = interpose
	})
}

// newTestEngineInDirWith is newTestEngineInDir with a last word on the
// engine's inputs.
func newTestEngineInDirWith(
	t *testing.T, ctx context.Context, mock *engineMockClient, tmpDir string, configure func(*engineInputs),
) (*testEngine, string) {
	t.Helper()

	dataDir := filepath.Join(tmpDir, "data")
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")
	require.NoError(t, os.MkdirAll(dataDir, 0o750), "creating test data dir")

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)
	accountEmail := engineTestEmail
	accountCID := testEngineAccountCanonicalID()

	inputs := &engineInputs{
		DBPath:             dbPath,
		SyncRoot:           syncRoot,
		DataDir:            dataDir,
		DriveID:            driveID,
		AccountEmail:       accountEmail,
		AccountCanonicalID: accountCID,
		Fetcher:            mock,
		SocketIOFetcher:    mock,
		Items:              mock,
		Downloads:          mock,
		Uploads:            mock,
		PathConvergence:    &enginePathConvergenceStub{},
		FolderDelta:        mock,
		RecursiveLister:    mock,
		PermChecker:        mock,
		Logger:             logger,
	}
	if configure != nil {
		configure(inputs)
	}
	eng, err := newEngine(ctx, inputs)
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
	applyRegisteredDispatchOrder(t, eng)
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   flow,
	}
	seedTestEngineAccount(t, dataDir, inputs.AccountCanonicalID)

	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(context.WithoutCancel(ctx)), "Engine.Close")
	})

	return testEng, syncRoot
}

// testEngineAccountCanonicalID is the personal account a test engine syncs
// with, at engineTestEmail.
func testEngineAccountCanonicalID() driveid.CanonicalID {
	return driveid.MustCanonicalID(driveid.DriveTypePersonal + ":" + engineTestEmail)
}

// seedTestEngineAccount records the test engine's account in the catalog of
// dataDir, without a sign-in requirement; an engine with no account records
// none.
func seedTestEngineAccount(t *testing.T, dataDir string, account driveid.CanonicalID) {
	t.Helper()

	if account.IsZero() {
		return
	}
	require.NoError(t, config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		record := config.CatalogAccount{
			CanonicalID: account.String(),
			Email:       account.Email(),
			DriveType:   account.DriveType(),
		}
		catalog.UpsertAccount(&record)
		return nil
	}))
}

// newTestEngineWithLogger is like newTestEngine but accepts a custom logger
// for tests that need to capture or filter log output.
func newTestEngineWithLogger(t *testing.T, mock *engineMockClient, logger *slog.Logger) (*testEngine, string) {
	t.Helper()

	return newTestEngineWithLoggerContext(t, t.Context(), mock, logger)
}

// newTestEngineWithLoggerContext is like newTestEngineWithContext but accepts a
// custom logger for tests that need to capture or filter log output.
func newTestEngineWithLoggerContext(t *testing.T, ctx context.Context, mock *engineMockClient, logger *slog.Logger) (*testEngine, string) {
	t.Helper()

	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")
	require.NoError(t, os.MkdirAll(dataDir, 0o750), "creating test data dir")

	driveID := driveid.New(engineTestDriveID)
	accountEmail := "sync-user@example.com"
	accountCID := testEngineAccountCanonicalID()

	eng, err := newEngine(ctx, &engineInputs{
		DBPath:             dbPath,
		SyncRoot:           syncRoot,
		DataDir:            dataDir,
		DriveID:            driveID,
		AccountEmail:       accountEmail,
		AccountCanonicalID: accountCID,
		Fetcher:            mock,
		SocketIOFetcher:    mock,
		Items:              mock,
		Downloads:          mock,
		Uploads:            mock,
		PathConvergence:    &enginePathConvergenceStub{},
		FolderDelta:        mock,
		RecursiveLister:    mock,
		PermChecker:        mock,
		Logger:             logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
	applyRegisteredDispatchOrder(t, eng)
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   flow,
	}
	seedTestEngineAccount(t, dataDir, accountCID)

	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(context.WithoutCancel(ctx)), "Engine.Close")
	})

	return testEng, syncRoot
}

func newDownloadDeltaMock(driveID driveid.ID, item *graph.Item, token string, content []byte) *engineMockClient {
	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				*item,
			}, token), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}
}

// setupWatchEngine initializes an engine with DepGraph + dispatchCh + watchRuntime
// for processBatch tests. Returns the dispatchCh for reading dispatched actions.
// Replaces the old two-call pattern of setupWatchEngine + newTestWatchState.
func setupWatchEngine(t *testing.T, eng *testEngine) <-chan *TrackedAction {
	t.Helper()

	rt := newWatchRuntime(eng.Engine)
	rt.sched.graph = NewDepGraph(eng.logger)
	rt.sched.dispatchCh = make(chan *TrackedAction, 1024)
	rt.scopes.state = NewScopeState(eng.nowFunc, eng.logger)
	eng.runtime = rt
	eng.flow = rt.engineFlow

	return rt.sched.dispatchCh
}

// newTestWatchState initializes watch state on an engine for testing.
// Creates retryTimerCh, refreshResults, and scope detection state — the
// minimum set needed for watch-mode tests.
func newTestWatchState(t *testing.T, eng *testEngine) {
	t.Helper()

	_ = setupWatchEngine(t, eng)
}

func setTestBlockScope(t *testing.T, eng *testEngine, block *BlockScope) {
	t.Helper()

	require.NotNil(t, block)

	if block.TrialInterval <= 0 {
		block.TrialInterval = time.Minute
	}
	if block.NextTrialAt.IsZero() {
		block.NextTrialAt = eng.nowFunc().Add(block.TrialInterval)
	}
	require.NoError(t, eng.baseline.UpsertBlockScope(context.Background(), block))
	if eng.runtime != nil {
		active := ActiveScopeFromBlockScopeRow(block)
		eng.runtime.scopes.upsertActiveScope(&active)
	}
}

func lookupTestWatchRuntime(eng *testEngine) (*watchRuntime, bool) {
	return eng.runtime, eng.runtime != nil
}

func lookupTestEngineFlow(eng *testEngine) (*engineFlow, bool) {
	if eng.runtime != nil {
		return eng.runtime.engineFlow, true
	}
	if eng.flow != nil {
		return eng.flow, true
	}

	return nil, false
}

func testWatchRuntime(t *testing.T, eng *testEngine) *watchRuntime {
	t.Helper()

	rt, ok := lookupTestWatchRuntime(eng)
	require.True(t, ok, "watch runtime must be initialized for this test")

	return rt
}

func testEngineFlow(t *testing.T, eng *testEngine) *engineFlow {
	t.Helper()

	flow, ok := lookupTestEngineFlow(eng)
	require.True(t, ok, "engine flow must be initialized for this test")

	return flow
}

func attachDebugEventRecorder(eng *testEngine) *debugEventRecorder {
	return attachDebugEventRecorderWithHook(eng, nil)
}

func attachDebugEventRecorderWithHook(
	eng *testEngine,
	hook func(EngineDebugEvent),
) *debugEventRecorder {
	recorder := newDebugEventRecorder()
	eng.SetDebugEventHook(func(event DebugEvent) {
		recorder.record(event)
		if hook != nil {
			hook(event)
		}
	})
	return recorder
}

// installManualClock swaps the engine's clock and refreshes any runtime already
// built from it.
//
// runtimeDeps snapshots the clock when a run starts, which is the right rule
// for production -- a run's capabilities do not change underneath it -- so a
// test that swaps the clock after building the runtime has to say so rather
// than relying on the runtime re-reading the engine.
func installManualClock(eng *Engine, clock *manualClock) {
	eng.clock = clock
}

// errManualClockSkipsScopeWait is what the manual-clock harness answers a
// one-shot scope wait with: no wait, no extra pass, as before the wait existed.
var errManualClockSkipsScopeWait = errors.New("manual clock: scope wait skipped")

// installManualClockForTestEngine is the testEngine-aware form: it also
// refreshes the deps of the runtime and flow the harness already built.
func installManualClockForTestEngine(eng *testEngine, clock *manualClock) {
	installManualClock(eng.Engine, clock)
	eng.baseline.SetNowFunc(clock.Now)
	// A one-shot scope wait is wall-clock time; on a manual clock it would
	// pass without making the trial due. A test that wants the wait sets a
	// waitFn of its own that advances its clock.
	if eng.waitFn == nil {
		eng.waitFn = func(context.Context, time.Duration) error {
			return errManualClockSkipsScopeWait
		}
	}

	if rt, ok := lookupTestWatchRuntime(eng); ok {
		rt.deps = newRuntimeDeps(eng.Engine)
	}
	if flow, ok := lookupTestEngineFlow(eng); ok {
		flow.deps = newRuntimeDeps(eng.Engine)
	}
}

func runFullRemoteRefreshAsyncForTest(t *testing.T, eng *testEngine, ctx context.Context, bl *Baseline) {
	t.Helper()
	testWatchRuntime(t, eng).runFullRemoteRefreshAsync(ctx, bl)
}

func isTestBlockScopeed(eng *testEngine, key ScopeKey) bool {
	if eng.runtime != nil {
		if eng.runtime.scopes.hasActiveScope(key) {
			return true
		}
	}

	blocks, err := eng.baseline.ListBlockScopes(context.Background())
	if err != nil {
		panic(fmt.Sprintf("ListBlockScopes: %v", err))
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return true
		}
	}
	return false
}

func getTestBlockScope(eng *testEngine, key ScopeKey) (BlockScope, bool) {
	if eng.runtime != nil {
		if block, ok := eng.runtime.scopes.lookupActiveScope(key); ok {
			row, err := BlockScopeRowFromActiveScope(block)
			if err != nil {
				panic(fmt.Sprintf("BlockScopeRowFromActiveScope: %v", err))
			}
			return *row, true
		}
	}

	blocks, err := eng.baseline.ListBlockScopes(context.Background())
	if err != nil {
		panic(fmt.Sprintf("ListBlockScopes: %v", err))
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return *blocks[i], true
		}
	}
	return BlockScope{}, false
}

// seedBaseline commits outcomes and an optional delta token to the baseline,
// using per-outcome CommitMutation inputs (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []ActionOutcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		require.NoError(t, mgr.CommitMutation(ctx, MutationFromActionOutcome(&outcomes[i])), "seed CommitMutation[%d]", i)
	}

	if deltaToken != "" {
		require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(engineTestDriveID), deltaToken), "seed CommitObservationCursor")
	}
}

// observeEngineClock wraps the engine's current clock so callers can attach
// hooks, returning the wrapper for further configuration.
func observeEngineClock(eng *Engine) *observingClock {
	if existing, ok := eng.clock.(*observingClock); ok {
		return existing
	}

	wrapped := &observingClock{SyncClock: eng.clock}
	eng.clock = wrapped

	return wrapped
}
