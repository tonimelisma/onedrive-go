package graph

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// contentFallbackServers builds a Graph server whose item has no downloadUrl
// and whose /content route redirects to a separate content host.
func contentFallbackServers(t *testing.T, payload string) (*httptest.Server, *[]string) {
	t.Helper()

	return contentFallbackServersWith(t, payload, false, "")
}

// contentFallbackServersWith lets a test make the content host ignore Range
// (answer 200 with the whole file) or make Graph's /content refuse one
// bearer token with 401.
func contentFallbackServersWith(
	t *testing.T, payload string, ignoreRange bool, rejectedToken string,
) (*httptest.Server, *[]string) {
	t.Helper()

	var seenAuth []string
	contentSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		body := payload
		if rng := r.Header.Get("Range"); rng != "" && !ignoreRange {
			assert.Equal(t, "bytes=3-", rng, "the resume offset reaches the content host")
			body = payload[3:]
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 3-%d/%d", len(payload)-1, len(payload)))
			w.WriteHeader(http.StatusPartialContent)
		}
		writeTestResponse(t, w, body)
	}))
	t.Cleanup(contentSrv.Close)

	graphSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/content") {
			if rejectedToken != "" && r.Header.Get("Authorization") == "Bearer "+rejectedToken {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			assert.NotEmpty(t, r.Header.Get("Authorization"), "Graph itself needs the bearer")
			w.Header().Set("Location", contentSrv.URL+"/file?tempauth=none")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		writeTestResponsef(t, w, `{
			"id": "item-1", "name": "a.bin", "size": %d, "eTag": "v1",
			"createdDateTime": "2024-01-01T00:00:00Z", "lastModifiedDateTime": "2024-01-01T00:00:00Z",
			"parentReference": {"id": "parent", "driveId": "d"},
			"file": {"mimeType": "application/octet-stream"}
		}`, len(payload))
	}))
	t.Cleanup(graphSrv.Close)

	return graphSrv, &seenAuth
}

func allowAnyContentHost(client *Client) {
	client.validators.contentRedirect = func(*url.URL) error { return nil }
}

// Validates: R-5.9.1
func TestDownload_ContentEndpointNeverSendsBearerToRedirectHost(t *testing.T) {
	graphSrv, contentAuth := contentFallbackServers(t, "abcdefgh")
	client := newTestClient(t, graphSrv.URL)
	allowAnyContentHost(client)

	var buf bytes.Buffer
	n, err := client.Download(t.Context(), driveid.New("d"), "item-1", &buf)
	require.NoError(t, err)
	assert.Equal(t, "abcdefgh", buf.String())
	assert.Equal(t, int64(8), n)
	require.Len(t, *contentAuth, 1)
	assert.Empty(t, (*contentAuth)[0], "the bearer must never reach the content host (L-20)")
}

// Validates: R-5.9.1
func TestDownloadRange_ContentEndpointResumes(t *testing.T) {
	graphSrv, contentAuth := contentFallbackServers(t, "abcdefgh")
	client := newTestClient(t, graphSrv.URL)
	allowAnyContentHost(client)

	var buf bytes.Buffer
	_, err := client.DownloadRange(t.Context(), driveid.New("d"), "item-1", &buf, 3, DownloadVersion{ETag: "v1"})
	require.NoError(t, err)
	assert.Equal(t, "defgh", buf.String())
	assert.Empty(t, (*contentAuth)[0])
}

// Validates: R-5.9.1
func TestDownload_ContentRedirectToUntrustedHostRefused(t *testing.T) {
	graphSrv, contentAuth := contentFallbackServers(t, "abcdefgh")
	client := newTestClient(t, graphSrv.URL)
	// The production validator: an http loopback IP is not a Microsoft
	// content host. A loopback base URL relaxes the default for fixtures,
	// so the production rule is installed explicitly here.
	client.validators.contentRedirect = validateUploadURL

	_, err := client.Download(t.Context(), driveid.New("d"), "item-1", &bytes.Buffer{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "content download redirect")
	assert.Empty(t, *contentAuth, "nothing is fetched from an untrusted host")
}

// Validates: R-5.9.1
func TestDownload_ContentEndpoint401ForcesRefresh(t *testing.T) {
	graphSrv, contentAuth := contentFallbackServersWith(t, "abcdefgh", false, "revoked-early")
	token := &forcingToken{current: "revoked-early", next: "fresh"}
	client := MustNewClient(normalizeTestBaseURL(graphSrv.URL), http.DefaultClient, token, slog.Default(), "test-agent")
	allowAnyContentHost(client)

	var buf bytes.Buffer
	_, err := client.Download(t.Context(), driveid.New("d"), "item-1", &buf)
	require.NoError(t, err)
	assert.Equal(t, "abcdefgh", buf.String())
	assert.Equal(t, 1, token.forced, "the /content leg takes the normal 401 forced-refresh path")
	assert.Empty(t, (*contentAuth)[0])
}

// Validates: R-5.9.1
func TestDownloadRange_ContentEndpointRefusesAFullBodyAtAnOffset(t *testing.T) {
	graphSrv, _ := contentFallbackServersWith(t, "abcdefgh", true, "")
	client := newTestClient(t, graphSrv.URL)
	allowAnyContentHost(client)

	var buf bytes.Buffer
	_, err := client.DownloadRange(t.Context(), driveid.New("d"), "item-1", &buf, 3, DownloadVersion{ETag: "v1"})
	require.ErrorIs(t, err, ErrRangeResumeRejected)
	assert.Zero(t, buf.Len(), "nothing is appended to the partial")
}
