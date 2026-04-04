package cli

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// httpClientTimeout is the default timeout for HTTP requests.
// Prevents hung connections from blocking CLI commands indefinitely.
const httpClientTimeout = 30 * time.Second

// syncMetaClientTimeout is the timeout for sync metadata requests. Sync uses
// this client for delta, item metadata, and permission validation; Graph's
// /permissions endpoint can be materially slower than ordinary interactive
// CLI reads, so sync gets the longer default data budget.
const syncMetaClientTimeout = 60 * time.Second

// Transfer transport constants protect against stalled connections
// without bounding total transfer time (which varies with file size
// and bandwidth).
const (
	// transferResponseHeaderTimeout detects servers that accept
	// connections but never start responding. 2 minutes is generous
	// enough for slow API responses while catching true stalls.
	transferResponseHeaderTimeout = 2 * time.Minute

	// transferDialTimeout matches http.DefaultTransport's 30s dial timeout.
	transferDialTimeout = 30 * time.Second

	// TCP keepalive parameters detect dead connections (crash, network
	// partition) within ~60s: 30s idle + 3 probes at 10s intervals.
	transferKeepAliveIdle     = 30 * time.Second
	transferKeepAliveInterval = 10 * time.Second
	transferKeepAliveCount    = 3
)

// defaultHTTPClient returns an HTTP client with a sensible timeout.
func defaultHTTPClient(logger *slog.Logger) *http.Client {
	return &http.Client{
		Timeout: httpClientTimeout,
		Transport: &retry.RetryTransport{
			Inner:  http.DefaultTransport,
			Policy: retry.TransportPolicy(),
			Logger: logger,
		},
	}
}

// transferTransport returns an *http.Transport with connection-level
// deadlines that detect stalled connections without bounding total
// transfer time. ResponseHeaderTimeout catches servers that accept but
// never respond; TCP keepalives detect dead connections from crashes
// or network partitions. Cloned from http.DefaultTransport so we
// inherit its connection pool, TLS settings, and proxy support.
func transferTransport() *http.Transport {
	// Clone to avoid mutating the shared DefaultTransport.
	dt, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// Should never happen in practice — http.DefaultTransport is always
		// *http.Transport. Fall back to a bare transport with our settings.
		return &http.Transport{
			ResponseHeaderTimeout: transferResponseHeaderTimeout,
		}
	}

	t := dt.Clone()
	t.ResponseHeaderTimeout = transferResponseHeaderTimeout
	t.DialContext = (&net.Dialer{
		Timeout:   transferDialTimeout,
		KeepAlive: -1, // disable legacy KeepAlive; use KeepAliveConfig
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     transferKeepAliveIdle,
			Interval: transferKeepAliveInterval,
			Count:    transferKeepAliveCount,
		},
	}).DialContext

	return t
}

// transferHTTPClient returns an HTTP client for upload/download operations.
// No client-level timeout — large file transfers on slow connections can
// exceed any fixed bound. Connection-level protection is provided by
// transferTransport() (ResponseHeaderTimeout + TCP keepalives).
func transferHTTPClient(logger *slog.Logger) *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &retry.RetryTransport{
			Inner:  transferTransport(),
			Policy: retry.TransportPolicy(),
			Logger: logger,
		},
	}
}

// syncMetaHTTPClient returns an HTTP client for sync metadata requests. No
// RetryTransport — sync workers never block on retry backoff (R-6.8.7). Failed
// requests return immediately as GraphError for engine-level classification
// and tracker re-queue.
func syncMetaHTTPClient() *http.Client {
	return &http.Client{Timeout: syncMetaClientTimeout}
}

// syncTransferHTTPClient returns an HTTP client for sync transfers. Same
// rationale as syncMetaHTTPClient — no RetryTransport. No client-level
// timeout. Connection-level protection via transferTransport()
// (ResponseHeaderTimeout + TCP keepalives) prevents indefinite hangs.
func syncTransferHTTPClient() *http.Client {
	return &http.Client{
		Timeout:   0,
		Transport: transferTransport(),
	}
}
