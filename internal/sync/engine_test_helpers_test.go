package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncexec"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

func newSingleOwnerEngine(t *testing.T) *testEngine {
	t.Helper()

	mock := &syncfixture.EngineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.hooks.clock = syncfixture.NewManualClock(time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC))
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
	_ syncdomain.DeltaFetcher            = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.SocketIOEndpointFetcher = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.ItemClient              = (*syncfixture.EngineMockClient)(nil)
	_ driveops.Downloader                = (*syncfixture.EngineMockClient)(nil)
	_ driveops.Uploader                  = (*syncfixture.EngineMockClient)(nil)
	_ driveops.ItemUploader              = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.FolderDeltaFetcher      = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.RecursiveLister         = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.PermissionChecker       = (*syncfixture.EngineMockClient)(nil)
	_ syncdomain.DriveVerifier           = (*syncfixture.EngineMockClient)(nil)
)

// The transfer manager type-asserts the capability, so losing it would fail
// only the lookups that need it, at run time.
var _ driveops.ItemReader = (*syncfixture.EngineMockClient)(nil)

const (
	engineTestEmail = "sync-user@example.com"
)

func testThrottleDriveID() driveid.ID {
	return driveid.New(syncfixture.EngineTestDriveID)
}

func testThrottleScope() syncdomain.ScopeKey {
	return syncdomain.SKThrottleDrive(testThrottleDriveID())
}

type testEngine struct {
	*Engine
	runtime *watchRuntime
	flow    *engineFlow
	// crash receives the engine's executor and store crash points; nil on
	// an engine the test harness did not build.
	crash *crashSwitch
}

// crashSwitch is a test engine's crash hooks, fixed when the engine is built:
// it forwards the executor's and the store's crash points to the hook the
// test armed last, so a test arms a crash after the engine has synced.
type crashSwitch struct {
	exec  atomic.Pointer[func(point string) error]
	store atomic.Pointer[func(point string) error]
}

func (c *crashSwitch) execAt(point string) error {
	return reachArmedCrash(c.exec.Load(), point)
}

func (c *crashSwitch) storeAt(point string) error {
	return reachArmedCrash(c.store.Load(), point)
}

func reachArmedCrash(hook *func(point string) error, point string) error {
	if hook == nil {
		return nil
	}

	return (*hook)(point)
}

// armCrash makes hook the crash hook of both the executor and the store.
func (te *testEngine) armCrash(hook func(point string) error) {
	te.armExecCrash(hook)
	te.armStoreCrash(hook)
}

// armExecCrash makes hook the executor's crash hook.
func (te *testEngine) armExecCrash(hook func(point string) error) {
	te.crash.exec.Store(&hook)
}

// armStoreCrash makes hook the store's crash hook.
func (te *testEngine) armStoreCrash(hook func(point string) error) {
	te.crash.store.Store(&hook)
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
	flow := newEngineFlow(engine.flowInputs())

	return &testEngine{
		Engine: engine,
		flow:   flow,
	}
}

// newTestEngine creates an Engine backed by a temp dir with real SQLite
// and the given mock client. Returns the engine and sync root path.
func newTestEngine(t *testing.T, mock *syncfixture.EngineMockClient) (*testEngine, string) {
	t.Helper()

	return newTestEngineWithContext(t, t.Context(), mock)
}

// newTestEngineWithContext is like newTestEngine but lets callers thread a
// specific context through engine construction and cleanup-sensitive helpers.
func newTestEngineWithContext(t *testing.T, ctx context.Context, mock *syncfixture.EngineMockClient) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDir(t, ctx, mock, t.TempDir())
}

// newTestEngineInDir builds a test engine on tmpDir's data, test.db and sync,
// creating whichever are missing: a fresh directory gives a fresh engine, one
// an earlier engine left behind reopens its state (crash recovery tests).
func newTestEngineInDir(t *testing.T, ctx context.Context, mock *syncfixture.EngineMockClient, tmpDir string) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, ctx, mock, tmpDir, nil)
}

// newTestEngineOnFS builds a test engine whose sync tree runs through
// interpose: an emulated filesystem (internal/synctest, D-094).
func newTestEngineOnFS(
	t *testing.T, mock *syncfixture.EngineMockClient, interpose func(synctree.Handle) synctree.Handle,
) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, t.Context(), mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.hooks.interposeSyncTree = interpose
	})
}

