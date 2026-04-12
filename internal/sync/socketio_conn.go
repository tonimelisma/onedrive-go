package sync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/coder/websocket"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type socketIOReadResult struct {
	packet string
	err    error
}

func (s *SocketIOWakeSource) connect(
	ctx context.Context,
	endpoint *graph.SocketIOEndpoint,
) (*socketIOConn, time.Time, string, error) {
	if endpoint == nil {
		return nil, time.Time{}, "", fmt.Errorf("socket.io endpoint response is nil")
	}

	wsURL, err := socketIONotificationURLToWebsocketURL(endpoint.NotificationURL)
	if err != nil {
		return nil, time.Time{}, "", err
	}

	conn, resp, err := s.dialFunc(ctx, wsURL, &websocket.DialOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return nil, time.Time{}, "", err
	}
	conn.SetReadLimit(socketIOReadLimit)

	sid, err := s.completeHandshake(ctx, conn)
	if err != nil {
		if closeErr := conn.Close(websocket.StatusProtocolError, "handshake failed"); closeErr != nil {
			return nil, time.Time{}, "", errors.Join(err, fmt.Errorf("socket.io closing failed handshake: %w", closeErr))
		}

		return nil, time.Time{}, "", err
	}

	refreshAt := s.refreshDeadline(endpoint)
	return &socketIOConn{conn: conn}, refreshAt, sid, nil
}

func (s *SocketIOWakeSource) completeHandshake(ctx context.Context, conn *websocket.Conn) (string, error) {
	handshakeCtx, cancel := context.WithTimeout(ctx, s.handshakeTimeout)
	defer cancel()

	open, err := readEngineOpenPacket(handshakeCtx, conn)
	if err != nil {
		return "", err
	}

	for _, frame := range []string{"40", "40/notifications"} {
		if err := conn.Write(ctx, websocket.MessageText, []byte(frame)); err != nil {
			return "", fmt.Errorf("socket.io sending connect frame: %w", err)
		}
	}

	return open.SID, nil
}

func (s *SocketIOWakeSource) runConnection(
	ctx context.Context,
	conn *socketIOConn,
	endpointID string,
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

			s.logger.Debug("socket.io packet received",
				slog.String("drive_id", s.driveID.String()),
				slog.String("packet", truncateSocketIOPacket(result.packet)),
			)

			action, err := handleSocketIOPacket(ctx, conn.conn, result.packet, wakes)
			if err != nil {
				return err
			}
			if action.notificationWake {
				s.emitLifecycleEvent(SocketIOLifecycleEvent{
					Type:       SocketIOLifecycleEventNotificationWake,
					EndpointID: endpointID,
				})
			}
			if action.wakeCoalesced {
				s.emitLifecycleEvent(SocketIOLifecycleEvent{
					Type:       SocketIOLifecycleEventWakeCoalesced,
					EndpointID: endpointID,
				})
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

func truncateSocketIOPacket(packet string) string {
	const maxPacketLogLength = 200
	if len(packet) <= maxPacketLogLength {
		return packet
	}

	return packet[:maxPacketLogLength] + "...(truncated)"
}
