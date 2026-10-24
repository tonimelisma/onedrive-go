package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
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
