// Local observation's full scan and the scanner helpers it shares with the
// watch-mode handlers.
//
// Contents:
//   - FullScan:                orchestrates walk + hash phases → ScanResult
//   - makeWalkFunc:            builds the filepath.WalkDir callback
//   - classifyObservedInfo:    classifies one observed entry against baseline
//   - shouldObserveWithFilter: unified observation filter (Stage 1: name + path)
//   - IsOversizedFile:         Stage 2 observation filter (file size > 250GB)
//
// Related files:
//   - scanner_hash.go:            the hash phase and stable hashing
//   - scanner_collisions.go:      case and Unicode-form collision detection
//   - scanner_names.go:           OneDrive name and path validation
//   - observer_local.go:          LocalObserver struct and Watch() entry point
//   - observer_local_handlers.go: watch-mode event handlers

package syncobserve

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncdomain"
	"github.com/tonimelisma/onedrive-go/internal/sync/syncshortcut"
	"github.com/tonimelisma/onedrive-go/internal/synctree"
)

// ScanResult is the return type of FullScan. Rows are the whole current local
// snapshot that local_state persists: a path with no row is absent. Events
// list the entries that are new or changed against the baseline, by path and
// kind, with case collisions removed; they are not planner input. Skipped are
// user-actionable rejections (invalid names, path too long, file too large)
// that the engine should record.
type ScanResult struct {
	Events  []syncdomain.LocalEntry
	Rows    []syncdomain.LocalStateRow
	Skipped []syncdomain.SkippedItem
	// CrossedMounts are mount points this scan crossed under
	// cross_filesystems; the engine remembers them (mount_boundaries).
	CrossedMounts []string
}

// Constants for the local scanner.
const (
	deviceNameWithDigitLen = 4 // COM0-COM9, LPT0-LPT9 have exactly 4 characters

	// maxOneDrivePathLength is the maximum total path length OneDrive states,
	// in characters as oneDrivePathLength counts them. It is the only length
	// limit on a name: OneDrive has none for a single name (R-2.11.12). The
	// server is still the authority: it refused a path of 365 units below the
	// drive root (L-36), and its refusal maps to path_too_long (R-2.11.7).
	maxOneDrivePathLength = 400
	// maxOneDriveFileSize is the maximum file size OneDrive allows (250 GB).
	maxOneDriveFileSize = driveops.MaxOneDriveFileSize
	// bytesPerGB renders the oversized-file detail in GB (1024^3 bytes).
	bytesPerGB = 1 << 30
)

