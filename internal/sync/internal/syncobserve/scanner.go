// Local observation's full scan and the scanner helpers it shares with the
// watch-mode handlers.
//
// Contents:
//   - FullScan:                orchestrates walk + hash phases → ScanResult
//   - hashPhase:               parallel hash computation for discovered files
//   - makeWalkFunc:            builds the filepath.WalkDir callback
//   - classifyObservedInfo:    classifies one observed entry against baseline
//   - computeStableHashWith:   double-stat hash for actively-written files
//   - shouldObserveWithFilter: unified observation filter (Stage 1: name + path)
//   - IsOversizedFile:         Stage 2 observation filter (file size > 250GB)
//   - validateOneDriveName:    returns reason + detail for invalid names
//
// Related files:
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
	stdsync "sync"
	"sync/atomic"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/sync/errgroup"

	"github.com/tonimelisma/onedrive-go/internal/driveops"
	"github.com/tonimelisma/onedrive-go/internal/localpath"
	"github.com/tonimelisma/onedrive-go/internal/namekey"
	"github.com/tonimelisma/onedrive-go/internal/panicguard"
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
	Events  []syncdomain.ChangeEvent
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

// defaultCheckWorkers is the default parallel hash goroutine limit when
// checkWorkers is zero (not configured).
const defaultCheckWorkers = 4

// hashJob describes a file that needs hashing during FullScan phase 2.
type hashJob struct {
	fsPath           string
	dbRelPath        string
	size             int64
	mtime            int64
	ctime            int64
	localDevice      uint64
	localInode       uint64
	localBirthTime   int64
	localHasIdentity bool
	isNew            bool // true for creates, false for modifies
}

// resolveCheckWorkers returns the effective check worker count.
func (o *LocalObserver) resolveCheckWorkers() int {
	if o.checkWorkers > 0 {
		return o.checkWorkers
	}

	return defaultCheckWorkers
}

// FullScan walks the sync root directory and returns a ScanResult holding a
// row for every entry on disk, the entries that are new or changed against
// the baseline, and any skipped items that should be recorded as observation
// issues. A synced item that is gone has no row.
//
// Three-phase design:
//  1. Walk (sequential): collect observed map and rows, list new folders,
//     classify files that need hashing into a hashJob slice. Collects
//     SkippedItems for invalid names, too-long paths, and too-large files.
//  2. Hash (parallel): errgroup.SetLimit(checkWorkers) hashes files concurrently.
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
	var events []syncdomain.ChangeEvent
	var jobs []hashJob
	var skipped []syncdomain.SkippedItem
	var skippedEntries atomic.Int64
	observed := make(map[string]bool)
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
			return ScanResult{}, fmt.Errorf("sync: local scan canceled: %w", ctx.Err())
		}

		return ScanResult{}, fmt.Errorf("sync: walking %s: %w", syncRoot, err)
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

	// Phase 3: Case collision detection, after hashing (events finalized).
	// Colliding entries are dropped from events and recorded as findings;
	// their rows stay, so a collider never reads as a local delete (R-2.12.1).
	var caseSkipped []syncdomain.SkippedItem
	events, caseSkipped = DetectCaseCollisions(events, o.baseline, observed)
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
		return syncdomain.ErrSyncRootMissing
	}
	if err := o.expectedRoot.Validate(tree); err != nil {
		o.logger.Debug("sync root identity changed, aborting scan",
			slog.String("sync_root", syncRoot),
			slog.Any("error", err))
		return err
	}
	return nil
}

