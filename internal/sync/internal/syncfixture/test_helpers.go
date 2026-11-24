package syncfixture

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

type EngineMockClient struct {
	// DeltaFetcher
	DeltaFn                 func(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error)
	socketIOEndpointFn      func(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error)
	driveFn                 func(ctx context.Context, driveID driveid.ID) (*graph.Drive, error)
	FolderDeltaFn           func(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error)
	ListChildrenRecursiveFn func(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error)
	ListItemPermissionsFn   func(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error)

	// ItemClient
	GetItemFn         func(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error)
	GetItemByPathFn   func(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error)
	ListChildrenFn    func(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error)
	CreateFolderFn    func(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error)
	MoveItemFn        func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string) (*graph.Item, error)
	MoveItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, newParentID, newName, ifMatch string) (*graph.Item, error)
	// moveItemOptsFn sees every move with its full options; it wins over the
	// two narrower hooks.
	MoveItemOptsFn func(
		ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, opts graph.MoveOptions,
	) (*graph.Item, error)
	DeleteItemFn        func(ctx context.Context, driveID driveid.ID, itemID string) error
	DeleteItemIfMatchFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error
	PermanentDeleteFn   func(ctx context.Context, driveID driveid.ID, itemID string) error

	// Downloader
	DownloadFn func(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error)

	// Uploader
	UploadFn       func(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	UploadToItemFn func(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error)
	// uploadCreatingFn models the two-step create (empty item, then content)
	// with its hook; nil delegates to Upload and never calls the hook.
	UploadCreatingFn func(ctx context.Context, parentID, name string, content io.ReaderAt, size int64, onCreated graph.CreatedFunc) (*graph.Item, error)
}

// EngineMockClient is pinned to the sync item client it stands in for.
var _ syncdomain.ItemClient = (*EngineMockClient)(nil)

func (m *EngineMockClient) Delta(ctx context.Context, driveID driveid.ID, token string) (*graph.DeltaPage, error) {
	if m.DeltaFn != nil {
		return m.DeltaFn(ctx, driveID, token)
	}

	return &graph.DeltaPage{DeltaLink: "delta-token-1"}, nil
}

func (m *EngineMockClient) Drive(ctx context.Context, driveID driveid.ID) (*graph.Drive, error) {
	if m.driveFn != nil {
		return m.driveFn(ctx, driveID)
	}

	return &graph.Drive{ID: driveID}, nil
}

func (m *EngineMockClient) SocketIOEndpoint(ctx context.Context, driveID driveid.ID) (*graph.SocketIOEndpoint, error) {
	if m.socketIOEndpointFn != nil {
		return m.socketIOEndpointFn(ctx, driveID)
	}

	return &graph.SocketIOEndpoint{}, nil
}

func (m *EngineMockClient) DeltaFolderAll(ctx context.Context, driveID driveid.ID, folderID, token string) ([]graph.Item, string, error) {
	if m.FolderDeltaFn != nil {
		return m.FolderDeltaFn(ctx, driveID, folderID, token)
	}

	return nil, "folder-delta-token-1", nil
}

func (m *EngineMockClient) ListChildrenRecursive(ctx context.Context, driveID driveid.ID, folderID string) ([]graph.Item, error) {
	if m.ListChildrenRecursiveFn != nil {
		return m.ListChildrenRecursiveFn(ctx, driveID, folderID)
	}

	return nil, nil
}

func (m *EngineMockClient) ListItemPermissions(ctx context.Context, driveID driveid.ID, itemID string) ([]graph.Permission, error) {
	if m.ListItemPermissionsFn != nil {
		return m.ListItemPermissionsFn(ctx, driveID, itemID)
	}

	return nil, nil
}

func (m *EngineMockClient) GetItem(ctx context.Context, driveID driveid.ID, itemID string) (*graph.Item, error) {
	if m.GetItemFn != nil {
		return m.GetItemFn(ctx, driveID, itemID)
	}

	return DefaultMockGetItem(driveID, itemID), nil
}

