package graph

import (
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

func TestParseAndValidateUploadURL(t *testing.T) {
	t.Parallel()

	validated, err := parseAndValidateUploadURL("https://contoso.sharepoint.com/upload", validateUploadURL)
	require.NoError(t, err)
	assert.Equal(t, "https://contoso.sharepoint.com/upload", validated)

	_, err = parseAndValidateUploadURL("://bad url", validateUploadURL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing upload URL")

	_, err = parseAndValidateUploadURL("https://example.com/upload", validateUploadURL)
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
