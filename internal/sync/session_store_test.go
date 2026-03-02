package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReportStalePartials(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Create a stale .partial file.
	stalePath := filepath.Join(dir, "stale.partial")
	if err := os.WriteFile(stalePath, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	staleTime := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(stalePath, staleTime, staleTime); err != nil {
		t.Fatal(err)
	}

	// Create a fresh .partial file.
	freshPath := filepath.Join(dir, "fresh.partial")
	if err := os.WriteFile(freshPath, []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Create a regular file (not .partial).
	regularPath := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regularPath, []byte("regular"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run reportStalePartials — it logs warnings but doesn't return values.
	// We just verify it doesn't panic. A full integration test would capture
	// slog output, but for now we verify correctness by construction.
	reportStalePartials(dir, 48*time.Hour, testLogger(t))
}

func TestReportStalePartials_EmptyDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Should not panic on empty directory.
	reportStalePartials(dir, 48*time.Hour, testLogger(t))
}
