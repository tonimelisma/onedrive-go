package syncobserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
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

type socketIOConn struct {
	conn *websocket.Conn
}

type socketIOReadResult struct {
	packet string
	err    error
}

type socketIOOpenPacket struct {
	SID string `json:"sid"`
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
	if logger == nil {
		logger = slog.Default()
	}

	return &SocketIOWakeSource{
		fetcher:          fetcher,
		driveID:          driveID,
		logger:           logger,
		dialFunc:         websocket.Dial,
		sleepFunc:        TimeSleep,
		nowFunc:          time.Now,
		handshakeTimeout: socketIOHandshakeTimeout,
		refreshLeadTime:  socketIORefreshLeadTime,
		refreshInterval:  socketIOFallbackRefreshInterval,
		backoffMax:       socketIOMaxBackoff,
	}
}

// Run maintains the Socket.IO connection until ctx is canceled. Connection
// failures degrade silently to fallback polling while the wake source retries
// in the background.
func (s *SocketIOWakeSource) Run(ctx context.Context, wakes chan<- struct{}) error {
	if s.fetcher == nil {
		return nil
	}

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
		return s.retryAfterError(ctx, bo, "socket.io endpoint fetch failed", err, "waiting to refetch endpoint")
	}

	conn, refreshAt, err := s.connect(ctx, endpoint)
	if err != nil {
		return s.retryAfterError(ctx, bo, "socket.io connect failed", err, "waiting to reconnect")
	}

	bo.Reset()
	runErr := s.runConnection(ctx, conn, refreshAt, wakes)
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
		return false, nil
	}

	return s.retryAfterError(ctx, bo, "socket.io connection dropped", runErr, "waiting to reconnect after drop")
}

func (s *SocketIOWakeSource) retryAfterError(
	ctx context.Context,
	bo *retry.Backoff,
	logMessage string,
	cause error,
	waitAction string,
) (bool, error) {
	delay := bo.Next()
	s.logRetry(logMessage, cause, delay)

	return s.sleepUntilRetry(ctx, delay, waitAction)
}

func (s *SocketIOWakeSource) connect(
	ctx context.Context,
	endpoint *graph.SocketIOEndpoint,
) (*socketIOConn, time.Time, error) {
	if endpoint == nil {
		return nil, time.Time{}, fmt.Errorf("socket.io endpoint response is nil")
	}

	wsURL, err := socketIONotificationURLToWebsocketURL(endpoint.NotificationURL)
	if err != nil {
		return nil, time.Time{}, err
	}

	conn, resp, err := s.dialFunc(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	conn.SetReadLimit(socketIOReadLimit)

	if err := s.completeHandshake(ctx, conn); err != nil {
		if closeErr := conn.Close(websocket.StatusProtocolError, "handshake failed"); closeErr != nil {
			return nil, time.Time{}, errors.Join(err, fmt.Errorf("socket.io closing failed handshake: %w", closeErr))
		}

		return nil, time.Time{}, err
	}

	refreshAt := s.refreshDeadline(endpoint)
	return &socketIOConn{conn: conn}, refreshAt, nil
}

func (s *SocketIOWakeSource) completeHandshake(ctx context.Context, conn *websocket.Conn) error {
	handshakeCtx, cancel := context.WithTimeout(ctx, s.handshakeTimeout)
	defer cancel()

	open, err := readEngineOpenPacket(handshakeCtx, conn)
	if err != nil {
		return err
	}

	s.logger.Info("socket.io connected",
		slog.String("drive_id", s.driveID.String()),
		slog.String("sid", open.SID),
	)

	for _, frame := range []string{"40", "40/notifications"} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return fmt.Errorf("socket.io sending connect frame: %w", err)
		}
	}

	return nil
}

func readEngineOpenPacket(ctx context.Context, conn *websocket.Conn) (*socketIOOpenPacket, error) {
	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("socket.io awaiting engine open: %w", err)
		}
		if typ != websocket.MessageText {
			continue
		}

		packet := string(payload)
		open, ok, err := parseEngineOpenPacket(packet)
		if err != nil {
			return nil, err
		}
		if ok {
			return open, nil
		}
	}
}

