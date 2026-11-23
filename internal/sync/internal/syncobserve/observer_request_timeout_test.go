package syncobserve

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/retry"
	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// A page of the change list whose request timed out is retried like any
// other transport failure until the page policy's attempts are used.
//
// Validates: R-2.15.5, R-6.8.29
func TestFullDelta_RetriesATimedOutPageUpToItsPolicy(t *testing.T) {
	t.Parallel()

	server, _ := syncfixture.NewStalledServer(t)
	client, dispatched := syncfixture.StalledGraphClient(t, server, &http.Client{Timeout: syncfixture.StalledRequestTimeout})
	obs := NewRemoteObserver(client, syncfixture.EmptyBaseline(), driveid.New(syncfixture.EngineTestDriveID), syncfixture.TestLogger(t))
	var slept []time.Duration
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}

	_, _, err := obs.FullDelta(t.Context(), "drives/0000000000000001/root/delta?token=saved") // a saved position is a link

	require.ErrorIs(t, err, context.DeadlineExceeded)
	policy := retry.DeltaPagePolicy()
	assert.Equal(t, int64(policy.MaxAttempts), dispatched.Load(), "every attempt of the page was sent")
	assert.Len(t, slept, policy.MaxAttempts-1, "a backoff between attempts")
}

// A watch poll whose request timed out while the run goes on backs off and
// polls again.
//
// Validates: R-2.15.5, R-6.8.29
func TestRemoteObserverWatch_BacksOffAfterATimedOutPollInsteadOfStopping(t *testing.T) {
	t.Parallel()

	server, _ := syncfixture.NewStalledServer(t)
	client, dispatched := syncfixture.StalledGraphClient(t, server, &http.Client{Timeout: syncfixture.StalledRequestTimeout})
	obs := NewRemoteObserver(client, syncfixture.EmptyBaseline(), driveid.New(syncfixture.EngineTestDriveID), syncfixture.TestLogger(t))
	policy := retry.DeltaPagePolicy()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	sleeps := 0
	obs.clock = &observerTestClock{SyncClock: syncdomain.RealClock{}, sleep: func(sleepCtx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps < policy.MaxAttempts {
			return nil
		}
		// The poll's own backoff: the observer is still watching. The test
		// stops the watch here.
		stop()
		return sleepCtx.Err()
	}}

	err := syncfixture.RunWithin(t, ctx, "the remote watch to return", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "saved-token", make(chan RemoteObservationBatch), syncdomain.MinPollInterval, nil, nil)
	})

	require.NoError(t, err)
	assert.Equal(t, policy.MaxAttempts, sleeps, "the page backoffs, then the poll's backoff")
	assert.Equal(t, int64(policy.MaxAttempts), dispatched.Load())
}

// expiringConn is a connection whose reads, once it has expired, fail the way
// a read that ran out of time does: with context.DeadlineExceeded in the
// chain.
type expiringConn struct {
	net.Conn
	expired atomic.Bool
}

func (c *expiringConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil && c.expired.Load() {
		return n, fmt.Errorf("read socket.io connection: %w", context.DeadlineExceeded)
	}

	//nolint:wrapcheck // the connection's own answer, unchanged, as the websocket reads it without the wrapper
	return n, err
}

// expire ends the read in progress, and every later one, as timed out.
func (c *expiringConn) expire() error {
	c.expired.Store(true)
	if err := c.SetReadDeadline(time.Now()); err != nil {
		return fmt.Errorf("expire socket.io connection: %w", err)
	}

	return nil
}

// expiringSocketIODial dials the websocket through connections that can be
// expired, and hands each to dialed as it is made.
func expiringSocketIODial(
	t *testing.T, dialed chan<- *expiringConn,
) func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		var dialer net.Dialer
		conn, err := dialer.DialContext(ctx, network, addr)
		if err != nil {
			return nil, fmt.Errorf("dial the socket.io test server: %w", err)
		}
		expiring := &expiringConn{Conn: conn}
		select {
		case dialed <- expiring:
		default:
		}

		return expiring, nil
	}}
	t.Cleanup(transport.CloseIdleConnections)

	return func(ctx context.Context, url string, opts *websocket.DialOptions) (*websocket.Conn, *http.Response, error) {
		opts.HTTPClient = &http.Client{Transport: transport}

		return websocket.Dial(ctx, url, opts)
	}
}

