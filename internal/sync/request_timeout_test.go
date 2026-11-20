package sync

import (
	"context"
	"errors"
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
	"github.com/tonimelisma/onedrive-go/internal/synctest"
)

// stalledGraphRequestError is the error graph returns for one item request
// to a stalled server through kind's transport.
func stalledGraphRequestError(t *testing.T, kind stalledRequestKind) error {
	t.Helper()

	server, reached := newStalledServer(t)
	client, dispatched := stalledGraphClient(t, server, kind.client(t))

	_, err := client.GetItem(t.Context(), driveid.New(engineTestDriveID), "stalled-item")
	require.Error(t, err)
	assert.Equal(t, int64(1), dispatched.Load(), "one dispatch is one request")
	if !kind.reachesServer {
		assert.Zero(t, reached.Load(), "the dial gave up before a connection existed")
	}

	return fmt.Errorf("get the stalled item: %w", err)
}

// Every transport timeout reaches sync carrying context.DeadlineExceeded
// while the run goes on: it is a request that timed out, kept as retry work
// with its issue, not a shutdown. Once the run's context has ended the same
// error is the shutdown's own.
//
// Validates: R-6.8.29, R-6.8.10
func TestClassifyResult_TransportTimeoutIsARequestTimeoutUntilTheRunEnds(t *testing.T) {
	t.Parallel()

	for _, kind := range stalledRequestKinds() {
		t.Run(kind.name, func(t *testing.T) {
			t.Parallel()

			err := stalledGraphRequestError(t, kind)
			require.ErrorIs(t, err, context.DeadlineExceeded, "the transport's timeout has the shape of a deadline")

			live := classifyResult(&actionCompletion{Err: err, HTTPStatus: extractHTTPStatus(err)})
			assert.Equal(t, resultRequeue, live.Class, "%v", err)
			assert.Equal(t, persistRetryWork, live.Persistence)
			assert.Equal(t, issueRequestTimeout, live.ConditionType)

			ended := classifyResult(&actionCompletion{Err: err, HTTPStatus: extractHTTPStatus(err), RunEnded: true})
			assert.Equal(t, resultShutdown, ended.Class)
		})
	}
}

// Whether work failed because its run is ending is a fact about the run's
// context, never about the error: a deadline-shaped error with a live run is
// not a stop, any failure once the run has ended is, and no error is never.
//
// Validates: R-6.8.29
func TestRunEnded_AsksTheRunNotTheError(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	failure := errors.New("request failed")

	assert.False(t, runEnded(t.Context(), context.DeadlineExceeded), "a deadline while the run goes on")
	assert.False(t, runEnded(t.Context(), context.Canceled), "a cancel narrower than the run")
	assert.True(t, runEnded(ended, failure), "any failure once the run has ended")
	assert.False(t, runEnded(ended, nil), "no error is never a stop")
}

// A page of the change list whose request timed out is retried like any
// other transport failure until the page policy's attempts are used.
//
// Validates: R-2.15.5, R-6.8.29
func TestFullDelta_RetriesATimedOutPageUpToItsPolicy(t *testing.T) {
	t.Parallel()

	server, _ := newStalledServer(t)
	client, dispatched := stalledGraphClient(t, server, &http.Client{Timeout: stalledRequestTimeout})
	obs := newRemoteObserver(client, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))
	var slept []time.Duration
	obs.clock = &observerTestClock{syncClock: realClock{}, sleep: func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}}

	_, _, err := obs.FullDelta(t.Context(), "saved-token")

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

	server, _ := newStalledServer(t)
	client, dispatched := stalledGraphClient(t, server, &http.Client{Timeout: stalledRequestTimeout})
	obs := newRemoteObserver(client, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))
	policy := retry.DeltaPagePolicy()
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	sleeps := 0
	obs.clock = &observerTestClock{syncClock: realClock{}, sleep: func(sleepCtx context.Context, _ time.Duration) error {
		sleeps++
		if sleeps < policy.MaxAttempts {
			return nil
		}
		// The poll's own backoff: the observer is still watching. The test
		// stops the watch here.
		stop()
		return sleepCtx.Err()
	}}

	err := runWithin(t, ctx, "the remote watch to return", func(watchCtx context.Context) error {
		return obs.Watch(watchCtx, "saved-token", make(chan remoteObservationBatch), MinPollInterval, nil, nil)
	})

	require.NoError(t, err)
	assert.Equal(t, policy.MaxAttempts, sleeps, "the page backoffs, then the poll's backoff")
	assert.Equal(t, int64(policy.MaxAttempts), dispatched.Load())
}

// A failure to hand a poll's batch over, from the batch handler or from the
// batch's apply, ends the remote watch with that failure while the run goes
// on, even when it has the shape of a deadline. The same failure once the run
// has ended is the watch's stop.
//
// Validates: R-6.8.29
func TestRemoteObserverWatch_AFailedBatchEndsTheWatchUnlessItsRunEnded(t *testing.T) {
	t.Parallel()

	// A store write that ran out of time.
	failure := fmt.Errorf("commit observations: %w", context.DeadlineExceeded)
	for _, tc := range []struct {
		name      string
		failApply bool
		endRun    bool
	}{
		{name: "handler fails while the run goes on"},
		{name: "handler fails once the run ended", endRun: true},
		{name: "apply fails while the run goes on", failApply: true},
		{name: "apply fails once the run ended", failApply: true, endRun: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fetcher := &scriptedDeltaFetcher{answer: func(int, string) (*graph.DeltaPage, error) {
				return &graph.DeltaPage{Items: []graph.Item{cursorTestItem("a")}, DeltaLink: "token-after"}, nil
			}}
			obs := newRemoteObserver(fetcher, emptyBaseline(), driveid.New(engineTestDriveID), testLogger(t))
			runCtx, endRun := context.WithCancel(t.Context())
			defer endRun()
			handle := func(context.Context, []remoteItem, string, shortcutTopologyBatch) (remoteObservationBatch, error) {
				if tc.endRun {
					endRun()
				}
				if !tc.failApply {
					return remoteObservationBatch{}, failure
				}
				applied := make(chan error, 1)
				applied <- failure

				return remoteObservationBatch{applyAck: applied}, nil
			}

			err := runWithin(t, runCtx, "the remote watch to return", func(ctx context.Context) error {
				return obs.Watch(ctx, "saved-token", make(chan remoteObservationBatch, 1), MinPollInterval, nil, handle)
			})

			if tc.endRun {
				require.NoError(t, err, "a failure once the run has ended is the stop")
				return
			}
			require.ErrorIs(t, err, failure)
		})
	}
}

