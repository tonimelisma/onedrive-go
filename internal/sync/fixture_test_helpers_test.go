package sync

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	stdsync "sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
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
	getItemFn         func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	getItemByPathFn   func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	listChildrenFn    func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	createFolderFn    func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	moveItemFn        func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	moveItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	// moveItemOptsFn sees every move with its full options; it wins over the
	// two narrower hooks.
	moveItemOptsFn func(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error)
	deleteItemFn        func(ctx context.Context, driveID driveid.ID, itemID string) error
	deleteItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	permanentDeleteFn   func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	downloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	uploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	uploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	// uploadCreatingFn models the two-step create (empty item, then content)
	// with its hook; nil delegates to Upload and never calls the hook.
	uploadCreatingFn func(ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc) (*graph.Item, error)
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

// GetChildByName answers a read of one name from the folder ListChildren
// holds, as Graph answers it: the child of that name under OneDrive's name
// equivalence, or graph.ErrNotFound. With ListChildrenPages it makes the mock
// the transfer manager's driveops.ItemReader.
func (m *engineMockClient) GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, err
	}
	for i := range children {
		if namekey.Equal(children[i].Name, name) {
			return &children[i], nil
		}
	}

	return nil, fmt.Errorf("reading %q: %w", name, graph.ErrNotFound)
}

// ListChildrenPages hands the folder ListChildren holds over as one page.
func (m *engineMockClient) ListChildrenPages(
	ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc,
) error {
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return err
	}
	visit(children)

	return nil
}

func (m *engineMockClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.createFolderFn != nil {
		return m.createFolderFn(ctx, driveID, parentID, name)
	}

	return &graph.Item{ID: "new-folder-id"}, nil
}

func (m *engineMockClient) MoveItem(
	ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, keepModified time.Time,
) (*graph.Item, error) {
	return m.MoveItemIfMatch(ctx, driveID, itemID, newParentID, newName, graph.MoveOptions{KeepModified: keepModified})
}

func (m *engineMockClient) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	opts graph.MoveOptions,
) (*graph.Item, error) {
	if m.moveItemOptsFn != nil {
		return m.moveItemOptsFn(ctx, driveID, itemID, newParentID, newName, opts)
	}
	if m.moveItemIfMatchFn != nil {
		return m.moveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, opts.IfMatch)
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

func (m *engineMockClient) PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.permanentDeleteFn != nil {
		return m.permanentDeleteFn(ctx, driveID, itemID)
	}

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

func (m *engineMockClient) UploadCreating(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	if m.uploadCreatingFn != nil {
		return m.uploadCreatingFn(ctx, parentID, name, content, size, onCreated)
	}

	return m.Upload(ctx, driveID, parentID, name, content, size, mtime, progress)
}

func (m *engineMockClient) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadFn != nil {
		return faithfulUploadResult(content, size)(m.uploadFn(ctx, driveID, parentID, name, content, size, mtime, progress))
	}

	return faithfulUploadResult(content, size)(&graph.Item{
		ID:   "uploaded-item-id",
		Name: name,
	}, nil)
}

func (m *engineMockClient) UploadToItem(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.uploadToItemFn != nil {
		return faithfulUploadResult(content, size)(m.uploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress))
	}
	if m.uploadFn != nil {
		return faithfulUploadResult(content, size)(m.uploadFn(ctx, driveID, "", "", content, size, mtime, progress))
	}

	return faithfulUploadResult(content, size)(&graph.Item{ID: itemID}, nil)
}

type debugEventRecorder struct {
	events  chan EngineDebugEvent
	mu      sync.Mutex
	history []EngineDebugEvent
}

// debugEventRecorderBuffer is how many events a recorder holds that no wait
// has read yet; an emitter blocks beyond it.
const debugEventRecorderBuffer = 128

