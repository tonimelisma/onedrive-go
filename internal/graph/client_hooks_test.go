package graph

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Validates: R-2.10.47
func TestCancelUploadSession_DoesNotInvokeAuthenticatedSuccessHook(t *testing.T) {
	var hookCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Empty(t, r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	client := newTestClient(t, srv.URL)
	client.uploadURLValidator = func(*url.URL) error { return nil }
	client.SetAuthenticatedSuccessHook(func(_ context.Context) {
		hookCalls++
	})

	err := client.CancelUploadSession(t.Context(), &UploadSession{
		UploadURL: UploadURL(srv.URL + "/session"),
	})
	require.NoError(t, err)
	assert.Equal(t, 0, hookCalls)
}
