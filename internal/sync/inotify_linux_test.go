//go:build linux

package sync

import (
	"bytes"
	"fmt"
	"log/slog"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadInotifyLimit_ReadsRealProc(t *testing.T) {
	t.Parallel()

	limit, err := readInotifyLimit()
	require.NoError(t, err)
	assert.Positive(t, limit, "Linux should have a positive inotify limit")
}

func TestCheckInotifyCapacity_WarnsAboveThreshold(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Simulate 90% usage: if limit is 100, use 90 dirs.
	// We can't control the real limit, so use a very high number to guarantee warning.
	checkInotifyCapacity(999_999_999, logger)

	assert.Contains(t, buf.String(), "close to the system limit")
	assert.Contains(t, buf.String(), "max_user_watches")
}

func TestCheckInotifyCapacity_SilentBelowThreshold(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	// Use 1 directory — well below any threshold.
	checkInotifyCapacity(1, logger)

	assert.Empty(t, buf.String(), "should not warn at low usage")
}

// Validates: R-2.8.12
func TestClassifyWatcherCreateError_EMFILEIsInotifyInstanceLimit(t *testing.T) {
	t.Parallel()

	cause, ok := classifyWatcherCreateError(fmt.Errorf("couldn't initialize inotify: %w", syscall.EMFILE))
	require.True(t, ok)
	assert.Equal(t, watchCauseInstanceLimit, cause)
}