// newDebugEventRecorder is a recorder that no engine feeds: a test hands its
// record method to an observer as the debug event sink the watch runtime
// would install.
func newDebugEventRecorder() *debugEventRecorder {
	return &debugEventRecorder{events: make(chan EngineDebugEvent, debugEventRecorderBuffer)}
}

// record is the recorder's sink: it keeps the event and queues it for the
// waits.
//
//nolint:gocritic // the sink's signature is the hook's, which passes events by value
func (r *debugEventRecorder) record(event EngineDebugEvent) {
	r.mu.Lock()
	r.history = append(r.history, event)
	r.mu.Unlock()
	r.events <- event
}

func (r *debugEventRecorder) waitForEvent(
	t *testing.T,
	match func(EngineDebugEvent) bool,
	description string,
) {
	t.Helper()

	timer := time.NewTimer(eventWait)
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

// waitUntilSeenOrExit waits for a debug event while also watching the run's
// result channel. When the run returns first, the awaited event can never
// arrive, so the run's own error is reported instead of letting the wait
// expire into a timeout that says nothing about why.
func (r *debugEventRecorder) waitUntilSeenOrExit(
	t *testing.T,
	done <-chan error,
	match func(EngineDebugEvent) bool,
	description string,
) {
	t.Helper()

	if r.findEvent(match) {
		return
	}

	timer := time.NewTimer(eventWait)
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
		case err := <-done:
			if r.findEvent(match) {
				return
			}

			require.FailNow(t, "run returned before debug event",
				"%s: run returned %v", description, err)
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
		}
	}
}

func (r *debugEventRecorder) waitUntilSeen(
	t *testing.T,
	match func(EngineDebugEvent) bool,
	description string,
) {
	t.Helper()

	if r.findEvent(match) {
		return
	}

	timer := time.NewTimer(eventWait)
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

func (r *debugEventRecorder) findEvent(match func(EngineDebugEvent) bool) bool {
	return r.findEventIndex(match) >= 0
}

// countEvents is how many of the events recorded so far match.
func (r *debugEventRecorder) countEvents(match func(EngineDebugEvent) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	count := 0
	for i := range r.history {
		if match(r.history[i]) {
			count++
		}
	}

	return count
}

func (r *debugEventRecorder) findEventIndex(match func(EngineDebugEvent) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.history {
		if match(r.history[i]) {
			return i
		}
	}

	return -1
}

func (r *debugEventRecorder) eventsSnapshot() []EngineDebugEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]EngineDebugEvent, len(r.history))
	copy(events, r.history)

	return events
}

func (r *debugEventRecorder) requireOrderedSubsequence(
	t *testing.T,
	matches []func(EngineDebugEvent) bool,
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
	anchor func(EngineDebugEvent) bool,
	forbidden func(EngineDebugEvent) bool,
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
	match func(EngineDebugEvent) bool,
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
	// jitterFn, when set, answers Jitter instead of the fixed jitter: a
	// test that wants distinct jittered instants supplies a sequence.
	jitterFn func(maxDelay time.Duration) time.Duration
	// armed, when set, is told the instant of every timer armed, so a test
	// can drive the clock to it (autoAdvanceTimers).
	armed  chan<- time.Time
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

// SetJitterFunc makes Jitter answer from fn, called under the clock's lock.
func (c *manualClock) SetJitterFunc(fn func(maxDelay time.Duration) time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.jitterFn = fn
}

// autoAdvanceTimers moves the clock to every timer armed from now on, as it
// is armed, until the test ends: for a run that waits on its own timers in
// virtual time (a one-shot pass parking jittered batch members) with nobody
// else to advance the clock. The clock only ever moves forward.
func (c *manualClock) autoAdvanceTimers(t *testing.T) {
	t.Helper()

	armed := make(chan time.Time, 1024)
	c.mu.Lock()
	c.armed = armed
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case at := <-armed:
				c.AdvanceTo(at)
			case <-t.Context().Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		c.mu.Lock()
		c.armed = nil
		c.mu.Unlock()
		receiveWithin(t, done, "the manual clock's advancing goroutine to stop")
	})
}

