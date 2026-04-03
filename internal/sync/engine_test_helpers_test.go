package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncdispatch"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// ---------------------------------------------------------------------------
// Composite mock implementing synctypes.DeltaFetcher + synctypes.ItemClient + Downloader + Uploader
//
// Engine requires all 4 interfaces (unlike syncexec.Executor, which takes them
// individually), so a single mock is pragmatic here. syncexec.Executor tests split
// mocks by interface because each test exercises only 1-2 interfaces.
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ synctypes.DeltaFetcher = (*engineMockClient)(nil)
	_ synctypes.ItemClient   = (*engineMockClient)(nil)
	_ driveops.Downloader    = (*engineMockClient)(nil)
	_ driveops.Uploader      = (*engineMockClient)(nil)
)

type engineMockClient struct {
	// synctypes.DeltaFetcher
	deltaFn func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)

	// synctypes.ItemClient
	getItemFn      func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	listChildrenFn func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn     func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn   func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	uploadFn func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *engineMockClient) Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
	if m.deltaFn != nil {
		return m.deltaFn(ctx, driveID, token)
	}

	return &graph.DeltaPage{DeltaLink: "delta-token-1"}, nil
}

func (m *engineMockClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *engineMockClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.listChildrenFn != nil {
		return m.listChildrenFn(ctx, driveID, parentID)
	}

	return nil, fmt.Errorf("ListChildren not mocked")
}

func (m *engineMockClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return &graph.Item{ID: "new-folder-id"}, nil
}

func (m *engineMockClient) MoveItem(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error) {
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *engineMockClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *engineMockClient) PermanentDeleteItem(_ context.Context, _ driveid.ID, _ string) error {
	return nil
}

func (m *engineMockClient) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.downloadFn != nil {
		return m.downloadFn(ctx, driveID, itemID, w)
	}

	// Write some content so the file has data.
	n, err := w.Write([]byte("downloaded-content"))

	return int64(n), err
}

func (m *engineMockClient) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress)
	}

	return &graph.Item{
		ID:           "uploaded-item-id",
		Name:         name,
		Size:         size,
		QuickXorHash: "abc123hash",
	}, nil
}

const engineTestDriveID = "0000000000000001"

type testEngine struct {
	*Engine
	runtime *watchRuntime
	flow    *engineFlow
}

