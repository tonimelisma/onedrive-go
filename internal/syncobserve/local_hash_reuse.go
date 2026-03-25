package syncobserve

import (
	"io/fs"

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
	if info.Size() != base.Size || currentMtime != base.Mtime {
		return false
	}

	return observeStartNano-currentMtime >= nanosPerSecond
}
