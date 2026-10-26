package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/graph"
)

func graphNotFound() error {
	return &graph.GraphError{StatusCode: http.StatusNotFound, RequestID: "req-1", Message: "Item not found", Err: graph.ErrNotFound}
}

// Validates: R-6.6.26
func TestCommandErrorText_RemoteFailuresNameThePathAndNextStep(t *testing.T) {
	t.Parallel()

	err := newRemoteOpError("resolving", "/docs/a.txt", fmt.Errorf("resolve item path %q: %w", "docs/a.txt", graphNotFound()))

	text := commandErrorText(err, false)
	assert.Equal(t, "Error: /docs/a.txt was not found on OneDrive. Run 'onedrive-go ls /docs' to see what is there.", text)
	assert.NotContains(t, text, "req-1", "request IDs belong in --verbose details")
	assert.NotContains(t, text, "graph:")

	verbose := commandErrorText(err, true)
	assert.Contains(t, verbose, "\nDetails: resolving \"/docs/a.txt\": resolve item path", "--verbose keeps the full chain")
}

// Validates: R-6.6.26
func TestCommandErrorText_RemoteFailureClasses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{"forbidden", newRemoteOpError("listing", "/team", &graph.GraphError{StatusCode: 403, Err: graph.ErrForbidden}), "OneDrive denied access to /team."},
		{"locked", newRemoteOpError("deleting", "/a.docx", &graph.GraphError{StatusCode: 423, Err: graph.ErrLocked}), "/a.docx is locked on OneDrive"},
		{"throttled", &graph.GraphError{StatusCode: 429, Err: graph.ErrThrottled}, "OneDrive is limiting requests right now."},
		{"server", &graph.GraphError{StatusCode: 503, Err: graph.ErrServerError}, "temporary server problem"},
		{"quota", newRemoteOpError("uploading", "/big.iso", &graph.GraphError{StatusCode: http.StatusInsufficientStorage, Err: graph.ErrServerError}), "Your OneDrive storage is full."},
		{"move conflict", newRemoteOpError("moving", "/a.txt", &graph.GraphError{StatusCode: 409, Err: graph.ErrConflict}), "The destination for /a.txt already exists on OneDrive. Pass --force to replace it."},
		{"no path", fmt.Errorf("wrapped: %w", graphNotFound()), "Error: the item was not found on OneDrive."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Contains(t, commandErrorText(tt.err, false), tt.want)
		})
	}
}

// Validates: R-6.6.26
func TestCommandErrorText_LocalFileFailuresDoNotStutter(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("stating local path: %w", fmt.Errorf("stating /home/u/x: %w", &fs.PathError{Op: "stat", Path: "/home/u/x", Err: fs.ErrNotExist}))

	assert.Equal(t, "Error: /home/u/x does not exist.", commandErrorText(err, false))
}

// Validates: R-6.6.26, R-2.10.60, R-2.10.61, R-2.10.62
func TestCommandErrorText_LocalFilesystemConditionsArePlain(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "enospc",
			err:  fmt.Errorf("downloading: %w", &fs.PathError{Op: "write", Path: "/home/u/big.iso", Err: syscall.ENOSPC}),
			want: "Error: The local disk is full (or below the min_free_space reserve). Free up space, then run the command again.",
		},
		{
			name: "read-only filesystem",
			err:  fmt.Errorf("creating: %w", &fs.PathError{Op: "open", Path: "/mnt/ro/a.txt", Err: syscall.EROFS}),
			want: "Error: /mnt/ro/a.txt is on a read-only filesystem. Remount it read-write, then run the command again.",
		},
		{
			name: "file too large for filesystem",
			err:  fmt.Errorf("writing: %w", &fs.PathError{Op: "write", Path: "/media/usb/movie.mkv", Err: syscall.EFBIG}),
			want: "Error: The file is too large for the filesystem holding /media/usb/movie.mkv.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, commandErrorText(tt.err, false))
		})
	}
}

// Validates: R-6.6.27
func TestCommandErrorText_TimeoutIsReported(t *testing.T) {
	t.Parallel()

	err := fmt.Errorf("waiting for sign-in: %w", context.DeadlineExceeded)

	assert.Equal(t, "Error: The operation timed out. Run the command again.", commandErrorText(err, false))
}

// Validates: R-6.6.27
func TestCommandErrorText_UsageErrorPointsAtHelp(t *testing.T) {
	t.Parallel()

	err := &usageError{commandPath: "onedrive-go get", err: errors.New("accepts between 1 and 2 arg(s), received 0")}

	assert.Equal(t,
		"Error: accepts between 1 and 2 arg(s), received 0\nRun 'onedrive-go get --help' for usage.",
		commandErrorText(err, false))
}

// Validates: R-6.6.27
func TestMainWithWriters_UsageErrorsExitTwo(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	for _, args := range [][]string{
		{"get"},
		{"ls", "--frobnicate"},
		{"--quiet", "--verbose", "ls", "/"},
	} {
		var stdout, stderr bytes.Buffer
		exitCode := mainWithWriters(args, &stdout, &stderr)

		assert.Equal(t, exitUsage, exitCode, "%v", args)
		assert.Contains(t, stderr.String(), "--help' for usage.", "%v", args)
		assert.Empty(t, stdout.String(), "%v", args)
	}
}

// Validates: R-6.6.27
func TestWriteCommandError_JournalLinesCarryErrPriority(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	writeCommandError(&buf, "Error: one\nRun 'x --help' for usage.")
	require.Equal(t, "Error: one\nRun 'x --help' for usage.\n", buf.String(), "a plain writer is not the journal")
}

