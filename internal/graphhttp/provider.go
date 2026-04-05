package graphhttp

import (
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
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

// Provider owns Graph-facing HTTP client construction and any shared runtime
// state associated with those clients.
type Provider struct {
	logger *slog.Logger

	mu                  sync.Mutex
	bootstrapMeta       *http.Client
	interactiveMeta     map[string]*http.Client
	interactiveTransfer *http.Client
	syncClients         *ClientSet
}

// NewProvider constructs a Provider with no package-level shared state.
func NewProvider(logger *slog.Logger) *Provider {
	if logger == nil {
		logger = slog.Default()
	}

	return &Provider{
		logger:          logger,
		interactiveMeta: make(map[string]*http.Client),
	}
}

// BootstrapMeta returns the retrying metadata client used before account
// identity is known.
func (p *Provider) BootstrapMeta() *http.Client {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.bootstrapMeta == nil {
		p.bootstrapMeta = p.retryingMetadataClient(nil)
	}

	return p.bootstrapMeta
}

// InteractiveForDrive returns the Graph client set for one interactive
// drive target. Metadata clients share a throttle gate per account+drive.
func (p *Provider) InteractiveForDrive(account string, driveID driveid.ID) ClientSet {
	return p.interactiveForKey(interactiveDriveKey(account, driveID))
}

// InteractiveForSharedTarget returns the Graph client set for one interactive
// shared target. Metadata clients share a throttle gate per account+shared target.
func (p *Provider) InteractiveForSharedTarget(account, remoteDriveID, remoteItemID string) ClientSet {
	return p.interactiveForKey(interactiveSharedKey(account, remoteDriveID, remoteItemID))
}

func (p *Provider) interactiveForKey(targetKey string) ClientSet {
	p.mu.Lock()
	defer p.mu.Unlock()

	meta := p.interactiveMeta[targetKey]
	if meta == nil {
		meta = p.retryingMetadataClient(&retry.ThrottleGate{})
		p.interactiveMeta[targetKey] = meta
	}

	if p.interactiveTransfer == nil {
		p.interactiveTransfer = p.retryingTransferClient()
	}

	return ClientSet{
		Meta:     meta,
		Transfer: p.interactiveTransfer,
	}
}

// Sync returns the non-retrying Graph client set used by sync workers.
func (p *Provider) Sync() ClientSet {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.syncClients == nil {
		p.syncClients = &ClientSet{
			Meta: &http.Client{
				Timeout:   0,
				Transport: metadataTransport(),
			},
			Transfer: &http.Client{
				Timeout:   0,
				Transport: transferTransport(),
			},
		}
	}

	return *p.syncClients
}

func (p *Provider) retryingMetadataClient(gate *retry.ThrottleGate) *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &retry.RetryTransport{
			Inner:        metadataTransport(),
			Policy:       retry.TransportPolicy(),
			Logger:       p.logger,
			ThrottleGate: gate,
		},
	}
}

func (p *Provider) retryingTransferClient() *http.Client {
	return &http.Client{
		Timeout: 0,
		Transport: &retry.RetryTransport{
			Inner:  transferTransport(),
			Policy: retry.TransportPolicy(),
			Logger: p.logger,
		},
	}
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

func interactiveDriveKey(account string, driveID driveid.ID) string {
	return account + "|drive:" + driveID.String()
}

func interactiveSharedKey(account, remoteDriveID, remoteItemID string) string {
	return account + "|shared:" + remoteDriveID + ":" + remoteItemID
}
