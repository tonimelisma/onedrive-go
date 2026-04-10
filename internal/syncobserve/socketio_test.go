package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

type mockSocketIOEndpointFetcher struct {
	mu        sync.Mutex
	endpoints []*graph.SocketIOEndpoint
	calls     int
}

func (m *mockSocketIOEndpointFetcher) SocketIOEndpoint(_ context.Context, _ driveid.ID) (*graph.SocketIOEndpoint, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.calls++
	if len(m.endpoints) == 0 {
		return nil, assert.AnError
	}

	idx := m.calls - 1
	if idx >= len(m.endpoints) {
		idx = len(m.endpoints) - 1
	}

	return m.endpoints[idx], nil
}

func (m *mockSocketIOEndpointFetcher) CallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.calls
}

func newSocketIOTestServer(
	t *testing.T,
	handler func(*websocket.Conn, *http.Request) error,
) *httptest.Server {
	t.Helper()

	handlerErrs := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != socketIOPath {
			http.NotFound(w, r)
			return
		}

		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			select {
			case handlerErrs <- err:
			default:
			}
			return
		}
		defer conn.Close(websocket.StatusNormalClosure, "test done")

		if err := handler(conn, r); err != nil {
			select {
			case handlerErrs <- err:
			default:
			}
		}
	}))

	t.Cleanup(func() {
		server.Close()

		select {
		case err := <-handlerErrs:
			require.NoError(t, err)
		default:
		}
	})

	return server
}

func writeSocketIOText(ctx context.Context, conn *websocket.Conn, packet string) error {
	writeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := conn.Write(writeCtx, websocket.MessageText, []byte(packet)); err != nil {
		return fmt.Errorf("write websocket text %q: %w", packet, err)
	}

	return nil
}

func readSocketIOText(ctx context.Context, conn *websocket.Conn) (string, error) {
	readCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	typ, payload, err := conn.Read(readCtx)
	if err != nil {
		return "", fmt.Errorf("read websocket text: %w", err)
	}
	if typ != websocket.MessageText {
		return "", fmt.Errorf("read websocket text: got message type %v", typ)
	}

	return string(payload), nil
}

func expectSocketIOText(ctx context.Context, conn *websocket.Conn, want string) error {
	got, err := readSocketIOText(ctx, conn)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("read websocket text: got %q want %q", got, want)
	}

	return nil
}

// Validates: R-2.8.5
func TestSocketIOWakeSource_NotificationTriggersWake(t *testing.T) {
	t.Parallel()

	holdConn := make(chan struct{})
	server := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if got := r.URL.Query().Get("EIO"); got != "4" {
			return fmt.Errorf("query EIO: got %q want %q", got, "4")
		}
		if got := r.URL.Query().Get("transport"); got != "websocket" {
			return fmt.Errorf("query transport: got %q want %q", got, "websocket")
		}
		if got := r.URL.Query().Get("snthgk"); got != "secret-token" {
			return fmt.Errorf("query snthgk: got %q want %q", got, "secret-token")
		}
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-1","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		if err := writeSocketIOText(r.Context(), conn, `42["notification",{"sequence":1}]`); err != nil {
			return err
		}
		<-holdConn
		return nil
	})

	fetcher := &mockSocketIOEndpointFetcher{
		endpoints: []*graph.SocketIOEndpoint{{
			ID:              "endpoint-1",
			NotificationURL: graph.SocketIONotificationURL(server.URL + "/callback?snthgk=secret-token"),
		}},
	}

	var lifecycleEvents []SocketIOLifecycleEvent
	var lifecycleMu sync.Mutex
	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger:     synctest.TestLogger(t),
		BackoffMax: 10 * time.Millisecond,
		LifecycleHook: func(event SocketIOLifecycleEvent) {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			lifecycleEvents = append(lifecycleEvents, event)
		},
	})

	wakes := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, wakes)
	}()

	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "expected wake signal from socket.io notification")
	}

	cancel()
	close(holdConn)
	require.NoError(t, <-done)
	assert.Equal(t, 1, fetcher.CallCount())
	assert.True(t, containsLifecycleEvent(lifecycleEvents, SocketIOLifecycleEventConnected))
	assert.True(t, containsLifecycleEvent(lifecycleEvents, SocketIOLifecycleEventNotificationWake))
}

// Validates: R-2.8.5
func TestSocketIOWakeSource_ReconnectsAfterDisconnect(t *testing.T) {
	t.Parallel()

	firstServer := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-1","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		return nil
	})

	holdSecond := make(chan struct{})
	secondServer := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-2","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		if err := writeSocketIOText(r.Context(), conn, `42/notifications,["notification",{"sequence":2}]`); err != nil {
			return err
		}
		<-holdSecond
		return nil
	})

	fetcher := &mockSocketIOEndpointFetcher{
		endpoints: []*graph.SocketIOEndpoint{
			{ID: "endpoint-1", NotificationURL: graph.SocketIONotificationURL(firstServer.URL + "/callback?snthgk=first")},
			{ID: "endpoint-2", NotificationURL: graph.SocketIONotificationURL(secondServer.URL + "/callback?snthgk=second")},
		},
	}

	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger:     synctest.TestLogger(t),
		BackoffMax: 10 * time.Millisecond,
	})

	wakes := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, wakes)
	}()

	select {
	case <-wakes:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "expected wake after reconnect")
	}

	cancel()
	close(holdSecond)
	require.NoError(t, <-done)
	assert.GreaterOrEqual(t, fetcher.CallCount(), 2, "disconnect should force endpoint refetch and reconnect")
}

