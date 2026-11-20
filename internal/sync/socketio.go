package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/internal/graph"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	socketIOHandshakeTimeout        = 10 * time.Second
	socketIORefreshLeadTime         = 2 * time.Minute
	socketIOFallbackRefreshInterval = 30 * time.Minute
	socketIOMaxBackoff              = time.Minute
	socketIOReadLimit               = 1 << 20
	socketIOPath                    = "/socket.io/"
)

var errSocketIORefreshRequired = errors.New("sync: socket.io endpoint refresh required")

type socketIOLifecycleEventType string

const (
	SocketIOLifecycleEventStarted           socketIOLifecycleEventType = "started"
	SocketIOLifecycleEventEndpointFetchFail socketIOLifecycleEventType = "endpoint_fetch_failed"
	SocketIOLifecycleEventConnectFail       socketIOLifecycleEventType = "connect_failed"
	SocketIOLifecycleEventConnected         socketIOLifecycleEventType = "connected"
	SocketIOLifecycleEventRefreshRequested  socketIOLifecycleEventType = "refresh_requested"
	SocketIOLifecycleEventConnectionDropped socketIOLifecycleEventType = "connection_dropped"
	SocketIOLifecycleEventNotificationWake  socketIOLifecycleEventType = "notification_wake"
	SocketIOLifecycleEventWakeCoalesced     socketIOLifecycleEventType = "wake_coalesced"
	SocketIOLifecycleEventStopped           socketIOLifecycleEventType = "stopped"
)

type SocketIOLifecycleEvent struct {
	Type       socketIOLifecycleEventType
	DriveID    string
	EndpointID string
	SID        string
	Delay      time.Duration
	Note       string
	Error      string
}

type SocketIOWakeSourceOptions struct {
	Logger   *slog.Logger
	DialFunc func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	// Clock paces the source: the endpoint refresh deadline and timer and
	// the reconnect backoff sleeps. Nil means wall time. One clock rather
	// than separate now and sleep hooks, so a test cannot advance one while
	// the refresh timer keeps running on the other.
	Clock            SyncClock
	LifecycleHook    func(SocketIOLifecycleEvent)
	HandshakeTimeout time.Duration
	RefreshLeadTime  time.Duration
	RefreshInterval  time.Duration
	BackoffMax       time.Duration
}

type socketIOConn struct {
	conn *websocket.Conn
	// readerDone is closed as the connection's reader returns. runConnection
	// creates it and waits for it, so no reader outlives its connection.
	readerDone chan struct{}
}

// socketIOWakeSource owns the outbound Socket.IO/WebSocket lifecycle used to
// wake the remote delta observer. It never interprets change payloads as truth;
// every notification is reduced to a coalesced wake signal.
type socketIOWakeSource struct {
	fetcher          SocketIOEndpointFetcher
	driveID          driveid.ID
	logger           *slog.Logger
	dialFunc         func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	clock            SyncClock
	lifecycleHook    func(SocketIOLifecycleEvent)
	handshakeTimeout time.Duration
	refreshLeadTime  time.Duration
	refreshInterval  time.Duration
	backoffMax       time.Duration

	// retryReported is set once a retry has been reported at Info and
	// cleared when a connection succeeds, so a long outage logs one Info
	// line plus Debug repeats instead of a line per backoff step. Run is
	// the only goroutine that touches it.
	retryReported bool
}

func NewSocketIOWakeSourceWithOptions(
	fetcher SocketIOEndpointFetcher,
	driveID driveid.ID,
	opts SocketIOWakeSourceOptions,
) *socketIOWakeSource {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dialFunc := opts.DialFunc
	if dialFunc == nil {
		dialFunc = websocket.Dial
	}

	clock := opts.Clock
	if clock == nil {
		clock = RealClock{}
	}

	handshakeTimeout := opts.HandshakeTimeout
	if handshakeTimeout == 0 {
		handshakeTimeout = socketIOHandshakeTimeout
	}

	refreshLeadTime := opts.RefreshLeadTime
	if refreshLeadTime == 0 {
		refreshLeadTime = socketIORefreshLeadTime
	}

	refreshInterval := opts.RefreshInterval
	if refreshInterval == 0 {
		refreshInterval = socketIOFallbackRefreshInterval
	}

	backoffMax := opts.BackoffMax
	if backoffMax == 0 {
		backoffMax = socketIOMaxBackoff
	}

	return &socketIOWakeSource{
		fetcher:          fetcher,
		driveID:          driveID,
		logger:           logger,
		dialFunc:         dialFunc,
		clock:            clock,
		lifecycleHook:    opts.LifecycleHook,
		handshakeTimeout: handshakeTimeout,
		refreshLeadTime:  refreshLeadTime,
		refreshInterval:  refreshInterval,
		backoffMax:       backoffMax,
	}
}

// Run maintains the Socket.IO connection until ctx is canceled. Connection
// failures degrade silently to fallback polling while the wake source retries
// in the background; a panic of the connection's reader ends Run with it.
func (s *socketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
	if s.fetcher == nil {
		return nil
	}

	s.emitLifecycleEvent(SocketIOLifecycleEvent{Type: SocketIOLifecycleEventStarted})
	defer s.emitLifecycleEvent(SocketIOLifecycleEvent{Type: SocketIOLifecycleEventStopped})

	bo := retry.NewBackoff(retry.WatchRemotePolicy())
	bo.SetMaxOverride(s.backoffMax)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		stopped, err := s.runIteration(ctx, bo, wakes)
		if err != nil {
			return err
		}
		if stopped {
			return nil
		}
	}
}

