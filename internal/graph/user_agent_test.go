package graph

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-6.8.18
func TestUserAgent_DecoratedFormOnEveryRequest(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "ISV|onedrive-go|onedrive-go/1.2.3", UserAgent("1.2.3"))
	assert.Equal(t, "ISV|onedrive-go|onedrive-go/dev", UserAgent(""))

	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		writeClientTestBody(t, w, `{"id":"me"}`)
	}))
	defer srv.Close()

	for _, agent := range []string{UserAgent("1.2.3"), ""} {
		client := MustNewClient(srv.URL, http.DefaultClient, staticToken("t"), slog.Default(), agent)
		resp, err := client.do(t.Context(), http.MethodGet, "/me", nil)
		require.NoError(t, err)
		closeClientTestResponse(t, resp)
	}

	assert.Equal(t, []string{"ISV|onedrive-go|onedrive-go/1.2.3", "ISV|onedrive-go|onedrive-go/dev"}, seen)
}
