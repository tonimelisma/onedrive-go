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
	socketIOLifecycleEventStarted           socketIOLifecycleEventType = "started"
	socketIOLifecycleEventEndpointFetchFail socketIOLifecycleEventType = "endpoint_fetch_failed"
	socketIOLifecycleEventConnectFail       socketIOLifecycleEventType = "connect_failed"
	socketIOLifecycleEventConnected         socketIOLifecycleEventType = "connected"
	socketIOLifecycleEventRefreshRequested  socketIOLifecycleEventType = "refresh_requested"
	socketIOLifecycleEventConnectionDropped socketIOLifecycleEventType = "connection_dropped"
	socketIOLifecycleEventNotificationWake  socketIOLifecycleEventType = "notification_wake"
	socketIOLifecycleEventWakeCoalesced     socketIOLifecycleEventType = "wake_coalesced"
	socketIOLifecycleEventStopped           socketIOLifecycleEventType = "stopped"
)

type socketIOLifecycleEvent struct {
	Type       socketIOLifecycleEventType
	DriveID    string
	EndpointID string
	SID        string
	Delay      time.Duration
	Note       string
	Error      string
}

type socketIOWakeSourceOptions struct {
	Logger   *slog.Logger
	DialFunc func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	// Clock paces the source: the endpoint refresh deadline and timer and
	// the reconnect backoff sleeps. Nil means wall time. One clock rather
	// than separate now and sleep hooks, so a test cannot advance one while
	// the refresh timer keeps running on the other.
	Clock            syncClock
	LifecycleHook    func(socketIOLifecycleEvent)
	HandshakeTimeout time.Duration
	RefreshLeadTime  time.Duration
	RefreshInterval  time.Duration
	BackoffMax       time.Duration
}

type socketIOConn struct {
	conn *websocket.Conn
}

// socketIOWakeSource owns the outbound Socket.IO/WebSocket lifecycle used to
// wake the remote delta observer. It never interprets change payloads as truth;
// every notification is reduced to a coalesced wake signal.
type socketIOWakeSource struct {
	fetcher          socketIOEndpointFetcher
	driveID          driveid.ID
	logger           *slog.Logger
	dialFunc         func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	clock            syncClock
	lifecycleHook    func(socketIOLifecycleEvent)
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

func newSocketIOWakeSourceWithOptions(
	fetcher socketIOEndpointFetcher,
	driveID driveid.ID,
	opts socketIOWakeSourceOptions,
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
		clock = realClock{}
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
// in the background.
func (s *socketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
	if s.fetcher == nil {
		return nil
	}

	s.emitLifecycleEvent(socketIOLifecycleEvent{Type: socketIOLifecycleEventStarted})
	defer s.emitLifecycleEvent(socketIOLifecycleEvent{Type: socketIOLifecycleEventStopped})

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
			socketIOLifecycleEventEndpointFetchFail,
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
			socketIOLifecycleEventConnectFail,
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
	s.emitLifecycleEvent(socketIOLifecycleEvent{
		Type:       socketIOLifecycleEventConnected,
		EndpointID: endpoint.ID,
		SID:        sid,
		Note:       "socket.io connected",
	})

	bo.Reset()
	runErr := s.runConnection(ctx, conn, endpoint.ID, refreshAt, wakes)
	if closeErr := conn.conn.Close(websocket.StatusNormalClosure, "watch stop"); closeErr != nil && !watchShouldStop(ctx, closeErr) {
		// Debug: closing an already broken connection; nothing to do.
		s.logger.Debug("socket.io close failed",
			slog.String("drive_id", s.driveID.String()),
			slog.Any("error", closeErr),
		)
	}

	stop := ctx.Err() != nil || watchShouldStop(ctx, runErr)
	if stop {
		return true, nil
	}
	if errors.Is(runErr, errSocketIORefreshRequired) {
		s.logger.Info("socket.io endpoint refresh requested",
			slog.String("drive_id", s.driveID.String()),
		)
		s.emitLifecycleEvent(socketIOLifecycleEvent{
			Type:       socketIOLifecycleEventRefreshRequested,
			EndpointID: endpoint.ID,
		})
		return false, nil
	}

	return s.retryAfterError(
		ctx,
		bo,
		socketIOLifecycleEventConnectionDropped,
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
	s.emitLifecycleEvent(socketIOLifecycleEvent{
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

	stop := watchShouldStop(ctx, sleepErr)
	if stop {
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

func (s *socketIOWakeSource) emitLifecycleEvent(event socketIOLifecycleEvent) {
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