func (s *socketIOWakeSource) runIteration(
	ctx context.Context,
	bo *retry.Backoff,
	wakes chan<- struct{},
) (bool, error) {
	endpoint, err := s.fetcher.SocketIOEndpoint(ctx, s.driveID)
	if err != nil {
		return s.retryAfterError(
			ctx,
			bo,
			SocketIOLifecycleEventEndpointFetchFail,
			"socket.io endpoint fetch failed",
			err,
			"waiting to refetch endpoint",
			"",
		)
	}

	conn, refreshAt, sid, err := s.connect(ctx, endpoint)
	if err != nil {
		endpointID := ""
		if endpoint != nil {
			endpointID = endpoint.ID
		}

		return s.retryAfterError(
			ctx,
			bo,
			SocketIOLifecycleEventConnectFail,
			"socket.io connect failed",
			err,
			"waiting to reconnect",
			endpointID,
		)
	}

	if s.retryReported {
		s.retryReported = false
		s.logger.Info("Instant change notifications from OneDrive are working again")
	}
	s.emitLifecycleEvent(SocketIOLifecycleEvent{
		Type:       SocketIOLifecycleEventConnected,
		EndpointID: endpoint.ID,
		SID:        sid,
		Note:       "socket.io connected",
	})

	bo.Reset()
	runErr := s.runConnection(ctx, conn, endpoint.ID, refreshAt, wakes)

	// Only the end of the wake source's run stops it: a connection that
	// timed out while the run goes on reconnects after its backoff.
	select {
	case <-ctx.Done():
		return true, nil
	default:
	}
	if errors.Is(runErr, panicguard.ErrPanic) {
		// The connection's reader panicked. A reconnect would repeat the bug,
		// so the wake source ends and watch keeps polling.
		return false, runErr
	}
	if errors.Is(runErr, errSocketIORefreshRequired) {
		s.logger.Info("socket.io endpoint refresh requested",
			slog.String("drive_id", s.driveID.String()),
		)
		s.emitLifecycleEvent(SocketIOLifecycleEvent{
			Type:       SocketIOLifecycleEventRefreshRequested,
			EndpointID: endpoint.ID,
		})
		return false, nil
	}

	return s.retryAfterError(
		ctx,
		bo,
		SocketIOLifecycleEventConnectionDropped,
		"socket.io connection dropped",
		runErr,
		"waiting to reconnect after drop",
		endpoint.ID,
	)
}

func (s *socketIOWakeSource) retryAfterError(
	ctx context.Context,
	bo *retry.Backoff,
	eventType socketIOLifecycleEventType,
	logMessage string,
	cause error,
	waitAction string,
	endpointID string,
) (bool, error) {
	delay := bo.Next()
	s.emitLifecycleEvent(SocketIOLifecycleEvent{
		Type:       eventType,
		EndpointID: endpointID,
		Delay:      delay,
		Error:      errorString(cause),
	})
	s.logRetry(logMessage, cause, delay)

	return s.sleepUntilRetry(ctx, delay, waitAction)
}

func (s *socketIOWakeSource) refreshDeadline(endpoint *graph.SocketIOEndpoint) time.Time {
	now := s.clock.Now()
	if endpoint != nil && !endpoint.ExpirationTime.IsZero() {
		refreshAt := endpoint.ExpirationTime.Add(-s.refreshLeadTime)
		if refreshAt.Before(now) {
			return now
		}

		return refreshAt
	}

	return now.Add(s.refreshInterval)
}

func (s *socketIOWakeSource) sleepUntilRetry(ctx context.Context, delay time.Duration, action string) (bool, error) {
	sleepErr := s.clock.Sleep(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}

	if RunEnded(ctx, sleepErr) {
		return true, nil
	}

	return false, fmt.Errorf("socket.io %s: %w", action, sleepErr)
}

func (s *socketIOWakeSource) logRetry(message string, err error, delay time.Duration) {
	if err == nil {
		return
	}

	// Socket.io is only a best-effort wake source: watch keeps polling on
	// its interval whatever happens here, so a retry is never a warning.
	// The first retry of an outage is one Info line; repeats are Debug.
	fields := []any{
		slog.String("drive_id", s.driveID.String()),
		slog.Any("error", err),
		slog.Duration("retry_in", delay),
	}
	if s.retryReported {
		s.logger.Debug(message, fields...)
		return
	}

	s.retryReported = true
	s.logger.Info("Instant change notifications from OneDrive are unavailable; still checking for changes on the poll interval",
		fields...)
	s.logger.Debug(message, fields...)
}

func (s *socketIOWakeSource) emitLifecycleEvent(event SocketIOLifecycleEvent) {
	if s.lifecycleHook == nil {
		return
	}
	if event.DriveID == "" {
		event.DriveID = s.driveID.String()
	}
	s.lifecycleHook(event)
}

func errorString(err error) string {
	if err == nil {
		return ""
	}

	return err.Error()
}
