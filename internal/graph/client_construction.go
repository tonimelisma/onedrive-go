package graph

import (
	"fmt"
	"log/slog"
	"net/http"

	"github.com/tonimelisma/onedrive-go/internal/retry"
)

// NewClient creates a Graph API client.
// baseURL is typically "https://graph.microsoft.com/v1.0".
// userAgent is sent in every request; defaults to "onedrive-go/dev" if empty.
// Retry is handled by the httpClient's Transport (wrap with retry.RetryTransport
// for automatic retry, or use http.DefaultTransport for single-attempt dispatch).
func NewClient(
	baseURL string, httpClient *http.Client, token TokenSource,
	logger *slog.Logger, userAgent string,
) (*Client, error) {
	if token == nil {
		return nil, fmt.Errorf("graph.NewClient: token source must not be nil")
	}

	if logger == nil {
		logger = slog.Default()
	}

	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	if userAgent == "" {
		userAgent = defaultUserAgent
	}

	if err := validateGraphBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("graph.NewClient: invalid base URL: %w", err)
	}

	return &Client{
		baseURL:                    baseURL,
		httpClient:                 httpClient,
		token:                      token,
		logger:                     logger,
		userAgent:                  userAgent,
		deltaPreferHeader:          newDeltaPreferHeader(),
		childrenPreferHeader:       newChildrenPreferHeader(),
		maxDeltaPages:              defaultMaxDeltaPages,
		maxRecursionDepth:          defaultMaxRecursionDepth,
		driveDiscoveryPolicy:       retry.DriveDiscoveryPolicy(),
		rootChildrenPolicy:         retry.RootChildrenPolicy(),
		downloadMetadataPolicy:     retry.DownloadMetadataPolicy(),
		createFolderReadbackPolicy: retry.PathVisibilityPolicy(),
		simpleUploadMtimePolicy:    retry.SimpleUploadMtimePatchPolicy(),
		uploadSessionCreatePolicy:  retry.UploadSessionCreatePolicy(),
		copyDestinationPolicy:      retry.CopyDestinationPolicy(),
		simpleUploadCreatePolicy:   retry.SimpleUploadCreatePolicy(),
		uploadURLValidator:         validateUploadURL,
		copyMonitorValidator:       validateCopyMonitorURL,
		socketIOValidator:          validateSocketIONotificationURL,
	}, nil
}

// MustNewClient is for test/setup call sites that intentionally want a panic
// on invalid static construction parameters.
func MustNewClient(
	baseURL string, httpClient *http.Client, token TokenSource,
	logger *slog.Logger, userAgent string,
) *Client {
	client, err := NewClient(baseURL, httpClient, token, logger, userAgent)
	if err != nil {
		panic(err)
	}

	return client
}

func newDeltaPreferHeader() http.Header {
	return http.Header{
		"Prefer": {"deltashowremoteitemsaliasid, Include-Feature=AddToOneDrive"},
	}
}

func newChildrenPreferHeader() http.Header {
	return http.Header{
		"Prefer": {"Include-Feature=AddToOneDrive"},
	}
}
