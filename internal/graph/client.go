package graph

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// DefaultBaseURL is the production Microsoft Graph API v1.0 endpoint.
const DefaultBaseURL = "https://graph.microsoft.com/v1.0"

const (
	defaultUserAgent = "onedrive-go/dev"

	// maxErrBodySize caps error response body reads to prevent OOM from
	// malicious or buggy servers returning enormous error responses (B-314).
	maxErrBodySize = 64 * 1024

	// maxDeltaPages is the upper bound on pages fetched by DeltaAll/DeltaFolderAll.
	// A buggy API or circular NextLinks could loop forever without this guard.
	defaultMaxDeltaPages = 10000

	// maxRecursionDepth is the upper bound on folder nesting depth for
	// ListChildrenRecursive. Prevents stack overflow on pathological hierarchies
	// or circular references.
	defaultMaxRecursionDepth = 100
)

// TokenSource provides OAuth2 bearer tokens.
// Defined at the consumer (graph/) per "accept interfaces, return structs" —
// do not move this interface to the auth provider package.
type TokenSource interface {
	Token() (string, error)
}

// Client is a pure HTTP client for the Microsoft Graph API. It handles request
// construction, authentication (including 401 token refresh), and error
// classification. Generic retry logic lives in retry.RetryTransport, while the
// client itself keeps only narrow Graph-quirk retries for documented
// misreported errors. This separation keeps generic resilience in the transport
// layer and preserves caller control (CLI: RetryTransport, sync: raw
// transport, single attempt, engine records failure for the engine retry
// sweep).
type Client struct {
	baseURL                    string
	httpClient                 *http.Client
	token                      TokenSource
	logger                     *slog.Logger
	userAgent                  string
	authSuccessHook            func(context.Context)
	deltaPreferHeader          http.Header
	childrenPreferHeader       http.Header
	maxDeltaPages              int
	maxRecursionDepth          int
	driveDiscoveryPolicy       retry.Policy
	rootChildrenPolicy         retry.Policy
	downloadMetadataPolicy     retry.Policy
	createFolderReadbackPolicy retry.Policy
	simpleUploadMtimePolicy    retry.Policy
	uploadSessionCreatePolicy  retry.Policy
	copyDestinationPolicy      retry.Policy
	simpleUploadCreatePolicy   retry.Policy
	uploadURLValidator         func(*url.URL) error
	copyMonitorValidator       func(*url.URL) error
	socketIOValidator          func(*url.URL) error
}

// SetAuthenticatedSuccessHook installs a best-effort callback that runs after
// each successful authenticated Graph API response. Pre-authenticated upload,
// download, and copy-monitor URLs do not flow through this hook.
func (c *Client) SetAuthenticatedSuccessHook(hook func(context.Context)) {
	c.authSuccessHook = hook
}
