package sync

import (
	"io/fs"

	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// canReuseBaselineHash reports whether a caller can safely trust the cached
// baseline hash for a local file instead of re-reading the file contents.
//
// Every fact compared here is a local observation recorded by this host, so
// each compares exactly (R-2.2.4). Whole-second truncation (R-6.7.15) belongs
// only where a local time meets OneDrive's whole-second wire value; applying
// it here hid sub-second edits.
//
// The contract matches the scanner's fast path:
//   - only files are eligible
//   - size and the full-precision mtime must still match the baseline
//   - the inode change time (ctime) must still match: `cp -p`, `touch -r`
//     and `rsync -t --inplace` rewrite bytes and restore the old mtime, but
//     no unprivileged call can restore ctime
//   - the filesystem identity (device, inode) must still match: a different
//     file renamed over the synced one with the same size and mtime is new
//     content the baseline hash says nothing about
//   - files inside the 1-second racily-clean window are never trusted
//
// A baseline without ctime or identity cannot prove any of this and is
// re-read; the outcome of that read records both.
//
// This keeps the engine's retry/reobserve paths aligned with the main local
// scanner so we do not hash aggressively in one path while trusting metadata
// in another.
func canReuseBaselineHash(info fs.FileInfo, base *BaselineEntry, observeStartNano int64) bool {
	if info == nil || base == nil || info.IsDir() || base.ItemType != ItemTypeFile || base.LocalHash == "" {
		return false
	}

	currentMtime := info.ModTime().UnixNano()
	if !base.LocalSizeKnown || info.Size() != base.LocalSize || currentMtime != base.LocalMtime {
		return false
	}

	currentCtime, hasCtime := synctree.ChangeTimeFromFileInfo(info)
	if !hasCtime || base.LocalCtime == 0 || currentCtime != base.LocalCtime {
		return false
	}

	if !sameBaselineIdentity(info, base) {
		return false
	}

	return observeStartNano-currentMtime >= nanosPerSecond && observeStartNano-currentCtime >= nanosPerSecond
}

// sameBaselineIdentity reports whether info is the inode the baseline
// recorded. A baseline without identity proves nothing.
func sameBaselineIdentity(info fs.FileInfo, base *BaselineEntry) bool {
	identity, hasIdentity := synctree.IdentityFromFileInfo(info)
	if !hasIdentity || !base.LocalHasIdentity {
		return false
	}

	return synctree.SameIdentity(identity, synctree.FileIdentity{Device: base.LocalDevice, Inode: base.LocalInode})
}

// fileChangeTime returns the ctime a local_state row records for info: the
// inode change time for a regular file, and zero (unknown) for anything else,
// because a folder's ctime moves with every child and means nothing here.
func fileChangeTime(info fs.FileInfo) int64 {
	if info == nil || !info.Mode().IsRegular() {
		return 0
	}
	ctime, ok := synctree.ChangeTimeFromFileInfo(info)
	if !ok {
		return 0
	}

	return ctime
}
