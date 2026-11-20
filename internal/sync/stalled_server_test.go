package sync

import (
	"context"
	"net"
	"net/http"
	"syscall"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/sync/internal/syncfixture"
)

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

			transport := &http.Transport{ResponseHeaderTimeout: syncfixture.StalledRequestTimeout}
			t.Cleanup(transport.CloseIdleConnections)

			return &http.Client{Transport: transport}
		}},
		{name: "dial timeout", reachesServer: false, client: func(t *testing.T) *http.Client {
			t.Helper()

			// The connect waits for the dial's own deadline, as a dial to an
			// address that never answers does.
			dialer := &net.Dialer{
				Timeout: syncfixture.StalledRequestTimeout,
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

		return &http.Client{Timeout: syncfixture.StalledRequestTimeout}
	}}
}
