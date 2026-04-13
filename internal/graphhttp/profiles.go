package graphhttp

import (
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/perf"
	"github.com/tonimelisma/onedrive-go/internal/retry"
)

const (
	metadataResponseHeaderTimeout = 2 * time.Minute
	transferResponseHeaderTimeout = 2 * time.Minute
	dialTimeout                   = 30 * time.Second
	tlsHandshakeTimeout           = 15 * time.Second
	keepAliveIdle                 = 30 * time.Second
	keepAliveInterval             = 10 * time.Second
	keepAliveCount                = 3
)

// ClientSet is the paired Graph HTTP client set used to construct graph.Client
// instances for metadata and transfer traffic.
type ClientSet struct {
	Meta     *http.Client
	Transfer *http.Client
}

// BootstrapMetadataClient builds the retrying metadata client used before
// account identity is known.
func BootstrapMetadataClient(logger *slog.Logger) *http.Client {
	return retryingMetadataClient(logger, nil)
}

// InteractiveMetadataClient builds the retrying metadata client used for one
// interactive target. Callers own any shared throttle-gate lifetime.
func InteractiveMetadataClient(logger *slog.Logger, gate *retry.ThrottleGate) *http.Client {
	return retryingMetadataClient(logger, gate)
}

// InteractiveTransferClient builds the retrying transfer client shared by
// interactive commands once a target has been selected.
func InteractiveTransferClient(logger *slog.Logger) *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &retry.RetryTransport{
			Inner:  perf.RoundTripper{Inner: transferTransport()},
			Policy: retry.TransportPolicy(),
			Logger: normalizeLogger(logger),
		},
	}
}

// SyncClientSet builds the non-retrying HTTP clients used by sync workers.
func SyncClientSet() ClientSet {
	return ClientSet{
		Meta: &http.Client{
			Timeout:   0,
			Transport: perf.RoundTripper{Inner: metadataTransport()},
		},
		Transfer: &http.Client{
			Timeout:   0,
			Transport: perf.RoundTripper{Inner: transferTransport()},
		},
	}
}

func retryingMetadataClient(logger *slog.Logger, gate *retry.ThrottleGate) *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &retry.RetryTransport{
			Inner:        perf.RoundTripper{Inner: metadataTransport()},
			Policy:       retry.TransportPolicy(),
			Logger:       normalizeLogger(logger),
			ThrottleGate: gate,
		},
	}
}

func normalizeLogger(logger *slog.Logger) *slog.Logger {
	if logger == nil {
		return slog.Default()
	}

	return logger
}

func metadataTransport() *http.Transport {
	return buildTransport(metadataResponseHeaderTimeout)
}

func transferTransport() *http.Transport {
	return buildTransport(transferResponseHeaderTimeout)
}

func buildTransport(responseHeaderTimeout time.Duration) *http.Transport {
	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return &http.Transport{
			ResponseHeaderTimeout: responseHeaderTimeout,
			TLSHandshakeTimeout:   tlsHandshakeTimeout,
		}
	}

	transport := defaultTransport.Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.TLSHandshakeTimeout = tlsHandshakeTimeout
	transport.DialContext = (&net.Dialer{
		Timeout:   dialTimeout,
		KeepAlive: -1,
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     keepAliveIdle,
			Interval: keepAliveInterval,
			Count:    keepAliveCount,
		},
	}).DialContext

	return transport
}