func (m *EngineMockClient) GetItemByPath(ctx context.Context, driveID driveid.ID, remotePath string) (*graph.Item, error) {
	if m.GetItemByPathFn != nil {
		return m.GetItemByPathFn(ctx, driveID, remotePath)
	}

	return nil, graph.ErrNotFound
}

func (m *EngineMockClient) ListChildren(ctx context.Context, driveID driveid.ID, parentID string) ([]graph.Item, error) {
	if m.ListChildrenFn != nil {
		return m.ListChildrenFn(ctx, driveID, parentID)
	}

	return nil, errors.New("ListChildren not mocked")
}

// GetChildByName answers a read of one name from the folder ListChildren
// holds, as Graph answers it (ChildNamed). With ListChildrenPages it makes
// the mock the transfer manager's driveops.ItemReader and the sync item
// client.
func (m *EngineMockClient) GetChildByName(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return nil, err
	}

	return ChildNamed(children, name)
}

// ListChildrenPages hands the folder ListChildren holds over as one page.
func (m *EngineMockClient) ListChildrenPages(
	ctx context.Context, driveID driveid.ID, parentID string, visit graph.ChildrenPageFunc,
) error {
	children, err := m.ListChildren(ctx, driveID, parentID)
	if err != nil {
		return err
	}
	visit(children)

	return nil
}

func (m *EngineMockClient) CreateFolder(ctx context.Context, driveID driveid.ID, parentID, name string) (*graph.Item, error) {
	if m.CreateFolderFn != nil {
		return m.CreateFolderFn(ctx, driveID, parentID, name)
	}

	return &graph.Item{ID: "new-folder-id"}, nil
}

func (m *EngineMockClient) MoveItem(
	ctx context.Context, driveID driveid.ID, itemID, newParentID, newName string, keepModified time.Time,
) (*graph.Item, error) {
	return m.MoveItemIfMatch(ctx, driveID, itemID, newParentID, newName, graph.MoveOptions{KeepModified: keepModified})
}

func (m *EngineMockClient) MoveItemIfMatch(
	ctx context.Context,
	driveID driveid.ID,
	itemID string,
	newParentID string,
	newName string,
	opts graph.MoveOptions,
) (*graph.Item, error) {
	if m.MoveItemOptsFn != nil {
		return m.MoveItemOptsFn(ctx, driveID, itemID, newParentID, newName, opts)
	}
	if m.MoveItemIfMatchFn != nil {
		return m.MoveItemIfMatchFn(ctx, driveID, itemID, newParentID, newName, opts.IfMatch)
	}
	if m.MoveItemFn != nil {
		return m.MoveItemFn(ctx, driveID, itemID, newParentID, newName)
	}

	return &graph.Item{ID: itemID}, nil
}

