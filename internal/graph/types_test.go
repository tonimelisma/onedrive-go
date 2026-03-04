package graph

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDownloadURL_LogValuer verifies that DownloadURL implements slog.LogValuer
// and redacts the actual URL when logged, preventing accidental exposure of
// embedded authentication tokens (B-158).
func TestDownloadURL_LogValuer(t *testing.T) {
	t.Parallel()

	secretURL := DownloadURL("https://public.bn1304.livefilestore.com/y4msecret-token-here/file.txt")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		// Remove time for deterministic output.
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	}))

	logger.Info("download started", "url", secretURL)

	output := buf.String()

	assert.Contains(t, output, "[REDACTED]")
	assert.NotContains(t, output, "secret-token-here")
}

// TestDownloadURL_EmptyComparison verifies that an empty DownloadURL compares
// correctly with the empty string literal.
func TestDownloadURL_EmptyComparison(t *testing.T) {
	t.Parallel()

	var empty DownloadURL
	assert.Equal(t, DownloadURL(""), empty, "zero-value DownloadURL should equal empty string")

	populated := DownloadURL("https://example.com/download")
	assert.NotEqual(t, DownloadURL(""), populated, "populated DownloadURL should not equal empty string")
}

// TestDownloadURL_StringConversion verifies that DownloadURL can be converted
// to string for use in HTTP requests.
func TestDownloadURL_StringConversion(t *testing.T) {
	t.Parallel()

	url := DownloadURL("https://example.com/download?token=abc")
	s := string(url)

	assert.Equal(t, "https://example.com/download?token=abc", s)
}

// TestUploadURL_LogValuer verifies that UploadURL implements slog.LogValuer
// and redacts the actual URL when logged, preventing accidental exposure of
// embedded authentication tokens (B-315).
func TestUploadURL_LogValuer(t *testing.T) {
	t.Parallel()

	secretURL := UploadURL("https://storage.live.com/uploadSession/secret-token-here")

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}

			return a
		},
	}))

	logger.Info("upload started", "url", secretURL)

	output := buf.String()

	assert.Contains(t, output, "[REDACTED]")
	assert.NotContains(t, output, "secret-token-here")
}

// TestUploadURL_EmptyComparison verifies that an empty UploadURL compares
// correctly with the empty string literal.
func TestUploadURL_EmptyComparison(t *testing.T) {
	t.Parallel()

	var empty UploadURL
	assert.Equal(t, UploadURL(""), empty, "zero-value UploadURL should equal empty string")

	populated := UploadURL("https://example.com/upload")
	assert.NotEqual(t, UploadURL(""), populated, "populated UploadURL should not equal empty string")
}

// TestUploadURL_StringConversion verifies that UploadURL can be converted
// to string for use in HTTP requests.
func TestUploadURL_StringConversion(t *testing.T) {
	t.Parallel()

	url := UploadURL("https://example.com/upload?token=xyz")
	s := string(url)

	assert.Equal(t, "https://example.com/upload?token=xyz", s)
}
