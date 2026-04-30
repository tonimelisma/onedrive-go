package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/config"
	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

func newSingleOwnerEngine(t *testing.T) *testEngine {
	t.Helper()

	mock := &engineMockClient{}
	eng, _ := newTestEngine(t, mock)
	eng.nowFn = func() time.Time { return time.Date(2025, 6, 15, 12, 0, 0, 0, time.UTC) }
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

type engineMockClient struct {
	// DeltaFetcher
	deltaFn                 func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
	socketIOEndpointFn      func(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error)
	driveFn                 func(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
	folderDeltaFn           func(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
	listChildrenRecursiveFn func(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
	listItemPermissionsFn   func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)

	// ItemClient
	getItemFn           func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn     func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	listChildrenFn      func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn      func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn          func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	moveItemIfMatchFn   func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	deleteItemFn        func(ctx context.Context, driveID driveid.ID, itemID string) error
	deleteItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error

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

	return defaultMockGetItem(driveID, itemID), nil
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
	if m.moveItemIfMatchFn != nil {
		return m.moveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, "")
	}
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *engineMockClient) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	ifMatch string,
) (*graph.Item, error) {
	if m.moveItemIfMatchFn != nil {
		return m.moveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, ifMatch)
	}
	if m.moveItemFn != nil {
		return m.moveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *engineMockClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.deleteItemIfMatchFn != nil {
		return m.deleteItemIfMatchFn(ctx, driveID, itemID, "")
	}
	if m.deleteItemFn != nil {
		return m.deleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *engineMockClient) DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
	if m.deleteItemIfMatchFn != nil {
		return m.deleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
	}
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

const (
	engineTestDriveID = "0000000000000001"
	engineTestEmail   = "sync-user@example.com"
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

	tmpDir := t.TempDir()
	dataDir := filepath.Join(tmpDir, "data")
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")
	require.NoError(t, os.MkdirAll(dataDir, 0o750), "creating test data dir")

	logger := testLogger(t)
	driveID := driveid.New(engineTestDriveID)
	accountEmail := engineTestEmail

	eng, err := newEngine(ctx, &engineInputs{
		DBPath:          dbPath,
		SyncRoot:        syncRoot,
		DataDir:         dataDir,
		DriveID:         driveID,
		AccountEmail:    accountEmail,
		Fetcher:         mock,
		SocketIOFetcher: mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		PathConvergence: &enginePathConvergenceStub{},
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   flow,
	}
	require.NoError(t, config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID: fmt.Sprintf("personal:%s", accountEmail),
			Email:       accountEmail,
			DriveType:   "personal",
		}
		catalog.UpsertAccount(&account)
		return nil
	}))

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
	dataDir := filepath.Join(tmpDir, "data")
	dbPath := filepath.Join(tmpDir, "test.db")
	syncRoot := filepath.Join(tmpDir, "sync")

	require.NoError(t, os.MkdirAll(syncRoot, 0o750), "creating sync root")
	require.NoError(t, os.MkdirAll(dataDir, 0o750), "creating test data dir")

	driveID := driveid.New(engineTestDriveID)
	accountEmail := "sync-user@example.com"

	eng, err := newEngine(ctx, &engineInputs{
		DBPath:          dbPath,
		SyncRoot:        syncRoot,
		DataDir:         dataDir,
		DriveID:         driveID,
		AccountEmail:    accountEmail,
		Fetcher:         mock,
		SocketIOFetcher: mock,
		Items:           mock,
		Downloads:       mock,
		Uploads:         mock,
		PathConvergence: &enginePathConvergenceStub{},
		FolderDelta:     mock,
		RecursiveLister: mock,
		PermChecker:     mock,
		Logger:          logger,
	})
	require.NoError(t, err, "NewEngine")
	eng.assertInvariants = true
	flow := newEngineFlow(eng)
	testEng := &testEngine{
		Engine: eng,
		flow:   flow,
	}
	require.NoError(t, config.UpdateCatalogForDataDir(dataDir, func(catalog *config.Catalog) error {
		account := config.CatalogAccount{
			CanonicalID: fmt.Sprintf("personal:%s", accountEmail),
			Email:       accountEmail,
			DriveType:   "personal",
		}
		catalog.UpsertAccount(&account)
		return nil
	}))

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
	rt.depGraph = NewDepGraph(eng.logger)
	rt.dispatchCh = make(chan *TrackedAction, 1024)
	rt.scopeState = NewScopeState(eng.nowFunc, eng.logger)
	eng.runtime = rt
	eng.flow = rt.engineFlow

	return rt.dispatchCh
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
		active := activeScopeFromBlockScopeRow(block)
		eng.runtime.upsertActiveScope(&active)
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
	return attachDebugEventRecorderWithHook(eng, nil)
}

func attachDebugEventRecorderWithHook(
	eng *testEngine,
	hook func(engineDebugEvent),
) *debugEventRecorder {
	recorder := &debugEventRecorder{
		events: make(chan engineDebugEvent, 128),
	}
	eng.SetDebugEventHook(func(event DebugEvent) {
		recorder.mu.Lock()
		recorder.history = append(recorder.history, event)
		recorder.mu.Unlock()
		recorder.events <- event
		if hook != nil {
			hook(event)
		}
	})
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

func (c *manualClock) ActiveTimerCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	count := 0
	for _, timer := range c.timers {
		if timer == nil || timer.stopped || timer.fired {
			continue
		}
		count++
	}

	return count
}

func (c *manualClock) HasPendingTimerAt(at time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	for _, timer := range c.timers {
		if timer == nil || timer.stopped || timer.fired {
			continue
		}
		if timer.at.Equal(at) {
			return true
		}
	}

	return false
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

func runFullRemoteRefreshAsyncForTest(t *testing.T, eng *testEngine, ctx context.Context, bl *Baseline) {
	t.Helper()
	testWatchRuntime(t, eng).runFullRemoteRefreshAsync(ctx, bl)
}

func isTestBlockScopeed(eng *testEngine, key ScopeKey) bool {
	if eng.runtime != nil {
		if eng.runtime.hasActiveScope(key) {
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
		if block, ok := eng.runtime.lookupActiveScope(key); ok {
			row, err := blockScopeRowFromActiveScope(block)
			if err != nil {
				panic(fmt.Sprintf("blockScopeRowFromActiveScope: %v", err))
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
// using per-outcome CommitMutation inputs (the old batch Commit was removed).
func seedBaseline(t *testing.T, mgr *SyncStore, ctx context.Context, outcomes []ActionOutcome, deltaToken string) {
	t.Helper()

	for i := range outcomes {
		require.NoError(t, mgr.CommitMutation(ctx, mutationFromActionOutcome(&outcomes[i])), "seed CommitMutation[%d]", i)
	}

	if deltaToken != "" {
		require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(engineTestDriveID), deltaToken), "seed CommitObservationCursor")
	}
}

func saveObservationCursorForTest(t *testing.T, mgr *SyncStore, ctx context.Context, driveID string, cursor string) {
	t.Helper()
	require.NoError(t, mgr.CommitObservationCursor(ctx, driveid.New(driveID), cursor))
}

func readObservationCursorForTest(t *testing.T, mgr *SyncStore, ctx context.Context, expectedDriveID string) string {
	t.Helper()

	state, err := mgr.ReadObservationState(ctx)
	require.NoError(t, err)
	if expectedDriveID != "" && (!state.ContentDriveID.IsZero() || state.Cursor != "") {
		require.Equal(t, driveid.New(expectedDriveID).String(), state.ContentDriveID.String())
	}

	return state.Cursor
}