func newFlowBackedTestEngine(engine *Engine) *testEngine {
	flow := newEngineFlow(engine)

	return &testEngine{
		Engine: engine,
		flow:   &flow,
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

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(ctx, &synctypes.EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertScopeInvariants = true
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   &flow,
	}

	t.Cleanup(func() {
		assert.NoError(t, testEng.Close(context.WithoutCancel(ctx)), "Engine.Close")
	})

	return testEng, syncRoot
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
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")

	driveID := driveid.New(engineTestDriveID)

	eng, err := NewEngine(ctx, &synctypes.EngineConfig{
		DBPath:    dbPath,
		SyncRoot:  syncRoot,
		DriveID:   driveID,
		Fetcher:   mock,
		Items:     mock,
		Downloads: mock,
		Uploads:   mock,
		Logger:    logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertScopeInvariants = true
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   &flow,
	}

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

func mustReadFileUnderRoot(t *testing.T, root, relativePath string) []byte {
	t.Helper()

	cleanRoot := filepath.Clean(root)
	fullPath := filepath.Clean(filepath.Join(cleanRoot, relativePath))
	rootPrefix := cleanRoot + string(os.PathSeparator)
	require.True(t, strings.HasPrefix(fullPath, rootPrefix), "path must stay within sync root")

	data, err := os.ReadFile(fullPath)
	require.NoError(t, err, "reading resolved file")

	return data
}

// setupWatchEngine initializes an engine with syncdispatch.DepGraph + readyCh + watchRuntime
// for processBatch tests. Returns the readyCh for reading dispatched actions.
// Replaces the old two-call pattern of setupWatchEngine + newTestWatchState.
func setupWatchEngine(t *testing.T, eng *testEngine) <-chan *synctypes.TrackedAction {
	t.Helper()

	rt := newWatchRuntime(eng.Engine)
	rt.depGraph = syncdispatch.NewDepGraph(eng.logger)
	rt.readyCh = make(chan *synctypes.TrackedAction, 1024)
	rt.scopeState = syncdispatch.NewScopeState(eng.nowFunc, eng.logger)
	eng.runtime = rt
	eng.flow = &rt.engineFlow

	return rt.readyCh
}

// newTestWatchState initializes watch state on an engine for testing.
// Creates retryTimerCh, reconcileResults, and scope detection state — the
// minimum set needed for watch-mode tests.
func newTestWatchState(t *testing.T, eng *testEngine) {
	t.Helper()

	_ = setupWatchEngine(t, eng)
}

func testWorkDispatchState(t *testing.T, eng *testEngine, ctx context.Context) (*synctypes.Baseline, *synctypes.SafetyConfig) {
	t.Helper()

	bl, err := eng.baseline.Load(ctx)
	require.NoError(t, err)
	return bl, synctypes.DefaultSafetyConfig()
}

func runTestRetrierSweep(t *testing.T, eng *testEngine, ctx context.Context) []*synctypes.TrackedAction {
	t.Helper()

	bl, safety := testWorkDispatchState(t, eng, ctx)
	return testWatchRuntime(t, eng).runRetrierSweep(ctx, bl, synctypes.SyncBidirectional, safety)
}

func runTestTrialDispatch(t *testing.T, eng *testEngine, ctx context.Context) []*synctypes.TrackedAction {
	t.Helper()

	bl, safety := testWorkDispatchState(t, eng, ctx)
	return testWatchRuntime(t, eng).runTrialDispatch(ctx, bl, synctypes.SyncBidirectional, safety)
}

func setTestScopeBlock(t *testing.T, eng *testEngine, block *synctypes.ScopeBlock) {
	t.Helper()

	require.NotNil(t, block)

	if block.BlockedAt.IsZero() {
		block.BlockedAt = eng.nowFunc()
	}
	if block.TimingSource == "" {
		if block.NextTrialAt.IsZero() && block.TrialInterval == 0 {
			block.TimingSource = synctypes.ScopeTimingNone
		} else {
			block.TimingSource = synctypes.ScopeTimingBackoff
		}
	}
	if block.TimingSource != synctypes.ScopeTimingNone && block.NextTrialAt.IsZero() {
		block.NextTrialAt = block.BlockedAt.Add(block.TrialInterval)
	}
	require.NoError(t, eng.baseline.UpsertScopeBlock(context.Background(), block))
	if eng.runtime != nil {
		eng.runtime.upsertActiveScope(block)
	}
}

func lookupTestWatchRuntime(eng *testEngine) (*watchRuntime, bool) {
	return eng.runtime, eng.runtime != nil
}

func lookupTestEngineFlow(eng *testEngine) (*engineFlow, bool) {
	if eng.runtime != nil {
		return &eng.runtime.engineFlow, true
	}
	if eng.flow != nil {
		return eng.flow, true
	}

	return nil, false
}

func clearTestWatchRuntime(eng *testEngine) {
	eng.runtime = nil
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

func testScopeController(t *testing.T, eng *testEngine) *scopeController {
	t.Helper()

	return testEngineFlow(t, eng).scopeController()
}

func testShortcutCoordinator(t *testing.T, eng *testEngine) *shortcutCoordinator {
	t.Helper()

	return testEngineFlow(t, eng).shortcutCoordinator()
}

func handleRemovedShortcutsForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	deletedItemIDs map[string]bool,
	shortcuts []synctypes.Shortcut,
) error {
	t.Helper()
	return testShortcutCoordinator(t, eng).handleRemovedShortcuts(ctx, deletedItemIDs, shortcuts)
}

func loadActiveScopesForTest(t *testing.T, eng *testEngine, ctx context.Context) error {
	t.Helper()
	return testScopeController(t, eng).loadActiveScopes(ctx, testWatchRuntime(t, eng))
}

func createEventFromDBForTest(t *testing.T, eng *testEngine, ctx context.Context, row *synctypes.SyncFailureRow) *synctypes.ChangeEvent {
	t.Helper()
	return testEngineFlow(t, eng).createEventFromDB(ctx, row)
}

func isFailureResolvedForTest(t *testing.T, eng *testEngine, ctx context.Context, row *synctypes.SyncFailureRow) bool {
	t.Helper()
	return testEngineFlow(t, eng).isFailureResolved(ctx, row)
}

func clearFailureCandidateForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	row *synctypes.SyncFailureRow,
	caller string,
) {
	t.Helper()
	testEngineFlow(t, eng).clearFailureCandidate(ctx, row, caller)
}

func recordRetryTrialSkippedItemForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	row *synctypes.SyncFailureRow,
	skipped *synctypes.SkippedItem,
) {
	t.Helper()
	testEngineFlow(t, eng).recordRetryTrialSkippedItem(ctx, row, skipped)
}

func isObservationSuppressedForTest(t *testing.T, eng *testEngine, watch *watchRuntime) bool {
	t.Helper()
	return testScopeController(t, eng).isObservationSuppressed(watch)
}

func releaseTestScope(t *testing.T, eng *testEngine, ctx context.Context, key synctypes.ScopeKey) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testScopeController(t, eng).releaseScope(ctx, rt, key)
}

