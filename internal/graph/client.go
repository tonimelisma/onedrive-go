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

	// maxItemResponseSize caps a success-path driveItem body. Every other
	// response in this package is either streamed or already bounded; this one
	// is buffered whole to tell an empty success body apart from a real item,
	// so it needs its own ceiling. A driveItem is a few KB, so no legitimate
	// response approaches this.
	maxItemResponseSize = 1024 * 1024

	// defaultMaxListPages bounds every non-delta paged listing. A circular or
	// buggy nextLink chain would otherwise loop until the process dies, and
	// unlike delta these listings have no natural terminator to fall back on.
	defaultMaxListPages = 10000

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
// retryPolicies is the per-call-site retry configuration. It is one value
// rather than eight fields because the eight are configured together and read
// together: as separate fields, a caller could set three and silently inherit
// defaults for the rest, and nothing in the type said they were a set.
type retryPolicies struct {
	driveDiscovery       retry.Policy
	rootChildren         retry.Policy
	downloadMetadata     retry.Policy
	createFolderReadback retry.Policy
	simpleUploadMtime    retry.Policy
	uploadSessionCreate  retry.Policy
	copyDestination      retry.Policy
	simpleUploadCreate   retry.Policy
}

// urlValidators guard the server-supplied URLs the client will follow. They are
// grouped for the same reason as the policies: they are one safety decision,
// not three independent knobs.
type urlValidators struct {
	uploadURL   func(*url.URL) error
	copyMonitor func(*url.URL) error
	socketIO    func(*url.URL) error
}

type Client struct {
	baseURL              string
	httpClient           *http.Client
	token                TokenSource
	logger               *slog.Logger
	userAgent            string
	authSuccessHook      func(context.Context)
	deltaPreferHeader    http.Header
	childrenPreferHeader http.Header
	maxDeltaPages        int
	maxRecursionDepth    int
	policies             retryPolicies
	validators           urlValidators
}

// SetAuthenticatedSuccessHook installs a best-effort callback that runs after
// each successful authenticated Graph API response. Pre-authenticated upload,
// download, and copy-monitor URLs do not flow through this hook.
func (c *Client) SetAuthenticatedSuccessHook(hook func(context.Context)) {
	c.authSuccessHook = hook
}
