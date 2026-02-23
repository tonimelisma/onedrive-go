package sync

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// Verify status constants (used in VerifyResult.Status).
const (
	VerifyOK           = "ok"
	VerifyMissing      = "missing"
	VerifyHashMismatch = "hash_mismatch"
	VerifySizeMismatch = "size_mismatch"
)

// VerifyBaseline performs a full-tree hash verification of local files against
// baseline entries. Read-only — no database writes, no graph client needed.
// Files on disk not in the baseline are ignored (not yet synced). Folders are
// skipped (no content hash).
func VerifyBaseline(ctx context.Context, bl *Baseline, syncRoot string, logger *slog.Logger) (*VerifyReport, error) {
	report := &VerifyReport{}

	for relPath, entry := range bl.ByPath {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("sync: verify canceled: %w", ctx.Err())
		}

		// Skip folders and root entries — no content hash to verify.
		if entry.ItemType != ItemTypeFile {
			continue
		}

		absPath := filepath.Join(syncRoot, relPath)
		result := verifyEntry(absPath, entry, logger)

		if result.Status == VerifyOK {
			report.Verified++
		} else {
			report.Mismatches = append(report.Mismatches, result)
		}
	}

	return report, nil
}

// verifyEntry checks a single baseline entry against the local filesystem.
func verifyEntry(absPath string, entry *BaselineEntry, logger *slog.Logger) VerifyResult {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return VerifyResult{
				Path:     entry.Path,
				Status:   VerifyMissing,
				Expected: entry.LocalHash,
			}
		}

		// Stat error (permission etc.) — treat as missing with note.
		logger.Warn("verify: stat failed", slog.String("path", entry.Path), slog.String("error", err.Error()))

		return VerifyResult{
			Path:     entry.Path,
			Status:   VerifyMissing,
			Expected: entry.LocalHash,
			Actual:   err.Error(),
		}
	}

	// Check size first (fast path).
	if entry.Size > 0 && info.Size() != entry.Size {
		return VerifyResult{
			Path:     entry.Path,
			Status:   VerifySizeMismatch,
			Expected: fmt.Sprintf("%d", entry.Size),
			Actual:   fmt.Sprintf("%d", info.Size()),
		}
	}

	// Skip hash check if baseline has no local hash (e.g., SharePoint-enriched
	// files where only remote_hash is populated).
	if entry.LocalHash == "" {
		return VerifyResult{Path: entry.Path, Status: VerifyOK}
	}

	hash, err := computeQuickXorHash(absPath)
	if err != nil {
		logger.Warn("verify: hash failed", slog.String("path", entry.Path), slog.String("error", err.Error()))

		return VerifyResult{
			Path:     entry.Path,
			Status:   VerifyHashMismatch,
			Expected: entry.LocalHash,
			Actual:   err.Error(),
		}
	}

	if hash != entry.LocalHash {
		return VerifyResult{
			Path:     entry.Path,
			Status:   VerifyHashMismatch,
			Expected: entry.LocalHash,
			Actual:   hash,
		}
	}

	return VerifyResult{Path: entry.Path, Status: VerifyOK}
}