func discardTestScope(t *testing.T, eng *testEngine, ctx context.Context, key synctypes.ScopeKey) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testScopeController(t, eng).discardScope(ctx, rt, key)
}

func assertTestCurrentScopeInvariants(t *testing.T, eng *testEngine, ctx context.Context) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testEngineFlow(t, eng).assertCurrentScopeInvariants(ctx, rt)
}

func assertReleasedScopeForTest(t *testing.T, eng *testEngine, ctx context.Context, key synctypes.ScopeKey) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testEngineFlow(t, eng).assertReleasedScope(ctx, rt, key)
}

func assertDiscardedScopeForTest(t *testing.T, eng *testEngine, ctx context.Context, key synctypes.ScopeKey) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testEngineFlow(t, eng).assertDiscardedScope(ctx, rt, key)
}

func repairPersistedScopesForTest(t *testing.T, eng *testEngine, ctx context.Context) error {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testScopeController(t, eng).repairPersistedScopes(ctx, rt)
}

func admitReadyForTest(t *testing.T, eng *testEngine, ctx context.Context, ready []*synctypes.TrackedAction) []*synctypes.TrackedAction {
	t.Helper()
	if rt, ok := lookupTestWatchRuntime(eng); ok {
		return testScopeController(t, eng).admitReady(ctx, rt, ready)
	}

	return testScopeController(t, eng).admitReady(ctx, nil, ready)
}

func cascadeRecordAndCompleteForTest(t *testing.T, eng *testEngine, ctx context.Context, ta *synctypes.TrackedAction, scopeKey synctypes.ScopeKey) {
	t.Helper()
	testScopeController(t, eng).cascadeRecordAndComplete(ctx, ta, scopeKey)
}

func processWorkerResultForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) []*synctypes.TrackedAction {
	t.Helper()
	return processWorkerResultDetailedForTest(t, eng, ctx, r, bl).dispatched
}

func processWorkerResultDetailedForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	r *synctypes.WorkerResult,
	bl *synctypes.Baseline,
) routeOutcome {
	t.Helper()
	if rt, ok := lookupTestWatchRuntime(eng); ok {
		return rt.processWorkerResult(ctx, rt, r, bl)
	}

	flow, ok := lookupTestEngineFlow(eng)
	require.True(t, ok, "engine flow must be initialized for this test")
	return flow.processWorkerResult(ctx, nil, r, bl)
}

func processTrialResultForTest(t *testing.T, eng *testEngine, ctx context.Context, r *synctypes.WorkerResult) {
	t.Helper()
	if rt, ok := lookupTestWatchRuntime(eng); ok {
		rt.processWorkerResult(ctx, rt, r, nil)
		return
	}

	flow, ok := lookupTestEngineFlow(eng)
	require.True(t, ok, "engine flow must be initialized for this test")
	flow.processWorkerResult(ctx, nil, r, nil)
}

func processBatchForTest(
	t *testing.T,
	eng *testEngine,
	ctx context.Context,
	batch []synctypes.PathChanges,
	bl *synctypes.Baseline,
	safety *synctypes.SafetyConfig,
) []*synctypes.TrackedAction {
	t.Helper()
	return testWatchRuntime(t, eng).processBatch(ctx, batch, bl, synctypes.SyncBidirectional, safety)
}