// Validates: R-6.6.28
func TestBulkTransferError_NamesEachFailureAndFails(t *testing.T) {
	t.Parallel()

	require.NoError(t, bulkTransferError(nil, "download", nil))

	var stderr bytes.Buffer
	err := bulkTransferError(&stderr, "download", []string{"a.txt: boom", "b.txt: bang"})
	require.EqualError(t, err, "2 items failed to download")
	assert.Equal(t, "Failed: a.txt: boom\nFailed: b.txt: bang\n", stderr.String())

	require.EqualError(t, bulkTransferError(nil, "upload", []string{"x"}), "1 item failed to upload",
		"--json mode keeps failures in the document but still fails the command")
}

// Validates: R-6.6.22
func TestCommandErrorText_TransportFailuresArePlain(t *testing.T) {
	t.Parallel()

	reset := fmt.Errorf("listing %q: %w", "/", &url.Error{Op: "Get", URL: "https://graph.microsoft.com/v1.0/me", Err: io.ErrUnexpectedEOF})
	assert.Equal(t,
		"Error: the connection to OneDrive was interrupted. Check your network connection, then run the command again.",
		commandErrorText(reset, false))

	slow := &url.Error{Op: "Get", URL: "https://graph.microsoft.com/v1.0/me", Err: context.DeadlineExceeded}
	assert.Contains(t, commandErrorText(slow, false), "OneDrive did not answer in time")

	localTimeout := fmt.Errorf("reading: %w", os.ErrDeadlineExceeded)
	assert.NotContains(t, commandErrorText(localTimeout, false), "OneDrive", "only transport errors are described as OneDrive problems")
}

// Validates: R-6.6.26
func TestCommandErrorText_SignInCodeOutcomes(t *testing.T) {
	t.Parallel()

	assert.Contains(t, commandErrorText(fmt.Errorf("device login: %w", graph.ErrLoginCodeExpired), false), "sign-in code expired")
	assert.Contains(t, commandErrorText(fmt.Errorf("browser login: %w", graph.ErrLoginDeclined), false), "canceled or declined")
}

// http2StreamError has the shape and text of net/http's unexported HTTP/2
// stream error, which callers can only recognize by its text.
type http2StreamError struct{}

func (http2StreamError) Error() string {
	return "stream error: stream ID 7; INTERNAL_ERROR; received from peer"
}

type headerTimeoutError struct{}

func (headerTimeoutError) Error() string   { return "net/http: timeout awaiting response headers" }
func (headerTimeoutError) Timeout() bool   { return true }
func (headerTimeoutError) Temporary() bool { return true }

// Every transport failure, before or during the response, is one plain line
// rather than the whole wrapped chain; a local error that io.Copy returned
// during the same download is not blamed on OneDrive.
//
// Validates: R-6.6.22
func TestCommandErrorText_EveryTransportFailureIsPlain(t *testing.T) {
	t.Parallel()

	before := func(cause error) error {
		return fmt.Errorf("listing %q: %w", "/", &url.Error{Op: "Get", URL: "https://graph.microsoft.com/v1.0/me", Err: cause})
	}
	during := func(cause error) error {
		return fmt.Errorf("downloading %q: graph: streaming download content: %w", "/a.txt", &graph.ResponseReadError{Err: cause})
	}

	const interrupted = "Error: the connection to OneDrive was interrupted. Check your network connection, then run the command again."
	const slow = "Error: OneDrive did not answer in time. Check your network connection, then run the command again."

	for name, tc := range map[string]struct {
		err  error
		want string
	}{
		"EOF before the response":            {before(io.EOF), interrupted},
		"unexpected EOF while downloading":   {during(io.ErrUnexpectedEOF), interrupted},
		"HTTP/2 stream reset":                {before(http2StreamError{}), interrupted},
		"HTTP/2 stream reset mid-download":   {during(http2StreamError{}), interrupted},
		"connection reset mid-download":      {during(syscall.ECONNRESET), interrupted},
		"response-header timeout":            {before(headerTimeoutError{}), slow},
		"read timeout while downloading":     {during(os.ErrDeadlineExceeded), slow},
		"TLS alert":                          {before(tls.AlertError(40)), ""},
		"unknown certificate authority":      {before(x509.UnknownAuthorityError{}), ""},
		"certificate for the wrong hostname": {before(x509.HostnameError{Host: "graph.microsoft.com", Certificate: &x509.Certificate{}}), ""},
	} {
		got := commandErrorText(tc.err, false)
		if tc.want == "" {
			assert.Contains(t, got, "Error: the secure connection to OneDrive failed (", name)
			assert.NotContains(t, got, "listing", name)
			continue
		}
		assert.Equal(t, tc.want, got, name)
	}

	disk := fmt.Errorf("downloading %q: graph: streaming download content: %w", "/a.txt",
		&fs.PathError{Op: "write", Path: "/tmp/a.txt", Err: syscall.ENOSPC})
	assert.NotContains(t, commandErrorText(disk, false), "OneDrive", "a local write failure is not a OneDrive problem")
}

// A canceled or declined sign-in, or an expired sign-in code, is one plain
// line that says to run login again, never the wrapped sentinel chain.
//
// Validates: R-6.6.26
func TestCommandErrorText_SignInOutcomesSayRunLoginAgain(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		"Error: sign-in was canceled or declined. Run 'onedrive-go login' again.",
		commandErrorText(fmt.Errorf("authenticate account: browser login: %w", graph.ErrLoginDeclined), true))
	assert.Equal(t,
		"Error: the sign-in code expired before sign-in finished. Run 'onedrive-go login' again to get a new code.",
		commandErrorText(fmt.Errorf("authenticate account: device login: %w", graph.ErrLoginCodeExpired), true))
}