// FullScan walks the sync root directory and returns a ScanResult holding a
// row for every entry on disk, the entries that are new or changed against
// the baseline, and any skipped items that should be recorded as observation
// issues. A synced item that is gone has no row.
//
// Three-phase design:
//  1. Walk (sequential): collect observed map and rows, list new folders,
//     classify files that need hashing into a hashJob slice. Collects
//     SkippedItems for invalid names, too-long paths, and too-large files.
//  2. Hash (parallel): at most checkWorkers clock-started workers hash files concurrently.
//  3. Case collision detection (sequential) over the new and changed entries.
func (o *LocalObserver) FullScan(ctx context.Context, tree *synctree.Root) (ScanResult, error) {
	syncRoot := tree.Path()
	o.logger.Info("local observer starting full scan",
		slog.String("sync_root", syncRoot),
		slog.Int("baseline_entries", o.baseline.Len()),
	)

	if err := o.validateFullScanRoot(tree, syncRoot); err != nil {
		return ScanResult{}, err
	}
	rootDevice, err := rootDeviceForScan(tree)
	if err != nil {
		return ScanResult{}, err
	}
	o.scanAvailability = newScanAvailability(rootDevice)
	o.scanAvailability.tree = tree
	if flags := o.mountBoundaryFlags.Load(); flags != nil {
		o.scanAvailability.mountBoundaryFlags = *flags
	}
	defer func() { o.scanAvailability = nil }()

	// Phase 1: Walk — collect observed paths, folder events, hash jobs, and skipped items.
	var events []syncdomain.LocalEntry
	var jobs []hashJob
	var skipped []syncdomain.SkippedItem
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
	// names is every entry the walk accepts, under its on-disk spelling:
	// the collision phase compares new and changed entries against it.
	names := make(ScanNames)
	o.scanNames = names
	defer func() { o.scanNames = nil }()
	currentRows := make(map[string]syncdomain.LocalStateRow)
	scanStartNano := o.clock.Now().UnixNano()
	dirStack := rootObservedDirStack(syncRoot, o.logger)

	walkFn := o.makeWalkFunc(
		ctx,
		tree,
		observed,
		currentRows,
		&events,
		&jobs,
		&skipped,
		&skippedEntries,
		scanStartNano,
		dirStack,
	)
	if err := tree.WalkDir(walkFn); err != nil {
		if ctx.Err() != nil {
			return ScanResult{}, fmt.Errorf("syncobserve: local scan canceled: %w", ctx.Err())
		}

		return ScanResult{}, fmt.Errorf("syncobserve: walking %s: %w", syncRoot, err)
	}

	// Debug: unreadable entries are recorded as skipped items, which the
	// engine reports once when first found and `status` lists.
	if n := skippedEntries.Load(); n > 0 {
		o.logger.Debug("full scan: skipped entries due to walk errors",
			slog.Int64("count", n),
			slog.String("sync_root", syncRoot))
	}

	// Phase 2: Hash — parallel file hashing. Panics in hash goroutines are
	// recovered and converted to SkippedItems (defensive coding).
	jobs = withoutDroppedHashJobs(jobs, o.scanAvailability)
	if len(jobs) > 0 {
		hashEvents, hashRows, hashSkipped, err := o.hashPhase(ctx, jobs)
		if err != nil {
			return ScanResult{}, err
		}

		events = append(events, hashEvents...)
		for i := range hashRows {
			currentRows[hashRows[i].Path] = hashRows[i]
		}
		skipped = append(skipped, hashSkipped...)
	}

	// Phase 3: Case collision detection, after hashing (events finalized),
	// against every name the walk found on disk.
	// Colliding entries are dropped from events and recorded as findings;
	// their rows stay, so a collider never reads as a local delete (R-2.12.1).
	var caseSkipped []syncdomain.SkippedItem
	events, caseSkipped = DetectCaseCollisions(events, names)
	skipped = append(skipped, caseSkipped...)

	o.logger.Info("local observer completed full scan",
		slog.Int("rows", len(currentRows)),
		slog.Int("observed", len(observed)),
		slog.Int("hashed", len(jobs)),
		slog.Int("skipped", len(skipped)),
	)

	return ScanResult{
		Events:        events,
		Rows:          sortedLocalStateRows(currentRows),
		Skipped:       skipped,
		CrossedMounts: append([]string(nil), o.scanAvailability.crossedMounts...),
	}, nil
}