func externalDBChangedForTest(t *testing.T, eng *testEngine, ctx context.Context) bool {
	t.Helper()
	return testWatchRuntime(t, eng).externalDBChanged(ctx)
}

func handleExternalChangesForTest(t *testing.T, eng *testEngine, ctx context.Context) {
	t.Helper()
	testWatchRuntime(t, eng).handleExternalChanges(ctx)
}

func runFullReconciliationAsyncForTest(t *testing.T, eng *testEngine, ctx context.Context, bl *synctypes.Baseline) {
	t.Helper()
	testWatchRuntime(t, eng).runFullReconciliationAsync(ctx, bl)
}

func runWatchLoopForTest(eng *testEngine, ctx context.Context, p *watchPipeline) error {
	rt, ok := lookupTestWatchRuntime(eng)
	if !ok {
		return fmt.Errorf("watch runtime must be initialized for this test")
	}
	p.runtime = rt
	if p.reconcileResults == nil {
		p.reconcileResults = rt.reconcileResults
	}
	return rt.runWatchLoop(ctx, p)
}

func waitForQuiescenceForTest(t *testing.T, eng *testEngine, ctx context.Context) error {
	t.Helper()
	rt := testWatchRuntime(t, eng)
	return rt.runWatchUntilQuiescent(ctx, &watchPipeline{runtime: rt}, nil)
}

func bootstrapSyncForTest(t *testing.T, eng *testEngine, ctx context.Context, mode synctypes.SyncMode, pipe *watchPipeline) error {
	t.Helper()
	rt := testWatchRuntime(t, eng)
	pipe.runtime = rt
	return rt.bootstrapSync(ctx, mode, pipe)
}

func activeBlockingScopeForTest(t *testing.T, eng *testEngine, ta *synctypes.TrackedAction) synctypes.ScopeKey {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	return testScopeController(t, eng).activeBlockingScope(rt, ta)
}

func applyScopeBlockForTest(t *testing.T, eng *testEngine, ctx context.Context, sr synctypes.ScopeUpdateResult) {
	t.Helper()
	rt := testWatchRuntime(t, eng)
	testScopeController(t, eng).applyScopeBlock(ctx, rt, sr)
}

func feedScopeDetectionForTest(t *testing.T, eng *testEngine, ctx context.Context, r *synctypes.WorkerResult) {
	t.Helper()
	rt, _ := lookupTestWatchRuntime(eng)
	testScopeController(t, eng).feedScopeDetection(ctx, rt, r)
}

func isTestScopeBlocked(eng *testEngine, key synctypes.ScopeKey) bool {
	blocks, err := eng.baseline.ListScopeBlocks(context.Background())
	if err != nil {
		panic(fmt.Sprintf("ListScopeBlocks: %v", err))
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return true
		}
	}
	return false
}

func getTestScopeBlock(eng *testEngine, key synctypes.ScopeKey) (synctypes.ScopeBlock, bool) {
	blocks, err := eng.baseline.ListScopeBlocks(context.Background())
	if err != nil {
		panic(fmt.Sprintf("ListScopeBlocks: %v", err))
	}
	for i := range blocks {
		if blocks[i].Key == key {
			return *blocks[i], true
		}
	}
	return synctypes.ScopeBlock{}, false
}

// deltaPageWithItems returns a DeltaPage with the given items and a delta link.
func deltaPageWithItems(items []graph.Item, deltaLink string) *graph.DeltaPage {
	return &graph.DeltaPage{
		Items:     items,
		DeltaLink: deltaLink,
	}
}

// writeLocalFile creates a file in syncRoot for local observer to find.
func writeLocalFile(t *testing.T, syncRoot, relPath, content string) {
	t.Helper()

	absPath := filepath.Join(syncRoot, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o750), "MkdirAll")
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600), "WriteFile")
}

// seedBaseline commits outcomes and an optional delta token to the baseline,
// using the per-outcome CommitOutcome API (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *syncstore.SyncStore, ctx context.Context, outcomes []synctypes.Outcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		require.NoError(t, mgr.CommitOutcome(ctx, &outcomes[i]), "seed CommitOutcome[%d]", i)
	}

	if deltaToken != "" {
		require.NoError(t, mgr.CommitDeltaToken(ctx, deltaToken, engineTestDriveID, "", engineTestDriveID), "seed CommitDeltaToken")
	}
}
