package sync

import (
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFailureTracker_SkipsAfterThreshold(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	path := "docs/report.docx"

	// First two failures: should not skip.
	ft.recordFailure(path, "upload failed: 500")
	assert.False(t, ft.shouldSkip(path), "should not skip after 1 failure")

	ft.recordFailure(path, "upload failed: 500")
	assert.False(t, ft.shouldSkip(path), "should not skip after 2 failures")

	// Third failure: threshold reached, should skip.
	ft.recordFailure(path, "upload failed: 500")
	assert.True(t, ft.shouldSkip(path), "should skip after 3 failures")
}

func TestFailureTracker_CooldownResetsCount(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	now := time.Now()
	ft.nowFunc = func() time.Time { return now }

	path := "data/big.csv"

	// Record threshold failures.
	for range failureThreshold {
		ft.recordFailure(path, "timeout")
	}

	require.True(t, ft.shouldSkip(path), "should skip after threshold failures")

	// Advance past cooldown.
	ft.nowFunc = func() time.Time { return now.Add(failureCooldown + time.Second) }

	assert.False(t, ft.shouldSkip(path), "should not skip after cooldown expires")
}

func TestFailureTracker_SuccessClearsRecord(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	path := "images/photo.jpg"

	for range failureThreshold {
		ft.recordFailure(path, "hash mismatch")
	}

	require.True(t, ft.shouldSkip(path), "should skip after threshold failures")

	ft.recordSuccess(path)

	assert.False(t, ft.shouldSkip(path), "should not skip after success clears record")
}

func TestFailureTracker_DifferentPathsIndependent(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	path1 := "a/file1.txt"
	path2 := "b/file2.txt"

	for range failureThreshold {
		ft.recordFailure(path1, "error")
	}

	assert.True(t, ft.shouldSkip(path1), "path1 should be skipped")
	assert.False(t, ft.shouldSkip(path2), "path2 should not be affected by path1 failures")
}
