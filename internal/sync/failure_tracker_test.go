package sync

import (
	"log/slog"
	"os"
	"testing"
	"time"
)

func TestFailureTracker_SkipsAfterThreshold(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	path := "docs/report.docx"

	// First two failures: should not skip.
	ft.recordFailure(path, "upload failed: 500")
	if ft.shouldSkip(path) {
		t.Fatal("should not skip after 1 failure")
	}

	ft.recordFailure(path, "upload failed: 500")
	if ft.shouldSkip(path) {
		t.Fatal("should not skip after 2 failures")
	}

	// Third failure: threshold reached, should skip.
	ft.recordFailure(path, "upload failed: 500")
	if !ft.shouldSkip(path) {
		t.Fatal("should skip after 3 failures")
	}
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

	if !ft.shouldSkip(path) {
		t.Fatal("should skip after threshold failures")
	}

	// Advance past cooldown.
	ft.nowFunc = func() time.Time { return now.Add(failureCooldown + time.Second) }

	if ft.shouldSkip(path) {
		t.Fatal("should not skip after cooldown expires")
	}
}

func TestFailureTracker_SuccessClearsRecord(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	ft := newFailureTracker(logger)

	path := "images/photo.jpg"

	for range failureThreshold {
		ft.recordFailure(path, "hash mismatch")
	}

	if !ft.shouldSkip(path) {
		t.Fatal("should skip after threshold failures")
	}

	ft.recordSuccess(path)

	if ft.shouldSkip(path) {
		t.Fatal("should not skip after success clears record")
	}
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

	if !ft.shouldSkip(path1) {
		t.Fatal("path1 should be skipped")
	}

	if ft.shouldSkip(path2) {
		t.Fatal("path2 should not be affected by path1 failures")
	}
}