// Each cancel the worker pool and the one-shot pass make carries a cause of
// its own, and the cause answers to context.Canceled: a request it interrupts
// carries the cause in place of context.Canceled and still reads as canceled.
//
// Validates: R-6.8.29
func TestCancelCauses_AnswerToCanceledAndNameTheirCancel(t *testing.T) {
	t.Parallel()

	texts := make(map[string]string)
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{name: "action returned", cause: errActionReturned},
		{name: "worker pool stopped", cause: errWorkerPoolStopped},
		{name: "one-shot pass failed", cause: errOneShotPassFailed},
		{name: "one-shot pass ended", cause: errOneShotPassEnded},
	} {
		require.ErrorIs(t, tc.cause, context.Canceled, tc.name)
		require.NotErrorIs(t, tc.cause, context.DeadlineExceeded, tc.name)
		assert.True(t, isCancellationEcho(fmt.Errorf("request canceled: %w", tc.cause)), tc.name)
		assert.NotContains(t, texts, tc.cause.Error(), "%s has a text of its own", tc.name)
		texts[tc.cause.Error()] = tc.name
	}
}

// A watch step that failed is the stop only when the run has ended and the
// failure is that end's own cancellation coming back: a disk error during the
// stop, or a store that did not answer within its bound, is still the watch's
// error, and while the run goes on neither is a stop.
//
// Validates: R-6.8.29, R-2.8.3
func TestIsWatchShutdownError_OnlyTheEndedRunsOwnCancellation(t *testing.T) {
	t.Parallel()

	ended, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := fmt.Errorf("watch step: %w", context.Canceled)
	expired := fmt.Errorf("watch step: %w", context.DeadlineExceeded)
	diskFailure := errors.New("write sync state: disk I/O error")
	storeSilent := fmt.Errorf("record retry work: %w: %w", context.DeadlineExceeded, errStoreAccessTimedOut)

	assert.True(t, isWatchShutdownError(ended, canceled), "the cancel coming back")
	assert.True(t, isWatchShutdownError(ended, expired), "an ended context's deadline coming back")
	assert.False(t, isWatchShutdownError(ended, diskFailure), "a separate failure during the stop")
	assert.False(t, isWatchShutdownError(ended, storeSilent), "a store that did not answer within its bound")
	assert.False(t, isWatchShutdownError(t.Context(), canceled), "a cancel while the run goes on")
	assert.False(t, isWatchShutdownError(t.Context(), expired), "a timed-out request while the run goes on")
	assert.False(t, isWatchShutdownError(t.Context(), diskFailure), "a failure while the run goes on")
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
	t *testing.T, events <-chan socketIOLifecycleEvent, want socketIOLifecycleEventType,
) socketIOLifecycleEvent {
	t.Helper()

	for {
		event := receiveWithin(t, events, "the socket.io "+string(want)+" event")
		if event.Type == want {
			return event
		}
		require.NotEqual(t, socketIOLifecycleEventStopped, event.Type, "the wake source stopped before %s", want)
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
	fetcher := &mockSocketIOEndpointFetcher{endpoints: []*graph.SocketIOEndpoint{{
		ID: "endpoint-1", NotificationURL: graph.SocketIONotificationURL(server.URL + "/callback?snthgk=test"),
	}}}
	clock := newManualClock(time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC))
	events := make(chan socketIOLifecycleEvent, 32)
	dialed := make(chan *expiringConn, 4)
	source := newSocketIOWakeSourceWithOptions(fetcher, driveid.New(synctest.TestDriveID), socketIOWakeSourceOptions{
		Logger:        synctest.TestLogger(t),
		DialFunc:      expiringSocketIODial(t, dialed), //nolint:bodyclose // the wake source's connect closes the dial's response body
		Clock:         clock,
		LifecycleHook: func(event socketIOLifecycleEvent) { events <- event },
		BackoffMax:    time.Second,
	})
	cancel, done := runInBackground(t, t.Context(), func(ctx context.Context) error {
		return source.Run(ctx, make(chan struct{}, 1))
	})

	nextSocketIOEvent(t, events, socketIOLifecycleEventConnected)
	require.NoError(t, receiveWithin(t, dialed, "the first connection").expire())
	dropped := nextSocketIOEvent(t, events, socketIOLifecycleEventConnectionDropped)
	assert.Contains(t, dropped.Error, context.DeadlineExceeded.Error())
	retryAt := clock.Now().Add(dropped.Delay)
	require.Eventually(t, func() bool { return clock.HasPendingTimerAt(retryAt) }, eventWait, time.Millisecond,
		"the wake source waits out its backoff")
	clock.Advance(dropped.Delay)

	nextSocketIOEvent(t, events, socketIOLifecycleEventConnected)
	assert.Equal(t, 2, fetcher.CallCount(), "the reconnect fetches the endpoint again")
	stopWatch(t, cancel, done)
}