func (m *EngineMockClient) DeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.DeleteItemIfMatchFn != nil {
		return m.DeleteItemIfMatchFn(ctx, driveID, itemID, "")
	}
	if m.DeleteItemFn != nil {
		return m.DeleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *EngineMockClient) DeleteItemIfMatch(ctx context.Context, driveID driveid.ID, itemID, ifMatch string) error {
	if m.DeleteItemIfMatchFn != nil {
		return m.DeleteItemIfMatchFn(ctx, driveID, itemID, ifMatch)
	}
	if m.DeleteItemFn != nil {
		return m.DeleteItemFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *EngineMockClient) PermanentDeleteItem(ctx context.Context, driveID driveid.ID, itemID string) error {
	if m.PermanentDeleteFn != nil {
		return m.PermanentDeleteFn(ctx, driveID, itemID)
	}

	return nil
}

func (m *EngineMockClient) Download(ctx context.Context, driveID driveid.ID, itemID string, w io.Writer) (int64, error) {
	if m.DownloadFn != nil {
		return m.DownloadFn(ctx, driveID, itemID, w)
	}

	// Write some content so the file has data.
	n, err := w.Write([]byte("downloaded-content"))

	return int64(n), err
}

func (m *EngineMockClient) UploadCreating(
	ctx context.Context, driveID driveid.ID, parentID, name string,
	content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc, onCreated graph.CreatedFunc,
) (*graph.Item, error) {
	if m.UploadCreatingFn != nil {
		return m.UploadCreatingFn(ctx, parentID, name, content, size, onCreated)
	}

	return m.Upload(ctx, driveID, parentID, name, content, size, mtime, progress)
}

func (m *EngineMockClient) Upload(ctx context.Context, driveID driveid.ID, parentID, name string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.UploadFn != nil {
		return faithfulUploadResult(content, size)(m.UploadFn(ctx, driveID, parentID, name, content, size, mtime, progress))
	}

	return faithfulUploadResult(content, size)(&graph.Item{
		ID:   "uploaded-item-id",
		Name: name,
	}, nil)
}

func (m *EngineMockClient) UploadToItem(ctx context.Context, driveID driveid.ID, itemID, ifMatch string, content io.ReaderAt, size int64, mtime time.Time, progress graph.ProgressFunc) (*graph.Item, error) {
	if m.UploadToItemFn != nil {
		return faithfulUploadResult(content, size)(m.UploadToItemFn(ctx, driveID, itemID, ifMatch, content, size, mtime, progress))
	}
	if m.UploadFn != nil {
		return faithfulUploadResult(content, size)(m.UploadFn(ctx, driveID, "", "", content, size, mtime, progress))
	}

	return faithfulUploadResult(content, size)(&graph.Item{ID: itemID}, nil)
}

type DebugEventRecorder struct {
	Events  chan syncdomain.EngineDebugEvent
	mu      sync.Mutex // guards history
	history []syncdomain.EngineDebugEvent
}

// debugEventRecorderBuffer is how many events a recorder holds that no wait
// has read yet; an emitter blocks beyond it.
const debugEventRecorderBuffer = 128

// NewDebugEventRecorder is a recorder that no engine feeds: a test hands its
// record method to an observer as the debug event sink the watch runtime
// would install.
func NewDebugEventRecorder() *DebugEventRecorder {
	return &DebugEventRecorder{Events: make(chan syncdomain.EngineDebugEvent, debugEventRecorderBuffer)}
}

// Record is the recorder's sink: it keeps the event and queues it for the
// waits.
//
//nolint:gocritic // the sink's signature is the hook's, which passes events by value
func (r *DebugEventRecorder) Record(event syncdomain.EngineDebugEvent) {
	r.mu.Lock()
	r.history = append(r.history, event)
	r.mu.Unlock()
	r.Events <- event
}

func (r *DebugEventRecorder) WaitForEvent(
	t *testing.T,
	match func(syncdomain.EngineDebugEvent) bool,
	description string,
) {
	t.Helper()

	timer := time.NewTimer(EventWait)
	defer timer.Stop()

	for {
		select {
		case event := <-r.Events:
			if match(event) {
				return
			}
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
		}
	}
}

// WaitUntilSeenOrExit waits for a debug event while also watching the run's
// result channel. When the run returns first, the awaited event can never
// arrive, so the run's own error is reported instead of letting the wait
// expire into a timeout that says nothing about why.
//
// It returns the channel the caller reads the run's result from from then
// on: done itself, or, when the wait took the run's one result because the
// run returned with the event already recorded, a channel that holds that
// result. Reading done again after the wait took its value would wait on an
// empty channel.
func (r *DebugEventRecorder) WaitUntilSeenOrExit(
	t *testing.T,
	done <-chan error,
	match func(syncdomain.EngineDebugEvent) bool,
	description string,
) <-chan error {
	t.Helper()

	if r.FindEvent(match) {
		return done
	}

	timer := time.NewTimer(EventWait)
	defer timer.Stop()

	for {
		select {
		case event := <-r.Events:
			if match(event) {
				return done
			}
			if r.FindEvent(match) {
				return done
			}
		case err := <-done:
			if r.FindEvent(match) {
				result := make(chan error, 1)
				result <- err
				return result
			}

			require.FailNow(t, "run returned before debug event",
				"%s: run returned %v", description, err)
			return done
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
			return done
		}
	}
}

func (r *DebugEventRecorder) WaitUntilSeen(
	t *testing.T,
	match func(syncdomain.EngineDebugEvent) bool,
	description string,
) {
	t.Helper()

	if r.FindEvent(match) {
		return
	}

	timer := time.NewTimer(EventWait)
	defer timer.Stop()

	for {
		select {
		case event := <-r.Events:
			if match(event) {
				return
			}
			if r.FindEvent(match) {
				return
			}
		case <-timer.C:
			require.FailNow(t, "timed out waiting for debug event", description)
		}
	}
}

func (r *DebugEventRecorder) FindEvent(match func(syncdomain.EngineDebugEvent) bool) bool {
	return r.findEventIndex(match) >= 0
}

// CountEvents is how many of the events recorded so far match.
func (r *DebugEventRecorder) CountEvents(match func(syncdomain.EngineDebugEvent) bool) int {
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

func (r *DebugEventRecorder) findEventIndex(match func(syncdomain.EngineDebugEvent) bool) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := range r.history {
		if match(r.history[i]) {
			return i
		}
	}

	return -1
}

func (r *DebugEventRecorder) eventsSnapshot() []syncdomain.EngineDebugEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	events := make([]syncdomain.EngineDebugEvent, len(r.history))
	copy(events, r.history)

	return events
}