// newTestEngineInDirWith is newTestEngineInDir with a last word on the
// engine's inputs.
func newTestEngineInDirWith(
	t *testing.T, ctx context.Context, mock *syncfixture.EngineMockClient, tmpDir string, configure func(*engineInputs),
) (*testEngine, string) {
	t.Helper()

	inputs := newTestEngineInputs(t, mock, tmpDir)
	crash := &crashSwitch{}
	inputs.hooks.exec.CrashAt = crash.execAt
	inputs.hooks.store.CrashAt = crash.storeAt
	if configure != nil {
		configure(inputs)
	}
	eng, err := newEngine(ctx, inputs)
	require.NoError(t, err, "NewEngine")
	eng.hooks.assertInvariants = true
	applyRegisteredDispatchOrder(t, eng)
	flow := newEngineFlow(eng.flowInputs())
	testEng := &testEngine{
		Engine: eng,
		flow:   flow,
		crash:  crash,
	}
	seedTestEngineAccount(t, inputs.DataDir, inputs.AccountCanonicalID)

	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(context.WithoutCancel(ctx)), "Engine.Close")
	})

	return testEng, inputs.SyncRoot
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
func newTestEngineWithLogger(t *testing.T, mock *syncfixture.EngineMockClient, logger *slog.Logger) (*testEngine, string) {
	t.Helper()

	return newTestEngineWithLoggerContext(t, t.Context(), mock, logger)
}

// newTestEngineWithLoggerContext is like newTestEngineWithContext but accepts a
// custom logger for tests that need to capture or filter log output.
func newTestEngineWithLoggerContext(t *testing.T, ctx context.Context, mock *syncfixture.EngineMockClient, logger *slog.Logger) (*testEngine, string) {
	t.Helper()

	return newTestEngineInDirWith(t, ctx, mock, t.TempDir(), func(inputs *engineInputs) {
		inputs.Logger = logger
	})
}

func newDownloadDeltaMock(driveID driveid.ID, item *graph.Item, token string, content []byte) *syncfixture.EngineMockClient {
	return &syncfixture.EngineMockClient{
		DeltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return syncfixture.DeltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				*item,
			}, token), nil
		},
		DownloadFn: func(_ context.Context, _ driveid.ID, _ string, w io.Writer) (int64, error) {
			n, err := w.Write(content)
			return int64(n), err
		},
	}
}

