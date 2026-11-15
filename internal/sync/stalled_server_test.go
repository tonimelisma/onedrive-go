package sync

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

// stalledRequestTimeout is the transport timeout of a request to a stalled
// server: short, so no test waits a second, and long enough for a loopback
// request to be sent before it fires.
const stalledRequestTimeout = 40 * time.Millisecond

// stalledRequestKind is one way the transport gives up on a request to a
// server that never answers.
type stalledRequestKind struct {
	name string
	// reachesServer is false for a dial that times out before a connection
	// exists.
	reachesServer bool
	client        func(t *testing.T) *http.Client
}

// stalledRequestKinds are the transport timeouts a sync request can end in:
// the response-header timeout, a dial timeout and the client's whole-request
// timeout.
func stalledRequestKinds() []stalledRequestKind {
	return []stalledRequestKind{
		{name: "response header timeout", reachesServer: true, client: func(t *testing.T) *http.Client {
			t.Helper()

			transport := &http.Transport{ResponseHeaderTimeout: stalledRequestTimeout}
			t.Cleanup(transport.CloseIdleConnections)

			return &http.Client{Transport: transport}
		}},
		{name: "dial timeout", reachesServer: false, client: func(t *testing.T) *http.Client {
			t.Helper()

			// The connect waits for the dial's own deadline, as a dial to an
			// address that never answers does.
			dialer := &net.Dialer{
				Timeout: stalledRequestTimeout,
				ControlContext: func(ctx context.Context, _, _ string, _ syscall.RawConn) error {
					<-ctx.Done()
					return nil
				},
			}
			transport := &http.Transport{DialContext: dialer.DialContext}
			t.Cleanup(transport.CloseIdleConnections)

			return &http.Client{Transport: transport}
		}},
		clientTimeoutKind(),
	}
}

// clientTimeoutKind is the client's whole-request timeout.
func clientTimeoutKind() stalledRequestKind {
	return stalledRequestKind{name: "client timeout", reachesServer: true, client: func(t *testing.T) *http.Client {
		t.Helper()

		return &http.Client{Timeout: stalledRequestTimeout}
	}}
}

// newStalledServer starts a loopback server that never answers: each request
// waits until its client gives up. It returns the server and the count of
// requests that reached it, which a request the client gave up on may join
// late or never: count dispatches with stalledGraphClient's count instead.
func newStalledServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var requests atomic.Int64
	released := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-released:
		}
	}))
	t.Cleanup(server.Close)
	// Cleanups run last-registered first: the handlers are released before
	// the server waits for them.
	t.Cleanup(func() { close(released) })

	return server, &requests
}

// stalledGraphClient is a graph client whose every request goes to server
// through httpClient, and the count of the requests it dispatched. The count
// is taken on the client's side, so it is complete the moment a request has
// failed, whether or not the request reached the server.
func stalledGraphClient(
	t *testing.T, server *httptest.Server, httpClient *http.Client,
) (*graph.Client, *atomic.Int64) {
	t.Helper()

	next := httpClient.Transport
	if next == nil {
		next = http.DefaultTransport
	}
	counter := &dispatchCounter{next: next}
	counted := *httpClient
	counted.Transport = counter
	client, err := graph.NewClient(server.URL, &counted, stalledServerToken{},
		slog.New(slog.DiscardHandler), "stalled-server")
	require.NoError(t, err)

	return client, &counter.dispatched
}

// dispatchCounter counts the requests handed to its transport.
type dispatchCounter struct {
	next       http.RoundTripper
	dispatched atomic.Int64
}

func (c *dispatchCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	c.dispatched.Add(1)

	//nolint:wrapcheck // the transport's own answer, unchanged, as the client would get it without the count
	return c.next.RoundTrip(req)
}

// stalledServerToken hands graph a fixed token.
type stalledServerToken struct{}

func (stalledServerToken) Token(context.Context) (string, error) {
	return "stalled-server", nil
}