// AdvanceTo moves the clock forward to at; an instant already passed moves
// nothing.
func (c *manualClock) AdvanceTo(at time.Time) {
	c.mu.Lock()
	delay := at.Sub(c.now)
	c.mu.Unlock()
	if delay > 0 {
		c.Advance(delay)
	}
}

func (c *manualClock) AfterFunc(delay time.Duration, fn func()) SyncTimer {
	c.mu.Lock()
	defer c.mu.Unlock()

	timer := &manualSyncTimer{
		clock: c,
		at:    c.now.Add(delay),
		fn:    fn,
	}
	c.timers = append(c.timers, timer)
	if c.armed != nil {
		select {
		case c.armed <- timer.at:
		default:
		}
	}

	return timer
}

func (c *manualClock) NewTicker(interval time.Duration) SyncTicker {
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

	if c.jitterFn != nil {
		return min(max(c.jitterFn(maxDelay), 0), maxDelay)
	}
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

// testQuickXor is the QuickXorHash a faithful server reports for content,
// for fixtures built outside a test's scope.
func testQuickXor(content string) string {
	h := quickxorhash.New()
	h.Write([]byte(content))

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func hashContentQuickXor(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "quickxorhash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// observingClock decorates a SyncClock so a test can observe when the runtime
// creates a ticker, arms a timer, or sleeps. It wraps rather than replaces:
// the underlying clock still supplies the behavior, so observation cannot
// produce the incoherent mix of real and fake time that separate injectable
// function fields allowed.
type observingClock struct {
	SyncClock
	onNewTicker func(time.Duration)
	onAfterFunc func(time.Duration)
	onSleep     func(time.Duration)
}

// NewTicker and AfterFunc register with the wrapped clock before signaling.
// A test that waits for the signal and then advances a manual clock must find
// the ticker or timer already registered; signaling first let the advance
// land in the gap, so the tick was never delivered and the test timed out
// under -race -count=50 load (TestRunWatch_FallbackSleepHonorsCancellation).
func (c *observingClock) NewTicker(interval time.Duration) SyncTicker {
	ticker := c.SyncClock.NewTicker(interval)
	if c.onNewTicker != nil {
		c.onNewTicker(interval)
	}

	return ticker
}

func (c *observingClock) AfterFunc(delay time.Duration, fn func()) SyncTimer {
	timer := c.SyncClock.AfterFunc(delay, fn)
	if c.onAfterFunc != nil {
		c.onAfterFunc(delay)
	}

	return timer
}

func (c *observingClock) Sleep(ctx context.Context, delay time.Duration) error {
	if c.onSleep != nil {
		c.onSleep(delay)
	}

	//nolint:wrapcheck // decorator delegates to the wrapped clock; adding context here would attribute the failure to the test seam rather than the clock that produced it
	return c.SyncClock.Sleep(ctx, delay)
}

const engineTestDriveID = "0000000000000001"

// testDriveID is the canonical drive ID used by engine tests.
const testDriveID = synctest.TestDriveID

// eventWait is how long a test waits for an event that must come: a debug
// event, an observer's change, a pass's result, a goroutine's return. A wait
// returns the moment its event arrives, so a passing test never spends it; it
// only bounds how long a broken one hangs. It is long because events that take
// milliseconds alone took over ten seconds on a loaded two-core host under the
// race detector, and a ten-second wait failed tests there that were correct.
// A test never shows that something did not happen by letting a wait run out:
// it waits for the signal that the input was handled, then checks.
const eventWait = time.Minute

// emptyBaseline returns a Baseline with initialized but empty maps.
func emptyBaseline() *Baseline {
	return NewBaselineForTest(nil)
}

// baselineWith creates a Baseline pre-populated with the given entries.
func baselineWith(entries ...*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// newBaselineForTest seeds a baseline using the store-owned test helper so
// sync tests stay aligned with the current baseline owner.
func newBaselineForTest(entries []*BaselineEntry) *Baseline {
	return NewBaselineForTest(entries)
}

// testLogger returns a *slog.Logger wired to t.Log for clean test output.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return synctest.TestLogger(t)
}

func newTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return synctest.TestLogger(tb)
}

func setTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

func mustOpenSyncTree(t *testing.T, path string) *synctree.Root {
	t.Helper()

	tree, err := synctree.Open(path)
	if err != nil {
		panic(fmt.Sprintf("open sync tree %s: %v", path, err))
	}

	return tree
}

// enospcWatcher returns ENOSPC after N successful Add calls.
// Used by engine tests that verify the ENOSPC fallback-to-polling path.
type enospcWatcher struct {
	events       chan fsnotify.Event
	errs         chan error
	addCount     int
	failAfter    int
	closeOne     stdsync.Once
	failOne      stdsync.Once
	failCh       chan struct{}
	addedPaths   []string
	failedPaths  []string
	removedPaths []string
}

func newEnospcWatcher(failAfter int) *enospcWatcher {
	return &enospcWatcher{
		events:    make(chan fsnotify.Event, 10),
		errs:      make(chan error, 10),
		failAfter: failAfter,
		failCh:    make(chan struct{}),
	}
}

func (w *enospcWatcher) Add(name string) error {
	w.addCount++
	if w.addCount > w.failAfter {
		w.failedPaths = append(w.failedPaths, name)
		w.failOne.Do(func() { close(w.failCh) })
		return syscall.ENOSPC
	}

	w.addedPaths = append(w.addedPaths, name)

	return nil
}

func (w *enospcWatcher) Remove(name string) error {
	w.removedPaths = append(w.removedPaths, name)
	return nil
}

func (w *enospcWatcher) Events() <-chan fsnotify.Event { return w.events }

func (w *enospcWatcher) Errors() <-chan error { return w.errs }

func (w *enospcWatcher) Failures() <-chan struct{} { return w.failCh }

func (w *enospcWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}

// skipInShort marks an exhaustive test: a breadth, replay, randomized or
// many-pass variant of behavior that targeted tests in this package already
// cover. `go test -short` (the `verify quick` and `verify platform` profiles,
// which are the per-PR CI lane) skips it; the full profiles (`verify default`
// and `verify public`, the local gate and nightly CI) run it. Call it before
// t.Parallel so the skip costs nothing.
func skipInShort(tb testing.TB) {
	tb.Helper()

	if testing.Short() {
		tb.Skip("exhaustive: runs in the full verify profiles and nightly CI")
	}
}

// seedVars names the environment variables of one seeded model family: one
// seed, which wins over the other two, or a run of count consecutive seeds
// from start.
type seedVars struct {
	single string
	count  string
	start  string
}

// setIn reports whether getenv holds any of the family's variables.
func (v seedVars) setIn(getenv func(string) string) bool {
	return getenv(v.single) != "" || getenv(v.count) != "" || getenv(v.start) != ""
}

// seedsFromEnv returns the seeds of one run of a seeded model family: the one
// seed its single variable names, else count consecutive seeds from start,
// defaultCount and 1 where the variables are unset. A count below 1, or a
// value that does not parse, fails the test with one sentence that names the
// variable and its value.
func seedsFromEnv(tb testing.TB, vars seedVars, defaultCount int) []uint64 {
	tb.Helper()

	seeds, err := seedRun(os.Getenv, vars, defaultCount)
	require.NoError(tb, err)

	return seeds
}

// seedRun is the rule of seedsFromEnv over getenv.
func seedRun(getenv func(string) string, vars seedVars, defaultCount int) ([]uint64, error) {
	if raw := getenv(vars.single); raw != "" {
		seed, err := parseSeedVar(vars.single, raw)
		if err != nil {
			return nil, err
		}

		return []uint64{seed}, nil
	}
	count := defaultCount
	if raw := getenv(vars.count); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s=%q is not a whole number of seeds", vars.count, raw)
		}
		count = parsed
	}
	if count < 1 {
		return nil, fmt.Errorf("%s=%d is below 1, and a seeded run takes at least one seed", vars.count, count)
	}
	start := uint64(1)
	if raw := getenv(vars.start); raw != "" {
		parsed, err := parseSeedVar(vars.start, raw)
		if err != nil {
			return nil, err
		}
		start = parsed
	}
	if start > math.MaxUint64-uint64(count-1) {
		return nil, fmt.Errorf("%s=%d with %d seeds runs past the largest seed", vars.start, start, count)
	}
	seeds := make([]uint64, count)
	for i := range seeds {
		seeds[i] = start + uint64(i)
	}

	return seeds, nil
}

