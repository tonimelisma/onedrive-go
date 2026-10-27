package sync

import (
	"errors"
	"fmt"
	"log/slog"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Cross-platform smoke tests: verify inotify functions compile and don't panic.

func TestReadInotifyLimit_NoPanic(t *testing.T) {
	t.Parallel()

	limit, err := readInotifyLimit()
	// On Linux: limit > 0, err == nil. On other platforms: limit == 0, err == nil.
	if err != nil {
		t.Logf("readInotifyLimit returned error (ok on non-Linux): %v", err)
	}

	t.Logf("inotify limit: %d", limit)
}

func TestCheckInotifyCapacity_NoPanic(t *testing.T) {
	t.Parallel()

	// Must not panic regardless of platform.
	checkInotifyCapacity(100, slog.Default())
}

// Validates: R-2.8.12
func TestClassifyWatchAddError_ResourceLimitsAreWatchUnavailable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		err       error
		wantCause watchUnavailableCause
		wantOK    bool
	}{
		{name: "ENOSPC is the watch limit", err: syscall.ENOSPC, wantCause: watchCauseWatchLimit, wantOK: true},
		{name: "wrapped ENOSPC", err: fmt.Errorf("add watch %q: %w", "/x", syscall.ENOSPC), wantCause: watchCauseWatchLimit, wantOK: true},
		{name: "EMFILE is the open-file limit", err: syscall.EMFILE, wantCause: watchCauseFileLimit, wantOK: true},
		{name: "ENFILE is the open-file limit", err: syscall.ENFILE, wantCause: watchCauseFileLimit, wantOK: true},
		{name: "ENOMEM is kernel memory", err: syscall.ENOMEM, wantCause: watchCauseKernelMemory, wantOK: true},
		{name: "EPERM is a per-directory failure", err: syscall.EPERM},
		{name: "nil", err: nil},
		{name: "plain error", err: errors.New("permission denied")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cause, ok := classifyWatchAddError(tt.err)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantCause, cause)
		})
	}
}

// Validates: R-2.8.12
func TestClassifyWatcherCreateError_ResourceLimits(t *testing.T) {
	t.Parallel()

	cause, ok := classifyWatcherCreateError(fmt.Errorf("create: %w", syscall.ENFILE))
	require.True(t, ok)
	assert.Equal(t, watchCauseFileLimit, cause)

	cause, ok = classifyWatcherCreateError(syscall.ENOMEM)
	require.True(t, ok)
	assert.Equal(t, watchCauseKernelMemory, cause)

	cause, ok = classifyWatcherCreateError(syscall.EMFILE)
	require.True(t, ok)
	assert.Equal(t, watchCreateEMFILECause, cause)

	_, ok = classifyWatcherCreateError(errors.New("unsupported platform"))
	assert.False(t, ok)
}
