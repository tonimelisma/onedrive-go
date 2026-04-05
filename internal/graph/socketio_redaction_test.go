package graph

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildGraphError_RedactsNotificationURL(t *testing.T) {
	t.Parallel()

	err := buildGraphError(
		http.StatusBadRequest,
		"req-123",
		0,
		[]byte(`{
			"error": {
				"code": "badRequest",
				"message": "socket endpoint is https://f3hb0mpua.svc.ms/zbaehwg/callback?snthgk=secret-token"
			},
			"notificationUrl": "https://f3hb0mpua.svc.ms/zbaehwg/callback?snthgk=secret-token"
		}`),
	)

	require.Equal(t, "badRequest", err.Code)
	assert.Contains(t, err.Message, "[REDACTED_URL]")
	assert.NotContains(t, err.Message, "secret-token")
	assert.NotContains(t, err.RawBody, "secret-token")
	assert.Contains(t, err.RawBody, `"notificationUrl":"[REDACTED]"`)
}
