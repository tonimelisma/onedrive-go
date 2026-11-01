package graph

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

type failingRoundTripper struct{ err error }

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// Validates: R-6.8.17
func TestDispatchRequest_RedactsPreAuthURLFromTransportError(t *testing.T) {
	t.Parallel()

	client := &Client{httpClient: &http.Client{Transport: failingRoundTripper{err: errors.New("unexpected EOF")}}}

	for _, rawURL := range []string{
		"https://public.bn1304.livefilestore.com/y4msecret-token-here/file.txt",
		"https://contoso-my.sharepoint.com/personal/u/_api/v2.0/drives/d/items/i/uploadSession?guid=g&tempauth=secret-tempauth",
		"https://my.microsoftpersonalcontent.com/personal/upload/secret-token?sig=abc",
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, rawURL, http.NoBody)
		require.NoError(t, err)

		resp, err := client.dispatchRequest(req)
		if resp != nil {
			require.NoError(t, resp.Body.Close())
		}
		require.Error(t, err)

		assert.NotContains(t, err.Error(), "secret")
		assert.Contains(t, err.Error(), "/[REDACTED]")
		assert.Contains(t, err.Error(), "unexpected EOF", "the cause must survive redaction")

		var urlErr *url.Error
		assert.ErrorAs(t, err, &urlErr, "callers still classify the transport failure")
	}
}

// Validates: R-6.8.17
func TestDispatchRequest_KeepsGraphAPIURLInTransportError(t *testing.T) {
	t.Parallel()

	client := &Client{httpClient: &http.Client{Transport: failingRoundTripper{err: errors.New("unexpected EOF")}}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://graph.microsoft.com/v1.0/me/drive/root", http.NoBody)
	require.NoError(t, err)

	resp, err := client.dispatchRequest(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}
	require.Error(t, err)
	assert.Contains(t, err.Error(), "https://graph.microsoft.com/v1.0/me/drive/root")
}

// Validates: R-6.8.17
//
// The Socket.IO notification URL carries its credential in the snthgk query
// parameter, and the websocket dial rewrites it to /socket.io on the same
// svc.ms host. Both spellings are sensitive, not only the /callback path.
func TestIsSensitiveURL_SocketIONotificationURLs(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"https://pushchannel.svc.ms/callback?snthgk=SECRET":                   true,
		"https://x.svc.ms/socket.io/?EIO=4&snthgk=SECRET&transport=websocket": true,
		"https://x.svc.ms/socket.io/?EIO=4&transport=websocket":               true,
		"wss://x.svc.ms/socket.io/?EIO=4&snthgk=SECRET&transport=websocket":   true,
		"https://x.svc.ms/anything?snthgk=SECRET":                             true,
		"https://example.com/?snthgk=SECRET":                                  true,
		"https://x.svc.ms/health":                                             false,
	}

	for rawURL, want := range tests {
		assert.Equal(t, want, isSensitiveURL(rawURL), rawURL)
	}
}

// Validates: R-6.8.17
func TestRedactURLError_ScrubsSocketIOWebsocketURL(t *testing.T) {
	t.Parallel()

	err := redactURLError(&url.Error{
		Op:  "Get",
		URL: "https://x.svc.ms/socket.io/?EIO=4&snthgk=SECRETTOKEN&transport=websocket",
		Err: errors.New("connection refused"),
	})

	assert.NotContains(t, err.Error(), "SECRETTOKEN")
	assert.Contains(t, err.Error(), "https://x.svc.ms/[REDACTED]")
	assert.Contains(t, err.Error(), "connection refused", "the cause must survive redaction")
}

// Validates: R-6.8.17
//
// fmt.Errorf captures the wrapped error's text when it wraps, so scrubbing
// the *url.Error afterwards leaves the outer message intact. The transport
// boundary therefore redacts before wrapping; this pins the limitation so
// nobody moves the call later in the chain.
func TestRedactURLError_MustRunBeforeWrapping(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("dial: %w", &url.Error{
		Op:  "Get",
		URL: "https://x.svc.ms/socket.io/?snthgk=SECRETTOKEN",
		Err: errors.New("connection refused"),
	})

	assert.Contains(t, redactURLError(wrapped).Error(), "SECRETTOKEN",
		"an already wrapped message cannot be scrubbed in place")
	assert.NotContains(t, fmt.Errorf("dial: %w", redactURLError(errors.Unwrap(wrapped))).Error(), "SECRETTOKEN",
		"redacting first, then wrapping, is the working order")
}
