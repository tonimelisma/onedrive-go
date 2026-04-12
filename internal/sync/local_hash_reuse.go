package sync

import (
	"io/fs"
	"time"

	"github.com/tonimelisma/onedrive-go/internal/synctypes"
)

// CanReuseBaselineHash reports whether a caller can safely trust the cached
// baseline hash for a local file instead of re-reading the file contents.
//
// The contract intentionally matches the scanner's fast path:
//   - only files are eligible
//   - size and mtime must still match the baseline
//   - files inside the 1-second racily-clean window are never trusted
//
// This keeps the engine's retry/reobserve paths aligned with the main local
// scanner so we do not hash aggressively in one path while trusting metadata
// in another.
func CanReuseBaselineHash(info fs.FileInfo, base *synctypes.BaselineEntry, observeStartNano int64) bool {
	if info == nil || base == nil || info.IsDir() || base.ItemType != synctypes.ItemTypeFile || base.LocalHash == "" {
		return false
	}

	currentMtime := info.ModTime().UnixNano()
	if !base.LocalSizeKnown || info.Size() != base.LocalSize || !sameOneDriveComparableMtime(currentMtime, base.LocalMtime) {
		return false
	}

	return observeStartNano-currentMtime >= nanosPerSecond
}

// sameOneDriveComparableMtime compares local mtimes using the same precision
// OneDrive preserves on the wire. Local filesystems can retain sub-second
// precision that never survives a round-trip through Graph, so comparing raw
// UnixNano values would spuriously invalidate cached hashes after an otherwise
// lossless sync.
func sameOneDriveComparableMtime(leftNano, rightNano int64) bool {
	return truncateToWholeSecondUTC(leftNano) == truncateToWholeSecondUTC(rightNano)
}

func truncateToWholeSecondUTC(unixNano int64) int64 {
	return time.Unix(0, unixNano).UTC().Truncate(time.Second).UnixNano()
}