func parseSeedVar(name string, raw string) (uint64, error) {
	seed, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a seed, which is a whole number from 0", name, raw)
	}

	return seed, nil
}

// seedReplayPrefix starts the line a failed seed logs. devtool reads the
// command after it from a go test log, raw or -json, for the nightly report's
// issue, the nightly check's key and `verify failed-test`.
const seedReplayPrefix = "seed-replay: "

// seedReplayPackage is the package a replay runs, from the module root.
const seedReplayPackage = "./internal/sync"

// seedReplayTB is what logSeedReplayOnFailure needs of a test.
type seedReplayTB interface {
	Helper()
	Name() string
	Failed() bool
	Cleanup(cleanup func())
	Logf(format string, args ...any)
}

// seedAssignment is one environment assignment in front of a replay command.
type seedAssignment struct {
	name  string
	value string
}

// logSeedReplayOnFailure registers a cleanup that, when the seed's test
// failed, logs the one command that reruns it:
//
//	seed-replay: <ENV>=<seed> [ONEDRIVE_GO_DISPATCH_ORDER=<order>] go test [-race] -run '^<TopLevelTest>$' ./internal/sync
//
// with -race when this test binary was built with the race detector, under
// which some families do other work for one seed, and the dispatch order when
// the run had one, which replaces the seed's own shuffle (seedReplayInputs).
// Call it first in the seed's subtest: cleanups run last-registered first, so
// the line ends what the subtest logs. A test that go test's deadline ends
// runs no cleanup and prints no line; devtool then builds the replay from the
// subtest's name, seed=<seed>.
func logSeedReplayOnFailure(t seedReplayTB, envName string, seed uint64) {
	t.Helper()

	assignments := seedReplayInputs(os.Getenv, envName, seed)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		test, _, _ := strings.Cut(t.Name(), "/")
		t.Logf("%s", seedReplayLine(test, raceDetectorEnabled, assignments))
	})
}

// seedReplayInputs are the assignments of a seed's replay line, read through
// getenv: the seed in envName, then every other input that changes what the
// seed does and was set, which is the dispatch order alone.
func seedReplayInputs(getenv func(string) string, envName string, seed uint64) []seedAssignment {
	assignments := []seedAssignment{{name: envName, value: strconv.FormatUint(seed, 10)}}
	if order := getenv(dispatchOrderEnv); order != "" {
		assignments = append(assignments, seedAssignment{name: dispatchOrderEnv, value: order})
	}

	return assignments
}

// seedReplayLine is the replay line of a seed of test: the assignments, then
// go test of exactly that top-level test in this package.
func seedReplayLine(test string, race bool, assignments []seedAssignment) string {
	var b strings.Builder
	b.WriteString(seedReplayPrefix)
	for _, assignment := range assignments {
		b.WriteString(assignment.name + "=" + assignment.value + " ")
	}
	b.WriteString("go test ")
	if race {
		b.WriteString("-race ")
	}
	b.WriteString("-run '^" + test + "$' " + seedReplayPackage)

	return b.String()
}