func (o *LocalObserver) validateFullScanRoot(tree *synctree.Root, syncRoot string) error {
	// Guard: abort if the sync root directory does not exist. Without this,
	// WalkDir silently succeeds with zero events (walkFn's SkipEntry returns
	// filepath.SkipDir for the root error, so WalkDir returns nil).
	if !SyncRootExists(syncRoot) {
		// Debug: the returned error is reported once by the caller.
		o.logger.Debug("sync root missing, aborting scan",
			slog.String("sync_root", syncRoot))
		return errSyncRootMissing
	}
	if err := o.expectedRoot.Validate(tree); err != nil {
		o.logger.Debug("sync root identity changed, aborting scan",
			slog.String("sync_root", syncRoot),
			slog.Any("error", err))
		return err
	}
	return nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline. Folder events are appended to events directly.
// Files that need hashing are appended to jobs for phase 2. User-actionable
// rejections are appended to skipped for engine recording.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, tree *synctree.Root, observed map[string]bool, currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.LocalEntry, jobs *[]hashJob, skipped *[]syncdomain.SkippedItem,
	skippedEntries *atomic.Int64, scanStartNano int64, dirStack map[string]struct{},
) fs.WalkDirFunc {
	syncRoot := tree.Path()

	return func(fsPath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil && fsPath == syncRoot {
			// The sync folder itself could not be read (EIO on a dying disk,
			// a stale network mount). A read boundary would be recorded at
			// ".", which no path lies under, so every synced file would look
			// deleted. Nothing about the tree is known: stop the scan (D-005).
			return fmt.Errorf("%w: reading the sync folder: %w", syncdomain.ErrMountRootUnavailable, walkErr)
		}
		if walkErr != nil {
			// Debug: a persistent unreadable folder would warn on every
			// scan; the skipped item below is the single user-facing report.
			o.logger.Debug("walk error", slog.String("path", fsPath), slog.Any("error", walkErr))
			// Any unreadable entry other than one that vanished mid-walk is a
			// read boundary: an I/O error must not make content look deleted
			// any more than a permission error does.
			if relPath, err := tree.Rel(fsPath); err == nil {
				recordUnreadableObservedBoundary(skipped, syncdomain.StoredPathOf(relPath), walkErr, unreadableDirectoryDetail)
			}
			skippedEntries.Add(1)
			return skipEntry(d)
		}

		if ctx.Err() != nil {
			return ctx.Err()
		}

		// Skip the sync root directory itself.
		if fsPath == syncRoot {
			return nil
		}

		relPath, err := tree.Rel(fsPath)
		if err != nil {
			return fmt.Errorf("syncobserve: computing relative path for %s: %w", fsPath, err)
		}

		// Normalize: forward slashes for cross-platform consistency + NFC Unicode.
		dbRelPath := syncdomain.StoredPathOf(relPath)
		name := namekey.Stored(d.Name())

		if d.Type()&fs.ModeSymlink != 0 {
			if !syncdomain.NewContentFilter(o.filterConfig).ShouldFollowSymlinks() {
				o.rememberExcludedSymlink(dbRelPath)
				o.recordSyncedPathReplaced(skipped, dbRelPath, "a symbolic link")
				o.observeSetAside(observed, dbRelPath, fsPath)
				o.logger.Debug("skipping symlink", slog.String("path", dbRelPath))
				return skipEntry(d)
			}

			return o.processSymlinkPath(
				ctx,
				fsPath,
				dbRelPath,
				name,
				observed,
				currentRows,
				events,
				jobs,
				skipped,
				scanStartNano,
				dirStack,
			)
		}

		o.forgetExcludedSymlink(dbRelPath)

		// Stage 1 observation filter: name validation + path length (cheap, no syscall).
		if skipItem := shouldObserveWithFilter(
			name,
			dbRelPath,
			dirEntryKind(d),
			o.filterConfig,
			o.protectedRoots,
			o.observationRules,
		); skipItem != nil {
			if skipItem.Reason != "" {
				*skipped = append(*skipped, *skipItem)
				o.logger.Debug("skipping invalid entry",
					slog.String("path", dbRelPath),
					slog.String("reason", string(skipItem.Reason)))
			} else {
				o.logger.Debug("skipping excluded entry", slog.String("path", dbRelPath))
			}
			if synctree.IsDetachArtifactName(name) && d.Type().IsRegular() {
				// A file set aside by a removal a crash interrupted: user
				// content left under a hidden name. Report it every scan
				// until the user deals with it; never delete it (R-6.4.9).
				// Only a regular file is ever set aside, so a folder or a
				// special file with such a name is the user's own: it stays
				// excluded like every entry with that name, unreported.
				*skipped = append(*skipped, syncdomain.SkippedItem{
					Path:   dbRelPath,
					Reason: syncdomain.IssueLocalInterruptedRemoval,
					Detail: "sync set this file aside while removing it and was interrupted; " +
						"rename it back to keep it, or delete it",
				})
			}
			// A synced item renamed to this skipped name (invalid, too long,
			// excluded) is still the synced item: keep its synced path
			// unavailable instead of letting it read as deleted.
			o.recordSyncedItemMovedOutOfReach(skipped, o.scanAvailability, dbRelPath, d)

			return skipEntry(d)
		}

		return o.processEntry(fsPath, dbRelPath, d, observed, currentRows, events, jobs, skipped, scanStartNano)
	}
}

