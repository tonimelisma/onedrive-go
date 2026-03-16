package syncstore

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/synctypes"
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
func VerifyBaseline(ctx context.Context, bl *synctypes.Baseline, syncRoot string, logger *slog.Logger) (*synctypes.VerifyReport, error) {
	report := &synctypes.VerifyReport{}

	var ctxErr error

	bl.ForEachPath(func(relPath string, entry *synctypes.BaselineEntry) {
		if ctxErr != nil {
			return
		}

		if ctx.Err() != nil {
			ctxErr = fmt.Errorf("sync: verify canceled: %w", ctx.Err())
			return
		}

		// Skip folders and root entries — no content hash to verify.
		if entry.ItemType != synctypes.ItemTypeFile {
			return
		}

		absPath := filepath.Join(syncRoot, relPath)
		result := verifyEntry(absPath, entry, logger)

		if result.Status == VerifyOK {
			report.Verified++
		} else {
			report.Mismatches = append(report.Mismatches, result)
		}
	})

	if ctxErr != nil {
		return nil, ctxErr
	}

	return report, nil
}

// verifyEntry checks a single baseline entry against the local filesystem.
func verifyEntry(absPath string, entry *synctypes.BaselineEntry, logger *slog.Logger) synctypes.VerifyResult {
	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return synctypes.VerifyResult{
				Path:     entry.Path,
				Status:   VerifyMissing,
				Expected: entry.LocalHash,
			}
		}

		// Stat error (permission etc.) — treat as missing with note.
		logger.Warn("verify: stat failed", slog.String("path", entry.Path), slog.String("error", err.Error()))

		return synctypes.VerifyResult{
			Path:     entry.Path,
			Status:   VerifyMissing,
			Expected: entry.LocalHash,
			Actual:   err.Error(),
		}
	}

	// Check size first (fast path).
	if entry.Size > 0 && info.Size() != entry.Size {
		return synctypes.VerifyResult{
			Path:     entry.Path,
			Status:   VerifySizeMismatch,
			Expected: fmt.Sprintf("%d", entry.Size),
			Actual:   fmt.Sprintf("%d", info.Size()),
		}
	}

	// Skip hash check if baseline has no local hash (e.g., SharePoint-enriched
	// files where only remote_hash is populated).
	if entry.LocalHash == "" {
		return synctypes.VerifyResult{Path: entry.Path, Status: VerifyOK}
	}

	hash, err := driveops.ComputeQuickXorHash(absPath)
	if err != nil {
		logger.Warn("verify: hash failed", slog.String("path", entry.Path), slog.String("error", err.Error()))

		return synctypes.VerifyResult{
			Path:     entry.Path,
			Status:   VerifyHashMismatch,
			Expected: entry.LocalHash,
			Actual:   err.Error(),
		}
	}

	if hash != entry.LocalHash {
		return synctypes.VerifyResult{
			Path:     entry.Path,
			Status:   VerifyHashMismatch,
			Expected: entry.LocalHash,
			Actual:   hash,
		}
	}

	return synctypes.VerifyResult{Path: entry.Path, Status: VerifyOK}
}
