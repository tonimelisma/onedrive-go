package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/coder/websocket"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

type socketIOOpenPacket struct {
	SID string `json:"sid"`
}

type socketIOPacketAction struct {
	notificationWake bool
	wakeCoalesced    bool
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

func handleSocketIOPacket(
	ctx context.Context,
	conn *websocket.Conn,
	packet string,
	wakes chan<- struct{},
) (socketIOPacketAction, error) {
	switch {
	case packet == "2":
		if err := conn.Write(ctx, websocket.MessageText, []byte("3")); err != nil {
			return socketIOPacketAction{}, fmt.Errorf("socket.io sending pong: %w", err)
		}
		return socketIOPacketAction{}, nil
	case packet == "3":
		return socketIOPacketAction{}, nil
	case strings.HasPrefix(packet, "41"):
		return socketIOPacketAction{}, fmt.Errorf("socket.io server disconnect: %s", packet)
	case strings.HasPrefix(packet, "42"):
		eventName, ok, err := parseSocketIOEventName(packet)
		if err != nil {
			return socketIOPacketAction{}, err
		}
		if ok && eventName == "notification" {
			action := socketIOPacketAction{}
			select {
			case wakes <- struct{}{}:
				action.notificationWake = true
			default:
				action.wakeCoalesced = true
			}
			return action, nil
		}
		return socketIOPacketAction{}, nil
	default:
		return socketIOPacketAction{}, nil
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