// processEntry reads file info, marks the path as observed, and classifies
// the local change against the baseline. Folder events are appended to
// events directly; files that need hashing are appended to jobs for phase 2.
// Stage 2 observation filter: file size > 250GB is checked here (after stat).
func (o *LocalObserver) processEntry(
	fsPath, dbRelPath string, d fs.DirEntry, observed map[string]bool,
	currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.LocalEntry, jobs *[]hashJob, skipped *[]syncdomain.SkippedItem, scanStartNano int64,
) error {
	info, err := d.Info()
	if err != nil {
		if errors.Is(err, os.ErrPermission) {
			*skipped = append(*skipped, syncdomain.SkippedItem{
				Path:   dbRelPath,
				Reason: syncdomain.IssueLocalReadDenied,
				Detail: "file not accessible (check filesystem permissions)",
			})
		} else {
			// Anything but "it vanished" (an I/O error, a stale network
			// handle) is unknown, not absent (D-005).
			recordUnreadableEntry(skipped, dbRelPath, err)
		}
		// File disappeared between readdir and stat (an expected race), or
		// is unreadable (recorded above) -- skip and continue.
		o.logger.Debug("stat failed (file may have disappeared)",
			slog.String("path", dbRelPath), slog.Any("error", err))
		return nil
	}

	if admitted, skipErr := o.admitSyncedEntry(fsPath, dbRelPath, info, currentRows, skipped); !admitted {
		return skipErr
	}

	return o.processObservedInfo(
		fsPath,
		dbRelPath,
		info,
		dirEntryKind(d),
		observed,
		currentRows,
		events,
		jobs,
		skipped,
		scanStartNano,
	)
}

// admitSyncedEntry reports whether the walk classifies an entry as the entry
// of its stored path. A second file or folder whose name maps to the stored
// path of a synced item (the same name in another Unicode form) is a local
// name collision in which neither entry is chosen (R-2.13.2): the case-
// collision finding blocks the path until one of the two is gone, and the
// path's row keeps describing the synced item.
//
// The synced item is the entry of the baseline's kind; for a file it must
// also be the inode the baseline recorded, so of two files the synced one
// keeps the row with its own change, if any. When neither entry is the
// synced file, the row keeps the baseline's facts. The other entry's size,
// time and hash never become the row: its hash jobs are dropped, and a folder
// twin is not walked, or, walked first, loses the rows found below it. Two
// folders with one stored path are not handled here. Symlinks, special files,
// mounts and protected roots never reach here, and a stored path with no
// synced item is the collision check's case. The error is filepath.SkipDir
// when the refused entry is a folder.
func (o *LocalObserver) admitSyncedEntry(
	fsPath, dbRelPath string, info fs.FileInfo, currentRows map[string]syncdomain.LocalStateRow, skipped *[]syncdomain.SkippedItem,
) (bool, error) {
	if !info.Mode().IsRegular() && !info.IsDir() {
		return true, nil
	}
	base, found := o.baseline.GetByPath(dbRelPath)
	if !found || (base.ItemType != syncdomain.ItemTypeFile && base.ItemType != syncdomain.ItemTypeFolder) {
		return true, nil
	}

	availability := o.scanAvailability
	entries := availability.syncedEntries
	current := syncedEntry{fsPath: fsPath, isDir: info.IsDir(), isSynced: isSyncedEntry(info, &base)}
	first, seen := entries[dbRelPath]
	if !seen {
		entries[dbRelPath] = current
		return true, nil
	}
	if first.isDir && current.isDir && base.ItemType == syncdomain.ItemTypeFolder {
		return true, nil
	}

	if !first.twinReported {
		*skipped = append(*skipped, syncdomain.SkippedItem{
			Path:   dbRelPath,
			Reason: syncdomain.IssueCaseCollision,
			Detail: sameUnicodeFormCollisionDetail,
		})
		o.logger.Debug("a second entry has a synced item's name in another Unicode form",
			slog.String("path", dbRelPath))
	}
	first.twinReported = true
	current.twinReported = true

	switch {
	case first.isSynced:
		entries[dbRelPath] = first
		return false, current.skipError()
	case current.isSynced:
		availability.dropEntry(first, dbRelPath, currentRows)
		o.forgetScannedName(dbRelPath, first.fsPath)
		entries[dbRelPath] = current
		return true, nil
	default:
		availability.dropEntry(first, dbRelPath, currentRows)
		o.forgetScannedName(dbRelPath, first.fsPath)
		entries[dbRelPath] = first
		currentRows[dbRelPath] = baselineLocalStateRow(&base)
		return false, current.skipError()
	}
}

