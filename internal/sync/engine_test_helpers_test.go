package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/syncstore"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// ---------------------------------------------------------------------------
// Composite mock implementing synctypes.DeltaFetcher + synctypes.ItemClient + Downloader + Uploader
//
// Engine requires all 4 interfaces (unlike Executor, which takes them
// individually), so a single mock is pragmatic here. Executor tests split
// mocks by interface because each test exercises only 1-2 interfaces.
// ---------------------------------------------------------------------------

// Compile-time interface satisfaction checks.
var (
	_ synctypes.DeltaFetcher            = (*engineMockClient)(nil)
	_ synctypes.SocketIOEndpointFetcher = (*engineMockClient)(nil)
	_ synctypes.ItemClient              = (*engineMockClient)(nil)
	_ driveops.Downloader               = (*engineMockClient)(nil)
	_ driveops.Uploader                 = (*engineMockClient)(nil)
	_ driveops.ItemUploader             = (*engineMockClient)(nil)
	_ synctypes.FolderDeltaFetcher      = (*engineMockClient)(nil)
	_ synctypes.RecursiveLister         = (*engineMockClient)(nil)
	_ synctypes.PermissionChecker       = (*engineMockClient)(nil)
	_ synctypes.DriveVerifier           = (*engineMockClient)(nil)
)

type engineMockClient struct {
	// synctypes.DeltaFetcher
	deltaFn                 func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
	socketIOEndpointFn      func(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error)
	driveFn                 func(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
	folderDeltaFn           func(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
	listChildrenRecursiveFn func(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
	listItemPermissionsFn   func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)

	// synctypes.ItemClient
	getItemFn       func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	listChildrenFn  func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn  func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn      func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	deleteItemFn    func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	uploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	uploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
}

func (m *engineMockClient) Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
	if m.deltaFn != nil {
		return m.deltaFn(ctx, driveID, token)
	}

	return &graph.DeltaPage{DeltaLink: "delta-token-1"}, nil
}

func (m *engineMockClient) Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error) {
	if m.driveFn != nil {
		return m.driveFn(ctx, driveID)
	}

	return &graph.Drive{ID: driveID}, nil
}

func (m *engineMockClient) SocketIOEndpoint(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error) {
	if m.socketIOEndpointFn != nil {
		return m.socketIOEndpointFn(ctx, driveID)
	}

	return &graph.SocketIOEndpoint{}, nil
}

func (m *engineMockClient) DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error) {
	if m.folderDeltaFn != nil {
		return m.folderDeltaFn(ctx, driveID, folderID, token)
	}

	return nil, "folder-delta-token-1", nil
}

func (m *engineMockClient) ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error) {
	if m.listChildrenRecursiveFn != nil {
		return m.listChildrenRecursiveFn(ctx, driveID, folderID)
	}

	return nil, nil
}

func (m *engineMockClient) ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
	if m.listItemPermissionsFn != nil {
		return m.listItemPermissionsFn(ctx, driveID, itemID)
	}

	return nil, nil
}

func (m *engineMockClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.getItemFn != nil {
		return m.getItemFn(ctx, driveID, itemID)
	}

	return nil, fmt.Errorf("GetItem not mocked")
}

func (m *engineMockClient) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	if m.getItemByPathFn != nil {
		return m.getItemByPathFn(ctx, driveID, remotePath)
	}

	return nil, graph.ErrNotFound
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

func (m *engineMockClient) UploadToItem(ctx context.Context, driveID driveid.ID, itemID string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadToItemFn != nil {
		return m.uploadToItemFn(ctx, driveID, itemID, content, size, mtime, progress)
	}
	if m.uploadFn != nil {
		return m.uploadFn(ctx, driveID, "", "", content, size, mtime, progress)
	}

	return &graph.Item{
		ID:           itemID,
		Size:         size,
		QuickXorHash: "abc123hash",
	}, nil
}

const engineTestDriveID = "0000000000000001"

func testThrottleDriveID() driveid.ID {
	return driveid.New(engineTestDriveID)
}

func testThrottleScope() synctypes.ScopeKey {
	return synctypes.SKThrottleDrive(testThrottleDriveID())
}

type testEngine struct {
	*Engine
	runtime *watchRuntime
	flow    *engineFlow
}