func (r *DebugEventRecorder) RequireOrderedSubsequence(
	t *testing.T,
	matches []func(syncdomain.EngineDebugEvent) bool,
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

func (r *DebugEventRecorder) RequireNoEventAfter(
	t *testing.T,
	anchor func(syncdomain.EngineDebugEvent) bool,
	forbidden func(syncdomain.EngineDebugEvent) bool,
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

func (r *DebugEventRecorder) RequireEventCount(
	t *testing.T,
	match func(syncdomain.EngineDebugEvent) bool,
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

type ManualClock struct {
	Mu     sync.Mutex // guards every field below and the state of each of Timers and Ticks
	now    time.Time
	jitter time.Duration
	// jitterFn, when set, answers Jitter instead of the fixed jitter: a
	// test that wants distinct jittered instants supplies a sequence.
	jitterFn func(maxDelay time.Duration) time.Duration
	// armed, when set, is told the instant of every timer armed, so a test
	// can drive the clock to it (autoAdvanceTimers).
	armed  chan<- time.Time
	Timers []*manualSyncTimer
	Ticks  []*manualSyncTicker
}

type manualSyncTimer struct {
	clock   *ManualClock
	at      time.Time
	Fn      func()
	stopped bool
	fired   bool
}

type manualSyncTicker struct {
	clock    *ManualClock
	ch       chan time.Time
	Interval time.Duration
	next     time.Time
	stopped  bool
}

func NewManualClock(start time.Time) *ManualClock {
	return &ManualClock{now: start}
}

func (c *ManualClock) Now() time.Time {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	return c.now
}

func (c *ManualClock) SetJitter(delay time.Duration) {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	c.jitter = delay
}

// SetJitterFunc makes Jitter answer from fn, called under the clock's lock.
func (c *ManualClock) SetJitterFunc(fn func(maxDelay time.Duration) time.Duration) {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	c.jitterFn = fn
}

// AutoAdvanceTimers moves the clock to every timer armed from now on, as it
// is armed, until the test ends: for a run that waits on its own timers in
// virtual time (a one-shot pass parking jittered batch members) with nobody
// else to advance the clock. The clock only ever moves forward.
func (c *ManualClock) AutoAdvanceTimers(t *testing.T) {
	t.Helper()

	armed := make(chan time.Time, 1024)
	c.Mu.Lock()
	c.armed = armed
	c.Mu.Unlock()

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
		c.Mu.Lock()
		c.armed = nil
		c.Mu.Unlock()
		ReceiveWithin(t, done, "the manual clock's advancing goroutine to stop")
	})
}

// AdvanceTo moves the clock forward to at; an instant already passed moves
// nothing.
func (c *ManualClock) AdvanceTo(at time.Time) {
	c.Mu.Lock()
	delay := at.Sub(c.now)
	c.Mu.Unlock()
	if delay > 0 {
		c.Advance(delay)
	}
}

func (c *ManualClock) AfterFunc(delay time.Duration, fn func()) syncdomain.SyncTimer {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	timer := &manualSyncTimer{
		clock: c,
		at:    c.now.Add(delay),
		Fn:    fn,
	}
	c.Timers = append(c.Timers, timer)
	if c.armed != nil {
		select {
		case c.armed <- timer.at:
		default:
		}
	}

	return timer
}

func (c *ManualClock) NewTicker(interval time.Duration) syncdomain.SyncTicker {
	if interval <= 0 {
		return nil
	}

	c.Mu.Lock()
	defer c.Mu.Unlock()

	ticker := &manualSyncTicker{
		clock:    c,
		ch:       make(chan time.Time, 16),
		Interval: interval,
		next:     c.now.Add(interval),
	}
	c.Ticks = append(c.Ticks, ticker)

	return ticker
}

func (c *ManualClock) Sleep(ctx context.Context, delay time.Duration) error {
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

// Go starts fn on a new goroutine, as the real clock does: a manual clock
// controls time, not which goroutine runs.
func (c *ManualClock) Go(fn func()) {
	go fn()
}

func (c *ManualClock) Jitter(maxDelay time.Duration) time.Duration {
	c.Mu.Lock()
	defer c.Mu.Unlock()

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

func (c *ManualClock) Advance(delay time.Duration) {
	c.Mu.Lock()
	c.now = c.now.Add(delay)
	now := c.now

	var timers []func()
	for _, timer := range c.Timers {
		if timer == nil || timer.stopped || timer.fired || timer.at.After(now) {
			continue
		}
		timer.fired = true
		timers = append(timers, timer.Fn)
	}

	for _, ticker := range c.Ticks {
		if ticker == nil || ticker.stopped {
			continue
		}
		for !ticker.next.After(now) {
			select {
			case ticker.ch <- ticker.next:
			default:
			}
			ticker.next = ticker.next.Add(ticker.Interval)
		}
	}
	c.Mu.Unlock()

	for _, fn := range timers {
		if fn != nil {
			fn()
		}
	}
}

func (c *ManualClock) ActiveTimerCount() int {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	count := 0
	for _, timer := range c.Timers {
		if timer == nil || timer.stopped || timer.fired {
			continue
		}
		count++
	}

	return count
}

func (c *ManualClock) HasPendingTimerAt(at time.Time) bool {
	c.Mu.Lock()
	defer c.Mu.Unlock()

	for _, timer := range c.Timers {
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

	t.clock.Mu.Lock()
	defer t.clock.Mu.Unlock()

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

	t.clock.Mu.Lock()
	defer t.clock.Mu.Unlock()

	t.stopped = true
}

// DeltaPageWithItems returns a DeltaPage with the given items and a delta link.
func DeltaPageWithItems(items []graph.Item, deltaLink string) *graph.DeltaPage {
	return &graph.DeltaPage{
		Items:     items,
		DeltaLink: deltaLink,
	}
}

// WriteLocalFile creates a file in syncRoot for local observer to find.
func WriteLocalFile(t *testing.T, syncRoot, relPath, content string) {
	t.Helper()

	absPath := filepath.Join(syncRoot, relPath)
	require.NoError(t, os.MkdirAll(filepath.Dir(absPath), 0o750), "MkdirAll")
	require.NoError(t, os.WriteFile(absPath, []byte(content), 0o600), "WriteFile")
}

// TestQuickXor is the QuickXorHash a faithful server reports for content,
// for fixtures built outside a test's scope.
func TestQuickXor(content string) string {
	h := quickxorhash.New()
	h.Write([]byte(content))

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func HashContentQuickXor(t *testing.T, content string) string {
	t.Helper()

	h := quickxorhash.New()
	_, err := h.Write([]byte(content))
	require.NoError(t, err, "quickxorhash.Write")

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// ObservingClock decorates a SyncClock so a test can observe when the runtime
// creates a ticker, arms a timer, or sleeps. It wraps rather than replaces:
// the underlying clock still supplies the behavior, so observation cannot
// produce the incoherent mix of real and fake time that separate injectable
// function fields allowed.
type ObservingClock struct {
	syncdomain.SyncClock
	OnNewTicker func(time.Duration)
	OnAfterFunc func(time.Duration)
	OnSleep     func(time.Duration)
}

// NewTicker and AfterFunc register with the wrapped clock before signaling.
// A test that waits for the signal and then advances a manual clock must find
// the ticker or timer already registered; signaling first let the advance
// land in the gap, so the tick was never delivered and the test timed out
// under -race -count=50 load (TestRunWatch_FallbackSleepHonorsCancellation).
func (c *ObservingClock) NewTicker(interval time.Duration) syncdomain.SyncTicker {
	ticker := c.SyncClock.NewTicker(interval)
	if c.OnNewTicker != nil {
		c.OnNewTicker(interval)
	}

	return ticker
}

func (c *ObservingClock) AfterFunc(delay time.Duration, fn func()) syncdomain.SyncTimer {
	timer := c.SyncClock.AfterFunc(delay, fn)
	if c.OnAfterFunc != nil {
		c.OnAfterFunc(delay)
	}

	return timer
}

func (c *ObservingClock) Sleep(ctx context.Context, delay time.Duration) error {
	if c.OnSleep != nil {
		c.OnSleep(delay)
	}

	//nolint:wrapcheck // decorator delegates to the wrapped clock; adding context here would attribute the failure to the test seam rather than the clock that produced it
	return c.SyncClock.Sleep(ctx, delay)
}

const EngineTestDriveID = "0000000000000001"

// TestDriveID is the canonical drive ID used by engine tests.
const TestDriveID = synctest.TestDriveID

// EventWait is how long a test waits for an event that must come: a debug
// event, an observer's change, a pass's result, a goroutine's return. A wait
// returns the moment its event arrives, so a passing test never spends it; it
// only bounds how long a broken one hangs. It is long because events that take
// milliseconds alone took over ten seconds on a loaded two-core host under the
// race detector, and a ten-second wait failed tests there that were correct.
// A test never shows that something did not happen by letting a wait run out:
// it waits for the signal that the input was handled, then checks.
const EventWait = time.Minute

// EmptyBaseline returns a baseline with no entries.
func EmptyBaseline() *syncdomain.BaselineSnapshot {
	return syncdomain.NewBaselineSnapshot(nil)
}

// BaselineWith returns a baseline holding the given entries.
func BaselineWith(entries ...*syncdomain.BaselineEntry) *syncdomain.BaselineSnapshot {
	return syncdomain.NewBaselineSnapshot(entries)
}

// NewBaselineForTest returns a baseline holding the given entries.
func NewBaselineForTest(entries []*syncdomain.BaselineEntry) *syncdomain.BaselineSnapshot {
	return syncdomain.NewBaselineSnapshot(entries)
}

// TestLogger returns a *slog.Logger wired to t.Log for clean test output.
func TestLogger(t *testing.T) *slog.Logger {
	t.Helper()
	return synctest.TestLogger(t)
}

func NewTestLogger(tb testing.TB) *slog.Logger {
	tb.Helper()
	return synctest.TestLogger(tb)
}

func SetTestDirPermissions(t *testing.T, path string, perms os.FileMode) {
	t.Helper()

	require.NoError(t, os.Chmod(path, perms))
}

func MustOpenSyncTree(t *testing.T, path string) *synctree.Root {
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
	closeOne     sync.Once
	failOne      sync.Once
	failCh       chan struct{}
	addedPaths   []string
	failedPaths  []string
	RemovedPaths []string
}

func NewEnospcWatcher(failAfter int) *enospcWatcher {
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
	w.RemovedPaths = append(w.RemovedPaths, name)
	return nil
}

func (w *enospcWatcher) Events() <-chan fsnotify.Event { return w.events }

func (w *enospcWatcher) Errors() <-chan error { return w.errs }

func (w *enospcWatcher) Failures() <-chan struct{} { return w.failCh }

func (w *enospcWatcher) Close() error {
	w.closeOne.Do(func() { close(w.events); close(w.errs) })

	return nil
}

// SkipInShort marks an exhaustive test: a breadth, replay, randomized or
// many-pass variant of behavior that targeted tests in this package already
// cover. `go test -short` (the `verify quick` and `verify platform` profiles,
// which are the per-PR CI lane) skips it; the full profiles (`verify default`
// and `verify public`, the local gate and nightly CI) run it. Call it before
// t.Parallel so the skip costs nothing.
func SkipInShort(tb testing.TB) {
	tb.Helper()

	if testing.Short() {
		tb.Skip("exhaustive: runs in the full verify profiles and nightly CI")
	}
}

// FullTier reports whether this test binary is the full unit tier's: one
// built with coverage, which only the full verify profiles ask for
// (`verify default` and `verify public`, which nightly CI runs; their
// `go test -race -coverpkg=./... -coverprofile`, devtool's runUnitTests). An
// exhaustive test whose full breadth costs more than a per-PR run can carry
// runs all of it there and a fixed sample of it in every other non-short run:
// the race profile on every push to main, the stress lane and a plain
// `go test`. `go test -cover` runs the whole of it by hand.
func FullTier() bool {
	return testing.CoverMode() != ""
}

// SeedVars names the environment variables of one seeded model family: one
// seed, which wins over the other two, or a run of count consecutive seeds
// from start.
type SeedVars struct {
	Single string
	Count  string
	Start  string
}

// SetIn reports whether getenv holds any of the family's variables.
func (v SeedVars) SetIn(getenv func(string) string) bool {
	return getenv(v.Single) != "" || getenv(v.Count) != "" || getenv(v.Start) != ""
}

// SeedsFromEnv returns the seeds of one run of a seeded model family: the one
// seed its single variable names, else count consecutive seeds from start,
// defaultCount and 1 where the variables are unset. A count below 1, or a
// value that does not parse, fails the test with one sentence that names the
// variable and its value.
func SeedsFromEnv(tb testing.TB, vars SeedVars, defaultCount int) []uint64 {
	tb.Helper()

	seeds, err := SeedRun(os.Getenv, vars, defaultCount)
	require.NoError(tb, err)

	return seeds
}

// SeedRun is the rule of seedsFromEnv over getenv.
func SeedRun(getenv func(string) string, vars SeedVars, defaultCount int) ([]uint64, error) {
	if raw := getenv(vars.Single); raw != "" {
		seed, err := parseSeedVar(vars.Single, raw)
		if err != nil {
			return nil, err
		}

		return []uint64{seed}, nil
	}
	count := defaultCount
	if raw := getenv(vars.Count); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("%s=%q is not a whole number of seeds", vars.Count, raw)
		}
		count = parsed
	}
	if count < 1 {
		return nil, fmt.Errorf("%s=%d is below 1, and a seeded run takes at least one seed", vars.Count, count)
	}
	start := uint64(1)
	if raw := getenv(vars.Start); raw != "" {
		parsed, err := parseSeedVar(vars.Start, raw)
		if err != nil {
			return nil, err
		}
		start = parsed
	}
	if start > math.MaxUint64-uint64(count-1) {
		return nil, fmt.Errorf("%s=%d with %d seeds runs past the largest seed", vars.Start, start, count)
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

// SeedReplayPrefix starts the line a failed seed logs. devtool reads the
// command after it from a go test log, raw or -json, for the nightly report's
// issue, the nightly check's key and `verify failed-test`.
const SeedReplayPrefix = "seed-replay: "

// seedReplayPackage is the package a replay of this test binary's seeds runs,
// from the module root: go test runs a package's tests in its directory, and
// the directory's path below the one holding go.mod names the package.
func seedReplayPackage() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for root := dir; ; root = filepath.Dir(root) {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			rel, relErr := filepath.Rel(root, dir)
			if relErr != nil {
				return "."
			}

			return "./" + filepath.ToSlash(rel)
		}
		if filepath.Dir(root) == root {
			return "."
		}
	}
}

// seedReplayTB is what logSeedReplayOnFailure needs of a test.
type seedReplayTB interface {
	Helper()
	Name() string
	Failed() bool
	Cleanup(cleanup func())
	Logf(format string, args ...any)
}

// SeedAssignment is one environment assignment in front of a replay command.
type SeedAssignment struct {
	Name  string
	Value string
}

// LogSeedReplayOnFailure registers a cleanup that, when the seed's test
// failed, logs the one command that reruns it:
//
//	seed-replay: <ENV>=<seed> [ONEDRIVE_GO_DISPATCH_ORDER=<order>] go test [-race] -run '^<TopLevelTest>$' ./<package>
//
// with -race when this test binary was built with the race detector, under
// which some families do other work for one seed, and the dispatch order when
// the run had one, which replaces the seed's own shuffle (seedReplayInputs).
// Call it first in the seed's subtest: cleanups run last-registered first, so
// the line ends what the subtest logs. A test that go test's deadline ends
// runs no cleanup and prints no line; devtool then builds the replay from the
// subtest's name, seed=<seed>.
func LogSeedReplayOnFailure(t seedReplayTB, envName string, seed uint64) {
	t.Helper()

	assignments := seedReplayInputs(os.Getenv, envName, seed)
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		test, _, _ := strings.Cut(t.Name(), "/")
		t.Logf("%s", SeedReplayLine(test, seedReplayPackage(), RaceDetectorEnabled, assignments))
	})
}

// seedReplayInputs are the assignments of a seed's replay line, read through
// getenv: the seed in envName, then every other input that changes what the
// seed does and was set, which is the dispatch order alone.
func seedReplayInputs(getenv func(string) string, envName string, seed uint64) []SeedAssignment {
	assignments := []SeedAssignment{{Name: envName, Value: strconv.FormatUint(seed, 10)}}
	if order := getenv(DispatchOrderEnv); order != "" {
		assignments = append(assignments, SeedAssignment{Name: DispatchOrderEnv, Value: order})
	}

	return assignments
}

// SeedReplayLine is the replay line of a seed of test: the assignments, then
// go test of exactly that top-level test in pkg.
func SeedReplayLine(test, pkg string, race bool, assignments []SeedAssignment) string {
	var b strings.Builder
	b.WriteString(SeedReplayPrefix)
	for _, assignment := range assignments {
		b.WriteString(assignment.Name + "=" + assignment.Value + " ")
	}
	b.WriteString("go test ")
	if race {
		b.WriteString("-race ")
	}
	b.WriteString("-run '^" + test + "$' " + pkg)

	return b.String()
}
