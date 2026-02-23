package sync

import (
	"context"
	"encoding/base64"
	"testing"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
	"github.com/tonimelisma/onedrive-go/pkg/quickxorhash"
)

// hashBytes computes the QuickXorHash of raw bytes and returns the
// base64-encoded digest, matching the format stored in the baseline.
func hashBytes(t *testing.T, data []byte) string {
	t.Helper()

	h := quickxorhash.New()
	h.Write(data)

	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

func TestVerifyBaseline_AllMatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "hello verify"

	writeTestFile(t, dir, "docs/readme.md", content)
	writeTestFile(t, dir, "notes.txt", content)

	hash := hashBytes(t, []byte(content))
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs/readme.md": {
				Path: "docs/readme.md", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
			"notes.txt": {
				Path: "notes.txt", DriveID: driveid.New("d"), ItemID: "i2",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	if report.Verified != 2 {
		t.Errorf("Verified = %d, want 2", report.Verified)
	}

	if len(report.Mismatches) != 0 {
		t.Errorf("expected 0 mismatches, got %d", len(report.Mismatches))
	}
}

func TestVerifyBaseline_MissingFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Baseline references a file that doesn't exist on disk.
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"ghost.txt": {
				Path: "ghost.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 100,
			},
		},
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	if report.Verified != 0 {
		t.Errorf("Verified = %d, want 0", report.Verified)
	}

	if len(report.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d", len(report.Mismatches))
	}

	if report.Mismatches[0].Status != VerifyMissing {
		t.Errorf("Status = %q, want %q", report.Mismatches[0].Status, VerifyMissing)
	}
}

func TestVerifyBaseline_HashMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "modified content"

	writeTestFile(t, dir, "changed.txt", content)

	// Baseline has a different hash than what's on disk.
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"changed.txt": {
				Path: "changed.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "wrong-hash", Size: int64(len(content)),
			},
		},
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	if report.Verified != 0 {
		t.Errorf("Verified = %d, want 0", report.Verified)
	}

	if len(report.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d", len(report.Mismatches))
	}

	if report.Mismatches[0].Status != VerifyHashMismatch {
		t.Errorf("Status = %q, want %q", report.Mismatches[0].Status, VerifyHashMismatch)
	}

	// Actual should be the real hash.
	actualHash := hashBytes(t, []byte(content))
	if report.Mismatches[0].Actual != actualHash {
		t.Errorf("Actual = %q, want %q", report.Mismatches[0].Actual, actualHash)
	}
}

func TestVerifyBaseline_EmptyBaseline(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	bl := &Baseline{
		ByPath: make(map[string]*BaselineEntry),
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	if report.Verified != 0 {
		t.Errorf("Verified = %d, want 0", report.Verified)
	}

	if len(report.Mismatches) != 0 {
		t.Errorf("expected 0 mismatches, got %d", len(report.Mismatches))
	}
}

func TestVerifyBaseline_SkipsFolders(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "file content"

	writeTestFile(t, dir, "docs/file.txt", content)

	hash := hashBytes(t, []byte(content))
	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"docs": {
				Path: "docs", DriveID: driveid.New("d"), ItemID: "folder1",
				ItemType: ItemTypeFolder,
			},
			"docs/file.txt": {
				Path: "docs/file.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: hash, Size: int64(len(content)),
			},
		},
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	// Only the file should be verified, not the folder.
	if report.Verified != 1 {
		t.Errorf("Verified = %d, want 1", report.Verified)
	}

	if len(report.Mismatches) != 0 {
		t.Errorf("expected 0 mismatches, got %d", len(report.Mismatches))
	}
}

func TestVerifyBaseline_SizeMismatch(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "short"

	writeTestFile(t, dir, "size.txt", content)

	bl := &Baseline{
		ByPath: map[string]*BaselineEntry{
			"size.txt": {
				Path: "size.txt", DriveID: driveid.New("d"), ItemID: "i1",
				ItemType: ItemTypeFile, LocalHash: "somehash", Size: 99999,
			},
		},
	}

	ctx := context.Background()
	logger := testLogger(t)

	report, err := VerifyBaseline(ctx, bl, dir, logger)
	if err != nil {
		t.Fatalf("VerifyBaseline: %v", err)
	}

	if len(report.Mismatches) != 1 {
		t.Fatalf("expected 1 mismatch, got %d", len(report.Mismatches))
	}

	if report.Mismatches[0].Status != VerifySizeMismatch {
		t.Errorf("Status = %q, want %q", report.Mismatches[0].Status, VerifySizeMismatch)
	}
}
