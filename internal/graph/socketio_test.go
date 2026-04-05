package graph

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

func TestSocketIOEndpoint_Success(t *testing.T) {
	t.Parallel()

	driveID := driveid.New("drive-123")
	srv := newGraphServer(t, http.StatusOK, `{
		"id": "opaque-subscription-id",
		"notificationUrl": "https://f3hb0mpua.svc.ms/zbaehwg/callback?snthgk=secret-token",
		"expirationDateTime": "2026-04-04T18:30:00Z"
	}`, graphServerOptions{
		contentType: "application/json",
		assertRequest: func(r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/drives/0000000drive-123/root/subscriptions/socketIo", r.URL.Path)
			assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		},
	})
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	endpoint, err := client.SocketIOEndpoint(t.Context(), driveID)
	require.NoError(t, err)

	require.NotNil(t, endpoint)
	assert.Equal(t, "opaque-subscription-id", endpoint.ID)
	assert.Equal(
		t,
		SocketIONotificationURL("https://f3hb0mpua.svc.ms/zbaehwg/callback?snthgk=secret-token"),
		endpoint.NotificationURL,
	)
	assert.Equal(t, time.Date(2026, 4, 4, 18, 30, 0, 0, time.UTC), endpoint.ExpirationTime)
}

func TestSocketIOEndpoint_InvalidNotificationURL(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, http.StatusOK, `{
		"id": "opaque-subscription-id",
		"notificationUrl": "http://not-https.example.com/socket"
	}`, graphServerOptions{
		contentType: "application/json",
	})
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	_, err := client.SocketIOEndpoint(t.Context(), driveid.New("drive-123"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid socket.io notification URL")
}

func TestSocketIOEndpoint_MissingNotificationURL(t *testing.T) {
	t.Parallel()

	srv := newGraphServer(t, http.StatusOK, `{
		"id": "opaque-subscription-id"
	}`, graphServerOptions{
		contentType: "application/json",
	})
	t.Cleanup(srv.Close)

	client := newTestClient(t, srv.URL)
	_, err := client.SocketIOEndpoint(t.Context(), driveid.New("drive-123"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing notificationUrl")
}
