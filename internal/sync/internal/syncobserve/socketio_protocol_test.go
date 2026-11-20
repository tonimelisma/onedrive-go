package syncobserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func TestSocketIONotificationURLToWebsocketURL(t *testing.T) {
	t.Parallel()

	wsURL, err := socketIONotificationURLToWebsocketURL("https://example.com/callback?snthgk=secret")
	require.NoError(t, err)
	assert.Equal(t, "wss://example.com/socket.io/?EIO=4&snthgk=secret&transport=websocket", wsURL)
}

func TestParseEngineOpenPacket(t *testing.T) {
	t.Parallel()

	open, ok, err := parseEngineOpenPacket(`0{"sid":"sid-1"}`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "sid-1", open.SID)
}

func TestParseSocketIOEventName(t *testing.T) {
	t.Parallel()

	eventName, ok, err := parseSocketIOEventName(`42/notifications,["notification",{"sequence":1}]`)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "notification", eventName)
}

func TestParseSocketIOEventName_MalformedPayload(t *testing.T) {
	t.Parallel()

	_, _, err := parseSocketIOEventName(`42/notifications`)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing namespace payload")
}

func TestHandleSocketIOPacket_CoalescesWake(t *testing.T) {
	t.Parallel()

	wakes := make(chan struct{}, 1)
	wakes <- struct{}{}

	action, err := handleSocketIOPacket(context.Background(), nil, `42["notification",{"sequence":1}]`, wakes)
	require.NoError(t, err)
	assert.False(t, action.notificationWake)
	assert.True(t, action.wakeCoalesced)
}

func TestSocketIONotificationURLToWebsocketURL_InvalidScheme(t *testing.T) {
	t.Parallel()

	_, err := socketIONotificationURLToWebsocketURL(graph.SocketIONotificationURL("ftp://example.com/callback"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not supported")
}
