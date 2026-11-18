package perf

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closeRecordingBody is a request body that records whether it was closed.
type closeRecordingBody struct {
	*strings.Reader
	closed bool
}

func (b *closeRecordingBody) Close() error {
	b.closed = true
	return nil
}

// A transport with no inner transport refuses the request and sends nothing:
// a default in its place would send the request without the timeouts and
// connection settings of the client it was built for. The request's body is
// closed, as the RoundTripper contract asks of every answer, and a request
// never sent is not counted.
//
// Validates: R-6.6.14
func TestRoundTripper_WithoutAnInnerTransportRefusesTheRequest(t *testing.T) {
	t.Parallel()

	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		served.Add(1)
	}))
	t.Cleanup(srv.Close)

	collector := NewCollector(nil)
	body := &closeRecordingBody{Reader: strings.NewReader("payload")}
	req, err := http.NewRequestWithContext(WithCollector(t.Context(), collector), http.MethodPut, srv.URL, body)
	require.NoError(t, err)

	resp, err := RoundTripper{}.RoundTrip(req)
	if resp != nil {
		require.NoError(t, resp.Body.Close())
	}

	require.EqualError(t, err, "the performance transport has no inner transport to send the request through")
	assert.Nil(t, resp)
	assert.Zero(t, served.Load(), "nothing was sent")
	assert.True(t, body.closed, "the request's body is closed")
	assert.Zero(t, collector.Snapshot().HTTPRequestCount, "a request never sent is not counted")
}