func parseEngineOpenPacket(packet string) (*socketIOOpenPacket, bool, error) {
	if !strings.HasPrefix(packet, "0") {
		return nil, false, nil
	}

	var open socketIOOpenPacket
	if err := json.Unmarshal([]byte(packet[1:]), &open); err != nil {
		return nil, false, fmt.Errorf("socket.io parsing engine open: %w", err)
	}

	return &open, true, nil
}

func (s *SocketIOWakeSource) runConnection(
	ctx context.Context,
	conn *socketIOConn,
	refreshAt time.Time,
	wakes chan<- struct{},
) error {
	readCtx, cancelRead := context.WithCancel(ctx)
	defer cancelRead()

	readResults := make(chan socketIOReadResult, 1)
	go s.readPackets(readCtx, conn.conn, readResults)

	var refreshTimer <-chan time.Time
	var refresh *time.Timer
	if !refreshAt.IsZero() {
		delay := time.Until(refreshAt)
		if delay < 0 {
			delay = 0
		}
		refresh = time.NewTimer(delay)
		refreshTimer = refresh.C
		defer refresh.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-refreshTimer:
			return errSocketIORefreshRequired
		case result := <-readResults:
			if result.err != nil {
				return result.err
			}

			if err := handleSocketIOPacket(ctx, conn.conn, result.packet, wakes); err != nil {
				return err
			}
		}
	}
}

func (s *SocketIOWakeSource) readPackets(ctx context.Context, conn *websocket.Conn, out chan<- socketIOReadResult) {
	for {
		typ, payload, err := conn.Read(ctx)
		if err != nil {
			select {
			case out <- socketIOReadResult{err: fmt.Errorf("socket.io reading packet: %w", err)}:
			case <-ctx.Done():
			}
			return
		}
		if typ != websocket.MessageText {
			continue
		}

		select {
		case out <- socketIOReadResult{packet: string(payload)}:
		case <-ctx.Done():
			return
		}
	}
}

func handleSocketIOPacket(
	ctx context.Context,
	conn *websocket.Conn,
	packet string,
	wakes chan<- struct{},
) error {
	switch {
	case packet == "2":
		if err := conn.Write(ctx, websocket.MessageText, []byte("3")); err != nil {
			return fmt.Errorf("socket.io sending pong: %w", err)
		}
		return nil
	case packet == "3":
		return nil
	case strings.HasPrefix(packet, "41"):
		return fmt.Errorf("socket.io server disconnect: %s", packet)
	case strings.HasPrefix(packet, "42"):
		eventName, ok, err := parseSocketIOEventName(packet)
		if err != nil {
			return err
		}
		if ok && eventName == "notification" {
			select {
			case wakes <- struct{}{}:
			default:
			}
		}
		return nil
	default:
		return nil
	}
}

func parseSocketIOEventName(packet string) (string, bool, error) {
	if !strings.HasPrefix(packet, "42") {
		return "", false, nil
	}

	payload := packet[2:]
	if strings.HasPrefix(payload, "/") {
		comma := strings.IndexByte(payload, ',')
		if comma < 0 {
			return "", false, fmt.Errorf("socket.io parsing event packet: missing namespace payload")
		}
		payload = payload[comma+1:]
	}

	var parts []json.RawMessage
	if err := json.Unmarshal([]byte(payload), &parts); err != nil {
		return "", false, fmt.Errorf("socket.io parsing event payload: %w", err)
	}
	if len(parts) == 0 {
		return "", false, nil
	}

	var eventName string
	if err := json.Unmarshal(parts[0], &eventName); err != nil {
		return "", false, fmt.Errorf("socket.io parsing event name: %w", err)
	}

	return eventName, true, nil
}

func socketIONotificationURLToWebsocketURL(raw graph.SocketIONotificationURL) (string, error) {
	parsed, err := url.Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("socket.io parsing notification URL: invalid URL")
	}

	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	case "http":
		parsed.Scheme = "ws"
	default:
		return "", fmt.Errorf("socket.io notification URL scheme %q is not supported", parsed.Scheme)
	}

	parsed.Path = socketIOPath

	query := parsed.Query()
	query.Set("EIO", "4")
	query.Set("transport", "websocket")
	parsed.RawQuery = query.Encode()

	return parsed.String(), nil
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