// hashPhase runs hash jobs in parallel using errgroup with checkWorkers limit.
// Returns a row for every hashed file, an event for each one whose content
// differs from the baseline, plus any skipped items from panics in hash
// goroutines. Panics are recovered and
// converted to SkippedItem entries — a single corrupted file cannot crash the
// entire scan (defensive coding per eng philosophy).
//
//nolint:funlen // The hashing pipeline keeps local-scan concurrency and panic recovery in one explicit owner.
func (o *LocalObserver) hashPhase(
	ctx context.Context, jobs []hashJob,
) ([]syncdomain.ChangeEvent, []syncdomain.LocalStateRow, []syncdomain.SkippedItem, error) {
	workers := o.resolveCheckWorkers()

	o.logger.Debug("starting parallel hash phase",
		slog.Int("jobs", len(jobs)),
		slog.Int("workers", workers),
	)

	hashFn := o.hashFunc
	if hashFn == nil {
		hashFn = driveops.ComputeQuickXorHashContext
	}

	// mu guards the three result slices below. The walk fans out to workers
	// bounded by g.SetLimit, and each appends its own findings through skip and
	// keep, the only holders of mu, which release it with defer: a worker's
	// panic boundary records the file through skip, and would wait forever on
	// a lock a panic left held.
	var mu stdsync.Mutex
	var events []syncdomain.ChangeEvent
	var rows []syncdomain.LocalStateRow
	var skipped []syncdomain.SkippedItem
	skip := func(item syncdomain.SkippedItem) {
		mu.Lock()
		defer mu.Unlock()
		skipped = append(skipped, item)
	}
	keep := func(ev *syncdomain.ChangeEvent, row *syncdomain.LocalStateRow) {
		mu.Lock()
		defer mu.Unlock()
		if ev != nil {
			events = append(events, *ev)
		}
		rows = append(rows, *row)
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	recorder := newHashCacheRecorder(o.hashCache, o.logger)
	defer recorder.flush(ctx)

	for _, job := range jobs {
		g.Go(func() (retErr error) {
			// Recover from panics in hash computation (e.g., corrupt file
			// triggering a nil dereference in the hash library). Convert to
			// SkippedItem so the rest of the scan completes normally.
			defer func() {
				if r := recover(); r != nil {
					panicErr := panicguard.Recovered(o.logger, "while hashing "+job.dbRelPath, r,
						slog.String("path", job.dbRelPath))
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueHashPanic,
						Detail: panicErr.Error(),
					})
				}
			}()

			if gCtx.Err() != nil {
				return gCtx.Err()
			}

			hash, err := hashFn(gCtx, job.fsPath)
			if err != nil {
				// Debug: this repeats on every safety scan for a file that
				// stats but cannot be opened. An unreadable file is recorded
				// as a skipped item instead, reported once and in `status`.
				o.logger.Debug("hash computation failed, emitting event with empty hash",
					slog.String("path", job.dbRelPath), slog.Any("error", err))
				switch {
				case errors.Is(err, os.ErrPermission):
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueLocalReadDenied,
						Detail: "file not readable (check filesystem permissions)",
					})
				case !errors.Is(err, os.ErrNotExist):
					// An I/O error is not a content change: without the
					// issue the empty hash would read as "changed", and an
					// upload that failed the same way would be planned on
					// every pass (D-005). The issue makes the path
					// unavailable truth.
					skip(syncdomain.SkippedItem{
						Path:   job.dbRelPath,
						Reason: syncdomain.IssueHashPanic,
						Detail: fmt.Sprintf("could not read the file to check its content (%v)", err),
					})
				}
			}

			row := syncdomain.LocalStateRow{
				Path:             job.dbRelPath,
				ItemType:         syncdomain.ItemTypeFile,
				Hash:             hash,
				Size:             job.size,
				Mtime:            job.mtime,
				Ctime:            job.ctime,
				LocalDevice:      job.localDevice,
				LocalInode:       job.localInode,
				LocalBirthTime:   job.localBirthTime,
				LocalHasIdentity: job.localHasIdentity,
			}

			// For modifies: check if hash matches baseline (no real change).
			if !job.isNew && hash != "" {
				if existing, found := o.baseline.GetByPath(job.dbRelPath); found && hash == existing.LocalHash {
					keep(nil, &row)
					return nil
				}
			}

			recorder.addJob(gCtx, &job, hash)

			keep(&syncdomain.ChangeEvent{Path: job.dbRelPath, ItemType: syncdomain.ItemTypeFile}, &row)

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, fmt.Errorf("sync: hash phase: %w", err)
	}

	return events, rows, skipped, nil
}