// observeSetAside marks an entry the walk sets aside (an unfollowed symlink,
// a special file, a protected root, a mount boundary) as present. When the
// path holds a synced item, its spelling on disk also stays among the names
// the collision phase compares against: OneDrive still holds that item under
// that spelling, so a new case or normalization variant beside it is a
// collision, not a fresh name whose upload would land in the synced item.
func (o *LocalObserver) observeSetAside(observed map[string]bool, dbRelPath, fsPath string) {
	observed[dbRelPath] = true
	if o.scanNames == nil {
		return
	}
	if _, synced := o.baseline.GetByPath(dbRelPath); synced {
		o.scanNames.Add(dbRelPath, filepath.Base(fsPath))
	}
}

// forgetScannedName takes an entry the walk set aside out of the names the
// collision phase compares against: the twin finding above already reports
// its path, and a set-aside entry is not one OneDrive will receive.
func (o *LocalObserver) forgetScannedName(dbRelPath, fsPath string) {
	if o.scanNames != nil {
		o.scanNames.removeSpelling(dbRelPath, filepath.Base(fsPath))
	}
}

// isSyncedEntry reports whether info can be the synced item base records:
// an entry of its kind, and for a file the inode the baseline recorded.
func isSyncedEntry(info fs.FileInfo, base *syncdomain.BaselineEntry) bool {
	if base.ItemType == syncdomain.ItemTypeFolder {
		return info.IsDir()
	}

	return info.Mode().IsRegular() && sameBaselineIdentity(info, base)
}

// baselineLocalStateRow is the local row the last sync recorded for an item.
func baselineLocalStateRow(base *syncdomain.BaselineEntry) syncdomain.LocalStateRow {
	return syncdomain.LocalStateRow{
		Path:             base.Path,
		ItemType:         base.ItemType,
		Hash:             base.LocalHash,
		Size:             base.LocalSize,
		Mtime:            base.LocalMtime,
		Ctime:            base.LocalCtime,
		LocalDevice:      base.LocalDevice,
		LocalInode:       base.LocalInode,
		LocalBirthTime:   base.LocalBirthTime,
		LocalHasIdentity: base.LocalHasIdentity,
	}
}

