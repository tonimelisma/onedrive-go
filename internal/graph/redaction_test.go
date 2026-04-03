package graph

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildGraphError_RedactsStructuredSecrets(t *testing.T) {
	t.Parallel()

	err := buildGraphError(
		http.StatusBadRequest,
		"req-123",
		0,
		[]byte(`{
			"error": {
				"code": "badRequest",
				"message": "resume at https://my.microsoftpersonalcontent.com/personal/upload/secret-token?sig=abc and Bearer top-secret-token",
				"innerError": {
					"code": "inner"
				}
			},
			"access_token": "access-secret",
			"uploadUrl": "https://storage.live.com/uploadSession/secret-value"
		}`),
	)

	require.Equal(t, "badRequest", err.Code)
	require.Equal(t, []string{"inner"}, err.InnerCodes)
	assert.NotContains(t, err.Message, "secret-token")
	assert.NotContains(t, err.Message, "top-secret-token")
	assert.Contains(t, err.Message, "[REDACTED_URL]")
	assert.Contains(t, err.Message, "Bearer [REDACTED]")
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NotContains(t, err.Error(), "top-secret-token")
	assert.NotContains(t, err.RawBody, "access-secret")
	assert.NotContains(t, err.RawBody, "secret-value")
	assert.Contains(t, err.RawBody, `"access_token":"[REDACTED]"`)
	assert.Contains(t, err.RawBody, `"uploadUrl":"[REDACTED]"`)
}

func TestBuildGraphError_RedactsPlainTextSecrets(t *testing.T) {
	t.Parallel()

	err := buildGraphError(
		http.StatusBadRequest,
		"",
		0,
		[]byte(`Authorization: Bearer top-secret-token https://public.bn1304.livefilestore.com/y4msecret-token-here/file.txt`),
	)

	assert.Contains(t, err.Message, "Bearer [REDACTED]")
	assert.Contains(t, err.Message, "[REDACTED_URL]")
	assert.NotContains(t, err.Message, "top-secret-token")
	assert.NotContains(t, err.Message, "y4msecret-token-here")
	assert.NotContains(t, err.Error(), "top-secret-token")
	assert.NotContains(t, err.Error(), "y4msecret-token-here")
	assert.Equal(t, err.Message, err.RawBody)
}