// makeWalkFunc returns a WalkDirFunc that classifies filesystem entries
// against the baseline. Folder events are appended to events directly.
// Files that need hashing are appended to jobs for phase 2. User-actionable
// rejections are appended to skipped for engine recording.
func (o *LocalObserver) makeWalkFunc(
	ctx context.Context, tree *synctree.Root, observed map[string]bool, currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.ChangeEvent, jobs *[]hashJob, skipped *[]syncdomain.SkippedItem,
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
			return fmt.Errorf("sync: computing relative path for %s: %w", fsPath, err)
		}

		// Normalize: forward slashes for cross-platform consistency + NFC Unicode.
		dbRelPath := syncdomain.StoredPathOf(relPath)
		name := namekey.Stored(d.Name())

		if d.Type()&fs.ModeSymlink != 0 {
			if !syncdomain.NewContentFilter(o.filterConfig).ShouldFollowSymlinks() {
				o.rememberExcludedSymlink(dbRelPath)
				o.recordSyncedPathReplaced(skipped, dbRelPath, "a symbolic link")
				observed[dbRelPath] = true
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
				// until the user deals with it; never delete it (P-4).
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
	events *[]syncdomain.ChangeEvent, jobs *[]hashJob, skipped *[]syncdomain.SkippedItem, scanStartNano int64,
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
		entries[dbRelPath] = current
		return true, nil
	default:
		availability.dropEntry(first, dbRelPath, currentRows)
		entries[dbRelPath] = first
		currentRows[dbRelPath] = baselineLocalStateRow(&base)
		return false, current.skipError()
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

// withoutDroppedHashJobs removes the jobs of twins admitSyncedEntry set
// aside: a file twin's own job and every job below a folder twin.
func withoutDroppedHashJobs(jobs []hashJob, availability *scanAvailability) []hashJob {
	if len(availability.droppedHashJobs) == 0 && len(availability.droppedTrees) == 0 {
		return jobs
	}
	kept := jobs[:0]
	for i := range jobs {
		if !availability.hashJobDropped(jobs[i].fsPath) {
			kept = append(kept, jobs[i])
		}
	}

	return kept
}

func (o *LocalObserver) processObservedInfo(
	fsPath, dbRelPath string,
	info fs.FileInfo,
	kind syncdomain.ObservedKind,
	observed map[string]bool,
	currentRows map[string]syncdomain.LocalStateRow,
	events *[]syncdomain.ChangeEvent,
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
			observed[dbRelPath] = true
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
		observed[dbRelPath] = true
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
			observed[dbRelPath] = true
			o.logger.Debug("not crossing into another filesystem", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
		if o.flaggedMountIsGone(o.scanAvailability, dbRelPath) {
			*skipped = append(*skipped, VanishedMountSkippedItem(dbRelPath))
			observed[dbRelPath] = true
			o.logger.Debug("a crossed mount point is no longer mounted", slog.String("path", dbRelPath))
			return filepath.SkipDir
		}
	}

	observed[dbRelPath] = true
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
	events *[]syncdomain.ChangeEvent,
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
			*events = append(*events, syncdomain.ChangeEvent{Path: dbRelPath, ItemType: syncdomain.ItemTypeFolder})
		} else if hash, ok := o.cachedHashForNewFile(dbRelPath, info, &observedRow, scanStartNano); ok {
			// A scan before this one hashed the file and recorded it; the
			// file has not changed since by every fact the fast path checks.
			*events = append(*events, syncdomain.ChangeEvent{Path: dbRelPath, ItemType: syncdomain.ItemTypeFile})
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

// DetectCaseCollisions finds events where two paths in the same directory
// differ only in case. Both colliders are removed from events and returned
// as SkippedItems. OneDrive uses a case-insensitive namespace — uploading
// both would cause one to silently overwrite the other (R-2.12.1).
//
// observed is the set of paths this scan found on disk. A baseline variant
// that is not on disk any more is the old name of a case-only rename (or a
// local delete), not a collision; the planner orders the remote side so the
// old item is gone before the new name is created (R-2.12.1). A nil observed
// set treats every baseline variant as present.
//
// O(n) time, O(n) memory. Pure function — no side effects.
func DetectCaseCollisions(
	events []syncdomain.ChangeEvent, baseline syncdomain.BaselineView, observed map[string]bool,
) (clean []syncdomain.ChangeEvent, collisions []syncdomain.SkippedItem) {
	if len(events) == 0 {
		return nil, nil
	}

	// Group event indices by (directory, OneDrive namespace key): case,
	// Unicode form and ignored emoji all make two local names one remote one.
	groups := make(map[caseGroupKey][]int, len(events))
	for i := range events {
		key := caseGroupKey{dir: filepath.Dir(events[i].Path), nameKey: namekey.Of(filepath.Base(events[i].Path))}
		groups[key] = append(groups[key], i)
	}

	// Build the collider set — all indices that participate in a collision.
	colliderSet := make(map[int]struct{})
	for _, indices := range groups {
		if len(indices) > 1 {
			for _, idx := range indices {
				colliderSet[idx] = struct{}{}
			}
		}
	}

	// Cross-check single-event groups against baseline.
	crossCheckBaseline(events, groups, baseline, observed, colliderSet)

	// Suppress children of colliding directories.
	childColliderSet, collidingDirPrefixes := suppressDirectoryChildren(events, colliderSet)

	if len(colliderSet) == 0 {
		return events, nil
	}

	// Build SkippedItems with Detail naming the other collider(s).
	collisions = buildCollisionSkippedItems(
		events, groups, colliderSet, childColliderSet, collidingDirPrefixes, baseline, observed)

	// Build clean events — those not in the collider set.
	clean = make([]syncdomain.ChangeEvent, 0, len(events)-len(colliderSet))
	for i := range events {
		if _, collider := colliderSet[i]; !collider {
			clean = append(clean, events[i])
		}
	}

	return clean, collisions
}

// caseGroupKey groups events by (directory, namespace key) for collision detection.
type caseGroupKey struct {
	dir     string
	nameKey namekey.Key
}

// crossCheckBaseline flags single-event groups that collide with already-synced
// baseline entries. A new file whose lowercased name matches a baseline entry
// with different exact casing is a collision (the baseline file produced no event
// because it was unchanged by the fast-path content-change check).
func crossCheckBaseline(
	events []syncdomain.ChangeEvent,
	groups map[caseGroupKey][]int,
	baseline syncdomain.BaselineView,
	observed map[string]bool,
	colliderSet map[int]struct{},
) {
	if baseline == nil {
		return
	}

	for key, indices := range groups {
		if len(indices) != 1 {
			continue
		}

		if _, already := colliderSet[indices[0]]; already {
			continue
		}

		ev := &events[indices[0]]
		variants := baseline.GetCaseVariants(key.dir, filepath.Base(ev.Path))

		for i := range variants {
			if v := &variants[i]; v.Path != ev.Path && baselineVariantPresent(v.Path, observed) {
				colliderSet[indices[0]] = struct{}{}

				break
			}
		}
	}
}

// baselineVariantPresent reports whether a baseline case variant still exists
// on disk in this scan. Without an observed set every variant counts.
func baselineVariantPresent(path string, observed map[string]bool) bool {
	return observed == nil || observed[path]
}

// suppressDirectoryChildren marks children of colliding directories as colliders.
// They can't be uploaded to a folder that won't exist on OneDrive.
func suppressDirectoryChildren(
	events []syncdomain.ChangeEvent, colliderSet map[int]struct{},
) (childColliderSet map[int]struct{}, collidingDirPrefixes []string) {
	childColliderSet = make(map[int]struct{})

	for idx := range colliderSet {
		if events[idx].ItemType == syncdomain.ItemTypeFolder {
			collidingDirPrefixes = append(collidingDirPrefixes, events[idx].Path+"/")
		}
	}

	for i := range events {
		if _, already := colliderSet[i]; already {
			continue
		}

		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(events[i].Path, prefix) {
				colliderSet[i] = struct{}{}
				childColliderSet[i] = struct{}{}

				break
			}
		}
	}

	return childColliderSet, collidingDirPrefixes
}

// buildCollisionSkippedItems constructs SkippedItems with Detail messages for
// event-vs-event collisions, baseline cross-check collisions, and child collisions.
func buildCollisionSkippedItems(
	events []syncdomain.ChangeEvent,
	groups map[caseGroupKey][]int,
	colliderSet, childColliderSet map[int]struct{},
	collidingDirPrefixes []string,
	baseline syncdomain.BaselineView,
	observed map[string]bool,
) []syncdomain.SkippedItem {
	collisions := make([]syncdomain.SkippedItem, 0, len(colliderSet))

	// Event-vs-event and baseline collisions.
	for _, indices := range groups {
		if len(indices) <= 1 {
			collisions = appendSingleGroupCollision(
				collisions, events, indices, colliderSet, childColliderSet, baseline, observed)

			continue
		}

		collisions = appendMultiGroupCollisions(
			collisions, events, indices, childColliderSet)
	}

	// Child collisions — distinct Detail indicating the parent directory collision.
	for idx := range childColliderSet {
		ev := &events[idx]

		parentDir := ""
		for _, prefix := range collidingDirPrefixes {
			if strings.HasPrefix(ev.Path, prefix) {
				parentDir = strings.TrimSuffix(prefix, "/")

				break
			}
		}

		collisions = append(collisions, syncdomain.SkippedItem{
			Path:   ev.Path,
			Reason: syncdomain.IssueCaseCollision,
			Detail: fmt.Sprintf("parent directory %q has a case collision",
				filepath.Base(parentDir)),
		})
	}

	return collisions
}

// appendSingleGroupCollision handles SkippedItem construction for a group with
// exactly one event (flagged by baseline cross-check).
func appendSingleGroupCollision(
	collisions []syncdomain.SkippedItem,
	events []syncdomain.ChangeEvent,
	indices []int,
	colliderSet, childColliderSet map[int]struct{},
	baseline syncdomain.BaselineView,
	observed map[string]bool,
) []syncdomain.SkippedItem {
	idx := indices[0]

	if _, flagged := colliderSet[idx]; !flagged {
		return collisions
	}

	if _, isChild := childColliderSet[idx]; isChild {
		return collisions // handled in child pass
	}

	ev := &events[idx]

	if baseline == nil {
		return collisions
	}

	variants := baseline.GetCaseVariants(filepath.Dir(ev.Path), filepath.Base(ev.Path))
	for i := range variants {
		if v := &variants[i]; v.Path != ev.Path && baselineVariantPresent(v.Path, observed) {
			return append(collisions, syncdomain.SkippedItem{
				Path:   ev.Path,
				Reason: syncdomain.IssueCaseCollision,
				Detail: collisionDetail("conflicts with synced file", filepath.Base(ev.Path), []string{filepath.Base(v.Path)}),
			})
		}
	}

	return collisions
}

// collisionDetail names the other colliding file(s) and what to do. When the
// names differ in characters OneDrive ignores when comparing names (emoji and
// anything else beyond U+FFFF), the visible difference would leave the user
// puzzled about why `a.txt` and `a😀.txt` collide, so the detail says why.
func collisionDetail(prefix, name string, others []string) string {
	detail := prefix + " " + strings.Join(others, ", ")
	for _, other := range others {
		if namekey.DiffersInIgnoredCharacters(name, other) {
			return fmt.Sprintf("%s: OneDrive ignores emoji and other characters outside the Basic Multilingual Plane "+
				"when it compares names, so %q and %q are one name there; rename one of them", detail, name, other)
		}
	}

	if len(others) == 0 {
		return detail
	}

	return fmt.Sprintf("%s: OneDrive treats %q and %q as the same name; rename one of them", detail, name, others[0])
}

// sameUnicodeFormCollisionDetail is the finding's detail for two entries of
// one folder whose names differ only in Unicode form: stored paths are NFC
// (R-2.13.1), so both have one path, and naming the other would print the
// same name twice.
const sameUnicodeFormCollisionDetail = "another file here has the same name in a different Unicode form " +
	"(for example é typed as e plus an accent); rename one of them"

// appendMultiGroupCollisions handles SkippedItem construction for groups with
// multiple events (event-vs-event collisions).
func appendMultiGroupCollisions(
	collisions []syncdomain.SkippedItem,
	events []syncdomain.ChangeEvent,
	indices []int,
	childColliderSet map[int]struct{},
) []syncdomain.SkippedItem {
	seen := make(map[string]bool, len(indices))
	for i, idx := range indices {
		if _, isChild := childColliderSet[idx]; isChild {
			continue
		}

		var others []string
		sameForm := false
		for j, otherIdx := range indices {
			if j == i {
				continue
			}
			if events[otherIdx].Path == events[idx].Path {
				sameForm = true
				continue
			}
			others = append(others, filepath.Base(events[otherIdx].Path))
		}

		if seen[events[idx].Path] {
			continue // the same normalized path is reported once
		}
		seen[events[idx].Path] = true

		detail := collisionDetail("conflicts with", filepath.Base(events[idx].Path), others)
		if sameForm {
			detail = sameUnicodeFormCollisionDetail
			if len(others) > 0 {
				detail += "; it also conflicts with " + strings.Join(others, ", ")
			}
		}

		collisions = append(collisions, syncdomain.SkippedItem{
			Path:   events[idx].Path,
			Reason: syncdomain.IssueCaseCollision,
			Detail: detail,
		})
	}

	return collisions
}

// ---------------------------------------------------------------------------
// File hashing
// ---------------------------------------------------------------------------

// computeStableHash hashes a file and verifies it was not modified during the
// hash operation by comparing pre/post stat results. Returns ErrFileChangedDuringHash
// if the file changed (B-119). Caller-specific handling: handleWrite skips
// (Write events guarantee a follow-up), handleCreate and scanNewDirectory emit
// with empty hash (Create events and directory scans have no guaranteed follow-up; B-203).
//
// The double os.Stat is intentional: pre-stat captures baseline metadata,
// post-stat detects changes that occurred during hashing. The caller's earlier
// stat cannot substitute because time may pass between the caller's stat and
// the hash operation.
func computeStableHashWith(
	ctx context.Context,
	fsPath string,
	hashFunc func(ctx context.Context, path string) (string, error),
) (string, error) {
	pre, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat before hashing: %w", err)
	}

	if hashFunc == nil {
		hashFunc = driveops.ComputeQuickXorHashContext
	}

	hash, err := hashFunc(ctx, fsPath)
	if err != nil {
		return "", fmt.Errorf("compute quickxor hash: %w", err)
	}

	post, err := trustedStat(fsPath)
	if err != nil {
		return "", fmt.Errorf("stat after hashing: %w", err)
	}

	// ctime too: an in-place rewrite that restores the old mtime during the
	// read leaves size and mtime equal, and the hash would describe neither
	// version (R-2.2.4).
	if pre.Size() != post.Size() || pre.ModTime() != post.ModTime() || syncdomain.FileChangeTime(pre) != syncdomain.FileChangeTime(post) {
		return "", syncdomain.ErrFileChangedDuringHash
	}

	return hash, nil
}

func trustedStat(path string) (os.FileInfo, error) {
	file, err := localpath.OpenRegular(path)
	if err != nil {
		// No path here: the *os.PathError already names it.
		return nil, fmt.Errorf("open trusted path: %w", err)
	}

	info, statErr := file.Stat()
	closeErr := file.Close()
	if statErr != nil {
		if closeErr != nil {
			return nil, errors.Join(statErr, closeErr)
		}

		return nil, fmt.Errorf("stat trusted path: %w", statErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("closing %s: %w", path, closeErr)
	}

	return info, nil
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

// validateOneDriveName checks whether a filename is valid for OneDrive.
// Returns ("", "") for valid names. For invalid names, returns the issue
// type constant and a human-readable detail string. The detail never repeats
// the name: every caller prints it next to the path ("Not syncing a/CON: the
// name is reserved by Windows"), and %q-quoting the name again doubled any
// backslash in it.
//
// Checks (ordered by specificity): empty name, trailing dot/space, leading
// space, reserved device names, reserved patterns, invalid characters. A
// name is never refused for its length: OneDrive limits the path, which
// counts the name (oneDrivePathLength, R-2.11.12).
func validateOneDriveName(name string) (reason syncdomain.IssueType, detail string) {
	return validateOneDriveNameFor(name, false)
}

// validateOneDriveNameFor applies the name rules of the target drive type.
// personal relaxes the Business/SharePoint-only reservations (lockfiles,
// desktop.ini, "~$" owner files) that OneDrive Personal accepts.
func validateOneDriveNameFor(name string, personal bool) (reason syncdomain.IssueType, detail string) {
	if name == "" {
		return syncdomain.IssueInvalidFilename, "the name is empty"
	}

	// Graph would reinterpret invalid bytes (0xFF arrives as "ÿ"), and the
	// renamed remote item would then rename the local file to match.
	if !utf8.ValidString(name) {
		return syncdomain.IssueInvalidFilename, "the name is not valid UTF-8"
	}

	if containsControlChar(name) {
		return syncdomain.IssueInvalidFilename, "the name contains a control character (such as a tab or newline)"
	}

	if name[len(name)-1] == '.' {
		return syncdomain.IssueInvalidFilename, "the name ends with a period"
	}

	if name[len(name)-1] == ' ' {
		return syncdomain.IssueInvalidFilename, "the name ends with a space"
	}

	if name[0] == ' ' {
		return syncdomain.IssueInvalidFilename, "the name starts with a space"
	}

	lower := strings.ToLower(name)

	if isReservedDeviceName(lower) {
		return syncdomain.IssueInvalidFilename, "the name is reserved by Windows"
	}

	if strings.Contains(lower, "_vti_") {
		return syncdomain.IssueInvalidFilename, `OneDrive reserves names containing "_vti_"`
	}

	if !personal && isReservedPattern(name, lower) {
		return syncdomain.IssueInvalidFilename, "OneDrive for work or school reserves this name"
	}

	if containsInvalidChars(name) {
		return syncdomain.IssueInvalidFilename, `the name contains a character OneDrive does not allow (" * : < > ? / \ |)`
	}

	return "", ""
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

func validateObservedName(name, path string, rules syncdomain.LocalObservationRules) (reason syncdomain.IssueType, detail string) {
	if reason, detail := validateOneDriveNameFor(name, rules.PersonalNameRules); reason != "" {
		return reason, detail
	}

	if rules.RejectSharePointRootForms && isSharePointRootForms(name, path) {
		return syncdomain.IssueInvalidFilename, "the name is reserved at the root of a SharePoint library"
	}

	return "", ""
}

func isSharePointRootForms(name, path string) bool {
	return path == name && strings.EqualFold(name, "forms")
}

// isReservedDeviceName returns true for Windows reserved device names
// (case-insensitive): CON, PRN, AUX, NUL, COM0-COM9, LPT0-LPT9, with or
// without an extension. OneDrive rejects "con.txt" and "nul.tar.gz" exactly
// like "con", so only the part before the first period matters.
func isReservedDeviceName(lower string) bool {
	if base, _, found := strings.Cut(lower, "."); found {
		lower = base
	}

	switch lower {
	case "con", "prn", "aux", "nul":
		return true
	}

	// COM0-COM9, LPT0-LPT9: exactly 4 characters, prefix + single digit.
	if len(lower) == deviceNameWithDigitLen &&
		(strings.HasPrefix(lower, "com") || strings.HasPrefix(lower, "lpt")) {
		digit := lower[3]
		return digit >= '0' && digit <= '9'
	}

	return false
}

// containsControlChar reports C0 control characters and DEL, which Graph
// rejects in names.
func containsControlChar(name string) bool {
	for _, c := range name {
		if c < 0x20 || c == 0x7f {
			return true
		}
	}

	return false
}

// isReservedPattern returns true for the names OneDrive for work or school
// reserves: .lock extension, desktop.ini, ~$ prefix (Office temp). "_vti_" is
// checked separately because every drive type rejects it.
func isReservedPattern(name, lower string) bool {
	if strings.HasSuffix(lower, ".lock") {
		return true
	}

	if lower == "desktop.ini" {
		return true
	}

	return strings.HasPrefix(name, "~$")
}

// containsInvalidChars returns true if the name contains characters
// forbidden by OneDrive: " * : < > ? / \ |
func containsInvalidChars(name string) bool {
	for _, c := range name {
		switch c {
		case '"', '*', ':', '<', '>', '?', '/', '\\', '|':
			return true
		}
	}

	return false
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

// oneDrivePathLength counts a path the way OneDrive's 400-character limit
// does: in UTF-16 code units, the unit its error messages and its 260-unit
// emoji names measure (L-05). Counting UTF-8 bytes skipped valid paths: a
// 150-character CJK path is 450 bytes, and 300 "ä" are 600.
func oneDrivePathLength(path string) int {
	return len(utf16.Encode([]rune(path)))
}