// Validates: R-2.8.5
func TestSocketIOWakeSource_RefreshesEndpointBeforeExpiry(t *testing.T) {
	t.Parallel()

	const (
		refreshLeadTime   = 100 * time.Millisecond
		firstEndpointTTL  = 500 * time.Millisecond
		testWaitTimeout   = 5 * time.Second
		longLivedEndpoint = time.Hour
	)

	firstReleased := make(chan struct{})
	firstServer := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-1","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		<-firstReleased
		return nil
	})

	holdSecond := make(chan struct{})
	secondServer := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-2","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		if err := writeSocketIOText(r.Context(), conn, `42["notification",{"sequence":3}]`); err != nil {
			return err
		}
		<-holdSecond
		return nil
	})

	now := time.Now()
	fetcher := &mockSocketIOEndpointFetcher{
		endpoints: []*graph.SocketIOEndpoint{
			{
				ID:              "endpoint-1",
				NotificationURL: graph.SocketIONotificationURL(firstServer.URL + "/callback?snthgk=first"),
				ExpirationTime:  now.Add(firstEndpointTTL),
			},
			{
				ID:              "endpoint-2",
				NotificationURL: graph.SocketIONotificationURL(secondServer.URL + "/callback?snthgk=second"),
				ExpirationTime:  now.Add(longLivedEndpoint),
			},
		},
	}

	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger:          synctest.TestLogger(t),
		RefreshLeadTime: refreshLeadTime,
		BackoffMax:      10 * time.Millisecond,
	})

	wakes := make(chan struct{}, 1)
	ctx, cancel := context.WithTimeout(t.Context(), testWaitTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, wakes)
	}()

	select {
	case <-wakes:
	case <-time.After(testWaitTimeout):
		require.FailNow(t, "expected wake after endpoint refresh")
	}

	cancel()
	close(firstReleased)
	close(holdSecond)
	require.NoError(t, <-done)
	assert.GreaterOrEqual(t, fetcher.CallCount(), 2, "expiry should trigger endpoint refresh")
}

func TestSocketIOWakeSource_EndpointFetchFailureEmitsLifecycleEvent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var events []SocketIOLifecycleEvent
	source := NewSocketIOWakeSourceWithOptions(&mockSocketIOEndpointFetcher{}, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger: synctest.TestLogger(t),
		SleepFunc: func(_ context.Context, _ time.Duration) error {
			cancel()
			return context.Canceled
		},
		LifecycleHook: func(event SocketIOLifecycleEvent) {
			events = append(events, event)
		},
	})

	err := source.Run(ctx, make(chan struct{}, 1))
	require.NoError(t, err)
	assert.True(t, containsLifecycleEvent(events, SocketIOLifecycleEventEndpointFetchFail))
	assert.True(t, containsLifecycleEvent(events, SocketIOLifecycleEventStopped))
}

func TestSocketIOWakeSource_ConnectFailureEmitsLifecycleEvent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	fetcher := &mockSocketIOEndpointFetcher{
		endpoints: []*graph.SocketIOEndpoint{{
			ID:              "endpoint-1",
			NotificationURL: graph.SocketIONotificationURL("https://example.test/callback"),
		}},
	}

	var events []SocketIOLifecycleEvent
	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger: synctest.TestLogger(t),
		DialFunc: func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
			return nil, nil, errors.New("dial failed")
		},
		SleepFunc: func(_ context.Context, _ time.Duration) error {
			cancel()
			return context.Canceled
		},
		LifecycleHook: func(event SocketIOLifecycleEvent) {
			events = append(events, event)
		},
	})

	err := source.Run(ctx, make(chan struct{}, 1))
	require.NoError(t, err)
	assert.True(t, containsLifecycleEvent(events, SocketIOLifecycleEventConnectFail))
	assert.True(t, containsLifecycleEvent(events, SocketIOLifecycleEventStopped))
}

func TestSocketIOWakeSource_PingPongHandling(t *testing.T) {
	t.Parallel()

	holdConn := make(chan struct{})
	server := newSocketIOTestServer(t, func(conn *websocket.Conn, r *http.Request) error {
		if err := writeSocketIOText(r.Context(), conn, `0{"sid":"sid-1","pingInterval":25000,"pingTimeout":60000}`); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "40/notifications"); err != nil {
			return err
		}
		if err := writeSocketIOText(r.Context(), conn, "2"); err != nil {
			return err
		}
		if err := expectSocketIOText(r.Context(), conn, "3"); err != nil {
			return err
		}
		<-holdConn
		return nil
	})

	fetcher := &mockSocketIOEndpointFetcher{
		endpoints: []*graph.SocketIOEndpoint{{
			ID:              "endpoint-1",
			NotificationURL: graph.SocketIONotificationURL(server.URL + "/callback"),
		}},
	}

	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger: synctest.TestLogger(t),
	})

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- source.Run(ctx, make(chan struct{}, 1))
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()
	close(holdConn)
	require.NoError(t, <-done)
}

func containsLifecycleEvent(events []SocketIOLifecycleEvent, target SocketIOLifecycleEventType) bool {
	for _, event := range events {
		if event.Type == target {
			return true
		}
	}

	return false
}