func (o *LocalObserver) processObservedInfo(
	fsPath, dbRelPath string,
	info fs.FileInfo,
	kind syncdomain.ObservedKind,
	observed map[string]bool,
	currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.LocalEntry,
	jobs *[]hashJob,
	skipped *[]syncdomain.SkippedItem,
	scanStartNano int64,
) error {
	if protectedRoot, ok := syncshortcut.ProtectedRootIdentityReservation(dbRelPath, info, o.protectedRoots); ok {
		o.reportProtectedRootEvent(syncshortcut.ProtectedRootEvent{
			Type:         syncshortcut.ProtectedRootEventIdentityMatch,
			Path:         dbRelPath,
			ReservedPath: protectedRoot.Path,
			MountID:      protectedRoot.MountID,
			BindingID:    protectedRoot.BindingID,
		})
		if kind == syncdomain.ObservedKindDir {
			identity := protectedRoot.FileIdentity()
			if currentIdentity, hasIdentity := synctree.IdentityFromFileInfo(info); hasIdentity {
				identity = currentIdentity
			}
			o.observeSetAside(observed, dbRelPath, fsPath)
			currentRows[dbRelPath] = syncdomain.LocalStateRow{
				Path:             dbRelPath,
				ItemType:         syncdomain.ItemTypeFolder,
				Size:             info.Size(),
				Mtime:            info.ModTime().UnixNano(),
				LocalDevice:      identity.Device,
				LocalInode:       identity.Inode,
				LocalBirthTime:   identity.BirthTime,
				LocalHasIdentity: true,
			}
		}
		o.logger.Debug("skipping protected root identity match",
			slog.String("path", dbRelPath),
			slog.String("reserved_path", protectedRoot.Path))
		return nil
	}

	// FIFOs, sockets and devices have no content to sync, and opening a FIFO
	// to hash it would block until a writer appears. Marking the path
	// observed keeps deletion detection from treating it as removed.
	if isSpecialFile(info) {
		o.recordSyncedPathReplaced(skipped, dbRelPath, "a FIFO, socket or device file")
		o.observeSetAside(observed, dbRelPath, fsPath)
		o.logger.Debug("skipping special file",
			slog.String("path", dbRelPath),
			slog.String("mode", info.Mode().Type().String()))
		return nil
	}

	// Stage 2 observation filter: file size check (requires stat, hence here).
	// FullScan records SkippedItems for oversized files; watch handlers don't
	// (the safety scan catches them).
	if kind == syncdomain.ObservedKindFile && o.IsOversizedFile(info.Size(), dbRelPath) {
		*skipped = append(*skipped, syncdomain.SkippedItem{
			Path:     dbRelPath,
			Reason:   syncdomain.IssueFileTooLarge,
			Detail:   fmt.Sprintf("file is %d GB; OneDrive allows at most 250 GB", info.Size()/bytesPerGB),
			FileSize: info.Size(),
		})
		return nil
	}

	if kind == syncdomain.ObservedKindDir {
		identity, hasIdentity := synctree.IdentityFromFileInfo(info)
		if o.isForeignMountPoint(o.scanAvailability, dbRelPath, identity, hasIdentity) {
			*skipped = append(*skipped, MountBoundarySkippedItem(dbRelPath))
			o.observeSetAside(observed, dbRelPath, fsPath)
			o.logger.Debug("not crossing into another filesystem", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
		if o.flaggedMountIsGone(o.scanAvailability, dbRelPath) {
			*skipped = append(*skipped, VanishedMountSkippedItem(dbRelPath))
			o.observeSetAside(observed, dbRelPath, fsPath)
			o.logger.Debug("a crossed mount point is no longer mounted", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
	}

	observed[dbRelPath] = true
	if o.scanNames != nil {
		o.scanNames.Add(dbRelPath, filepath.Base(fsPath))
	}
	row := syncdomain.LocalStateRow{
		Path:  dbRelPath,
		Mtime: info.ModTime().UnixNano(),
	}
	row.RecordIdentity(info)
	switch kind {
	case syncdomain.ObservedKindDir:
		row.ItemType = syncdomain.ItemTypeFolder
		row.Size = info.Size()
		currentRows[dbRelPath] = row
	case syncdomain.ObservedKindFile:
		row.ItemType = syncdomain.ItemTypeFile
		row.Size = info.Size()
		row.Ctime = syncdomain.FileChangeTime(info)
		currentRows[dbRelPath] = row
	case syncdomain.ObservedKindUnknown:
		return fmt.Errorf("walk observed entry %s: unknown observed kind", dbRelPath)
	}

	return o.classifyObservedInfo(fsPath, dbRelPath, info, kind, currentRows, events, jobs, scanStartNano)
}

// classifyObservedInfo determines the change type for a single observed local
// entry by comparing it against the baseline. Folder events go directly to
// events; files that need hashing are appended to jobs for the parallel hash
// phase.
func (o *LocalObserver) classifyObservedInfo(
	fsPath, dbRelPath string,
	info fs.FileInfo,
	kind syncdomain.ObservedKind,
	currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.LocalEntry,
	jobs *[]hashJob,
	scanStartNano int64,
) error {
	var existing *syncdomain.BaselineEntry
	if baselineEntry, found := o.baseline.GetByPath(dbRelPath); found {
		existing = &baselineEntry
	}

	// No baseline entry — this is a new item.
	if existing == nil {
		observedRow := currentRows[dbRelPath]
		if kind == syncdomain.ObservedKindDir {
			// A new folder goes directly to events (no hashing needed).
			*events = append(*events, syncdomain.LocalEntry{Path: dbRelPath, ItemType: syncdomain.ItemTypeFolder})
		} else if hash, ok := o.cachedHashForNewFile(dbRelPath, info, &observedRow, scanStartNano); ok {
			// A scan before this one hashed the file and recorded it; the
			// file has not changed since by every fact the fast path checks.
			*events = append(*events, syncdomain.LocalEntry{Path: dbRelPath, ItemType: syncdomain.ItemTypeFile})
			currentRows[dbRelPath] = syncdomain.LocalStateRow{
				Path:             dbRelPath,
				ItemType:         syncdomain.ItemTypeFile,
				Hash:             hash,
				Size:             info.Size(),
				Mtime:            info.ModTime().UnixNano(),
				Ctime:            observedRow.Ctime,
				LocalDevice:      observedRow.LocalDevice,
				LocalInode:       observedRow.LocalInode,
				LocalBirthTime:   observedRow.LocalBirthTime,
				LocalHasIdentity: observedRow.LocalHasIdentity,
			}
		} else {
			// New file — needs hashing in phase 2.
			*jobs = append(*jobs, hashJob{
				fsPath:           fsPath,
				dbRelPath:        dbRelPath,
				size:             info.Size(),
				mtime:            info.ModTime().UnixNano(),
				ctime:            observedRow.Ctime,
				localDevice:      observedRow.LocalDevice,
				localInode:       observedRow.LocalInode,
				localBirthTime:   observedRow.LocalBirthTime,
				localHasIdentity: observedRow.LocalHasIdentity,
				isNew:            true,
			})
		}

		return nil
	}

	// Existing folder — OS-level mtime changes (e.g. adding a file) are noise;
	// the contained files generate their own events.
	if kind == syncdomain.ObservedKindDir {
		return nil
	}

	return o.detectFileContentChange(fsPath, dbRelPath, info, existing, currentRows, jobs, scanStartNano)
}

// detectFileContentChange compares a file against its baseline entry to detect
// content modifications. Uses mtime+size as a fast path — only adds a hash
// job when metadata suggests a change. This is the industry standard
// (rsync, rclone, Syncthing, Git all use this pattern). Includes a
// racily-clean guard: files whose mtime is within 1 second of scan start
// are always hashed, because they may have been modified in the same clock
// tick as the last sync (Git's "racily clean" problem).
func (o *LocalObserver) detectFileContentChange(
	fsPath, dbRelPath string, info fs.FileInfo, base *syncdomain.BaselineEntry,
	currentRows map[string]syncdomain.LocalStateRow,
	jobs *[]hashJob, scanStartNano int64,
) error {
	currentMtime := info.ModTime().UnixNano()
	currentSize := info.Size()
	observedRow := currentRows[dbRelPath]

	if CanReuseBaselineHash(info, base, scanStartNano) {
		o.logger.Debug("fast path: mtime+size match, skipping hash",
			slog.String("path", dbRelPath))
		currentRows[dbRelPath] = syncdomain.LocalStateRow{
			Path:             dbRelPath,
			ItemType:         syncdomain.ItemTypeFile,
			Hash:             base.LocalHash,
			Size:             currentSize,
			Mtime:            currentMtime,
			Ctime:            observedRow.Ctime,
			LocalDevice:      observedRow.LocalDevice,
			LocalInode:       observedRow.LocalInode,
			LocalBirthTime:   observedRow.LocalBirthTime,
			LocalHasIdentity: observedRow.LocalHasIdentity,
		}

		return nil
	}

	if base.LocalSizeKnown && currentSize == base.LocalSize && currentMtime == base.LocalMtime {
		o.logger.Debug("size and mtime match but ctime, identity or the racily-clean window force a hash check",
			slog.String("path", dbRelPath))
	}

	// Slow path: metadata differs (or racily clean) — queue for hash phase.
	*jobs = append(*jobs, hashJob{
		fsPath:           fsPath,
		dbRelPath:        dbRelPath,
		size:             currentSize,
		mtime:            currentMtime,
		ctime:            observedRow.Ctime,
		localDevice:      observedRow.LocalDevice,
		localInode:       observedRow.LocalInode,
		localBirthTime:   observedRow.LocalBirthTime,
		localHasIdentity: observedRow.LocalHasIdentity,
		isNew:            false,
	})

	return nil
}

func sortedLocalStateRows(current map[string]syncdomain.LocalStateRow) []syncdomain.LocalStateRow {
	paths := make([]string, 0, len(current))
	for path := range current {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	rows := make([]syncdomain.LocalStateRow, 0, len(paths))
	for _, path := range paths {
		rows = append(rows, current[path])
	}

	return rows
}

// ---------------------------------------------------------------------------
// Unified observation filter
// ---------------------------------------------------------------------------

// IsOversizedFile returns true if the file exceeds the OneDrive 250 GB size
// limit. Logs a debug message when skipping. This is Stage 2 of the two-stage
// observation filter — requires a stat result, so it runs after stat.
func (o *LocalObserver) IsOversizedFile(size int64, path string) bool {
	if size > maxOneDriveFileSize {
		o.logger.Debug("skipping oversized file",
			slog.String("path", path),
			slog.Int64("size", size))
		return true
	}
	return false
}

func shouldObserveWithFilter(
	name, path string,
	kind syncdomain.ObservedKind,
	filter syncdomain.ContentFilterConfig,
	protectedRoots []syncshortcut.ProtectedRoot,
	rules syncdomain.LocalObservationRules,
) *syncdomain.SkippedItem {
	normalizedPath := strings.TrimPrefix(filepath.ToSlash(path), "/")
	if _, found := syncshortcut.ProtectedRootPathReservation(normalizedPath, protectedRoots); found {
		return &syncdomain.SkippedItem{}
	}

	if !syncdomain.NewContentFilter(filter).ShouldObserveLocalPath(path, kind) {
		return &syncdomain.SkippedItem{}
	}

	if reason, detail := validateObservedName(name, path, rules); reason != "" {
		return &syncdomain.SkippedItem{Path: path, Reason: reason, Detail: detail}
	}

	if pathLength := oneDrivePathLength(path); pathLength > maxOneDrivePathLength {
		return &syncdomain.SkippedItem{
			Path:   path,
			Reason: syncdomain.IssuePathTooLong,
			Detail: fmt.Sprintf("path length %d exceeds %d-character limit", pathLength, maxOneDrivePathLength),
		}
	}

	return nil
}

// isSpecialFile reports a non-directory that is not a regular file: a FIFO,
// socket or device. Symlinks are classified before this, on the link itself.
func isSpecialFile(info fs.FileInfo) bool {
	return !info.IsDir() && !info.Mode().IsRegular()
}

func dirEntryKind(d fs.DirEntry) syncdomain.ObservedKind {
	if d.IsDir() {
		return syncdomain.ObservedKindDir
	}

	return syncdomain.ObservedKindFile
}

// ---------------------------------------------------------------------------
// Filtering and validation helpers
// ---------------------------------------------------------------------------

// SyncRootExists returns true if the sync root directory exists and is a directory.
func SyncRootExists(syncRoot string) bool {
	tree, err := synctree.Open(syncRoot)
	if err != nil {
		return false
	}

	info, err := tree.Stat(".")
	return err == nil && info.IsDir()
}

// skipEntry returns filepath.SkipDir for directories (to skip the subtree)
// or nil for files (to continue the walk with the next entry).
func skipEntry(d fs.DirEntry) error {
	if d != nil && d.IsDir() {
		return filepath.SkipDir
	}

	return nil
}

// BuildLocalStateRows converts an observation scan into the rows local_state
// persists. It belongs to observation rather than the store: observation is
// what produces the snapshot, and putting the conversion in the store made the
// store's file family depend on an observation type.
func BuildLocalStateRows(result ScanResult) []syncdomain.LocalStateRow {
	rows := make([]syncdomain.LocalStateRow, len(result.Rows))
	copy(rows, result.Rows)
	return rows
}

// errSyncRootMissing is returned when the sync root directory does not exist
// or is not a directory.
var errSyncRootMissing = errors.New("sync folder does not exist")
