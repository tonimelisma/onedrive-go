// Package syncverify re-hashes local files against the persisted sync baseline
// through a rooted sync-tree capability.
package syncverify

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"

	"github.com/tonimelisma/onedrive-go/internal/synctree"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	syncengine "github.com/tonimelisma/onedrive-go/internal/sync"
)

type verifyRoot interface {
	// Spelling resolves a stored NFC path to the entry's spelling on disk
	// (R-2.13.2).
	Spelling(rel string) string
	Stat(rel string) (os.FileInfo, error)
	Abs(rel string) (string, error)
}

type hashComputer func(absPath string) (string, error)

// Verify status constants (used in VerifyResult.Status).
const (
	VerifyOK           = "ok"
	VerifyMissing      = "missing"
	VerifyHashMismatch = "hash_mismatch"
	VerifySizeMismatch = "size_mismatch"

	// VerifyUnverifiable marks a baseline entry that carries no local hash to
	// compare against, so its content was never checked.
	VerifyUnverifiable = "unverifiable"

	// VerifyUnreadable marks a file that exists (or may exist) but could not
	// be stat'ed or read. It is not evidence of corruption or loss, so it is
	// reported apart from missing and hash_mismatch, with the reason.
	VerifyUnreadable = "unreadable"
)

// VerifyBaseline performs a full-tree hash verification of local files against
// baseline entries. Read-only — no database writes, no Graph client needed.
// Files on disk not in the baseline are ignored (not yet synced). Folders are
// skipped (no content hash).
func VerifyBaseline(
	ctx context.Context,
	bl *syncengine.Baseline,
	tree *synctree.Root,
	logger *slog.Logger,
) (*Report, error) {
	return verifyBaselineWithHasher(ctx, bl, tree, driveops.ComputeQuickXorHash, logger)
}

func verifyBaselineWithHasher(
	ctx context.Context,
	bl *syncengine.Baseline,
	tree verifyRoot,
	computeHash hashComputer,
	logger *slog.Logger,
) (*Report, error) {
	report := &Report{}

	var ctxErr error

	bl.ForEachPath(func(relPath string, entry *syncengine.BaselineEntry) {
		if ctxErr != nil {
			return
		}

		if ctx.Err() != nil {
			ctxErr = fmt.Errorf("verify canceled: %w", ctx.Err())
			return
		}

		// Skip folders and root entries — no content hash to verify.
		if entry.ItemType != syncengine.ItemTypeFile {
			return
		}

		result := verifyEntry(tree, relPath, entry, computeHash, logger)
		switch result.Status {
		case VerifyOK:
			report.Verified++
		case VerifyUnverifiable:
			report.Unverifiable = append(report.Unverifiable, result)
		default:
			report.Mismatches = append(report.Mismatches, result)
		}
	})

	if ctxErr != nil {
		return nil, ctxErr
	}

	sort.Slice(report.Mismatches, func(i, j int) bool {
		return report.Mismatches[i].Path < report.Mismatches[j].Path
	})
	sort.Slice(report.Unverifiable, func(i, j int) bool {
		return report.Unverifiable[i].Path < report.Unverifiable[j].Path
	})

	return report, nil
}

func unreadableResult(entry *syncengine.BaselineEntry, err error) Result {
	return Result{
		Path:     entry.Path,
		Status:   VerifyUnreadable,
		Expected: entry.LocalHash,
		Reason:   err.Error(),
	}
}

// verifyEntry checks a single baseline entry against the local filesystem.
func verifyEntry(
	tree verifyRoot,
	relPath string,
	entry *syncengine.BaselineEntry,
	computeHash hashComputer,
	logger *slog.Logger,
) Result {
	// The baseline path is NFC; on a byte-exact filesystem an NFD-named file
	// is found only by its own spelling.
	local := tree.Spelling(relPath)
	info, err := tree.Stat(local)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Result{
				Path:     entry.Path,
				Status:   VerifyMissing,
				Expected: entry.LocalHash,
			}
		}

		// The result is the report of this file; a per-file WARN on top of it
		// would print every unreadable file twice.
		logger.Debug("verify: stat failed", slog.String("path", entry.Path), slog.Any("error", err))

		return unreadableResult(entry, err)
	}

	// Check size first (fast path) when the baseline knows the local file size.
	if entry.LocalSizeKnown && info.Size() != entry.LocalSize {
		return Result{
			Path:     entry.Path,
			Status:   VerifySizeMismatch,
			Expected: fmt.Sprintf("%d", entry.LocalSize),
			Actual:   fmt.Sprintf("%d", info.Size()),
		}
	}

	// A baseline entry can carry no local hash: SharePoint-enriched files
	// populate only remote_hash, and the scanner records an empty hash when
	// hashing fails. There is nothing to compare against, so the file is
	// reported as unchecked rather than counted among the verified.
	if entry.LocalHash == "" {
		return Result{Path: entry.Path, Status: VerifyUnverifiable}
	}

	absPath, err := tree.Abs(local)
	if err != nil {
		logger.Debug("verify: rooted path failed", slog.String("path", entry.Path), slog.Any("error", err))

		return unreadableResult(entry, err)
	}

	hash, err := computeHash(absPath)
	if err != nil {
		logger.Debug("verify: hash failed", slog.String("path", entry.Path), slog.Any("error", err))

		return unreadableResult(entry, err)
	}

	if hash != entry.LocalHash {
		return Result{
			Path:     entry.Path,
			Status:   VerifyHashMismatch,
			Expected: entry.LocalHash,
			Actual:   hash,
		}
	}

	return Result{Path: entry.Path, Status: VerifyOK}
}
