package syncobserve

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
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

const (
	socketIOHandshakeTimeout        = 10 * time.Second
	socketIORefreshLeadTime         = 2 * time.Minute
	socketIOFallbackRefreshInterval = 30 * time.Minute
	socketIOMaxBackoff              = time.Minute
	socketIOReadLimit               = 1 << 20
	socketIOPath                    = "/socket.io/"
)

var errSocketIORefreshRequired = errors.New("syncobserve: socket.io endpoint refresh required")

type SocketIOLifecycleEventType string

const (
	SocketIOLifecycleEventStarted           SocketIOLifecycleEventType = "started"
	SocketIOLifecycleEventEndpointFetchFail SocketIOLifecycleEventType = "endpoint_fetch_failed"
	SocketIOLifecycleEventConnectFail       SocketIOLifecycleEventType = "connect_failed"
	SocketIOLifecycleEventConnected         SocketIOLifecycleEventType = "connected"
	SocketIOLifecycleEventRefreshRequested  SocketIOLifecycleEventType = "refresh_requested"
	SocketIOLifecycleEventConnectionDropped SocketIOLifecycleEventType = "connection_dropped"
	SocketIOLifecycleEventNotificationWake  SocketIOLifecycleEventType = "notification_wake"
	SocketIOLifecycleEventWakeCoalesced     SocketIOLifecycleEventType = "wake_coalesced"
	SocketIOLifecycleEventStopped           SocketIOLifecycleEventType = "stopped"
)

type SocketIOLifecycleEvent struct {
	Type       SocketIOLifecycleEventType
	DriveID    string
	EndpointID string
	SID        string
	Delay      time.Duration
	Note       string
	Error      string
}

type SocketIOWakeSourceOptions struct {
	Logger           *slog.Logger
	DialFunc         func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	SleepFunc        func(context.Context, time.Duration) error
	NowFunc          func() time.Time
	LifecycleHook    func(SocketIOLifecycleEvent)
	HandshakeTimeout time.Duration
	RefreshLeadTime  time.Duration
	RefreshInterval  time.Duration
	BackoffMax       time.Duration
}

type socketIOConn struct {
	conn *websocket.Conn
}

// SocketIOWakeSource owns the outbound Socket.IO/WebSocket lifecycle used to
// wake the remote delta observer. It never interprets change payloads as truth;
// every notification is reduced to a coalesced wake signal.
type SocketIOWakeSource struct {
	fetcher          synctypes.SocketIOEndpointFetcher
	driveID          driveid.ID
	logger           *slog.Logger
	dialFunc         func(context.Context, string, *websocket.DialOptions) (*websocket.Conn, *http.Response, error)
	sleepFunc        func(context.Context, time.Duration) error
	nowFunc          func() time.Time
	lifecycleHook    func(SocketIOLifecycleEvent)
	handshakeTimeout time.Duration
	refreshLeadTime  time.Duration
	refreshInterval  time.Duration
	backoffMax       time.Duration
}

// NewSocketIOWakeSource creates a wake source for one drive root.
func NewSocketIOWakeSource(
	fetcher synctypes.SocketIOEndpointFetcher,
	driveID driveid.ID,
	logger *slog.Logger,
) *SocketIOWakeSource {
	return NewSocketIOWakeSourceWithOptions(fetcher, driveID, SocketIOWakeSourceOptions{
		Logger: logger,
	})
}

func NewSocketIOWakeSourceWithOptions(
	fetcher synctypes.SocketIOEndpointFetcher,
	driveID driveid.ID,
	opts SocketIOWakeSourceOptions,
) *SocketIOWakeSource {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}

	dialFunc := opts.DialFunc
	if dialFunc == nil {
		dialFunc = websocket.Dial
	}

	sleepFunc := opts.SleepFunc
	if sleepFunc == nil {
		sleepFunc = TimeSleep
	}

	nowFunc := opts.NowFunc
	if nowFunc == nil {
		nowFunc = time.Now
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

	return &SocketIOWakeSource{
		fetcher:          fetcher,
		driveID:          driveID,
		logger:           logger,
		dialFunc:         dialFunc,
		sleepFunc:        sleepFunc,
		nowFunc:          nowFunc,
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
func (s *SocketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
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

func (s *SocketIOWakeSource) runIteration(
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

	s.emitLifecycleEvent(SocketIOLifecycleEvent{
		Type:       SocketIOLifecycleEventConnected,
		EndpointID: endpoint.ID,
		SID:        sid,
		Note:       "socket.io connected",
	})

	bo.Reset()
	runErr := s.runConnection(ctx, conn, endpoint.ID, refreshAt, wakes)
	if closeErr := conn.conn.Close(websocket.StatusNormalClosure, "watch stop"); closeErr != nil && !watchShouldStop(ctx, closeErr) {
		s.logger.Warn("socket.io close failed",
			slog.String("drive_id", s.driveID.String()),
			slog.String("error", closeErr.Error()),
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

func (s *SocketIOWakeSource) retryAfterError(
	ctx context.Context,
	bo *retry.Backoff,
	eventType SocketIOLifecycleEventType,
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

func (s *SocketIOWakeSource) refreshDeadline(endpoint *graph.SocketIOEndpoint) time.Time {
	now := s.nowFunc()
	if endpoint != nil && !endpoint.ExpirationTime.IsZero() {
		refreshAt := endpoint.ExpirationTime.Add(-s.refreshLeadTime)
		if refreshAt.Before(now) {
			return now
		}

		return refreshAt
	}

	return now.Add(s.refreshInterval)
}

func (s *SocketIOWakeSource) sleepUntilRetry(ctx context.Context, delay time.Duration, action string) (bool, error) {
	sleepErr := s.sleepFunc(ctx, delay)
	if sleepErr == nil {
		return false, nil
	}

	stop := watchShouldStop(ctx, sleepErr)
	if stop {
		return true, nil
	}

	return false, fmt.Errorf("socket.io %s: %w", action, sleepErr)
}

func (s *SocketIOWakeSource) logRetry(message string, err error, delay time.Duration) {
	if err == nil {
		return
	}

	s.logger.Warn(message,
		slog.String("drive_id", s.driveID.String()),
		slog.String("error", err.Error()),
		slog.Duration("retry_in", delay),
	)
}

func (s *SocketIOWakeSource) emitLifecycleEvent(event SocketIOLifecycleEvent) {
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