// serveSocketIOUntilTheClientGoes completes the handshake and holds the
// connection until the client closes it.
func serveSocketIOUntilTheClientGoes(conn *websocket.Conn, r *http.Request) error {
	open := `0{"sid":"sid-1","pingInterval":25000,"pingTimeout":60000}`
	if err := writeSocketIOText(r.Context(), conn, open); err != nil {
		return err
	}
	for _, frame := range []string{"40", "40/notifications"} {
		if err := expectSocketIOText(r.Context(), conn, frame); err != nil {
			return err
		}
	}
	<-conn.CloseRead(r.Context()).Done()

	return nil
}

// nextSocketIOEvent returns the next lifecycle event of type want, and fails
// the test when the wake source stops first.
func nextSocketIOEvent(
	t *testing.T, events <-chan SocketIOLifecycleEvent, want socketIOLifecycleEventType,
) SocketIOLifecycleEvent {
	t.Helper()

	for {
		event := syncfixture.ReceiveWithin(t, events, "the socket.io "+string(want)+" event")
		if event.Type == want {
			return event
		}
		require.NotEqual(t, SocketIOLifecycleEventStopped, event.Type, "the wake source stopped before %s", want)
	}
}

// A Socket.IO connection that ends with a timed-out read while the run goes
// on is dropped and connected again after its backoff, not read as the wake
// source's stop.
//
// Validates: R-2.8.5, R-6.8.29
func TestSocketIOWakeSource_ReconnectsAfterAConnectionThatTimedOut(t *testing.T) {
	t.Parallel()

	server := newSocketIOTestServer(t, serveSocketIOUntilTheClientGoes)
	fetcher := &syncfixture.MockSocketIOEndpointFetcher{Endpoints: []*graph.SocketIOEndpoint{{
		ID: "endpoint-1", NotificationURL: graph.SocketIONotificationURL(server.URL + "/callback?snthgk=test"),
	}}}
	clock := syncfixture.NewManualClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	events := make(chan SocketIOLifecycleEvent, 32)
	dialed := make(chan *expiringConn, 4)
	source := NewSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), SocketIOWakeSourceOptions{
		Logger:        synctest.TestLogger(t),
		DialFunc:      expiringSocketIODial(t, dialed), //nolint:bodyclose // the wake source's connect closes the dial's response body
		Clock:         clock,
		LifecycleHook: func(event SocketIOLifecycleEvent) { events <- event },
		BackoffMax:    time.Second,
	})
	cancel, done := syncfixture.RunInBackground(t, t.Context(), func(ctx context.Context) error {
		return source.Run(ctx, make(chan struct{}, 1))
	})

	nextSocketIOEvent(t, events, SocketIOLifecycleEventConnected)
	require.NoError(t, syncfixture.ReceiveWithin(t, dialed, "the first connection").expire())
	dropped := nextSocketIOEvent(t, events, SocketIOLifecycleEventConnectionDropped)
	assert.Contains(t, dropped.Error, context.DeadlineExceeded.Error())
	retryAt := clock.Now().Add(dropped.Delay)
	require.Eventually(t, func() bool { return clock.HasPendingTimerAt(retryAt) }, syncfixture.EventWait, time.Millisecond,
		"the wake source waits out its backoff")
	clock.Advance(dropped.Delay)

	nextSocketIOEvent(t, events, SocketIOLifecycleEventConnected)
	assert.Equal(t, 2, fetcher.CallCount(), "the reconnect fetches the endpoint again")
	syncfixture.StopWatch(t, cancel, done)
}