type enginePathConvergenceStub struct{}

func (s *enginePathConvergenceStub) ForTarget(_ driveid.ID, _ string) driveops.PathConvergence {
	return s
}

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

	eng, err := newEngine(ctx, &synctypes.EngineConfig{
		DBPath:                 dbPath,
		SyncRoot:               syncRoot,
		DriveID:                driveID,
		Fetcher:                mock,
		SocketIOFetcher:        mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		PathConvergenceFactory: &enginePathConvergenceStub{},
		FolderDelta:            mock,
		RecursiveLister:        mock,
		PermChecker:            mock,
		Logger:                 logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
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

	eng, err := newEngine(ctx, &synctypes.EngineConfig{
		DBPath:                 dbPath,
		SyncRoot:               syncRoot,
		DriveID:                driveID,
		Fetcher:                mock,
		SocketIOFetcher:        mock,
		Items:                  mock,
		Downloads:              mock,
		Uploads:                mock,
		PathConvergenceFactory: &enginePathConvergenceStub{},
		FolderDelta:            mock,
		RecursiveLister:        mock,
		PermChecker:            mock,
		Logger:                 logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
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

func newTwoFileDownloadDeltaMock(
	t *testing.T,
	driveID driveid.ID,
	contents map[string]string,
	downloaded *[]string,
	token string,
) *engineMockClient {
	t.Helper()

	return &engineMockClient{
		deltaFn: func(_ context.Context, _ driveid.ID, _ string) (*graph.DeltaPage, error) {
			return deltaPageWithItems([]graph.Item{
				{ID: "root", IsRoot: true, DriveID: driveID},
				{
					ID:           "keep-item",
					Name:         "keep.txt",
					ParentID:     "root",
					DriveID:      driveID,
					QuickXorHash: hashContentQuickXor(t, contents["keep-item"]),
					Size:         int64(len(contents["keep-item"])),
				},
				{
					ID:           "drop-item",
					Name:         "drop.txt",
					ParentID:     "root",
					DriveID:      driveID,
					QuickXorHash: hashContentQuickXor(t, contents["drop-item"]),
					Size:         int64(len(contents["drop-item"])),
				},
			}, token), nil
		},
		downloadFn: func(_ context.Context, _ driveid.ID, itemID string, w io.Writer) (int64, error) {
			*downloaded = append(*downloaded, itemID)
			n, err := w.Write([]byte(contents[itemID]))
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

func readFileUnderRootIfExists(t *testing.T, root, relativePath string) ([]byte, bool) {
	t.Helper()

	cleanRoot := filepath.Clean(root)
	fullPath := filepath.Clean(filepath.Join(cleanRoot, relativePath))
	rootPrefix := cleanRoot + string(os.PathSeparator)
	require.True(t, strings.HasPrefix(fullPath, rootPrefix), "path must stay within sync root")

	data, err := os.ReadFile(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false
		}
		require.NoError(t, err, "reading resolved file if present")
	}

	return data, true
}

// setupWatchEngine initializes an engine with DepGraph + dispatchCh + watchRuntime
// for processBatch tests. Returns the dispatchCh for reading dispatched actions.
// Replaces the old two-call pattern of setupWatchEngine + newTestWatchState.
func setupWatchEngine(t *testing.T, eng *testEngine) <-chan *synctypes.TrackedAction {
	t.Helper()

	rt := newWatchRuntime(eng.Engine)
	rt.depGraph = NewDepGraph(eng.logger)
	rt.dispatchCh = make(chan *synctypes.TrackedAction, 1024)
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	eng.runtime = rt
	eng.flow = &rt.engineFlow

	return rt.dispatchCh
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
	if !block.Key.IsPermRemote() {
		require.NoError(t, eng.baseline.UpsertScopeBlock(context.Background(), block))
	}
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

func suppressedShortcutTargetsForTest(t *testing.T, eng *testEngine, watch *watchRuntime) map[string]struct{} {
	t.Helper()
	return testScopeController(t, eng).suppressedShortcutTargets(watch)
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
	return testEngineFlow(t, eng).assertCurrentInvariants(ctx, rt)
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
	return testScopeController(t, eng).repairPersistedScopes(ctx, rt, driveIdentityProof{}, nil)
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

type debugEventRecorder struct {
	events  chan engineDebugEvent
	mu      sync.Mutex
	history []engineDebugEvent
}

// debugEventTimeout gives race-covered watch tests enough headroom to observe
// bootstrap and shutdown events under the repo's full verify load. These tests
// validate event ordering, not sub-second latency.
const debugEventTimeout = 10 * time.Second

func attachDebugEventRecorder(eng *testEngine) *debugEventRecorder {
	recorder := &debugEventRecorder{
		events: make(chan engineDebugEvent, 128),
	}
	eng.debugEventHook = func(event engineDebugEvent) {
		recorder.mu.Lock()
		recorder.history = append(recorder.history, event)
		recorder.mu.Unlock()
		recorder.events <- event
	}
	return recorder
}

func (r *debugEventRecorder) waitForEvent(
	t *testing.T,
	match func(engineDebugEvent) bool,
	description string,
) {
	t.Helper()

	timer := time.NewTimer(debugEventTimeout)
	defer timer.Stop()

	for {
		select {
		case event := <-r.events:
			if match(event) {
				return
			}
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
		}
	}
}

func (r *debugEventRecorder) waitUntilSeen(
	t *testing.T,
	match func(engineDebugEvent) bool,
	description string,
) {
	t.Helper()

	if r.findEvent(match) {
		return
	}

	timer := time.NewTimer(debugEventTimeout)
	defer timer.Stop()

	for {
		select {
		case event := <-r.events:
			if match(event) {
				return
			}
			if r.findEvent(match) {
				return
			}
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
		}
	}
}

func (r *debugEventRecorder) findEvent(match func(engineDebugEvent) bool) bool {
	return r.findEventIndex(match) >= 0
}

func (r *debugEventRecorder) findEventIndex(match func(engineDebugEvent) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.history {
		if match(r.history[i]) {
			return i
		}
	}

	return -1
}

func (r *debugEventRecorder) eventTypesSnapshot() []engineDebugEventType {
	events := r.eventsSnapshot()
	types := make([]engineDebugEventType, 0, len(events))
	for i := range events {
		types = append(types, events[i].Type)
	}

	return types
}

func (r *debugEventRecorder) eventsSnapshot() []engineDebugEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]engineDebugEvent, len(r.history))
	copy(events, r.history)

	return events
}

func (r *debugEventRecorder) requireOrderedSubsequence(
	t *testing.T,
	matches []func(engineDebugEvent) bool,
	description string,
) {
	t.Helper()

	events := r.eventsSnapshot()
	searchFrom := 0
	for i := range matches {
		found := false
		for j := searchFrom; j < len(events); j++ {
			if matches[i](events[j]) {
				searchFrom = j + 1
				found = true
				break
			}
		}
		if !found {
			require.FailNow(t, "expected ordered debug-event subsequence", description)
		}
	}
}

func (r *debugEventRecorder) requireNoEventAfter(
	t *testing.T,
	anchor func(engineDebugEvent) bool,
	forbidden func(engineDebugEvent) bool,
	description string,
) {
	t.Helper()

	events := r.eventsSnapshot()
	anchorIndex := -1
	for i := range events {
		if anchor(events[i]) {
			anchorIndex = i
			break
		}
	}
	require.NotEqual(t, -1, anchorIndex, "anchor event missing: %s", description)

	for i := anchorIndex + 1; i < len(events); i++ {
		if forbidden(events[i]) {
			require.FailNow(t, "unexpected debug event after anchor", description)
		}
	}
}

func (r *debugEventRecorder) requireEventCount(
	t *testing.T,
	match func(engineDebugEvent) bool,
	expected int,
	description string,
) {
	t.Helper()

	events := r.eventsSnapshot()
	count := 0
	for i := range events {
		if match(events[i]) {
			count++
		}
	}
	require.Equal(t, expected, count, description)
}

type manualClock struct {
	mu     sync.Mutex
	now    time.Time
	jitter time.Duration
	timers []*manualSyncTimer
	ticks  []*manualSyncTicker
}

type manualSyncTimer struct {
	clock   *manualClock
	at      time.Time
	fn      func()
	stopped bool
	fired   bool
}

type manualSyncTicker struct {
	clock    *manualClock
	ch       chan time.Time
	interval time.Duration
	next     time.Time
	stopped  bool
}

func newManualClock(start time.Time) *manualClock {
	return &manualClock{now: start}
}

func installManualClock(eng *Engine, clock *manualClock) {
	eng.nowFn = clock.Now
	eng.afterFunc = clock.AfterFunc
	eng.newTicker = clock.NewTicker
	eng.sleepFn = clock.Sleep
	eng.jitterFn = clock.Jitter
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *manualClock) SetJitter(delay time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.jitter = delay
}

func (c *manualClock) AfterFunc(delay time.Duration, fn func()) syncTimer {
	c.mu.Lock()
	defer c.mu.Unlock()

	timer := &manualSyncTimer{
		clock: c,
		at:    c.now.Add(delay),
		fn:    fn,
	}
	c.timers = append(c.timers, timer)

	return timer
}

func (c *manualClock) NewTicker(interval time.Duration) syncTicker {
	if interval <= 0 {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	ticker := &manualSyncTicker{
		clock:    c,
		ch:       make(chan time.Time, 16),
		interval: interval,
		next:     c.now.Add(interval),
	}
	c.ticks = append(c.ticks, ticker)

	return ticker
}

func (c *manualClock) Sleep(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	done := make(chan struct{})
	timer := c.AfterFunc(delay, func() { close(done) })
	defer timer.Stop()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("manual sleep: %w", ctx.Err())
	}
}

func (c *manualClock) Jitter(maxDelay time.Duration) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.jitter <= 0 {
		return 0
	}
	if c.jitter > maxDelay {
		return maxDelay
	}

	return c.jitter
}

func (c *manualClock) Advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now

	var timers []func()
	for _, timer := range c.timers {
		if timer == nil || timer.stopped || timer.fired || timer.at.After(now) {
			continue
		}
		timer.fired = true
		timers = append(timers, timer.fn)
	}

	for _, ticker := range c.ticks {
		if ticker == nil || ticker.stopped {
			continue
		}
		for !ticker.next.After(now) {
			select {
			case ticker.ch <- ticker.next:
			default:
			}
			ticker.next = ticker.next.Add(ticker.interval)
		}
	}
	c.mu.Unlock()

	for _, fn := range timers {
		if fn != nil {
			fn()
		}
	}
}

func (t *manualSyncTimer) Stop() bool {
	if t == nil || t.clock == nil {
		return false
	}

	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	if t.stopped || t.fired {
		return false
	}
	t.stopped = true

	return true
}

func (t *manualSyncTicker) Chan() <-chan time.Time {
	if t == nil {
		return nil
	}

	return t.ch
}

func (t *manualSyncTicker) Stop() {
	if t == nil || t.clock == nil {
		return
	}

	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()

	t.stopped = true
}

func syncStorePathForTest(t *testing.T, eng *testEngine) string {
	t.Helper()

	rows, err := eng.baseline.DB().Query("PRAGMA database_list")
	require.NoError(t, err, "PRAGMA database_list")
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name string
		var file string
		require.NoError(t, rows.Scan(&seq, &name, &file), "scan PRAGMA database_list")
		if name == "main" {
			require.NotEmpty(t, file, "main database path should not be empty")
			return file
		}
	}

	require.NoError(t, rows.Err(), "iterate PRAGMA database_list")
	require.FailNow(t, "main database path not found")
	return ""
}

func handleExternalChangesForTest(t *testing.T, eng *testEngine, ctx context.Context) {
	t.Helper()
	testWatchRuntime(t, eng).handleExternalChanges(ctx)
}

func runFullReconciliationAsyncForTest(t *testing.T, eng *testEngine, ctx context.Context, bl *synctypes.Baseline) {
	t.Helper()
	testWatchRuntime(t, eng).runFullReconciliationAsync(ctx, bl)
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
	if eng.runtime != nil {
		if eng.runtime.hasActiveScope(key) {
			return true
		}
	}

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
	if eng.runtime != nil {
		if block, ok := eng.runtime.lookupActiveScope(key); ok {
			return block, true
		}
	}

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

func hashContentQuickXor(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "quickxorhash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
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