// setupWatchEngine initializes an engine with DepGraph + dispatchCh + watchRuntime
// for processBatch tests. Returns the dispatchCh for reading dispatched actions.
// Replaces the old two-call pattern of setupWatchEngine + newTestWatchState.
func setupWatchEngine(t *testing.T, eng *testEngine) <-chan *syncdomain.TrackedAction {
	t.Helper()

	rt := newWatchRuntime(eng.Engine)
	rt.sched.graph = syncexec.NewDepGraph(eng.logger)
	rt.sched.dispatchCh = make(chan *syncdomain.TrackedAction, 1024)
	rt.scopes.state = syncexec.NewScopeState(eng.nowFunc, eng.logger)
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

// seedBlockScopeRowForTest writes only key's block_scopes row, which work
// recorded blocked under key references (the database refuses the blocked
// row without it); the run's scope ledger is left alone.
func seedBlockScopeRowForTest(t *testing.T, eng *testEngine, key syncdomain.ScopeKey) {
	t.Helper()

	require.NoError(t, eng.baseline.UpsertBlockScope(t.Context(), &syncdomain.BlockScope{
		Key:           key,
		TrialInterval: time.Minute,
		NextTrialAt:   eng.nowFunc().Add(time.Minute),
	}))
}

// activateScopeForTest does what a failure that activates sr's scope does:
// it records work blocked under the scope together with the scope row, in one
// transaction, and then activates the scope.
func activateScopeForTest(t *testing.T, flow *engineFlow, work syncdomain.RetryWorkKey, sr syncdomain.ScopeUpdateResult) {
	t.Helper()

	activation, err := flow.prepareBlockActivation(sr)
	require.NoError(t, err)
	_, err = flow.recordActivatingBlockedRetryWork(t.Context(), work, activation)
	require.NoError(t, err)
	require.NoError(t, flow.applyBlockActivation(t.Context(), activation))
}

func setTestBlockScope(t *testing.T, eng *testEngine, block *syncdomain.BlockScope) {
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
		active := syncdomain.ActiveScopeFromBlockScopeRow(block)
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

func attachDebugEventRecorder(eng *testEngine) *syncfixture.DebugEventRecorder {
	return attachDebugEventRecorderWithHook(eng, nil)
}

func attachDebugEventRecorderWithHook(
	eng *testEngine,
	hook func(syncdomain.EngineDebugEvent),
) *syncfixture.DebugEventRecorder {
	recorder := syncfixture.NewDebugEventRecorder()
	eng.SetDebugEventHook(func(event syncdomain.DebugEvent) {
		recorder.Record(event)
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
func installManualClock(eng *Engine, clock *syncfixture.ManualClock) {
	eng.hooks.clock = clock
}

// errManualClockSkipsScopeWait is what the manual-clock harness answers a
// one-shot scope wait with: no wait, no extra pass, as before the wait existed.
var errManualClockSkipsScopeWait = errors.New("manual clock: scope wait skipped")

// installManualClockForTestEngine is the testEngine-aware form: it also
// refreshes the deps of the runtime and flow the harness already built.
func installManualClockForTestEngine(eng *testEngine, clock *syncfixture.ManualClock) {
	installManualClock(eng.Engine, clock)
	eng.baseline.SetNowFunc(clock.Now)
	// A one-shot scope wait is wall-clock time; on a manual clock it would
	// pass without making the trial due. A test that wants the wait sets a
	// waitFn of its own that advances its clock.
	if eng.hooks.wait == nil {
		eng.hooks.wait = func(context.Context, time.Duration) error {
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

func runFullRemoteRefreshAsyncForTest(t *testing.T, eng *testEngine, ctx context.Context, bl syncdomain.BaselineView) {
	t.Helper()
	testWatchRuntime(t, eng).runFullRemoteRefreshAsync(ctx, bl)
}

func isTestBlockScopeed(eng *testEngine, key syncdomain.ScopeKey) bool {
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

func getTestBlockScope(eng *testEngine, key syncdomain.ScopeKey) (syncdomain.BlockScope, bool) {
	if eng.runtime != nil {
		if block, ok := eng.runtime.scopes.lookupActiveScope(key); ok {
			row, err := syncdomain.BlockScopeRowFromActiveScope(block)
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
	return syncdomain.BlockScope{}, false
}

// seedBaseline commits outcomes and an optional delta token to the baseline,
// using per-outcome CommitMutation inputs (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *syncstore.SyncStore, ctx context.Context, outcomes []syncdomain.ActionOutcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		require.NoError(t, mgr.CommitMutation(ctx, syncdomain.MutationFromActionOutcome(&outcomes[i])), "seed CommitMutation[%d]", i)
	}

	if deltaToken != "" {
		require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(syncfixture.EngineTestDriveID), deltaToken), "seed CommitObservationCursor")
	}
}

// observeEngineClock wraps the engine's current clock so callers can attach
// hooks, returning the wrapper for further configuration.
func observeEngineClock(eng *Engine) *syncfixture.ObservingClock {
	if existing, ok := eng.runClock().(*syncfixture.ObservingClock); ok {
		return existing
	}

	wrapped := &syncfixture.ObservingClock{SyncClock: eng.runClock()}
	eng.hooks.clock = wrapped

	return wrapped
}

// newTestEngineInputs is the engine inputs of a test engine on tmpDir's data,
// test.db and sync, creating whichever are missing, with every hook at its
// production zero value.
func newTestEngineInputs(t *testing.T, mock *syncfixture.EngineMockClient, tmpDir string) *engineInputs {
	t.Helper()
	syncfixture.SkipUnsampledInShort(t)

	dataDir := filepath.Join(tmpDir, "data")
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")
	require.NoError(t, os.MkdirAll(dataDir, 0o750), "creating test data dir")

	logger := syncfixture.TestLogger(t)
	driveID := driveid.New(syncfixture.EngineTestDriveID)
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
	// A test's store skips the waits for the disk unless the test proves
	// durability on the production setting (keepRealFsync).
	inputs.hooks.store.SkipFsync = !usesRealFsync(t)
	return inputs
}

// refreshTestFlowInputs gives the test engine's flow and watch runtime the
// engine's configuration, capabilities and hooks as they are now, for a test
// that changed them on the engine after the flow was built: a run takes its
// copy when it starts, so a live flow would not see the change.
func refreshTestFlowInputs(eng *testEngine) {
	in := eng.flowInputs()
	if rt, ok := lookupTestWatchRuntime(eng); ok {
		rt.cfg, rt.deps, rt.hooks = in.cfg, in.deps, in.hooks
	}
	if flow, ok := lookupTestEngineFlow(eng); ok {
		flow.cfg, flow.deps, flow.hooks = in.cfg, in.deps, in.hooks
	}
}

// withTestShortcutNamespace gives a test engine the test shortcut namespace,
// so it protects the shortcut child roots its store holds.
func withTestShortcutNamespace(inputs *engineInputs) {
	inputs.ShortcutNamespaceID = syncfixture.ShortcutNamespaceTestID
}
