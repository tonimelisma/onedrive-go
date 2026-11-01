package graph

import (
	"log/slog"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateGraphBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{
			name:   "allows public graph host",
			rawURL: "https://graph.microsoft.com",
		},
		{
			name:   "allows sovereign graph host",
			rawURL: "https://dod-graph.microsoft.us",
		},
		{
			name:   "allows loopback http host",
			rawURL: "http://127.0.0.1:8080",
		},
		{
			name:    "rejects userinfo",
			rawURL:  "https://user@graph.microsoft.com",
			wantErr: "must not contain userinfo",
		},
		{
			name:    "rejects query string",
			rawURL:  "https://graph.microsoft.com?debug=1",
			wantErr: "must not contain query or fragment",
		},
		{
			name:    "rejects empty host",
			rawURL:  "https:///v1.0",
			wantErr: "host is empty",
		},
		{
			name:    "rejects insecure remote host",
			rawURL:  "http://graph.microsoft.com",
			wantErr: "must use https",
		},
		{
			name:    "rejects untrusted host",
			rawURL:  "https://example.com",
			wantErr: "is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateGraphBaseURL(tt.rawURL)
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateUploadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{
			name:   "allows sharepoint upload host",
			rawURL: "https://contoso.sharepoint.com/upload",
		},
		{
			name:   "allows personal upload host",
			rawURL: "https://my.microsoftpersonalcontent.com/personal/upload",
		},
		{
			name:    "rejects nil URL",
			wantErr: "upload URL is nil",
		},
		{
			name:    "rejects userinfo",
			rawURL:  "https://user@contoso.sharepoint.com/upload",
			wantErr: "must not contain userinfo",
		},
		{
			name:    "rejects insecure scheme",
			rawURL:  "http://contoso.sharepoint.com/upload",
			wantErr: "must use https",
		},
		{
			name:    "rejects ip literal",
			rawURL:  "https://127.0.0.1/upload",
			wantErr: "must not be an IP literal",
		},
		{
			name:    "rejects untrusted host",
			rawURL:  "https://example.com/upload",
			wantErr: "is not allowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var parsed *url.URL
			if tt.rawURL != "" {
				var err error
				parsed, err = url.Parse(tt.rawURL)
				require.NoError(t, err)
			}

			err := validateUploadURL(parsed)
			if tt.wantErr == "" {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestValidateCopyMonitorURL(t *testing.T) {
	t.Parallel()

	parsed, err := url.Parse("https://graph.microsoft.com/v1.0/monitor")
	require.NoError(t, err)
	require.NoError(t, validateCopyMonitorURL(parsed))

	personal, err := url.Parse("https://my.microsoftpersonalcontent.com/personal/monitor")
	require.NoError(t, err)
	require.NoError(t, validateCopyMonitorURL(personal))

	untrusted, err := url.Parse("https://example.com/v1.0/monitor")
	require.NoError(t, err)

	err = validateCopyMonitorURL(untrusted)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not allowed")
}

func TestParseAndValidatePreAuthURL(t *testing.T) {
	t.Parallel()

	validated, err := parseAndValidatePreAuthURL("https://contoso.sharepoint.com/upload", "upload", validateUploadURL)
	require.NoError(t, err)
	assert.Equal(t, "https://contoso.sharepoint.com/upload", validated)

	_, err = parseAndValidatePreAuthURL("://bad url?tempauth=secret-token", "upload", validateUploadURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing upload URL")
	assert.NotContains(t, err.Error(), "secret-token",
		"a malformed pre-authenticated URL must not leak its credential into the error text")

	_, err = parseAndValidatePreAuthURL("://bad url?tempauth=secret-token", "download", validateDownloadURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing download URL")
	assert.NotContains(t, err.Error(), "secret-token")

	_, err = parseAndValidatePreAuthURL("https://example.com/upload", "upload", validateUploadURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is not allowed")
}

func TestMatchesAllowedHost(t *testing.T) {
	t.Parallel()

	assert.True(t, matchesAllowedHost("GRAPH.MICROSOFT.COM", "graph.microsoft.com"))
	assert.True(t, matchesAllowedHost("tenant.graph.microsoft.com", "graph.microsoft.com"))
	assert.False(t, matchesAllowedHost("example.com", "graph.microsoft.com"))
}

func TestIsLoopbackHostname(t *testing.T) {
	t.Parallel()

	assert.True(t, isLoopbackHostname("localhost"))
	assert.True(t, isLoopbackHostname("127.0.0.1"))
	assert.True(t, isLoopbackHostname("::1"))
	assert.False(t, isLoopbackHostname("10.0.0.1"))
}

// Validates: R-6.8.22
func TestValidateDownloadURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		rawURL  string
		wantErr string
	}{
		{name: "allows personal content host", rawURL: "https://my.microsoftpersonalcontent.com/personal/download?tempauth=t"},
		{name: "allows sharepoint host", rawURL: "https://contoso-my.sharepoint.com/personal/u/_layouts/15/download.aspx?tempauth=t"},
		{name: "allows 1drv host", rawURL: "https://public.dm.files.1drv.com/y4m/file.txt"},
		{name: "rejects http", rawURL: "http://my.microsoftpersonalcontent.com/download", wantErr: "must use https"},
		{name: "rejects IP literal", rawURL: "https://127.0.0.1/download", wantErr: "IP literal"},
		{name: "rejects non-Microsoft host", rawURL: "https://example.com/download", wantErr: "is not allowed"},
		{name: "rejects userinfo", rawURL: "https://someone@my.microsoftpersonalcontent.com/download", wantErr: "userinfo"},
		{name: "rejects graph host", rawURL: "https://graph.microsoft.com/v1.0/drives/d/items/i/content", wantErr: "is not allowed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := url.Parse(tt.rawURL)
			require.NoError(t, err)

			err = validateDownloadURL(parsed)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Contains(t, err.Error(), "download URL")
		})
	}
}

// Validates: R-6.8.22
//
// A loopback Graph base URL exists only for test fixtures; the fixture's
// pre-authenticated URLs point back at the same loopback server, so the
// client built against it accepts loopback http/https pre-auth URLs and
// nothing else beyond the production allow-lists.
func TestNewClient_LoopbackBaseURLAcceptsLoopbackPreAuthURLs(t *testing.T) {
	t.Parallel()

	loopbackClient := MustNewClient("http://127.0.0.1:8080", http.DefaultClient, staticToken("t"), slog.Default(), "test-agent")
	productionClient := MustNewClient(DefaultBaseURL, http.DefaultClient, staticToken("t"), slog.Default(), "test-agent")

	loopback, err := url.Parse("http://127.0.0.1:8080/download")
	require.NoError(t, err)
	public, err := url.Parse("https://example.com/download")
	require.NoError(t, err)
	trusted, err := url.Parse("https://my.microsoftpersonalcontent.com/download")
	require.NoError(t, err)

	for name, validate := range map[string]func(*url.URL) error{
		"download":         loopbackClient.validators.downloadURL,
		"upload":           loopbackClient.validators.uploadURL,
		"copy monitor":     loopbackClient.validators.copyMonitor,
		"content redirect": loopbackClient.validators.contentRedirect,
	} {
		require.NotNil(t, validate, name)
		require.NoError(t, validate(loopback), "%s: loopback fixture URL", name)
		require.NoError(t, validate(trusted), "%s: production host still allowed", name)
		require.Error(t, validate(public), "%s: a loopback base URL must not admit arbitrary hosts", name)
	}

	for name, validate := range map[string]func(*url.URL) error{
		"download":         productionClient.validators.downloadURL,
		"upload":           productionClient.validators.uploadURL,
		"copy monitor":     productionClient.validators.copyMonitor,
		"content redirect": productionClient.validators.contentRedirect,
	} {
		require.NotNil(t, validate, name)
		require.Error(t, validate(loopback), "%s: a production client never trusts loopback", name)
	}
}
